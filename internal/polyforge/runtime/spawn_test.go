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
	"github.com/silgrid/polyforge/internal/common/protocol"
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
	t.Setenv("SOME_OTHER_NAME", "machine-key") // renamed secret: value match
	sp := newPiSpawner(&Config{EnvSecrets: []string{"machine-key"}, SocketPath: "/run/polyforge.sock"})
	cmd := sp.buildCmd(SpawnParams{WorkItemID: "wi_env", CapsuleDir: "/caps", SessionHandle: "ab12cd34", SpawnN: 2})
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
	for _, want := range []string{
		"PF_WI_ID=wi_env", "PI_SESSION_DIR=/caps/session",
		// the daemon's UDS path is a DELIBERATE addition (the agent's
		// flush(reason) proxy): stripped as an inherited PF_* only to be
		// handed back explicitly
		"PF_DAEMON_SOCKET=/run/polyforge.sock",
		// v1.5 B2/B3: the PUBLIC session handle and the monotonic
		// derivation counter are the agent's flush fence identity
		"PF_SESSION_HANDLE=ab12cd34",
		"PF_SPAWN_N=2",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env must carry %q, got:\n%s", want, env)
		}
	}
}

func TestSpawnEnvNoSocketPathOmitted(t *testing.T) {
	sp := newPiSpawner(&Config{})
	cmd := sp.buildCmd(SpawnParams{WorkItemID: "wi_env", CapsuleDir: "/caps"})
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "PF_DAEMON_SOCKET") {
		t.Fatal("an unconfigured socket path must not inject an empty PF_DAEMON_SOCKET")
	}
	// the deleted v1 identity vars must never come back
	for _, gone := range []string{"PF_INSTANCE_ID", "PF_SESSION_ID", "PF_GENERATION"} {
		if strings.Contains(env, gone) {
			t.Fatalf("the deleted %s must not be injected, got:\n%s", gone, env)
		}
	}
}

// TestSpawnSessionIdentityEnvReachesChild drives the REAL spawn path
// (scripted harness) and proves the child's environment carries the v1.5
// identity pair — PF_SESSION_HANDLE (the public fingerprint) and
// PF_SPAWN_N (the derivation counter) — and NOT the session secret: the
// agent reads its flush fence identity from env, never the credential.
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

