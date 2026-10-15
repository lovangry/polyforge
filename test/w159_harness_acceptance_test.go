package test

// w159_harness_acceptance_test.go: polyforge#159 — per-step harness
// selection (pi or Claude Code) through the real pf / aihub binaries and
// the fake harness binary (test/fakepi), which is BOTH the fake `pi` and
// the fake `claude` (argv[1] == "-p", test/fakepi/claude.go). No real
// `claude` or `pi` ever runs, no model is called: the daemon is pointed
// at the fake with --pi-bin / --claude-bin, HOME is a scratch dir (the
// fake writes Claude's native session under it), and the cli.toml token
// is a sentinel.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const w159Token = "w159-SENTINEL-token"

// w159Daemon starts a daemon over one workspace (project e2e) whose
// cli.toml also carries the claude keys; claudeBin "" = the fake.
func w159Daemon(t *testing.T, inst *v1Install, ws, claudeBin string) (*w113Daemon, string) {
	t.Helper()
	sock := filepath.Join(shortTmp("/tmp", "w159sock"), "d.sock")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"), inst.url, inst.adminKey, sock,
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("anthropic_auth_token = \"" + w159Token + "\"\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if claudeBin == "" {
		claudeBin = filepath.Join(v1Bins(t), "fakepi")
	}
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg), w113DOptSock(sock), w113DOptArgs("--claude-bin", claudeBin))
	d.start()
	t.Cleanup(d.stop)
	return d, cfg
}

