package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI executes the full dispatch path (global flags -> command ->
// flags -> verb) with the given config injected through the environment
// (PF_SERVER / PF_API_KEY / PF_DAEMON_SOCKET / PF_USER_ID / PF_PROJECT —
// the documented override channel), returning the exit code plus
// everything written to stdout/stderr.
func runCLI(t *testing.T, cfg Config, stdin string, argv ...string) (int, string, string) {
	t.Helper()
	// Isolate the default config location: Run loads
	// ~/.config/polyforge/cli.toml when --config is absent, and no test
	// may depend on (or be broken by) the developer's real file.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PF_SERVER", cfg.Server)
	t.Setenv("PF_API_KEY", cfg.APIKey)
	t.Setenv("PF_DAEMON_SOCKET", cfg.DaemonSocket)
	t.Setenv("PF_USER_ID", cfg.UserID)
	t.Setenv("PF_PROJECT", cfg.Project)
	var stdout, stderr bytes.Buffer
	code := Run(argv, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// testCtxConfig points the aihub face at a test server and the daemon
// face at a nonexistent socket; the project default exercises config
// fallback for project-scoped verbs.
func testCtxConfig(server string) Config {
	return Config{Server: server, APIKey: "k", DaemonSocket: "/nonexistent/pf.sock", Project: "testproj"}
}

// daemonCtxConfig returns a config whose daemon face points at sock.
func daemonCtxConfig(sock string) Config {
	return Config{APIKey: "k", DaemonSocket: sock, Project: "testproj"}
}

// ---------------------------------------------------------------------------
// Data-plane verbs (direct to aihub, 00 §5.1): file / queue / cancel /
// recall, plus the supporting deps and budget faces.
// ---------------------------------------------------------------------------

func TestRunFileCreateFromFlags(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 201, `{"id":"wi_new","slug":"testproj#1","goal":"g","status":"queued","priority":"high","labels":["x"],"project":"testproj","created_at":"2026-01-01T00:00:00Z"}`, &cap)
	code, out, errOut := runCLI(t, testCtxConfig(srv.URL), "",
		"file", "create", "--project", "testproj", "--goal", "build the CLI",
		"--label", "x", "--priority", "high", "--bundle", "reflow",
		"--repo", "polyforge=git@example:polyforge.git", "--repo-base", "polyforge=abc123",
		"--attr", "triage=3")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/work-items" {
		t.Errorf("got %s %s", cap.Method, cap.Path)
	}
	var body map[string]any
	if err := decodeJSON([]byte(cap.Body), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["project"] != "testproj" || body["goal"] != "build the CLI" {
		t.Errorf("body = %s", cap.Body)
	}
	if body["priority"] != "high" || body["bundle_name"] != "reflow" {
		t.Errorf("body = %s", cap.Body)
	}
	if strings.Contains(cap.Body, "Bearer") {
		t.Errorf("api key must not leak into the body: %s", cap.Body)
	}
	if !strings.Contains(out, "testproj#1") {
		t.Errorf("human output missing slug: %q", out)
	}
}

func TestRunFileCreateFromStdinJSON(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 201, `{"id":"wi_x","slug":"p#1","goal":"g","status":"queued"}`, &cap)
	body := `{"goal":"from stdin","content":"bg","labels":["a"],"bundle_name":"b1"}`
	// --project supplies the only field the stdin body omits.
	code, _, errOut := runCLI(t, testCtxConfig(srv.URL), body,
		"file", "create", "--stdin", "--project", "stdinproj")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	var got map[string]any
	if err := decodeJSON([]byte(cap.Body), &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got["project"] != "stdinproj" || got["goal"] != "from stdin" || got["bundle_name"] != "b1" {
		t.Errorf("body = %s", cap.Body)
	}
	if _, ok := got["content"]; !ok {
		t.Errorf("stdin content dropped: %s", cap.Body)
	}
}

func TestRunFileCreateMissingGoal(t *testing.T) {
	// No backend needed: the guard fires before any request.
	code, _, errOut := runCLI(t, Config{}, "", "file", "create", "--project", "p")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "--goal") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestRunFileEditSendsOnlyProvidedFlags(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"id":"wi_x","slug":"p#1","goal":"g","status":"queued","priority":"high"}`, &cap)
	code, _, errOut := runCLI(t, testCtxConfig(srv.URL), "",
		"file", "edit", "wi_x", "--priority", "high", "--label", "a", "--label", "b")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "PATCH" || cap.Path != "/api/v1/work-items/wi_x" {
		t.Errorf("got %s %s", cap.Method, cap.Path)
	}
	var body map[string]any
	if err := decodeJSON([]byte(cap.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["priority"] != "high" {
		t.Errorf("body = %s", cap.Body)
	}
	// Only the provided flags may appear: absent PATCHable keys stay out.
	for _, absent := range []string{"goal", "content", "milestone", "waiting_on"} {
		if _, ok := body[absent]; ok {
			t.Errorf("PATCH body must not carry unflagged key %q: %s", absent, cap.Body)
		}
	}
}

func TestRunFileShowInclude(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"id":"wi_x","slug":"p#1","goal":"g","status":"running","priority":"normal","labels":[],"created_at":"t"}`, &cap)
	code, out, errOut := runCLI(t, testCtxConfig(srv.URL), "",
		"file", "show", "wi_x", "--include", "events,deps")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "GET" || cap.Path != "/api/v1/work-items/wi_x?include=events%2Cdeps" {
		t.Errorf("got %s %s", cap.Method, cap.Path)
	}
	if !strings.Contains(out, "p#1") {
		t.Errorf("render = %q", out)
	}
}