// TestPiRPCEnvelopeAndAck drives the real protocol layer against a
// scripted harness process: the daemon→agent control line rides a prompt
// command (streamingBehavior steer) whose message text is the control JSON
// VERBATIM, and the agent's ack is extracted from the assistant message_end
// record and routed to the waiter by (method, correlation).
func TestPiRPCEnvelopeAndAck(t *testing.T) {
	capDir, sp := scriptedHarness(t)
	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:    "wi_rpc",
		CapsuleDir:    capDir,
		SessionHandle: "ab12cd34",
		SpawnN:        1,
		InitialPrompt: "begin the work item",
		Journal:       func(pid int, startedAt string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Terminate(3 * time.Second) }()

	probe := protocol.ControlMsg{Method: protocol.ControlProbe, Nonce: "n-1", Seq: 7}
	if err := h.SendControl(probe); err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	if err := h.WaitControlAck(context.Background(), probe, 5*time.Second); err != nil {
		t.Fatalf("WaitControlAck: %v", err)
	}

	// the harness captured the raw stdin lines: assert the envelope
	raw, err := os.ReadFile(filepath.Join(capDir, "stdin-capture.jsonl"))
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

// TestSpawnRecordTapFeedsEveryStdoutRecord pins the RecordTap binding: every
// raw stdout record the production spawner reads also reaches the
// configured tap — the daemon binds the tap to its observe adapter, so the
// Observe lane's Records() stream receives exactly this line.
func TestSpawnRecordTapFeedsEveryStdoutRecord(t *testing.T) {
	capDir, sp := scriptedHarness(t)

	var mu sync.Mutex
	var seen []string
	sp.cfg.RecordTap = func(wi string, line []byte) {
		mu.Lock()
		if wi == "wi_tap" {
			seen = append(seen, string(line))
		}
		mu.Unlock()
	}

	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:    "wi_tap",
		CapsuleDir:    capDir,
		SessionHandle: "ab12cd34",
		SpawnN:        1,
		InitialPrompt: "begin",
		Journal:       func(pid int, startedAt string) error { return nil },
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
			t.Fatalf("RecordTap never saw the startup record, saw %q", seen)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(seen[0], `"type":"agent_start"`) {
		t.Fatalf("tap must see the raw record verbatim, got %q", seen[0])
	}
}

// TestSpawnRecordTapBoundAfterSpawnerBuild is the assembly-order
// regression for the e2e lane's high-severity bug: the daemon binds
// RecordTap AFTER DefaultSpawner is built (the tap is a method value on
// the daemon, which does not exist yet at spawner-construction time).
// The spawner holds the config by pointer, so a tap bound after the
// build — the exact production order — must still receive every record;
// with a value copy this test is the bug, not the pin.
func TestSpawnRecordTapBoundAfterSpawnerBuild(t *testing.T) {
	capDir, sp := scriptedHarness(t)

	var mu sync.Mutex
	var seen []string
	// THE PRODUCTION ORDER: spawner first, tap afterwards.
	sp.cfg.RecordTap = func(wi string, line []byte) {
		mu.Lock()
		if wi == "wi_tap_order" {
			seen = append(seen, string(line))
		}
		mu.Unlock()
	}

	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:    "wi_tap_order",
		CapsuleDir:    capDir,
		SessionHandle: "ab12cd34",
		SpawnN:        1,
		InitialPrompt: "begin",
		Journal:       func(pid int, startedAt string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Terminate(3 * time.Second) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("RecordTap bound after the spawner build never saw the record, saw %q", seen)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAckRoutingByMethodAndCorrelation pins the dual-key ack routing: two
// concurrent waiters of the SAME method with different correlations each
// settle on their OWN ack — a method-only matcher hands whichever ack
// arrives first to whichever waiter asked first.
func TestAckRoutingByMethodAndCorrelation(t *testing.T) {
	capDir, sp := scriptedHarness(t)
	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:    "wi_ack",
		CapsuleDir:    capDir,
		SessionHandle: "ab12cd34",
		SpawnN:        1,
		InitialPrompt: "begin",
		Journal:       func(pid int, startedAt string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Terminate(3 * time.Second) }()

	// two pf/answer methods in flight with different question ids
	a := protocol.ControlMsg{Method: protocol.ControlAnswer, Seq: 1, ID: "q-first", Answer: "A"}
	b := protocol.ControlMsg{Method: protocol.ControlAnswer, Seq: 2, ID: "q-second", Answer: "B"}
	if err := h.SendControl(a); err != nil {
		t.Fatal(err)
	}
	if err := h.SendControl(b); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 2)
	go func() { errCh <- h.WaitControlAck(context.Background(), a, 5*time.Second) }()
	go func() { errCh <- h.WaitControlAck(context.Background(), b, 5*time.Second) }()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("waiter %d: %v", i, err)
		}
	}
}