// w159Plan writes the fake's plan file verbatim (bad_models etc.).
func w159Plan(t *testing.T, inst *v1Install, wi string, body map[string]any) {
	t.Helper()
	blob, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst.planDir, wi+".json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

// w159Journal reads one capsule-side JSONL ledger (logs/<name>).
func w159Journal(t *testing.T, capDir, name string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(capDir, "logs", name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func w159Spawns(t *testing.T, capDir, name string) []map[string]any {
	var out []map[string]any
	for _, e := range w159Journal(t, capDir, name) {
		if e["type"] == "spawn" {
			out = append(out, e)
		}
	}
	return out
}

func w159Argv(e map[string]any) string {
	var parts []string
	if list, ok := e["argv"].([]any); ok {
		for _, a := range list {
			s, _ := a.(string)
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// spawnJournalEntries reads the capsule spawn journal loosely.
func w159SpawnJournal(t *testing.T, capDir string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capDir, "runtime-state", "spawn-journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

const w159Yaml = "steps:\n  e2e:\n    fix: { models: [w159/pi-a, claude-opus-5-5, claude-sonnet-5], harness: claude }\n"

// ① One work item, two harnesses: grill and verification on pi, fix on
// Claude Code. The result contract, the handoff injection, the clean
// claude argv/env, the mixed-list filtering and the transcript copy.
func TestW159MixedHarnessWorkItem(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, w159Yaml)
	d, _ := w159Daemon(t, inst, ws, "")
	wi := w113NewWI(t, inst, "e2e", "w159 mixed harness", envelopeQuickFix())
	capDir := w079Run(t, d, inst, ws, wi)

	if got := w079Model(t, capDir, "fix"); got != "claude-opus-5-5" {
		t.Fatalf("fix model %q, want the first claude id of the mixed list", got)
	}
	if got := w079Model(t, capDir, "grill"); strings.HasPrefix(got, "claude-") {
		t.Fatalf("grill (pi) model %q", got)
	}
	cl := w159Spawns(t, capDir, "fakeclaude.jsonl")
	if len(cl) != 1 || cl[0]["step"] != "fix" {
		t.Fatalf("claude spawns %v, want exactly the fix step", cl)
	}
	argv := w159Argv(cl[0])
	for _, want := range []string{"-p --bare", "--output-format stream-json --verbose --include-partial-messages",
		"--session-id ", "--model claude-opus-5-5", "--tools Bash,Read,Edit,Write,Glob,Grep,TodoWrite,Agent",
		"--allowedTools Bash,Read,Edit,Write,Glob,Grep,TodoWrite,Agent", "--permission-mode dontAsk",
		"--permission-prompts none", "--strict-mcp-config", "--disable-slash-commands"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("claude argv %q misses %q", argv, want)
		}
	}
	for _, banned := range []string{w159Token, "--setting-sources", "--fallback-model", "w159/pi-a"} {
		if strings.Contains(argv, banned) {
			t.Fatalf("claude argv carries %q: %s", banned, argv)
		}
	}
	for _, k := range []string{"auth_token_set", "background_off", "autoupdater_off", "startup_results", "prompt_has_handoff"} {
		if cl[0][k] != true {
			t.Fatalf("claude spawn %s = %v (entry %v)", k, cl[0][k], cl[0])
		}
	}
	if cl[0]["pi_agent_dir"] != "" {
		t.Fatalf("a claude step got the pi agent dir: %v", cl[0]["pi_agent_dir"])
	}
	// the pi steps ran on the fake pi (never the claude persona)
	pi := w159Spawns(t, capDir, "fakepi.jsonl")
	if len(pi) != 2 {
		t.Fatalf("pi spawns %d, want 2 (grill, verification)", len(pi))
	}
	for _, e := range pi {
		if strings.Contains(w159Argv(e), "--bare") {
			t.Fatalf("a pi step ran claude argv: %v", e)
		}
	}
	// the spawn journal names each dispatch's harness; the claude one
	// carries its session uuid, and its native session was copied onto
	// its own capsule transcript
	var claudeLine map[string]any
	for _, e := range w159SpawnJournal(t, capDir) {
		switch e["step_id"] {
		case "fix":
			claudeLine = e
		default:
			if e["harness"] != "pi" {
				t.Fatalf("pi dispatch journal line %v", e)
			}
		}
	}
	if claudeLine == nil || claudeLine["harness"] != "claude" || claudeLine["harness_session"] != cl[0]["session"] {
		t.Fatalf("claude journal line %v (spawn %v)", claudeLine, cl[0])
	}
	tr, _ := claudeLine["transcript"].(string)
	raw, err := os.ReadFile(tr)
	if err != nil || !strings.Contains(string(raw), `"sessionId"`) {
		t.Fatalf("claude transcript copy %s: %q (%v)", tr, raw, err)
	}
	// the fix step's V-gates ran on the claude result exactly as on pi
	x := readExecution(t, capDir)
	if x.Steps["fix"].Status != "done" || x.Steps["verification"].Status != "done" {
		t.Fatalf("steps %s", execDigest(x))
	}
	// the token value never lands anywhere the daemon writes: the capsule
	// tree (journals, transcripts, git objects), the data dir (state.json,
	// the event log) and the daemon's own output
	w159NoToken(t, capDir)
	w159NoToken(t, d.dataDir)
	w159NoToken(t, d.logPath)
}

// w159NoToken fails when any file under root carries the test token.
func w159NoToken(t *testing.T, root string) {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		n++
		if strings.Contains(string(raw), w159Token) {
			t.Fatalf("%s carries the auth token value", path)
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("token scan of %s: %d files (%v)", root, n, err)
	}
}

// ② D7 on claude output: a 429 retries the SAME model, a 401 switches to
// the next claude candidate (pi ids skipped), a 5xx retries the same
// model; ③ ended_without_result after an ok result with no envelope.
func TestW159ClaudeD7(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, w159Yaml)
	d, _ := w159Daemon(t, inst, ws, "")
	wi := w113NewWI(t, inst, "e2e", "w159 claude d7", nil)
	w159Plan(t, inst, wi, map[string]any{"actions": []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "claude_error": "rate_limit", "claude_status": 429, "claude_retries": 2},
		{"do": "envelope", "id": "fix", "claude_error": "authentication_failed", "claude_status": 401},
		{"do": "envelope", "id": "fix", "claude_error": "server_error", "claude_status": 500, "claude_message": "Server error mid-response"},
		{"do": "envelope", "id": "fix", "end_turn": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
	}})
	capDir := w079Run(t, d, inst, ws, wi)
	var models []string
	for _, e := range w159Spawns(t, capDir, "fakeclaude.jsonl") {
		m, _ := e["model"].(string)
		models = append(models, m)
	}
	want := []string{"claude-opus-5-5", "claude-opus-5-5", "claude-sonnet-5", "claude-sonnet-5", "claude-sonnet-5"}
	if strings.Join(models, ",") != strings.Join(want, ",") {
		t.Fatalf("claude dispatch models %v, want %v", models, want)
	}
	var causes []string
	for _, a := range readExecution(t, capDir).Steps["fix"].Attempts {
		if c, _ := a["failure_cause"].(string); c != "" { // the accepted attempt carries none
			causes = append(causes, c)
		}
	}
	wantCauses := []string{"rate_limited", "exit_model_class", "upstream_transient", "ended_without_result"}
	if strings.Join(causes, ",") != strings.Join(wantCauses, ",") {
		t.Fatalf("fix attempt causes %v, want %v", causes, wantCauses)
	}
}

// Candidates exhausted on claude: an unknown model on every claude
// candidate blocks the work item (never a pi id dispatched to claude).
func TestW159ClaudeCandidatesExhaustedBlocks(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, w159Yaml)
	d, _ := w159Daemon(t, inst, ws, "")
	wi := w113NewWI(t, inst, "e2e", "w159 exhausted", nil)
	w159Plan(t, inst, wi, map[string]any{
		"bad_models": []string{"claude-opus-5-5", "claude-sonnet-5"},
		"actions": []map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		}})
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(t, inst, ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	waitExecution(t, capDir, 180*time.Second, "the WI never blocked on exhausted claude candidates",
		func(x *executionView) bool { return x.WorkflowStatus == "blocked" })
	for _, e := range w159Spawns(t, capDir, "fakeclaude.jsonl") {
		if m, _ := e["model"].(string); !strings.HasPrefix(m, "claude-") {
			t.Fatalf("claude dispatched a non-claude id %q", m)
		}
	}
	if len(w159Journal(t, capDir, "fakeclaude.jsonl")) == 0 {
		t.Fatal("no claude dispatch happened")
	}
}

// ④ needs_input on a claude step → answer → the continuation resumes the
// SAME claude session (--resume <uuid>).
func TestW159ClaudeNeedsInputResumes(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, w159Yaml)
	d, _ := w159Daemon(t, inst, ws, "")
	const qid = "q_w159"
	wi := w113NewWI(t, inst, "e2e", "w159 needs_input", []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "needs_input", "question_id": qid, "question": "lean or compatible?"},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
	})
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(t, inst, ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	waitExecution(t, capDir, 120*time.Second, "fix never asked", func(x *executionView) bool {
		st := x.Steps["fix"]
		return st != nil && st.PendingQuestion != nil
	})
	out := d.pfWant(inst, -1, "answer", wi, "--text", "lean", "--question", qid, "--by", "usr_root")
	if !strings.Contains(out, "delivered") {
		t.Fatalf("pf answer: %q", out)
	}
	w113WaitCompleted(t, capDir, 180*time.Second)
	cl := w159Spawns(t, capDir, "fakeclaude.jsonl")
	if len(cl) != 2 {
		t.Fatalf("claude spawns %v, want 2 (ask, continue)", cl)
	}
	first, _ := cl[0]["session"].(string)
	if cl[1]["resume"] != true || cl[1]["session"] != first || !strings.Contains(w159Argv(cl[1]), "--resume "+first) {
		t.Fatalf("continuation %v did not resume session %s", cl[1], first)
	}
	// the resumed dispatch copied the (shared) native session onto its
	// OWN capsule transcript: both fix dispatches' files exist
	n := 0
	for _, e := range w159SpawnJournal(t, capDir) {
		if e["step_id"] != "fix" {
			continue
		}
		tr, _ := e["transcript"].(string)
		if raw, err := os.ReadFile(tr); err != nil || !strings.Contains(string(raw), first) {
			t.Fatalf("fix transcript %s: %q (%v)", tr, raw, err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("%d fix journal lines, want 2", n)
	}
}

// ⑥ A configuration whose claude step has no claude candidate refuses
// the claim (DAEMON-414, the WI stays queued) and doctor fails it; a
// valid one passes doctor's claude check without ever showing the token,
// and a missing claude binary fails it.
func TestW159MismatchRefusedAndDoctor(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, "steps:\n  e2e:\n    fix: { harness: claude }\n")
	d, cfg := w159Daemon(t, inst, ws, "")
	wi := w113NewWI(t, inst, "e2e", "w159 mismatch", envelopeQuickFix())
	out, code := w113Pf(t, inst, w113Env{socket: d.sock}, "submit", wi)
	w079Refused(t, d, inst, ws, wi, out, code, "model/harness mismatch", "e2e/quick-fix/fix: harness claude (steps.e2e.fix.harness)")

	doc := w079PfWant(t, inst, 1, w079Env{config: cfg}, "--json", "doctor")
	if !strings.Contains(doc, "model/harness mismatch") {
		t.Fatalf("doctor does not report the mismatch: %s", doc)
	}

	// fixed: doctor's claude check passes on the fake (verified version,
	// token set) and never prints the token
	w079Yaml(t, ws, w159Yaml)
	doc = w079PfWant(t, inst, -1, w079Env{config: cfg}, "--json", "doctor")
	parsed := w113DoctorJSON(t, doc)
	var claudeCheck *w113DoctorCheck
	for i := range parsed.Checks {
		if parsed.Checks[i].Name == "harness claude" {
			claudeCheck = &parsed.Checks[i]
		}
	}
	if claudeCheck == nil || claudeCheck.Status != "pass" || !strings.Contains(claudeCheck.Detail, "e2e/quick-fix/fix: claude") || !strings.Contains(claudeCheck.Detail, "2.1.296") {
		t.Fatalf("harness claude check %+v", claudeCheck)
	}
	if strings.Contains(doc, w159Token) {
		t.Fatal("doctor output carries the token value")
	}

	// a daemon whose claude binary does not resolve: doctor fails it
	d.stop()
	d2, cfg2 := w159Daemon(t, inst, ws, "/nonexistent/claude-w159")
	_ = d2
	doc = w079PfWant(t, inst, 1, w079Env{config: cfg2}, "--json", "doctor")
	if !strings.Contains(doc, "does not resolve on the daemon's PATH") {
		t.Fatalf("doctor misses the missing claude binary: %s", doc)
	}
}