func TestRunQueueListWithFilters(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"items":[{"id":"wi_a","slug":"p#1","goal":"g","status":"queued","priority":"normal","labels":[],"created_at":"t"}]}`, &cap)
	code, out, errOut := runCLI(t, testCtxConfig(srv.URL), "",
		"queue", "--status", "queued", "--assignee", "usr_1")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(cap.Path, "project=testproj") ||
		!strings.Contains(cap.Path, "status=queued") ||
		!strings.Contains(cap.Path, "assignee=usr_1") {
		t.Errorf("path = %s", cap.Path)
	}
	if !strings.Contains(out, "1 work items") || !strings.Contains(out, "p#1") {
		t.Errorf("render = %q", out)
	}
}

func TestRunQueueReadyHumanRender(t *testing.T) {
	var cap captured
	resp := `{"project":"pi","executable":[{"id":"wi_a","slug":"pi#1","goal":"do","priority":"urgent","status":"queued","bundle_name":"b","labels":[],"created_at":"t"}],"blocked":[{"id":"wi_b","slug":"pi#2","goal":"wait","priority":"normal","status":"blocked","waiting_on":"dependency:wi_a","blocked_by":["wi_a"],"labels":[],"created_at":"t"}]}`
	srv := newCapturingServer(t, 200, resp, &cap)
	code, out, errOut := runCLI(t, testCtxConfig(srv.URL), "", "queue", "--ready")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/api/v1/ready-queue?project=testproj" {
		t.Errorf("path = %s (config default project must apply)", cap.Path)
	}
	if !strings.Contains(out, "executable") || !strings.Contains(out, "pi#1") {
		t.Errorf("human render: %q", out)
	}
	if !strings.Contains(out, "dependency:wi_a") {
		t.Errorf("blocked render: %q", out)
	}
}

func TestRunCancel(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"id":"wi_x","slug":"p#1","goal":"g","status":"cancelled"}`, &cap)
	code, out, errOut := runCLI(t, testCtxConfig(srv.URL), "", "cancel", "wi_x")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/work-items/wi_x/cancel" {
		t.Errorf("got %s %s", cap.Method, cap.Path)
	}
	if !strings.Contains(out, "cancelled") {
		t.Errorf("render = %q", out)
	}
}

func TestRunRecallSearch(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"items":[{"id":"mem_1","type":"experience.debug","title":"lesson","project_id":"testproj","updated_at":"t","tags":[]}]}`, &cap)
	code, out, errOut := runCLI(t, testCtxConfig(srv.URL), "",
		"recall", "trigram", "search", "--type", "experience.debug", "--tags", "a,b", "--limit", "5")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"project=testproj", "q=trigram+search", "type=experience.debug", "tags=a%2Cb", "limit=5"} {
		if !strings.Contains(cap.Path, want) {
			t.Errorf("path missing %s: %s", want, cap.Path)
		}
	}
	if !strings.Contains(out, "1 hit(s)") || !strings.Contains(out, "mem_1") {
		t.Errorf("render = %q", out)
	}
}

func TestRunDepsAddListRemove(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"blocking":[],"blocked_by":[{"wi_id":"wi_a","kind":"blocks"}]}`, &cap)
	code, _, errOut := runCLI(t, testCtxConfig(srv.URL), "", "deps", "add", "wi_b", "--blocking", "wi_a")
	if code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/work-items/wi_b/dependencies" {
		t.Errorf("add: %s %s", cap.Method, cap.Path)
	}
	if cap.Body != `{"blocking_wi_id":"wi_a","kind":"blocks"}` {
		t.Errorf("add body = %s", cap.Body)
	}

	code, _, errOut = runCLI(t, testCtxConfig(srv.URL), "", "deps", "list", "wi_b")
	if code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/api/v1/work-items/wi_b/dependencies" {
		t.Errorf("list: %s", cap.Path)
	}

	code, _, errOut = runCLI(t, testCtxConfig(srv.URL), "", "deps", "remove", "wi_b", "wi_a", "blocks")
	if code != 0 {
		t.Fatalf("remove exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "DELETE" || cap.Path != "/api/v1/work-items/wi_b/dependencies/wi_a/blocks" {
		t.Errorf("remove: %s %s", cap.Method, cap.Path)
	}
}

