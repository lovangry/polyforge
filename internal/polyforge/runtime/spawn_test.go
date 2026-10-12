package runtime

// Spawn regressions: the birth isolation profile (three flags — the
// extension system stays reachable because the subagent tool is
// extension-provided, per the v0 live-tested baseline), the env sanitize
// point (POLYFORGE_*/PF_* prefixes + renamed secret values + the
// deliberate PF_DAEMON_SOCKET addition), the spawn journal, the RecordTap
// binding, and the real RPC envelope end-to-end against a scripted harness
// process (prompt commands in, ack routed to its waiter by
// (method, correlation)).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/spawn"
)

func TestSpawnIsolationFlags(t *testing.T) {
	sp := newPiSpawner(&Config{PiBin: "/nonexistent-pi"})
	cmd := sp.buildCmd(SpawnParams{
		WorkItemID: "wi_iso", CapsuleDir: "/caps", SessionHandle: "ab12cd34", SpawnN: 7,
	})
	args := cmd.Args
	if args[0] != "/nonexistent-pi" {
		t.Fatalf("argv[0] must be the harness binary, got %v", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--mode rpc", "--no-skills", "--no-context-files", "--no-prompt-templates"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv must contain %q, got %v", want, args)
		}
	}
	// --no-extensions is DELIBERATELY ABSENT (the v0 baseline): it would
	// remove the harness-native subagent tool the executor depends on.
	if strings.Contains(joined, "--no-extensions") {
		t.Fatal("argv must NOT contain --no-extensions")
	}
	if cmd.Dir != "/caps" {
		t.Fatalf("cwd must be the capsule root, got %q", cmd.Dir)
	}
}

func TestSpawnResumeAddsSessionArg(t *testing.T) {
	sp := newPiSpawner(&Config{})
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
	// the retired PF_AGENT_ROLE / PF_DAEMON_SOCKET markers must be
	// stripped with the rest of the inherited PF_* family and never
	// re-injected.
	t.Setenv("PF_AGENT_ROLE", "spoofed-role")
	t.Setenv("PF_CHAT_ID", "chat_leak") // a chat id is a notification label: never reaches a step agent
	t.Setenv("PF_DAEMON_SOCKET", "/spoofed.sock")
	t.Setenv("SOME_OTHER_NAME", "machine-key") // renamed secret: value match
	sp := newPiSpawner(&Config{EnvSecrets: []string{"machine-key"}})
	cmd := sp.buildCmd(SpawnParams{WorkItemID: "wi_env", CapsuleDir: "/caps", SessionHandle: "ab12cd34", SpawnN: 2})
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "POLYFORGE_API_KEY") || strings.Contains(env, "PF_LEAK=") {
		t.Fatalf("POLYFORGE_*/PF_* must be stripped, got:\n%s", env)
	}
	if strings.Contains(env, "PF_CHAT_ID") {
		t.Fatalf("PF_CHAT_ID must be stripped, got:\n%s", env)
	}
	if strings.Contains(env, "SOME_OTHER_NAME=machine-key") {
		t.Fatal("a renamed variable carrying the configured secret must be stripped")
	}
	if !strings.Contains(env, "PLAIN_VAR=keep-me") {
		t.Fatal("the blacklist must not strip unrelated variables")
	}
	for _, want := range []string{
		"PF_WI_ID=wi_env", "PI_SESSION_DIR=/caps/session",
		// v1.5 B2/B3: the PUBLIC session handle and the monotonic
		// derivation counter
		"PF_SESSION_HANDLE=ab12cd34",
		"PF_SPAWN_N=2",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env must carry %q, got:\n%s", want, env)
		}
	}
}

func TestSpawnEnvRetiredVarsNeverInjected(t *testing.T) {
	sp := newPiSpawner(&Config{})
	cmd := sp.buildCmd(SpawnParams{WorkItemID: "wi_env", CapsuleDir: "/caps"})
	env := strings.Join(cmd.Env, "\n")
	// the deleted identity/role/socket vars must never come back
	for _, gone := range []string{"PF_INSTANCE_ID", "PF_SESSION_ID", "PF_GENERATION", "PF_DAEMON_SOCKET", "PF_AGENT_ROLE"} {
		if strings.Contains(env, gone) {
			t.Fatalf("the deleted %s must not be injected, got:\n%s", gone, env)
		}
	}
}