// TestAckRefusalReachedTheWaiter: an ok=false ack is delivered, not dropped
// — the waiter must learn its method was refused instead of timing out.
func TestAckRefusalReachedTheWaiter(t *testing.T) {
	capDir, sp := scriptedHarness(t)
	t.Setenv("GO_BE_REFUSE", "1") // this harness refuses everything
	h, err := sp.Spawn(context.Background(), SpawnParams{
		WorkItemID:    "wi_refuse",
		CapsuleDir:    capDir,
		SessionHandle: "ab12cd34",
		SpawnN:        1,
		InitialPrompt: "begin",
		Journal:       func(pid int, startedAt string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Terminate(3 * time.Second) }()

	msg := protocol.ControlMsg{Method: protocol.ControlProbe, Nonce: "n-ref", Seq: 3}
	if err := h.SendControl(msg); err != nil {
		t.Fatal(err)
	}
	if err = h.WaitControlAck(context.Background(), msg, 5*time.Second); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("the refusal ack must reach its waiter, got %v", err)
	}
}

// TestAckHubOrphanClaim: an ack that arrives BEFORE its waiter registers
// (the SendControl→WaitControlAck race window) parks in the hub's orphan
// backlog and is claimed at registration — never dropped.
func TestAckHubOrphanClaim(t *testing.T) {
	h := newAckHub()
	msg := protocol.ControlMsg{Method: protocol.ControlAnswer, Seq: 5, ID: "q1", Answer: "A"}
	ack := protocol.ControlAck{Method: protocol.ControlAnswer, Seq: 5, Ref: "q1", OK: true}

	h.deliver(ack) // no waiter registered yet
	w := h.register(msg)
	got := h.claim(w)
	if got == nil || !got.OK || got.Seq != 5 {
		t.Fatalf("the raced-ahead ack must be claimed at registration, got %+v", got)
	}
	if again := h.claim(w); again != nil {
		t.Fatalf("the orphan backlog must be emptied by the claim, got %+v", again)
	}
}

// TestAckHubRoutedByKey: a registered waiter never consumes an ack that
// answers a DIFFERENT correlation of the same method.
func TestAckHubRoutedByKey(t *testing.T) {
	h := newAckHub()
	a := protocol.ControlMsg{Method: protocol.ControlAnswer, Seq: 1, ID: "qa"}
	b := protocol.ControlMsg{Method: protocol.ControlAnswer, Seq: 2, ID: "qb"}
	wa := h.register(a)
	wb := h.register(b)
	defer h.unregister(wb)

	// the ack for B arrives while BOTH waiters are parked
	h.deliver(protocol.ControlAck{Method: protocol.ControlAnswer, Seq: 2, Ref: "qb", OK: true})
	if got := h.claim(wa); got != nil {
		t.Fatalf("waiter A must not claim B's ack, got %+v", got)
	}
	if got := h.claim(wb); got == nil || got.Seq != 2 {
		t.Fatalf("waiter B must claim its own ack, got %+v", got)
	}
}

// scriptedHarness builds a piSpawner whose PiBin is a wrapper re-invoking
// THIS test binary as the harness (TestHelperProcess): the harness emits one
// fixture record at derivation, echoes correlation-carrying acks onto stdout
// for every pf/* control method, and captures every stdin line.
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
	// one fixture record at derivation: the RecordTap must see it verbatim
	os.Stdout.WriteString(`{"type":"agent_start"}` + "\n")
	br := bufio.NewReader(os.Stdin)
	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			fmt.Fprintln(f, strings.TrimRight(line, "\n"))
		}
		// the exact ack shape: a protocol.ControlAck inside the assistant
		// message_end record's text, ECHOING the control message's
		// correlation (seq + ref: the question id or the nonce). The
		// control line rides the outer prompt command's message text, so
		// the pf/ method name appears in its JSON-ESCAPED form.
		if strings.Contains(line, `\"method\":\"pf/`) {
			var outer struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(line), &outer) == nil {
				var ctrl protocol.ControlMsg
				if json.Unmarshal([]byte(outer.Message), &ctrl) == nil && strings.HasPrefix(ctrl.Method, "pf/") {
					ref := ctrl.ID
					if ref == "" {
						ref = ctrl.Nonce
					}
					var ack string
					if os.Getenv("GO_BE_REFUSE") == "1" {
						ack = fmt.Sprintf(`{"method":%q,"seq":%d,"ref":%q,"ok":false,"error":"refused by script"}`, ctrl.Method, ctrl.Seq, ref)
					} else {
						ack = fmt.Sprintf(`{"method":%q,"seq":%d,"ref":%q,"ok":true}`, ctrl.Method, ctrl.Seq, ref)
					}
					rec := fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, ack)
					os.Stdout.WriteString(rec + "\n")
				}
			}
		}
		if rerr != nil {
			os.Exit(0) // stdin EOF: graceful shutdown
		}
	}
}

const capsuleDirRuntimeState = "runtime-state"