func TestRunBudgetReadAndRaise(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"baseline":{},"effective":{},"max_seq":7}`, &cap)
	code, _, errOut := runCLI(t, testCtxConfig(srv.URL), "", "budget", "wi_x")
	if code != 0 {
		t.Fatalf("read exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/api/v1/work-items/wi_x/budget" {
		t.Errorf("read path = %s", cap.Path)
	}

	code, _, errOut = runCLI(t, testCtxConfig(srv.URL), "",
		"budget", "raise", "wi_x", "--dimension", "tokens", "--amount", "500", "--note", "more")
	if code != 0 {
		t.Fatalf("raise exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/api/v1/work-items/wi_x/budget-increments" {
		t.Errorf("raise path = %s", cap.Path)
	}
	// CAS: based_on_seq must be the max_seq read just before (row 12).
	if cap.Body != `{"dimension":"tokens","amount":500,"note":"more","based_on_seq":7}` {
		t.Errorf("raise body = %s", cap.Body)
	}
}

// ---------------------------------------------------------------------------
// Output modes and error rendering.
// ---------------------------------------------------------------------------

func TestRunJSONModePassthrough(t *testing.T) {
	var cap captured
	resp := `{"project":"pi","executable":[],"blocked":[]}`
	srv := newCapturingServer(t, 200, resp, &cap)
	code, out, _ := runCLI(t, testCtxConfig(srv.URL), "", "--json", "queue", "--ready")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.TrimSpace(out) != resp {
		t.Errorf("--json must emit the server body verbatim, got %q", out)
	}
}

func TestRunErrorRenderingCodeProminent(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 409, envelope("CORE-409", "ALREADY_CLAIMED", "wi is running"), &cap)
	code, _, errOut := runCLI(t, testCtxConfig(srv.URL), "", "cancel", "wi_x")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.HasPrefix(errOut, "pf: CORE-409 ALREADY_CLAIMED: wi is running") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestRunErrorJSONModeEmitsEnvelope(t *testing.T) {
	var cap captured
	body := envelope("CORE-410", "", "terminal state")
	srv := newCapturingServer(t, 410, body, &cap)
	code, _, errOut := runCLI(t, testCtxConfig(srv.URL), "", "--json", "cancel", "wi_x")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if strings.TrimSpace(errOut) != body {
		t.Errorf("--json error must emit the raw envelope, got %q", errOut)
	}
}

// ---------------------------------------------------------------------------
// Execution-plane verbs (local daemon unix-socket face, 00 §5.1):
// submit / observe / answer / batch-confirm / pause / resume / wrap / ops.
// ---------------------------------------------------------------------------

func TestRunSubmitClaimAndTakeover(t *testing.T) {
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200, `{"accepted":true,"work_item_id":"wi_x"}`, &cap)
	cfg := daemonCtxConfig(sock)

	code, out, errOut := runCLI(t, cfg, "", "submit", "wi_x")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/jobs/wi_x/claim" || cap.Body != `{"priority":"normal"}` {
		t.Errorf("claim: %s %s", cap.Path, cap.Body)
	}
	if !strings.Contains(out, "submitted") {
		t.Errorf("render = %q", out)
	}

	code, _, errOut = runCLI(t, cfg, "", "submit", "--takeover", "wi_x", "--priority", "high")
	if code != 0 {
		t.Fatalf("takeover exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/jobs/wi_x/takeover" || cap.Body != `{"priority":"high"}` {
		t.Errorf("takeover: %s %s", cap.Path, cap.Body)
	}
}

func TestRunResume(t *testing.T) {
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200, `{"accepted":true,"work_item_id":"wi_x"}`, &cap)
	code, out, errOut := runCLI(t, daemonCtxConfig(sock), "", "resume", "wi_x")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/jobs/wi_x/resume" {
		t.Errorf("path = %s", cap.Path)
	}
	if !strings.Contains(out, "resumed") {
		t.Errorf("render = %q", out)
	}
}

func TestRunObserveFallsBackToReadThrough(t *testing.T) {
	// Daemon answers DAEMON-404 (no local Capsule, no published progress
	// on its side either) -> observe falls back to the aihub row-6
	// progress face (04 §5.1a layer two).
	var dCap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	startUnixDaemonAtPath(t, sock, 404, envelope("DAEMON-404", "", "no local job"), &dCap)

	var aCap captured
	aihub := newCapturingServer(t, 200,
		`{"work_item_id":"wi_a","head_seq":5,"head_generation":1,"progress":{"current_step":"code"},"head_updated_at":"t","freshness_note":"data as of last publish"}`, &aCap)

	cfg := testCtxConfig(aihub.URL)
	cfg.DaemonSocket = sock
	code, out, errOut := runCLI(t, cfg, "", "observe", "wi_a")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if dCap.Path != "/jobs/wi_a/observe?after=0" {
		t.Errorf("daemon layer not tried first: %s", dCap.Path)
	}
	if aCap.Path != "/api/v1/work-items/wi_a/progress" {
		t.Errorf("read-through fallback not used: %s", aCap.Path)
	}
	if !strings.Contains(out, "data as of last publish") {
		t.Errorf("freshness note missing: %q", out)
	}
	if !strings.Contains(out, "current_step") {
		t.Errorf("progress JSON missing: %q", out)
	}
}

func TestRunObserveReadThroughFlagSkipsDaemon(t *testing.T) {
	var dCap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	startUnixDaemonAtPath(t, sock, 200, `{}`, &dCap)
	var aCap captured
	aihub := newCapturingServer(t, 200, `{"work_item_id":"wi_a","head_seq":null,"head_generation":null,"progress":null,"head_updated_at":null,"freshness_note":"none"}`, &aCap)
	cfg := testCtxConfig(aihub.URL)
	cfg.DaemonSocket = sock
	code, _, _ := runCLI(t, cfg, "", "observe", "wi_a", "--read-through")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if dCap.Path != "" {
		t.Errorf("--read-through must skip the daemon layer, saw %s", dCap.Path)
	}
	if aCap.Path != "/api/v1/work-items/wi_a/progress" {
		t.Errorf("progress path = %s", aCap.Path)
	}
}

func TestRunObserveDaemonLayerAnswered(t *testing.T) {
	// Local Capsule present: the event-stream layer wins, no aihub call.
	var dCap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	startUnixDaemonAtPath(t, sock, 200,
		`{"events":[{"seq":1,"type":"job_started"}],"state_snapshot":{"step":1},"next_cursor":1}`, &dCap)
	var aCap captured
	aihub := newCapturingServer(t, 200, `{}`, &aCap)
	cfg := testCtxConfig(aihub.URL)
	cfg.DaemonSocket = sock
	code, out, errOut := runCLI(t, cfg, "", "observe", "wi_a", "--after", "0")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if dCap.Path != "/jobs/wi_a/observe?after=0" {
		t.Errorf("daemon path = %s", dCap.Path)
	}
	if aCap.Path != "" {
		t.Errorf("aihub must not be touched when the local layer answers, saw %s", aCap.Path)
	}
	if !strings.Contains(out, "job_started") || !strings.Contains(out, "next_cursor: 1") {
		t.Errorf("observe render: %q", out)
	}
}

func TestRunAnswerGeneratesAnswerID(t *testing.T) {
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200, `{"applied":true}`, &cap)
	cfg := daemonCtxConfig(sock)
	cfg.UserID = "usr_9"
	code, out, errOut := runCLI(t, cfg, "",
		"answer", "wi_a", "--question", "q_7f3a1c", "--text", "go ahead")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/jobs/wi_a/answer" {
		t.Fatalf("path = %s", cap.Path)
	}
	// The relay body carries all five §3.4a fields flat; the answer id is
	// generated (a_ + 6 hex) and answered_by falls back to config.
	for _, want := range []string{
		`"question_id":"q_7f3a1c"`,
		`"answer_text":"go ahead"`,
		`"answered_by":"usr_9"`,
		`"answer_id":"a_`,
	} {
		if !strings.Contains(cap.Body, want) {
			t.Errorf("answer body missing %s: %s", want, cap.Body)
		}
	}
	if got := cap.Header.Get("X-PF-User"); got != "usr_9" {
		t.Errorf("X-PF-User = %q", got)
	}
	if !strings.Contains(out, "delivered") {
		t.Errorf("out = %q", out)
	}
}