// TestSpawnSessionIdentityEnvReachesChild drives the REAL spawn path
// (scripted harness) and proves the child's environment carries the v1.5
// identity pair — PF_SESSION_HANDLE (the public fingerprint) and
// PF_SPAWN_N (the derivation counter) — and NOT the session secret.
func TestSpawnSessionIdentityEnvReachesChild(t *testing.T) {
	capDir, sp := scriptedHarness(t)
	envDump := filepath.Join(capDir, "child-env.txt")
	t.Setenv("GO_BE_ENV", envDump)
	secret := fakeSession("wi_inst")
	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:    "wi_inst",
		CapsuleDir:    capDir,
		SessionHandle: api.SessionFingerprint(secret),
		SpawnN:        2,
		InitialPrompt: "begin",
		Journal:       func(pid int, startedAt string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Terminate(3 * time.Second) }()

	raw, err := os.ReadFile(envDump)
	for deadline := time.Now().Add(5 * time.Second); (err != nil || len(raw) == 0) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		raw, err = os.ReadFile(envDump)
	}
	if err != nil {
		t.Fatalf("the harness never dumped its env: %v", err)
	}
	fp := api.SessionFingerprint(secret)
	if !strings.Contains(string(raw), `"PF_SESSION_HANDLE=`+fp+`"`) {
		t.Fatalf("child env must carry the public session handle, got:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"PF_SPAWN_N=2"`) {
		t.Fatalf("child env must carry the derivation counter, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "PF_AGENT_ROLE") {
		t.Fatalf("the retired role marker must not reach the child, got:\n%s", raw)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the session SECRET must never reach the child env, got:\n%s", raw)
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
	p := SpawnParams{WorkItemID: "wi_j", CapsuleDir: capDir, SessionHandle: "ab12cd34", SpawnN: 1, TranscriptPath: "/t"}
	if err := writeSpawnJournal(p, 4242, "then", "ab12cd34"); err != nil {
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
	if entry["pid"].(float64) != 4242 || entry["spawn_token"] != "ab12cd34" || entry["work_item"] != "wi_j" {
		t.Fatalf("journal entry wrong: %+v", entry)
	}
	// v1.5 additive fields: the derivation counter and the PUBLIC handle
	if entry["spawn_n"].(float64) != 1 || entry["session_handle"] != "ab12cd34" {
		t.Fatalf("journal entry must carry spawn_n and session_handle: %+v", entry)
	}
	// the SECRET never lands in the capsule journal (B3)
	if _, ok := entry["session_id"]; ok {
		t.Fatalf("the journal must not carry a session_id key: %+v", entry)
	}
	// appends, not truncates — and the second line's spawn_n is the max
	if err := writeSpawnJournal(p, 4243, "then2", "ab12cd34#1"); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(filepath.Join(capDir, capsuleDirRuntimeState, "spawn-journal.jsonl"))
	if n := strings.Count(string(raw2), "\n"); n != 2 {
		t.Fatalf("journal must append, found %d lines", n)
	}
	if got := maxSpawnNFromJournal(capDir); got != 1 {
		t.Fatalf("maxSpawnNFromJournal = %d, want 1", got)
	}
}

// maxSpawnNFromJournal: the B2 fresh-clone source — the highest recorded
// spawn_n wins, missing/unparsable journals answer 0.
func TestMaxSpawnNFromJournal(t *testing.T) {
	capDir := t.TempDir()
	if got := maxSpawnNFromJournal(capDir); got != 0 {
		t.Fatalf("absent journal must answer 0, got %d", got)
	}
	path := filepath.Join(capDir, capsuleDirRuntimeState, "spawn-journal.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []byte(
		`{"work_item":"wi","spawn_n":1,"pid":1}` + "\n" +
			`{"work_item":"wi","spawn_n":5,"pid":2}` + "\n" +
			`{"work_item":"wi","pid":3}` + "\n" + // pre-v1.5 line: no spawn_n
			`not json` + "\n")
	if err := os.WriteFile(path, lines, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := maxSpawnNFromJournal(capDir); got != 5 {
		t.Fatalf("maxSpawnNFromJournal = %d, want 5", got)
	}
}

// TestPiRPCInitialPrompt: the initial prompt rides a prompt command
// ({"id":"pf-init","type":"prompt","message":..}) on the harness's stdin.
func TestPiRPCInitialPrompt(t *testing.T) {
	capDir, sp := scriptedHarness(t)
	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID: "wi_rpc", CapsuleDir: capDir, SessionHandle: "ab12cd34", SpawnN: 1,
		InitialPrompt: "begin the work item",
		Journal:       func(pid int, startedAt string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Terminate(3 * time.Second) }()
	var raw []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		raw, _ = os.ReadFile(filepath.Join(capDir, "stdin-capture.jsonl"))
		if len(raw) > 0 {
			break
		}
	}
	var cmd struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &cmd); err != nil {
		t.Fatalf("stdin line must be a prompt command: %v (%q)", err, raw)
	}
	if cmd.Type != "prompt" || cmd.ID != "pf-init" || cmd.Message != "begin the work item" {
		t.Fatalf("initial prompt envelope wrong: %+v", cmd)
	}
}

// TestSpawnOnRecordFeedsEveryStdoutRecord pins the OnRecord binding: every
// raw stdout record the production spawner reads also reaches the
// dispatch's callback — the driver binds it to the harness translator, so
// the supervisor's per-dispatch state receives exactly this line.
func TestSpawnOnRecordFeedsEveryStdoutRecord(t *testing.T) {
	capDir, sp := scriptedHarness(t)

	var mu sync.Mutex
	var seen []string
	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:    "wi_tap",
		CapsuleDir:    capDir,
		SessionHandle: "ab12cd34",
		SpawnN:        1,
		InitialPrompt: "begin",
		Journal:       func(pid int, startedAt string) error { return nil },
		OnRecord: func(line string) {
			mu.Lock()
			seen = append(seen, line)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Terminate(3 * time.Second) }()

	// the scripted harness writes one fixture record at derivation; the
	// tap must see it verbatim
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("OnRecord never saw the startup record, saw %q", seen)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(seen[0], `"type":"agent_start"`) {
		t.Fatalf("tap must see the raw record verbatim, got %q", seen[0])
	}
}

// TestSpawnExitInfoCarriesCodeStderrAndTailRecords pins the process-exit
// signal: a harness that writes its last record and an error line to
// stderr and exits non-zero is announced through OnExit AFTER its stdout
// drained (the tail record reached the tap first) and ExitInfo reports the
// code and the stderr tail — the raw material of the driver's
// exited_without_result classification.
func TestSpawnExitInfoCarriesCodeStderrAndTailRecords(t *testing.T) {
	capDir, sp := scriptedHarness(t)
	t.Setenv("GO_BE_DIE", "model sub2api/nope not found")

	var mu sync.Mutex
	var order []string
	var exitInfo spawn.ExitInfo
	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:    "wi_exit",
		CapsuleDir:    capDir,
		SessionHandle: "ab12cd34",
		SpawnN:        1,
		InitialPrompt: "begin",
		Journal:       func(pid int, startedAt string) error { return nil },
		OnRecord: func(line string) {
			mu.Lock()
			order = append(order, "record")
			mu.Unlock()
		},
		OnExit: func(info spawn.ExitInfo) {
			mu.Lock()
			order = append(order, "exit")
			exitInfo = info
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !h.Done() {
		if time.Now().After(deadline) {
			t.Fatal("the dying harness never reported Done")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 3 || order[len(order)-1] != "exit" {
		t.Fatalf("OnExit must come after every record: %v", order)
	}
	if exitInfo.Code != 7 || !strings.Contains(exitInfo.StderrTail, "model sub2api/nope not found") {
		t.Fatalf("exit info = %+v, want code 7 and the stderr tail", exitInfo)
	}
	er, ok := h.(spawn.ExitReporter)
	if !ok {
		t.Fatal("a harness process must implement spawn.ExitReporter")
	}
	if got := er.ExitInfo(); got.Code != 7 {
		t.Fatalf("ExitInfo() after Done = %+v", got)
	}
}

// scriptedHarness builds a piSpawner whose PiBin is a wrapper re-invoking
// THIS test binary as the harness (TestHelperProcess): the harness emits one
// fixture record at derivation and captures every stdin line.
func scriptedHarness(t *testing.T) (string, *piSpawner) {
	t.Helper()
	capDir := t.TempDir()
	t.Setenv("GO_BE_HARNESS", "1")
	t.Setenv("GO_BE_CAPTURE", filepath.Join(capDir, "stdin-capture.jsonl"))
	// PiBin points at a wrapper that re-invokes THIS test binary with only
	// the helper test selected; the spawner's argv (--mode rpc ...) rides
	// after `--` so the test framework's flag parsing ignores it.
	t.Setenv("GO_TESTBIN", os.Args[0])
	wrapper := filepath.Join(capDir, "fake-pi")
	script := "#!/bin/sh\nexec \"$GO_TESTBIN\" -test.run '^TestHelperProcess$' -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(capDir, "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	return capDir, newPiSpawner(&Config{PiBin: wrapper})
}

// ---------------------------------------------------------------------------
// the scripted harness: a re-invocation of the test binary (GO_BE_HARNESS)
// that emits one fixture record at derivation, echoes correlation-carrying
// ack records onto stdout for the pf/* control methods and captures every
// stdin line for the assertions above.

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_BE_HARNESS") != "1" {
		return // the normal test run: nothing to do
	}
	// optional env dump for the deliberate-addition regressions (e.g.
	// PF_INSTANCE_ID reaching the real child).
	if dump := os.Getenv("GO_BE_ENV"); dump != "" {
		if blob, err := json.Marshal(os.Environ()); err == nil {
			_ = os.WriteFile(dump, append(blob, '\n'), 0o644)
		}
	}
	capture := os.Getenv("GO_BE_CAPTURE")
	f, err := os.OpenFile(capture, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		os.Exit(3)
	}
	defer f.Close()
	// one fixture record at derivation: the tap must see it verbatim
	os.Stdout.WriteString(`{"type":"agent_start"}` + "\n")
	if die := os.Getenv("GO_BE_DIE"); die != "" {
		// the dying harness: a last error-turn record, a stderr line, exit 7
		os.Stdout.WriteString(`{"type":"agent_end","messages":[{"role":"assistant","stopReason":"error","errorMessage":"` + die + `"}]}` + "\n")
		fmt.Fprintln(os.Stderr, "fatal: "+die)
		os.Exit(7)
	}
	br := bufio.NewReader(os.Stdin)
	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			fmt.Fprintln(f, strings.TrimRight(line, "\n"))
		}
		if rerr != nil {
			os.Exit(0) // stdin EOF: graceful shutdown
		}
	}
}

const capsuleDirRuntimeState = "runtime-state"
