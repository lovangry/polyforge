package runtime

// Spawn regressions: the birth isolation profile (four flags), the env
// sanitize point (POLYFORGE_*/PF_* prefixes + renamed secret values), the
// spawn journal, and the real RPC envelope end-to-end against a scripted
// harness process (prompt commands in, ack extracted from the assistant
// message_end record).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func TestSpawnIsolationFlags(t *testing.T) {
	cfg := Config{PiBin: "/nonexistent-pi"}
	sp := newPiSpawner(cfg)
	cmd := sp.buildCmd(SpawnParams{
		WorkItemID: "wi_iso", CapsuleDir: "/caps", SessionID: "s1", Generation: 7,
	})
	args := cmd.Args
	if args[0] != "/nonexistent-pi" {
		t.Fatalf("argv[0] must be the harness binary, got %v", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--mode rpc", "--no-extensions", "--no-skills", "--no-context-files", "--no-prompt-templates"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv must contain %q, got %v", want, args)
		}
	}
	if cmd.Dir != "/caps" {
		t.Fatalf("cwd must be the capsule root, got %q", cmd.Dir)
	}
}

func TestSpawnResumeAddsSessionArg(t *testing.T) {
	sp := newPiSpawner(Config{})
	cmd := sp.buildCmd(SpawnParams{TranscriptPath: "/caps/session/t.jsonl", Resume: true, CapsuleDir: "/caps"})
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--session /caps/session/t.jsonl") {
		t.Fatalf("resume derivation must pass the transcript, got %v", cmd.Args)
	}
	fresh := sp.buildCmd(SpawnParams{CapsuleDir: "/caps"})
	if strings.Contains(strings.Join(fresh.Args, " "), "--session") {
		t.Fatal("fresh derivation must not pass --session")
	}
}

func TestSpawnEnvSanitized(t *testing.T) {
	t.Setenv("POLYFORGE_API_KEY", "machine-key")
	t.Setenv("PF_LEAK", "leak")
	t.Setenv("PLAIN_VAR", "keep-me")
	t.Setenv("SOME_OTHER_NAME", "machine-key") // renamed secret: value match
	sp := newPiSpawner(Config{EnvSecrets: []string{"machine-key"}})
	cmd := sp.buildCmd(SpawnParams{WorkItemID: "wi_env", CapsuleDir: "/caps", SessionID: "s", Generation: 1})
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "POLYFORGE_API_KEY") || strings.Contains(env, "PF_LEAK=") {
		t.Fatalf("POLYFORGE_*/PF_* must be stripped, got:\n%s", env)
	}
	if strings.Contains(env, "SOME_OTHER_NAME=machine-key") {
		t.Fatal("a renamed variable carrying the configured secret must be stripped")
	}
	if !strings.Contains(env, "PLAIN_VAR=keep-me") {
		t.Fatal("the blacklist must not strip unrelated variables")
	}
	for _, want := range []string{"PF_WI_ID=wi_env", "PF_SESSION_ID=s", "PI_SESSION_DIR=/caps/session", "PF_GENERATION=1"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env must carry %q, got:\n%s", want, env)
		}
	}
}

func TestEnvSanitizerUnit(t *testing.T) {
	t.Setenv("POLYFORGE_URL", "https://x")
	t.Setenv("PF_TOKEN", "y")
	t.Setenv("KEEP", "z")
	e := newEnvSanitizer("the-secret", "")
	t.Setenv("RENAMED_SECRET", "the-secret")
	env := e.childEnv("PF_WI_ID=w")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "POLYFORGE_URL") || strings.Contains(joined, "PF_TOKEN") || strings.Contains(joined, "RENAMED_SECRET") {
		t.Fatalf("sanitize failed:\n%s", joined)
	}
	if !strings.Contains(joined, "KEEP=z") || !strings.Contains(joined, "PF_WI_ID=w") {
		t.Fatalf("sanitize over-stripped:\n%s", joined)
	}
}