func TestRunAnswerRequiresIdentity(t *testing.T) {
	code, _, errOut := runCLI(t, Config{}, "", "answer", "wi_a", "--question", "q_x", "--text", "t")
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errOut, "answered_by") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestRunBatchConfirmRecommendationOnly(t *testing.T) {
	// Recommendation display creates NO Job: exactly one daemon call
	// (mode get_recommendation, the read-only aihub ready-queue answer)
	// and no batch_submit (04 §6.1b step 1).
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200,
		`{"project":"testproj","executable":[{"id":"wi_a","slug":"p#1","goal":"g","priority":"high","status":"queued","labels":[],"created_at":"t"}],"blocked":[]}`, &cap)
	code, out, errOut := runCLI(t, daemonCtxConfig(sock), "", "batch-confirm")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/mode" || !strings.Contains(cap.Body, `"action":"get_recommendation"`) {
		t.Errorf("want single get_recommendation call, got %s %s", cap.Path, cap.Body)
	}
	if !strings.Contains(out, "wi_a") {
		t.Errorf("render: %q", out)
	}
	if !strings.Contains(out, "confirm with") {
		t.Errorf("hint missing: %q", out)
	}
}

func TestRunBatchConfirmExplicitSet(t *testing.T) {
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200, `{"project":"testproj","executable":[],"blocked":[]}`, &cap)
	code, _, errOut := runCLI(t, daemonCtxConfig(sock), "", "batch-confirm", "wi_a", "wi_b")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	// The human names the wave boundary; the daemon never widens or
	// narrows it (00 §3.4b).
	if cap.Path != "/batch_submit" || cap.Body != `{"wi_ids":["wi_a","wi_b"]}` {
		t.Errorf("want batch_submit with the human-given set, got %s %s", cap.Path, cap.Body)
	}
}

func TestRunBatchConfirmAllSubmitsWholeRecommendation(t *testing.T) {
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200,
		`{"project":"testproj","executable":[{"id":"wi_a","slug":"p#1","goal":"g","priority":"high","status":"queued","labels":[],"created_at":"t"},{"id":"wi_b","slug":"p#2","goal":"h","priority":"normal","status":"queued","labels":[],"created_at":"t"}],"blocked":[]}`, &cap)
	code, _, errOut := runCLI(t, daemonCtxConfig(sock), "", "batch-confirm", "--all")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/batch_submit" || cap.Body != `{"wi_ids":["wi_a","wi_b"]}` {
		t.Errorf("--all must submit the whole recommendation, got %s %s", cap.Path, cap.Body)
	}
}

func TestRunPauseAndWrap(t *testing.T) {
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200, `{"ok":true}`, &cap)
	cfg := daemonCtxConfig(sock)

	code, out, errOut := runCLI(t, cfg, "", "pause", "wi_x")
	if code != 0 {
		t.Fatalf("pause exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/jobs/wi_x/pause" {
		t.Errorf("pause path = %s", cap.Path)
	}
	if !strings.Contains(out, "pause requested") {
		t.Errorf("pause render = %q", out)
	}

	code, _, errOut = runCLI(t, cfg, "", "wrap", "wi_x", "--fail")
	if code != 0 {
		t.Fatalf("wrap exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/jobs/wi_x/wrap" || cap.Body != `{"reason":"fail"}` {
		t.Errorf("wrap: %s %s", cap.Path, cap.Body)
	}
}

func TestRunShip(t *testing.T) {
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200,
		`{"work_item_id":"wi_x","branch":"polyforge/wi_x","repos":[{"name":"app","origin_url":"/srv/git/app.git","head":"0f1e2d","branch":"polyforge/wi_x","pushed":true,"reused":false,"compare_url":"/srv/git/app.git"}]}`,
		&cap)
	cfg := daemonCtxConfig(sock)

	code, out, errOut := runCLI(t, cfg, "", "ship", "wi_x")
	if code != 0 {
		t.Fatalf("ship exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/jobs/wi_x/ship" || cap.Body != "{}" {
		t.Errorf("ship: %s %s", cap.Path, cap.Body)
	}
	for _, want := range []string{"shipped wi_x", "branch polyforge/wi_x", "pushed", "compare:"} {
		if !strings.Contains(out, want) {
			t.Errorf("ship render must contain %q, got %q", want, out)
		}
	}

	// One positional only (a runtime error like every id-taking sibling).
	code, _, _ = runCLI(t, cfg, "", "ship")
	if code != 1 {
		t.Errorf("ship with no id: exit=%d, want 1", code)
	}
}

func TestRunOpsStatusAndMode(t *testing.T) {
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200,
		`{"machine_id":"m-01","mode":"interactive","shutting_down":false,`+
			`"jobs":[{"work_item_id":"wi_a","harness_adapter":"pi","harness_session_id":"s1","ring_latest":3}],`+
			`"queue":{"pending":0,"running":1,"max_concurrent":4}}`, &cap)
	cfg := daemonCtxConfig(sock)

	code, out, errOut := runCLI(t, cfg, "", "ops", "status")
	if code != 0 {
		t.Fatalf("status exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/status" || cap.Method != "GET" {
		t.Errorf("status: %s %s", cap.Method, cap.Path)
	}
	if !strings.Contains(out, "mode: interactive") || !strings.Contains(out, "wi_a") {
		t.Errorf("status render: %q", out)
	}

	// A bare mode query reads /status (POST /mode has no "get" action).
	code, out, errOut = runCLI(t, cfg, "", "ops", "mode")
	if code != 0 {
		t.Fatalf("mode query exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/status" {
		t.Errorf("mode query path = %s", cap.Path)
	}
	if !strings.Contains(out, "mode: interactive") {
		t.Errorf("mode query render: %q", out)
	}

	code, _, errOut = runCLI(t, cfg, "", "ops", "mode", "drain")
	if code != 0 {
		t.Fatalf("drain exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/mode" || cap.Body != `{"action":"drain"}` {
		t.Errorf("drain: %s %s", cap.Path, cap.Body)
	}

	code, _, errOut = runCLI(t, cfg, "", "ops", "mode", "set", "--max-concurrent-jobs", "4")
	if code != 0 {
		t.Fatalf("set exit=%d stderr=%s", code, errOut)
	}
	if cap.Body != `{"action":"set","max_concurrent_jobs":4}` {
		t.Errorf("set body = %s", cap.Body)
	}
}

func TestRunOpsShutdown(t *testing.T) {
	var cap captured
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	newUnixDaemonAt(t, sock, 200, `{"shutting_down":true}`, &cap)
	code, out, errOut := runCLI(t, daemonCtxConfig(sock), "", "ops", "shutdown")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/shutdown" {
		t.Errorf("path = %s", cap.Path)
	}
	if !strings.Contains(out, "graceful shutdown") {
		t.Errorf("render = %q", out)
	}
}

// ---------------------------------------------------------------------------
// Management face (M1-M5, direct to aihub; 02 §5.1 F group).
// ---------------------------------------------------------------------------

func TestRunUsersCreateAndPatch(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 201, `{"id":"usr_1","display_name":"Ada","kind":"human","role":"admin"}`, &cap)
	cfg := testCtxConfig(srv.URL)

	code, out, errOut := runCLI(t, cfg, "", "users", "create", "--display-name", "Ada", "--role", "admin")
	if code != 0 {
		t.Fatalf("create exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/users" {
		t.Errorf("create: %s %s", cap.Method, cap.Path)
	}
	if cap.Body != `{"display_name":"Ada","kind":"human","role":"admin"}` {
		t.Errorf("create body = %s", cap.Body)
	}
	if !strings.Contains(out, "created user usr_1") {
		t.Errorf("render = %q", out)
	}

	code, _, errOut = runCLI(t, cfg, "", "users", "patch", "usr_1", "--display-name", "Ada L")
	if code != 0 {
		t.Fatalf("patch exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "PATCH" || cap.Path != "/api/v1/users/usr_1" {
		t.Errorf("patch: %s %s", cap.Method, cap.Path)
	}
	if cap.Body != `{"display_name":"Ada L"}` {
		t.Errorf("patch body = %s (empty role must be omitted)", cap.Body)
	}
}

func TestRunApiKeysCreateAndRevoke(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 201, `{"id":"key_1","user_id":"usr_1","project_id":"proj","plaintext":"pfk_secret"}`, &cap)
	cfg := testCtxConfig(srv.URL)

	code, out, errOut := runCLI(t, cfg, "", "api-keys", "create", "--user", "usr_1", "--project-scope", "proj")
	if code != 0 {
		t.Fatalf("create exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/api-keys" {
		t.Errorf("create: %s %s", cap.Method, cap.Path)
	}
	if cap.Body != `{"user_id":"usr_1","project_id":"proj"}` {
		t.Errorf("create body = %s", cap.Body)
	}
	if !strings.Contains(out, "pfk_secret") || !strings.Contains(out, "exactly once") {
		t.Errorf("plaintext must be shown exactly once: %q", out)
	}

	code, _, errOut = runCLI(t, cfg, "", "api-keys", "revoke", "key_1")
	if code != 0 {
		t.Fatalf("revoke exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "DELETE" || cap.Path != "/api/v1/api-keys/key_1" {
		t.Errorf("revoke: %s %s", cap.Method, cap.Path)
	}
}

func TestRunProjectsCreateAndMembers(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 201, `{"id":"proj_1","name":"pi","visible":true}`, &cap)
	cfg := testCtxConfig(srv.URL)

	code, _, errOut := runCLI(t, cfg, "", "projects", "create", "--name", "pi")
	if code != 0 {
		t.Fatalf("create exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/projects" {
		t.Errorf("create: %s %s", cap.Method, cap.Path)
	}
	if cap.Body != `{"name":"pi","visible":true}` {
		t.Errorf("create body = %s", cap.Body)
	}

	code, _, errOut = runCLI(t, cfg, "", "projects", "members", "add", "proj_1", "--user", "u1", "--role", "maintainer")
	if code != 0 {
		t.Fatalf("members add exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/api/v1/projects/proj_1" || !strings.Contains(cap.Body, `"members":[{"user_id":"u1","role":"maintainer"}]`) {
		t.Errorf("members add: %s %s", cap.Path, cap.Body)
	}
}

func TestRunProjectsMembersRemove(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"id":"proj_1","name":"pi","visible":true}`, &cap)
	code, _, errOut := runCLI(t, testCtxConfig(srv.URL), "",
		"projects", "members", "remove", "proj_1", "--user", "u1", "--user", "u2")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/api/v1/projects/proj_1" || cap.Method != "PATCH" {
		t.Errorf("remove: %s %s", cap.Method, cap.Path)
	}
	if !strings.Contains(cap.Body, `"remove_members":["u1","u2"]`) {
		t.Errorf("remove body = %s", cap.Body)
	}
}

func TestRunBackup(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"backup":"snapshot-2026-01-01.db"}`, &cap)
	code, out, errOut := runCLI(t, testCtxConfig(srv.URL), "", "backup")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/admin/backup" {
		t.Errorf("got %s %s", cap.Method, cap.Path)
	}
	if !strings.Contains(out, "snapshot-2026-01-01.db") {
		t.Errorf("render = %q", out)
	}
}

func TestRunRotateSecretRollbackGuards(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"ok":true}`, &cap)
	cfg := testCtxConfig(srv.URL)
	// grace requires --grace-seconds.
	code, _, errOut := runCLI(t, cfg, "", "rotate-secret", "--mode", "grace")
	if code != 1 {
		t.Errorf("grace without seconds must fail, exit=%d stderr=%q", code, errOut)
	}
	// rollback must NOT carry a grace window.
	code, _, errOut = runCLI(t, cfg, "", "rotate-secret", "--mode", "rollback")
	if code != 0 {
		t.Fatalf("rollback exit=%d stderr=%s", code, errOut)
	}
	if cap.Path != "/api/v1/service/rotate-secret" {
		t.Errorf("path = %s", cap.Path)
	}
	if !strings.Contains(cap.Body, `"mode":"rollback"`) || strings.Contains(cap.Body, "grace_seconds") {
		t.Errorf("rollback body = %s", cap.Body)
	}

	code, _, errOut = runCLI(t, cfg, "", "rotate-secret", "--mode", "grace", "--grace-seconds", "300")
	if code != 0 {
		t.Fatalf("grace exit=%d stderr=%s", code, errOut)
	}
	if cap.Body != `{"mode":"grace","grace_seconds":300}` {
		t.Errorf("grace body = %s", cap.Body)
	}
}

// ---------------------------------------------------------------------------
// Dispatch table behavior.
// ---------------------------------------------------------------------------

func TestRunUnknownCommandAndHelp(t *testing.T) {
	code, _, errOut := runCLI(t, Config{}, "", "frobnicate")
	if code != 2 {
		t.Errorf("unknown command exit = %d", code)
	}
	if !strings.Contains(errOut, "unknown command") {
		t.Errorf("stderr = %q", errOut)
	}
	code, out, _ := runCLI(t, Config{}, "", "help")
	if code != 0 || !strings.Contains(out, "file") || !strings.Contains(out, "rotate-secret") {
		t.Errorf("help broken: code=%d out=%q", code, out)
	}
	// Per-command usage goes to stdout too.
	code, out, _ = runCLI(t, Config{}, "", "help", "batch-confirm")
	if code != 0 || !strings.Contains(out, "--all") {
		t.Errorf("help <cmd> broken: code=%d out=%q", code, out)
	}
}

// ---------------------------------------------------------------------------
// Regressions: default config file, human-mode output, filter keys,
// positional strictness, PATCH visibility semantics, usage text honesty.
// ---------------------------------------------------------------------------

func TestRunDefaultConfigFileAppliesWithoutConfigFlag(t *testing.T) {
	// Regression (FC-2): with no --config, Run must load the documented
	// default file. The file carries server/api_key/project; no PF_* env
	// is set, so the request must carry the file's values.
	var cap captured
	srv := newCapturingServer(t, 200, `{"project":"fileproj","executable":[],"blocked":[]}`, &cap)
	for _, k := range []string{"PF_SERVER", "PF_API_KEY", "PF_DAEMON_SOCKET", "PF_USER_ID", "PF_PROJECT"} {
		t.Setenv(k, "")
	}
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	if err := os.MkdirAll(filepath.Join(base, "polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "polyforge", "cli.toml")
	body := "server = \"" + srv.URL + "\"\n" +
		"api_key = \"k_fromfile\"\n" +
		"project = \"fileproj\"\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"queue", "--ready"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(cap.Path, "project=fileproj") {
		t.Errorf("path = %s, want the default file's project", cap.Path)
	}
	if got := cap.Header.Get("Authorization"); got != "Bearer k_fromfile" {
		t.Errorf("Authorization = %q, want the default file's key", got)
	}
}

func TestHumanModeOmitsRawJSON(t *testing.T) {
	// Regression (FC-1): human mode renders the view; the raw server
	// JSON must NOT also be emitted on stdout.
	var cap captured
	resp := `{"project":"pi","executable":[{"id":"wi_a","slug":"pi#1","goal":"do","priority":"urgent","status":"queued","labels":[],"created_at":"t"}],"blocked":[]}`
	srv := newCapturingServer(t, 200, resp, &cap)
	code, out, errOut := runCLI(t, testCtxConfig(srv.URL), "", "queue", "--ready")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if strings.Contains(out, "{\"project\"") || strings.Contains(out, "\"executable\"") {
		t.Errorf("human output must not contain the raw JSON body: %q", out)
	}
	if !strings.Contains(out, "pi#1") || !strings.Contains(out, "executable") {
		t.Errorf("human render missing: %q", out)
	}
	// --json still passes the raw body through verbatim.
	code, out, _ = runCLI(t, testCtxConfig(srv.URL), "", "--json", "queue", "--ready")
	if code != 0 || strings.TrimSpace(out) != resp {
		t.Errorf("--json passthrough broken: code=%d out=%q", code, out)
	}
}

func TestRunQueueWaitingOnFilter(t *testing.T) {
	// Regression (FC-4): the --waiting-on flag must reach the wire as the
	// openapi row-2 query parameter `waiting_on`, not be silently dropped.
	var cap captured
	srv := newCapturingServer(t, 200, `{"items":[]}`, &cap)
	code, _, errOut := runCLI(t, testCtxConfig(srv.URL), "",
		"queue", "--waiting-on", "dependency:wi_a")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(cap.Path, "waiting_on=dependency%3Awi_a") {
		t.Errorf("path = %s, want waiting_on filter on the wire", cap.Path)
	}
}

func TestRunFileCreateRejectsExtraPositionals(t *testing.T) {
	// Regression (FC-7): file create takes flags only; a stray positional
	// must be a usage error, not silently accepted.
	code, _, errOut := runCLI(t, Config{}, "",
		"file", "create", "--project", "p", "--goal", "g", "stray")
	if code != 1 {
		t.Errorf("exit = %d, want 1 (usage error)", code)
	}
	if !strings.Contains(errOut, "unexpected arguments") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestRunProjectsPatchVisibleExplicitSetOnly(t *testing.T) {
	// Regression (FC-3): the wire body carries `visible` only when the
	// flag was explicitly given; a plain patch must not flip a private
	// project visible (openapi ProjectPatchRequest: optional property).
	var cap captured
	srv := newCapturingServer(t, 200, `{"id":"proj_1","name":"pi","visible":false}`, &cap)
	cfg := testCtxConfig(srv.URL)

	// Plain patch: no `visible` key at all.
	code, _, errOut := runCLI(t, cfg, "", "projects", "patch", "proj_1")
	if code != 0 {
		t.Fatalf("plain patch exit=%d stderr=%s", code, errOut)
	}
	if cap.Body != `{}` {
		t.Errorf("plain patch body = %s, want {} (no visible key)", cap.Body)
	}

	// --visible: explicit true.
	code, _, errOut = runCLI(t, cfg, "", "projects", "patch", "proj_1", "--visible")
	if code != 0 {
		t.Fatalf("--visible exit=%d stderr=%s", code, errOut)
	}
	if cap.Body != `{"visible":true}` {
		t.Errorf("--visible body = %s", cap.Body)
	}

	// --visible=false: explicit false (how a project is made private).
	code, _, errOut = runCLI(t, cfg, "", "projects", "patch", "proj_1", "--visible=false")
	if code != 0 {
		t.Fatalf("--visible=false exit=%d stderr=%s", code, errOut)
	}
	if cap.Body != `{"visible":false}` {
		t.Errorf("--visible=false body = %s", cap.Body)
	}
}

func TestCancelUsageAdvertisesIdOnly(t *testing.T) {
	// Regression (FC-6): the cancel path takes the work item ID only
	// (openapi {id}: "wi_" + 12 random chars; no slug resolution), so the
	// usage text must not advertise <id|slug>.
	code, out, _ := runCLI(t, Config{}, "", "help", "cancel")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, "pf cancel <id>") {
		t.Errorf("usage = %q, want `pf cancel <id>`", out)
	}
	if strings.Contains(out, "slug") {
		t.Errorf("usage = %q, must not advertise slug", out)
	}
}