func TestSpawnJournalWritten(t *testing.T) {
	capDir := t.TempDir()
	p := SpawnParams{WorkItemID: "wi_j", CapsuleDir: capDir, SessionID: "s", TranscriptPath: "/t"}
	if err := writeSpawnJournal(p, 4242, "then", "ra_wi_j"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(capDir, capsuleDirRuntimeState, "spawn-journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(raw[:len(raw)-1], &entry); err != nil {
		t.Fatalf("journal line must be JSON: %v (%s)", err, raw)
	}
	if entry["pid"].(float64) != 4242 || entry["instance"] != "ra_wi_j" || entry["work_item"] != "wi_j" {
		t.Fatalf("journal entry wrong: %+v", entry)
	}
	// appends, not truncates
	if err := writeSpawnJournal(p, 4243, "then2", "ra_wi_j#1"); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(filepath.Join(capDir, capsuleDirRuntimeState, "spawn-journal.jsonl"))
	if n := strings.Count(string(raw2), "\n"); n != 2 {
		t.Fatalf("journal must append, found %d lines", n)
	}
}

// TestPiRPCEnvelopeAndAck drives the real protocol layer against a
// scripted harness process: the daemon→agent control line rides a prompt
// command (streamingBehavior steer) whose message text is the control JSON
// VERBATIM, and the agent's ack is extracted from the assistant
// message_end record.
func TestPiRPCEnvelopeAndAck(t *testing.T) {
	capDir := t.TempDir()
	capture := filepath.Join(capDir, "stdin-capture.jsonl")
	t.Setenv("GO_BE_HARNESS", "1")
	t.Setenv("GO_BE_CAPTURE", capture)
	// PiBin points at a wrapper that re-invokes THIS test binary with only
	// the helper test selected; the spawner's argv (--mode rpc ...) rides
	// after `--` so the test framework's flag parsing ignores it.
	t.Setenv("GO_TESTBIN", os.Args[0])
	wrapper := filepath.Join(capDir, "fake-pi")
	script := "#!/bin/sh\nexec \"$GO_TESTBIN\" -test.run '^TestHelperProcess$' -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	sp := newPiSpawner(Config{PiBin: wrapper})
	if err := os.MkdirAll(filepath.Join(capDir, "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:    "wi_rpc",
		CapsuleDir:    capDir,
		SessionID:     "s1",
		Generation:    1,
		InitialPrompt: "begin the work item",
		Journal:       func(pid int, startedAt string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Terminate(3 * time.Second) }()

	if err := h.SendControl(protocol.ControlMsg{Method: protocol.ControlProbe, Nonce: "n-1"}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	if err := h.WaitControlAck(context.Background(), protocol.ControlProbe, 5*time.Second); err != nil {
		t.Fatalf("WaitControlAck: %v", err)
	}

	// the harness captured the raw stdin lines: assert the envelope
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var controlLine string
	for _, l := range lines {
		if strings.Contains(l, "pf/probe") {
			controlLine = l
		}
	}
	if controlLine == "" {
		t.Fatalf("the control prompt never reached the harness: %q", raw)
	}
	var cmd struct {
		ID                string `json:"id"`
		Type              string `json:"type"`
		Message           string `json:"message"`
		StreamingBehavior string `json:"streamingBehavior"`
	}
	if err := json.Unmarshal([]byte(controlLine), &cmd); err != nil {
		t.Fatalf("stdin line must be a prompt command: %v (%s)", err, controlLine)
	}
	if cmd.Type != "prompt" || cmd.ID == "" || cmd.StreamingBehavior != "steer" {
		t.Fatalf("prompt command envelope wrong: %+v", cmd)
	}
	var ctrl protocol.ControlMsg
	if err := json.Unmarshal([]byte(cmd.Message), &ctrl); err != nil ||
		ctrl.Method != protocol.ControlProbe || ctrl.Nonce != "n-1" {
		t.Fatalf("control line must ride the message text verbatim: %v (%s)", err, cmd.Message)
	}
}

// ---------------------------------------------------------------------------
// the scripted harness: a re-invocation of the test binary (GO_BE_HARNESS)
// that echoes ack records onto stdout for the probe control method and
// captures every stdin line for the assertions above.

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_BE_HARNESS") != "1" {
		return // the normal test run: nothing to do
	}
	capture := os.Getenv("GO_BE_CAPTURE")
	f, err := os.OpenFile(capture, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		os.Exit(3)
	}
	defer f.Close()
	br := bufio.NewReader(os.Stdin)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			fmt.Fprintln(f, strings.TrimRight(line, "\n"))
		}
		if strings.Contains(line, protocol.ControlProbe) {
			// the exact ack shape: a protocol.ControlAck inside the
			// assistant message_end record's text, extracted by the
			// supervisor's classifier
			ack := `{"method":"pf/probe","ok":true}`
			rec := fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, ack)
			os.Stdout.WriteString(rec + "\n")
		}
		if err != nil {
			os.Exit(0) // stdin EOF: graceful shutdown
		}
	}
}

const capsuleDirRuntimeState = "runtime-state"
