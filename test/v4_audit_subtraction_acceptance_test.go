package test

// v4_audit_subtraction_acceptance_test.go: the polyforge#148 acceptance
// suite (audit-subtraction pass B) — authored against the SPEC
// (capsule artifacts/spec.md, mirrored scenario-by-scenario in
// artifacts/e2e-scenarios.md) BEFORE the change lands. Like the v3
// legacy-deletion suite, it pins the post-change contract through the
// project's real e2e surfaces only: the pf/fakepi binaries, the `pf`
// CLI, the daemon's UDS face (CLI client + raw HTTP), the capsule's
// runtime-state / committed-logs surfaces, the skills tree and the
// design docs — never engine internals.
//
// Suite posture (see artifacts/e2e-scenarios.md for the full mapping):
//   - Kept-face guards stay green BOTH before and after the change
//     (watch chat-id resolution: env only, env + --chat; the answer
//     route's not-running refusal; no --no-extensions in the step argv).
//   - New-behavior bars are RED before the change — assertion-level
//     reds, never build reds — and must go green after: the restart
//     verify outage park, the consecutive-recovery-failure bound, the
//     chat-less origin kind, the wrap --fail hint, unmapped recall /
//     remember, the skills rule drops, the attention classes beyond
//     needs_input, the question id on the wake line, the DAEMON-* answer
//     envelope, the watch precedence fixes, the step-agent directory,
//     the deleted-face grep floor and the doc-sync floor.
//
// Self-policing: the retired-face tokens this suite greps for are
// assembled at RUNTIME from fragments (v4Tok) so that THIS file never
// carries any of the tokens it polices.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

// ---------------------------------------------------------------------------
// Retired-face tokens, assembled from fragments at runtime.
// ---------------------------------------------------------------------------

// v4Tok joins parts into one legacy-face token. The fragmentation is
// deliberate: the grep-clean scenarios scan production code INCLUDING
// this file's package (the test tree is scanned for the doc floor), so
// the tokens policed must never appear as contiguous bytes here.
func v4Tok(parts ...string) string {
	return strings.Join(parts, "")
}

// v4Term is one grep-clean floor term: tok is the retired token, sub is
// the directory (repo-relative) the term is policed in — "" polices the
// production code surfaces (internal/ + cmd/, non-test Go and scripts).
type v4Term struct {
	tok string
	sub string
}

// v4ProdTerms is the spec-floor grep list for the deleted mechanisms
// (spec.md: C6/C7/C8, the wrap --fail blocked branch, the dead-code
// sweep). Only face-qualified tokens whose absence is unambiguous
// post-change are listed; wording-level items (the "machine"→"daemon"
// rewording, the drain-pattern rename's replacement name) are document
// scenarios in artifacts/e2e-scenarios.md, not blind token scans.
func v4ProdTerms() []v4Term {
	cliDir := filepath.Join("internal", "polyforge", "cli")
	rtDir := filepath.Join("internal", "polyforge", "runtime")
	return []v4Term{
		// C6: the verify-pending park and its background retry loop.
		{v4Tok("verify", "_", "pending"), ""},
		{v4Tok("retry", "Verify"), ""},
		// C7: the 60s storm ledger and its verdict.
		{v4Tok("storm", "Fails"), ""},
		{v4Tok("last", "Recovery", "At"), ""},
		{v4Tok("recovery", " ", "storm"), ""},
		{v4Tok("stability", " ", "window"), ""},
		// C8: the cli origin kind.
		{v4Tok("Origin", "CLI"), ""},
		// dead code: cgroup layer 0.
		{v4Tok("--", "cgroup", "-root"), ""},
		{"CgroupPath", ""},
		// dead code: capsule identity candidates.
		{"EffectiveCandidates", ""},
		// dead code: the injected-but-unread env markers.
		{v4Tok("PF_", "SESSION", "_HANDLE"), rtDir},
		{v4Tok("PF_", "SPAWN", "_N"), rtDir},
		// dead code: sessionjsonl beyond CopyReadonly.
		{"MarshalRaw", ""},
		{"timestampOf", ""},
		// dead code: gitops.InitLocal's production-reachable path.
		{"InitLocal", rtDir},
		// dead code: the submit-family resume/claim routes and client arms.
		{v4Tok("jobs/{wi}/", "resume\""), ""},
		{"DaemonClient) Resume(", cliDir},
		{"DaemonClient) Claim(", cliDir},
		// dead code: the parked-close compat hints.
		{"parkedWIClose", ""},
		// dead code: observe/watch faces.
		{"Progresses", ""},
		{"LastWorkAge", ""},
		{"ClassifyEvent", ""},
		{v4Tok("StatusSink) ", "Used"), ""},
		{v4Tok("EventLog) ", "Since("), ""},
		{v4Tok("Queue", "[T]) Len"), ""},
		{"JobRunning", ""},
		// dead code: the stale drain pattern name.
		{v4Tok("drain", "_degraded"), ""},
		// dead code: the unreachable daemon-verb help Run.
		{v4Tok("this path is help-only"), ""},
		// C9: the wrap --fail blocked branch's hub door call.
		{v4Tok("Terminate", "Blocked"), cliDir},
	}
}

// v4DocTerms is the doc-sync floor (spec.md deletion hygiene): the
// skills tree and the vnext design docs must carry no stale references
// to the removed mechanisms. Scoped per surface family.
func v4DocTerms() []v4Term {
	return []v4Term{
		{v4Tok("verify", "_", "pending"), "docs/design/polyforge-vnext"}, // guard: never re-referenced
		{v4Tok("storm", " guard"), "docs/design/polyforge-vnext"},
		{v4Tok("an ", "unmapped"), "skills/polyforge"},                // C15: the mapping restriction prose
		{v4Tok("terminate", "-blocked ", "door"), "skills/polyforge"}, // C9: the deleted CLI branch's doc
	}
}

// ---------------------------------------------------------------------------
// Local helpers (nothing here reaches into engine internals).
// ---------------------------------------------------------------------------

// v4PfEnv runs the real pf CLI with a fully controlled environment
// (PF_SERVER/PF_API_KEY/PF_DAEMON_SOCKET plus env) and returns the
// combined output and exit code with NO assertion on either. env's
// keys are added on top of the hermetic process environment; ambient
// PF_CHAT_ID/PF_DAEMON_SOCKET are stripped so chat-id resolution
// scenarios are exact.
func v4PfEnv(t *testing.T, inst *v1Install, sock string, env map[string]string, args ...string) (string, int) {
	t.Helper()
	return v4PfEnvCtx(t, context.Background(), inst, sock, env, args...)
}

// v4PfEnvCtx is v4PfEnv with a caller-owned context (the timeout lever
// for resident-CLI scenarios).
func v4PfEnvCtx(t *testing.T, ctx context.Context, inst *v1Install, sock string, env map[string]string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(ctx, inst.pfPath, append([]string{"--config", hermeticConfigPath(t)}, args...)...)
	base := make([]string, 0, len(os.Environ())+len(env)+6)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "PF_CHAT_ID", "PF_DAEMON_SOCKET", "PF_SERVER", "PF_API_KEY", "PF_USER_ID":
			continue
		}
		base = append(base, kv)
	}
	base = append(base, "PF_SERVER="+inst.url, "PF_API_KEY="+inst.adminKey, "PF_USER_ID=usr_root")
	if sock != "" {
		base = append(base, "PF_DAEMON_SOCKET="+sock)
	}
	for k, v := range env {
		base = append(base, k+"="+v)
	}
	cmd.Env = base
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("pf %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return out.String(), code
}

// v4PostDaemon issues one raw HTTP request against the daemon's UDS
// face and returns the status and the body verbatim — the wire surface
// `pf` itself speaks, so error-envelope scenarios see exactly what a
// client sees.
func v4PostDaemon(t *testing.T, sock, method, path, body string) (int, string) {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	c := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), method, "http://daemon"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, string(raw)
}

// v4ErrCode decodes a `{code, detail}` envelope body and asserts the
// standard shape: a non-empty code and a non-empty detail.
func v4ErrCode(t *testing.T, body string) (code, detail string) {
	t.Helper()
	var e struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body is not a JSON envelope: %v (%q)", err, body)
	}
	if e.Code == "" || e.Detail == "" {
		t.Fatalf("envelope missing code/detail: %q", body)
	}
	return e.Code, e.Detail
}

// v4FakepiJournal reads the fake agent's committed assertion ledger
// (capsule logs surface) as loose maps.
func v4FakepiJournal(t *testing.T, capDir string) []map[string]any {
	t.Helper()
	return fakeJournal(t, capDir)
}

// v4JobOf digs one work item's job out of a daemon status answer.
func v4JobOf(t *testing.T, st cli.StatusResponse, wi string) *cli.StatusJob {
	t.Helper()
	for i := range st.Jobs {
		if st.Jobs[i].WorkItemID == wi {
			return &st.Jobs[i]
		}
	}
	return nil
}

// v4WaitJob polls the daemon status until the work item's job appears
// (restart reconciliation window) and returns it.
func v4WaitJob(t *testing.T, d *v1Daemon, wi string, bound time.Duration) *cli.StatusJob {
	t.Helper()
	var job *cli.StatusJob
	waitUntil(t, bound, "job "+wi+" never appeared in the daemon status", func() bool {
		job = v4JobOf(t, d.status(t), wi)
		return job != nil
	})
	return job
}

// ---------------------------------------------------------------------------
// C6 — restart verify with aihub unreachable parks the WI blocked,
// resume-ready (RED before the change).
// ---------------------------------------------------------------------------

// TestV4RestartVerifyUnreachableBlocksThenResume: a daemon restart
// reconciles a mid-flight WI while aihub is unreachable. The restart
// must park the work item in the ORDINARY blocked state at the hub (no
// local verify-pending state, no background retry), with a blocked
// reason that literally names the unreachable-aihub restart /
// incomplete session verify — and the owner's plain `pf resume` then
// drives the workflow to completion through the normal blocked door.
func TestV4RestartVerifyUnreachableBlocksThenResume(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "v4 c6: restart verify with aihub unreachable -> blocked -> resume",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			// the crash window: the daemon dies mid-fix
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "ms": 120000},
			// the post-resume re-dispatch of fix, then verification
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
		}, nil)
	t.Cleanup(func() { killOrphans(wi) })

	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitStepStatus(t, d, wi, "fix", "in_progress", 90*time.Second)

	// the daemon dies; aihub goes down while it is down
	d.crash()
	inst.stopAihub()

	// restart on the same socket/data/workspace/machine: the restart-time
	// session verify cannot complete. Then aihub returns.
	d2 := restartSameDaemon(t, d, "v4c6-daemon2.log")
	inst.restartAihub()

	// the work item is parked in the ordinary blocked state (the hub's
	// answer, reachable once aihub is back — nothing parks locally)
	inst.waitWIStatus(t, wi, "blocked", 120*time.Second)
	raw := wiRawWithEvents(t, inst, wi)
	if !strings.Contains(raw, "unreachable") || !strings.Contains(raw, "verify") {
		t.Fatalf("the blocked reason does not name the unreachable-aihub restart / incomplete session verify: %s", raw)
	}

	// the owner's ordinary move: pf resume re-establishes execution and
	// the workflow completes
	out := d2.pf(t, "resume", wi)
	if !strings.Contains(out, "resumed") {
		t.Fatalf("resume answer: %q", out)
	}
	inst.waitWIStatus(t, wi, "wrapped", 180*time.Second)
}

// ---------------------------------------------------------------------------
// C7 — recovery failure accounting: one consecutive-failures counter.
// The behavioral bar (bounded by consecutive failures, verdict naming
// them) is a KEPT-FACE guard — green both before and after the change
// (the consecutive ledger is the one that stays); the subtraction bars
// (stormFails / lastRecoveryAt / stability window / the storm verdict
// gone) are the RED-before grep floor in TestV4GrepCleanDeletedFaces.
// ---------------------------------------------------------------------------

// TestV4RecoveryCrashLoopBoundedByConsecutiveFailures: recoveries that
// fail consecutively bound the work item: after the configured maximum
// of consecutive failed recoveries the WI is parked blocked, waiting on
// its owner, and the bounded-fail verdict names consecutive recovery
// failures — driven through the real daemon, the real spawner and real
// step processes (the healthy derivation is killed hard; every
// re-derivation the recovery ladder mints afterwards dies at birth).
// This bar must hold both before the change (the consecutive ledger
// already exists) and after it (the storm ledger's deletion must not
// touch it).
func TestV4RecoveryCrashLoopBoundedByConsecutiveFailures(t *testing.T) {
	inst := newV1Install(t)
	actions := []map[string]any{
		// ordinal 1 is the healthy original: it burns a 2.5s delay (the
		// kill window) before its envelope write
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true, "ms": 2500},
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
	}
	wi := inst.newWI(t, "v4 c7: consecutive recovery failures bound the WI", actions, nil)
	// ONE plan for the whole run: the original derivation executes the
	// actions above, and every derivation the recovery ladder mints
	// afterwards dies at birth (resume AND fresh re-derivations alike).
	writePlanExtra(t, inst, wi, actions, nil, map[string]any{
		"fail_spawn_resume": true,
		"fail_spawn_fresh":  true,
	})
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	// the original derivation is healthy (live, mid-delay), then killed
	waitSpawnCount(t, d, wi, 1, 30*time.Second)
	waitSpawnTranscript(t, d, wi, 1, 30*time.Second)
	pid := latestSpawnPID(t, d, wi)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}

	// the counter: consecutive failed recoveries, then the bound parks
	// the WI blocked with a verdict naming them
	inst.waitWIStatus(t, wi, "blocked", 120*time.Second)
	raw := wiRawWithEvents(t, inst, wi)
	if !strings.Contains(raw, "consecutive") || !strings.Contains(raw, "recover") {
		t.Fatalf("the bounded-fail verdict does not name consecutive recovery failures: %s", raw)
	}
	item, err := inst.client.GetWorkItem(wi)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	if item.Status != "blocked" || item.WaitingOn == nil || *item.WaitingOn != "owner" {
		t.Fatalf("the bounded WI is not blocked waiting on its owner: status %q waiting_on %v", item.Status, item.WaitingOn)
	}
}

// ---------------------------------------------------------------------------
// C8 — chat-less submits record origin kind `chat` (RED before the
// change: kind `cli`), and old persisted kinds are read back as
// chat-less manual submits (RED before the change).
// ---------------------------------------------------------------------------

// TestV4ChatlessSubmitOriginKindChat: an interactive submit with an
// owner and NO chat id is recorded as origin kind `chat` with an empty
// ChatID and the owner as usual; a daemon restart reads the persisted
// record back the same way (no cli kind anywhere, no compat branch).
func TestV4ChatlessSubmitOriginKindChat(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_v4c8"
	wi := inst.newWI(t, "v4 c8: chat-less submit origin kind",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": qid,
				"question": "v4: park here so the record is retained."},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
		}, nil)
	t.Cleanup(func() { killOrphans(wi) })

	d := newV1Daemon(t, inst)
	d.start()
	// chat-less: owner set, PF_CHAT_ID deliberately absent
	out, code := v4PfEnv(t, inst, d.sock, map[string]string{"PF_USER_ID": "usr_root"}, "submit", wi)
	if code != 0 || !strings.Contains(out, "accepted") {
		t.Fatalf("chat-less submit: exit %d, output %q", code, out)
	}
	waitQuestion(t, d, wi, qid, 60*time.Second)

	job := v4WaitJob(t, d, wi, 30*time.Second)
	if job.Origin == nil {
		t.Fatalf("the job carries no origin: %+v", job)
	}
	if job.Origin.Kind != "chat" || job.Origin.ChatID != "" || job.Origin.Owner != "usr_root" {
		t.Fatalf("chat-less origin = %+v, want kind chat, empty chat id, owner usr_root", job.Origin)
	}

	// persist the record (the retained pause), restart, read it back: a
	// persisted kind the post-change code does not recognize (the old cli
	// records) is read as a chat-less manual submit.
	d.pf(t, "pause", wi)
	inst.waitWIStatus(t, wi, "paused", 30*time.Second)
	d.crash()
	d2 := restartSameDaemon(t, d, "v4c8-daemon2.log")
	job2 := v4WaitJob(t, d2, wi, 60*time.Second)
	if job2.Origin == nil {
		t.Fatalf("the restarted job carries no origin: %+v", job2)
	}
	if job2.Origin.Kind != "chat" || job2.Origin.ChatID != "" || job2.Origin.Owner != "usr_root" {
		t.Fatalf("persisted origin read back as %+v, want kind chat, empty chat id, owner usr_root", job2.Origin)
	}

	// dead-code floor: the persisted daemon bookkeeping carries no
	// machine identity (daemonState.machine_id is deleted). The graceful
	// stop persists daemon-state.json under the data dir.
	d2.stop()
	raw, err := os.ReadFile(filepath.Join(d2.dataDir, "daemon-state.json"))
	if err != nil {
		t.Fatalf("daemon-state.json: %v", err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("daemon-state.json unparsable: %v (%s)", err, raw)
	}
	if _, has := st[v4Tok("machine", "_", "id")]; has {
		t.Fatalf("daemon-state.json still carries a machine identity field: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// C9 — pf wrap --fail on a blocked WI prints the dedicated
// resume-or-cancel hint (RED before the change).
// ---------------------------------------------------------------------------

// TestV4WrapFailOnBlockedPrintsResumeOrCancel: the owner runs
// `pf wrap <wi> --fail` on a WI parked blocked (here: the mid-flight
// capsule-missing restart path). The command prints the dedicated
// blocked message — use `pf resume` to continue or `pf cancel` to
// abandon — and the work item stays blocked at the hub (the parked
// workflow is NOT terminated).
func TestV4WrapFailOnBlockedPrintsResumeOrCancel(t *testing.T) {
	inst, _ := newV1InstallTaskRepo(t)
	wi := inst.newWI(t, "v4 c9: wrap --fail on a blocked WI",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "ms": 120000},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
		}, nil)
	t.Cleanup(func() { killOrphans(wi) })

	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitStepStatus(t, d, wi, "fix", "in_progress", 90*time.Second)
	d.crash()
	if err := os.RemoveAll(d.capsuleDir(wi)); err != nil {
		t.Fatal(err)
	}
	d2 := restartSameDaemon(t, d, "v4c9-daemon2.log")
	inst.waitWIStatus(t, wi, "blocked", 60*time.Second)

	out, code := v4PfEnv(t, inst, d2.sock, nil, "wrap", wi, "--fail")
	if !strings.Contains(out, "blocked") || !strings.Contains(out, "pf resume") || !strings.Contains(out, "pf cancel") {
		t.Fatalf("wrap --fail on a blocked WI must print the dedicated blocked message naming pf resume / pf cancel: exit %d, output %q", code, out)
	}
	// the parked workflow was NOT terminated: it stays blocked at the hub
	item, err := inst.client.GetWorkItem(wi)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	if item.Status != "blocked" {
		t.Fatalf("wrap --fail terminated the blocked work item (status %q); the owner must abandon it with pf cancel instead", item.Status)
	}
}

// ---------------------------------------------------------------------------
// C15 — recall / remember work without a local mapping (RED before the
// change); --project stays required (green both).
// ---------------------------------------------------------------------------

// TestV4RecallRememberWithoutLocalMapping: `pf recall` and
// `pf remember` succeed against aihub for a project that has NO local
// workspace mapping on this machine, while an unset --project is still
// an error on both verbs.
func TestV4RecallRememberWithoutLocalMapping(t *testing.T) {
	inst := newV1Install(t)
	// a project that exists on the hub but is not in any local mapping
	// (the hermetic CLI config carries no [workspace] section at all)
	if _, err := inst.client.CreateProject(aihubclient.ProjectCreateRequest{
		Name: "v4unmapped", Description: "v4 c15 unmapped scratch project", Repos: nil,
	}); err != nil {
		t.Fatalf("create unmapped project: %v", err)
	}

	out, code := v4PfEnv(t, inst, "", nil, "recall", "--project", "v4unmapped", "v4 c15 probe")
	if code != 0 || !strings.Contains(out, "hit(s)") {
		t.Fatalf("pf recall for an unmapped project: exit %d, output %q", code, out)
	}

	out, code = v4PfEnv(t, inst, "", nil, "remember", "--project", "v4unmapped",
		"--type", "fact.note", "v4 c15: recall and remember are not mapping-restricted")
	if code != 0 {
		t.Fatalf("pf remember for an unmapped project: exit %d, output %q", code, out)
	}

	// --project remains required on both verbs
	out, code = v4PfEnv(t, inst, "", nil, "recall", "v4 c15 probe")
	if code == 0 || !strings.Contains(out, "required") {
		t.Fatalf("pf recall without --project: exit %d, output %q", code, out)
	}
	out, code = v4PfEnv(t, inst, "", nil, "remember", "--type", "fact.note", "v4 c15 probe")
	if code == 0 || !strings.Contains(out, "required") {
		t.Fatalf("pf remember without --project: exit %d, output %q", code, out)
	}
}

// ---------------------------------------------------------------------------
// C14 — the skills drop the invented rules (RED before the change).
// ---------------------------------------------------------------------------

// TestV4SkillsDropInventedRules: skills/polyforge after the change —
// Memory-First is phrased as recall-only-when-relevant (not
// unconditional recall-first), verbs/queue.md carries no
// re-render/translate/re-order rules, verbs/ops.md carries no
// ten-minutes-of-silence-means-stuck rule.
func TestV4SkillsDropInventedRules(t *testing.T) {
	root := v1Root(t)
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(raw)
	}

	skill := read("skills/polyforge/SKILL.md")
	if strings.Contains(skill, "Before every substantive action, run `pf recall`") {
		t.Fatalf("SKILL.md still carries the unconditional Memory-First recall rule")
	}
	if !strings.Contains(skill, "only when relevant") {
		t.Fatalf("SKILL.md's Memory-First rule is not phrased as recall only when relevant")
	}

	queue := read("skills/polyforge/verbs/queue.md")
	for _, bad := range []string{
		"render it yourself",
		"Present the GOAL column in Chinese",
		"Order rows by PRIORITY",
	} {
		if strings.Contains(queue, bad) {
			t.Fatalf("verbs/queue.md still carries a re-render / translate / re-order rule: %q", bad)
		}
	}

	ops := read("skills/polyforge/verbs/ops.md")
	if strings.Contains(ops, "is a stalled job") {
		t.Fatalf("verbs/ops.md still carries the silence-means-stuck rule")
	}
}

// ---------------------------------------------------------------------------
// D12 — the answer route's errors use the standard DAEMON-* envelope
// (RED before the change: BAD-400 / ANSWER-400 / ANSWER-409).
// ---------------------------------------------------------------------------

// TestV4AnswerRouteErrorsUseStandardEnvelope: every failing POST
// /jobs/{wi}/answer answers with the standard top-level {code, detail}
// api.Error envelope carrying a DAEMON-* code and the correct HTTP
// status: an undecodable body (400), an unknown question id (409), and
// an unnamed question with none pending (400).
func TestV4AnswerRouteErrorsUseStandardEnvelope(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_v4ans"
	wi := inst.newWI(t, "v4 d12: answer route standard error envelope",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": qid,
				"question": "v4: park for the answer-route arms."},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "ms": 120000},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
		}, nil)
	t.Cleanup(func() { killOrphans(wi) })
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitQuestion(t, d, wi, qid, 60*time.Second)

	// arm 1: an undecodable body — 400, DAEMON-* envelope
	status, body := v4PostDaemon(t, d.sock, "POST", "/jobs/"+wi+"/answer", "{not json")
	if status != 400 {
		t.Fatalf("decode-error arm: HTTP %d, want 400 (body %q)", status, body)
	}
	if code, detail := v4ErrCode(t, body); !strings.HasPrefix(code, "DAEMON-") {
		t.Fatalf("decode-error arm: code %q (detail %q), want a DAEMON-* code — body %q", code, detail, body)
	}

	// arm 2: a question id that is not pending — 409, DAEMON-* envelope
	status, body = v4PostDaemon(t, d.sock, "POST", "/jobs/"+wi+"/answer",
		`{"question_id":"q_v4nope","answer_text":"x","answered_by":"usr_root"}`)
	if status != 409 {
		t.Fatalf("unknown-question arm: HTTP %d, want 409 (body %q)", status, body)
	}
	if code, detail := v4ErrCode(t, body); !strings.HasPrefix(code, "DAEMON-") {
		t.Fatalf("unknown-question arm: code %q (detail %q), want a DAEMON-* code — body %q", code, detail, body)
	}

	// arm 3: no question named and none pending (the real one was answered,
	// the next step is running) — 400, DAEMON-* envelope
	d.pf(t, "answer", wi, "--question", qid, "--text", "v4: proceed", "--by", "usr_root")
	waitNoQuestion(t, d, wi, 60*time.Second)
	waitStepStatus(t, d, wi, "fix", "in_progress", 90*time.Second)
	status, body = v4PostDaemon(t, d.sock, "POST", "/jobs/"+wi+"/answer",
		`{"answer_text":"x","answered_by":"usr_root"}`)
	if status != 400 {
		t.Fatalf("no-pending-question arm: HTTP %d, want 400 (body %q)", status, body)
	}
	if code, detail := v4ErrCode(t, body); !strings.HasPrefix(code, "DAEMON-") {
		t.Fatalf("no-pending-question arm: code %q (detail %q), want a DAEMON-* code — body %q", code, detail, body)
	}
}

// ---------------------------------------------------------------------------
// D11 — the needs_input wake line carries the question id (RED before
// the change).
// ---------------------------------------------------------------------------

// TestV4NeedsInputWakeLineCarriesQuestionID: a chat-originated work
// item parks in needs_input; the resident per-chat watcher (the real
// `pf watch --chat` face, --wait-decision) prints exactly the
// fixed-template wake line, which now includes the pending question id
// (charset-safe), and never any free text from the question.
func TestV4NeedsInputWakeLineCarriesQuestionID(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_v4wake-1"
	const secret = "drop the production database now"
	wi := inst.newWI(t, "v4 d11: wake line carries the question id",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": qid,
				"question": secret},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
		}, nil)
	t.Cleanup(func() { killOrphans(wi) })

	d := newV1Daemon(t, inst)
	d.start()
	out, code := v4PfEnv(t, inst, d.sock, map[string]string{
		"PF_CHAT_ID": "chat_v4wake", "PF_USER_ID": "usr_root",
	}, "submit", wi)
	if code != 0 || !strings.Contains(out, "accepted") {
		t.Fatalf("chat-origin submit: exit %d, output %q", code, out)
	}
	waitQuestion(t, d, wi, qid, 60*time.Second)

	// the resident watcher prints the decision line and exits
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wout, wcode := v4PfEnvCtx(t, ctx, inst, d.sock, nil,
		"watch", "--chat", "chat_v4wake", "--wait-decision", "--poll", "200ms", "--no-parent-check")
	lines := nonEmptyLines(wout)
	if len(lines) == 0 {
		t.Fatalf("the resident watcher printed no decision line (exit %d, output %q)", wcode, wout)
	}
	line := lines[0]
	if wcode != 0 {
		t.Fatalf("watch --wait-decision exited %d after printing its line: %q", wcode, wout)
	}
	if !strings.HasPrefix(line, "[polyforge] ") {
		t.Fatalf("the wake line does not start with the fixed template prefix: %q", line)
	}
	if !strings.Contains(line, "needs_input") {
		t.Fatalf("the wake line is not a needs_input decision: %q", line)
	}
	if !strings.Contains(line, qid) {
		t.Fatalf("the wake line does not carry the question id %q: %q", qid, line)
	}
	if !strings.HasSuffix(line, "the agent must not answer it") {
		t.Fatalf("the wake line lost the fixed template tail: %q", line)
	}
	if strings.Contains(wout, secret) {
		t.Fatalf("the wake line leaked free text from the question: %q", wout)
	}
}

// ---------------------------------------------------------------------------
// D9 — owner reminders cover decision classes beyond needs_input (RED
// before the change: attention filters to needs_input only).
// ---------------------------------------------------------------------------

// TestV4OwnerAttentionBeyondNeedsInput: with no live watcher for the
// origin chat, the chat-attention mechanism reminds the owner of the
// work item's decisions — needs_input (the kept face) AND the paused
// class (the new-behavior bar; a class that is not needs_input).
// Spec-level note: the full 11-class table is a document scenario in
// artifacts/e2e-scenarios.md (the terminal/hub-side classes' reminder
// surface is flagged there as under-specified by the spec).
func TestV4OwnerAttentionBeyondNeedsInput(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_v4d9"
	wiNeed := inst.newWI(t, "v4 d9: attention for needs_input (kept)",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": qid,
				"question": "v4: park for the attention probe."},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
		}, nil)
	wiPaused := inst.newWI(t, "v4 d9: attention for a non-needs_input class",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "ms": 120000},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
		}, nil)
	t.Cleanup(func() { killOrphans(wiNeed) })
	t.Cleanup(func() { killOrphans(wiPaused) })

	d := newV1Daemon(t, inst)
	d.start()
	for _, wi := range []string{wiNeed, wiPaused} {
		out, code := v4PfEnv(t, inst, d.sock, map[string]string{
			"PF_CHAT_ID": "chat_v4d9", "PF_USER_ID": "usr_root",
		}, "submit", wi)
		if code != 0 || !strings.Contains(out, "accepted") {
			t.Fatalf("chat-origin submit %s: exit %d, output %q", wi, code, out)
		}
	}
	waitQuestion(t, d, wiNeed, qid, 90*time.Second)
	waitStepStatus(t, d, wiPaused, "fix", "in_progress", 90*time.Second)
	d.pf(t, "pause", wiPaused)
	inst.waitWIStatus(t, wiPaused, "paused", 30*time.Second)

	// no live watcher for the chat (the subscriptions are idle); the
	// owner's attention probe through the real daemon client:
	c := cli.NewDaemonClient(d.sock)
	items, err := c.ChatAttention("chat_v4d9", "usr_root")
	if err != nil {
		t.Fatalf("chat attention: %v", err)
	}
	reasons := map[string]bool{}
	for _, it := range items {
		reasons[it.Reason] = true
	}
	if !reasons["needs_input"] {
		t.Fatalf("attention lost the needs_input class (kept face): %+v", items)
	}
	if !reasons["paused"] {
		t.Fatalf("attention still filters to needs_input only — the paused decision class is absent: %+v", items)
	}
}

// ---------------------------------------------------------------------------
// Addendum 1 — pf watch chat-id resolution precedence (mixed: two
// kept-face guards green both trees, two fixes RED before the change).
// ---------------------------------------------------------------------------

// TestV4WatchChatIDPrecedence: the five spec cases. --chat and
// --harness-session both outrank PF_CHAT_ID (the env + session case is
// RED pre-change: the env wins today); --chat + --harness-session
// together is a usage error (RED pre-change: one is silently picked);
// env-only and the no-chat error are kept faces (green both trees).
func TestV4WatchChatIDPrecedence(t *testing.T) {
	inst := newV1Install(t)
	d := newV1Daemon(t, inst)
	d.start()

	list := func(env map[string]string, args ...string) (string, int) {
		t.Helper()
		return v4PfEnv(t, inst, d.sock, env, append([]string{"watch", "list"}, args...)...)
	}

	// case 1: env only — PF_CHAT_ID is used (kept face)
	out, code := list(map[string]string{"PF_CHAT_ID": "chat_v4env"})
	if code != 0 || !strings.Contains(out, "chat chat_v4env:") {
		t.Fatalf("env-only case: exit %d, output %q", code, out)
	}

	// case 2: env + --chat — the flag wins (kept face)
	out, code = list(map[string]string{"PF_CHAT_ID": "chat_v4env"}, "--chat", "chat_v4flag")
	if code != 0 || !strings.Contains(out, "chat chat_v4flag:") || strings.Contains(out, "chat chat_v4env:") {
		t.Fatalf("env + --chat case: exit %d, output %q", code, out)
	}

	// case 3: env + --harness-session — the session-mapped chat wins
	// (RED pre-change: the environment wins)
	out, code = list(map[string]string{"PF_CHAT_ID": "chat_v4env"}, "--harness-session", "sess_v4")
	if code != 0 {
		t.Fatalf("env + --harness-session case: exit %d, output %q", code, out)
	}
	if !strings.Contains(out, "chat ") || strings.Contains(out, "chat chat_v4env:") {
		t.Fatalf("env + --harness-session case: the session lookup did not win over PF_CHAT_ID: %q", out)
	}

	// case 4: --chat and --harness-session together — a usage error, not
	// a silent pick (RED pre-change: --chat is silently used)
	out, code = list(nil, "--chat", "chat_v4flag", "--harness-session", "sess_v4")
	if code == 0 {
		t.Fatalf("--chat + --harness-session silently picked one instead of failing: exit 0, output %q", out)
	}
	if !strings.Contains(out, "chat") || !strings.Contains(out, "harness-session") {
		t.Fatalf("--chat + --harness-session usage error does not name both flags: %q", out)
	}

	// case 5: nothing provided — the existing no-chat-id error (kept face)
	out, code = list(nil)
	if code == 0 || !strings.Contains(out, "no chat id") {
		t.Fatalf("no-chat case: exit %d, output %q", code, out)
	}
}

// ---------------------------------------------------------------------------
// Addendum 2 — the dedicated step agent directory (RED before the
// change: no PI_CODING_AGENT_DIR at all).
// ---------------------------------------------------------------------------

// TestV4StepAgentDedicatedAgentDirectory: a step pi process spawned by
// the daemon runs with PI_CODING_AGENT_DIR pointing at a dedicated
// directory UNDER THE DAEMON DATA DIR (never a capsule), which declares
// no packages; --no-extensions is NOT among its flags (the subagent
// tool stays); the isolation holds even though the user-level settings
// under HOME declare a background-task package (the owner-reinstall
// shape); and no file of the dedicated directory ever appears inside
// the capsule.
func TestV4StepAgentDedicatedAgentDirectory(t *testing.T) {
	inst := newV1Install(t)
	// the owner-reinstall poison: user-level settings under the (hermetic)
	// HOME declare the background-task package; isolation must survive it
	// by construction (the agent dir is redirected, not the settings)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	agentUserDir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(agentUserDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentUserDir, "settings.json"),
		[]byte(`{"packages":["npm:pi-background-tasks"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	const qid = "q_v4iso"
	wi := inst.newWI(t, "v4 iso: dedicated step agent directory",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": qid,
				"question": "v4: park so the spawn ledger is inspectable."},
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
		}, nil)
	t.Cleanup(func() { killOrphans(wi) })

	d := newV1Daemon(t, inst)
	d.start()
	out, code := v4PfEnv(t, inst, d.sock, map[string]string{"PF_USER_ID": "usr_root"}, "submit", wi)
	if code != 0 || !strings.Contains(out, "accepted") {
		t.Fatalf("submit: exit %d, output %q", code, out)
	}
	waitQuestion(t, d, wi, qid, 90*time.Second)

	// the fake agent's committed spawn ledger records the derivation's
	// argv and the agent-directory override
	capDir := d.capsuleDir(wi)
	var spawn map[string]any
	for _, e := range v4FakepiJournal(t, capDir) {
		if k, _ := e["type"].(string); k == "spawn" {
			spawn = e
		}
	}
	if spawn == nil {
		t.Fatalf("no spawn entry in the fakepi journal (capsule %s)", capDir)
	}
	argv, _ := spawn["argv"].([]any)
	if len(argv) == 0 {
		t.Fatalf("the spawn ledger carries no argv: %+v", spawn)
	}
	flat := make([]string, len(argv))
	for i, a := range argv {
		flat[i], _ = a.(string)
	}
	joined := " " + strings.Join(flat, " ") + " "
	if strings.Contains(joined, " --no-extensions ") {
		t.Fatalf("--no-extensions must never be passed (the subagent tool stays): %v", flat)
	}

	agentDir, _ := spawn["agent_dir"].(string)
	if agentDir == "" {
		t.Fatalf("the step process was not given an agent directory override (PI_CODING_AGENT_DIR absent from its environment)")
	}
	if rel, rerr := filepath.Rel(d.dataDir, agentDir); rerr != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("the dedicated agent directory %s is not under the daemon data dir %s", agentDir, d.dataDir)
	}
	if rel, rerr := filepath.Rel(d.workspace, agentDir); rerr == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("the dedicated agent directory %s lives under the capsule workspace %s (capsules are pushed to aihub — this leaks)", agentDir, d.workspace)
	}
	st, serr := os.Stat(agentDir)
	if serr != nil || !st.IsDir() {
		t.Fatalf("the dedicated agent directory does not exist at %s: %v", agentDir, serr)
	}
	// it declares no packages: any settings.json inside carries no package
	// declarations (user-level packages load from the agent directory)
	var settingsHits []string
	_ = filepath.WalkDir(agentDir, func(p string, de os.DirEntry, err error) error {
		if err != nil || !de.Type().IsRegular() || de.Name() != "settings.json" {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr == nil && strings.Contains(string(raw), "packages") {
			settingsHits = append(settingsHits, p)
		}
		return nil
	})
	if len(settingsHits) > 0 {
		t.Fatalf("the dedicated agent directory declares packages: %v", settingsHits)
	}

	// no file of the dedicated directory ever appears inside the capsule:
	// no capsule path reaches into it, and no capsule file duplicates any
	// of its bytes (credentials included)
	agentHashes := map[[32]byte]string{}
	_ = filepath.WalkDir(agentDir, func(p string, de os.DirEntry, err error) error {
		if err != nil || !de.Type().IsRegular() {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		agentHashes[sha256.Sum256(raw)] = p
		return nil
	})
	var leaks []string
	_ = filepath.WalkDir(capDir, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.Contains(p, agentDir) {
			leaks = append(leaks, p)
			return nil
		}
		if !de.Type().IsRegular() {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if src, dup := agentHashes[sha256.Sum256(raw)]; dup {
			leaks = append(leaks, p+" duplicates "+src)
		}
		return nil
	})
	if len(leaks) > 0 {
		t.Fatalf("capsule content derived from the dedicated agent directory (%d):\n%s", len(leaks), strings.Join(leaks, "\n"))
	}
}

// ---------------------------------------------------------------------------
// v2-converge — the adapter-layer isolation guarantee is documented
// (RED before the change: the mechanism is absent from the design docs).
// ---------------------------------------------------------------------------

// TestV4V2ConvergeDocumentsAdapterIsolation: the vnext design tree
// names the concrete mechanism of the background-task prohibition —
// the dedicated step agent directory (PI_CODING_AGENT_DIR) — so the
// guarantee is defined as the harness adapter layer's responsibility
// with a checkable anchor. The full phrasing judgment (any future
// harness must provide an equivalent guarantee) is a document scenario
// in artifacts/e2e-scenarios.md.
func TestV4V2ConvergeDocumentsAdapterIsolation(t *testing.T) {
	root := v1Root(t)
	docs := filepath.Join(root, "docs", "design", "polyforge-vnext")
	var hits []string
	_ = filepath.WalkDir(docs, func(p string, de os.DirEntry, err error) error {
		if err != nil || !de.Type().IsRegular() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr == nil && bytes.Contains(raw, []byte(v4Tok("PI_", "CODING_", "AGENT_", "DIR"))) {
			hits = append(hits, p)
		}
		return nil
	})
	if len(hits) == 0 {
		t.Fatalf("no vnext design doc names the dedicated step agent directory mechanism")
	}
}

// ---------------------------------------------------------------------------
// Grep-clean floors: deleted faces on code surfaces, synced docs.
// ---------------------------------------------------------------------------

// TestV4GrepCleanDeletedFaces scans the production code surfaces
// (internal/ and cmd/, test files excluded) for the spec-floor term
// list and requires zero hits. RED before the change (the faces are
// present by design), GREEN after.
func TestV4GrepCleanDeletedFaces(t *testing.T) {
	root := v1Root(t)
	var hits []string
	check := func(base string, term v4Term) {
		err := v4WalkProdText(base, func(path string, raw []byte) {
			if bytes.Contains(raw, []byte(term.tok)) {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, rel+": "+term.tok)
			}
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	for _, term := range v4ProdTerms() {
		if term.sub == "" {
			// the unscoped terms police both production trees
			check(filepath.Join(root, "internal"), term)
			check(filepath.Join(root, "cmd"), term)
			continue
		}
		check(filepath.Join(root, filepath.FromSlash(term.sub)), term)
	}
	if len(hits) > 0 {
		t.Fatalf("deleted-face terms still present on production code surfaces (%d hits):\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

// v4WalkProdText walks a production tree's regular text files, skipping
// test files (their deletion/rewrite is judged by the delivery note and
// the behavioral bars, not by this token scan — fakepi and the engine's
// own unit tests legitimately reference retired faces mid-change).
func v4WalkProdText(root string, fn func(path string, raw []byte)) error {
	if _, err := os.Stat(root); err != nil {
		return nil // a scoped dir that does not exist is simply clean
	}
	return filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			if de.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !de.Type().IsRegular() {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			return err
		}
		sniff := raw
		if len(sniff) > 512 {
			sniff = sniff[:512]
		}
		if bytes.IndexByte(sniff, 0) >= 0 {
			return nil // binary
		}
		fn(path, raw)
		return nil
	})
}

// TestV4DocsSyncedForRemovedMechanisms is the doc-sync floor: the
// skills tree and the vnext design docs carry no stale references to
// the removed mechanisms (the storm ledger, the mapping restriction,
// the wrap --fail blocked branch's hub door). RED before the change.
func TestV4DocsSyncedForRemovedMechanisms(t *testing.T) {
	root := v1Root(t)
	var hits []string
	for _, term := range v4DocTerms() {
		base := filepath.Join(root, filepath.FromSlash(term.sub))
		err := filepath.WalkDir(base, func(path string, de os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !de.Type().IsRegular() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if bytes.Contains(raw, []byte(term.tok)) {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, rel+": "+term.tok)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("stale references to removed mechanisms on doc surfaces (%d):\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

// ---------------------------------------------------------------------------
// Kept-face guard: the answer route's not-running refusal already uses
// the standard envelope (green both trees — the D12 change must not
// regress it).
// ---------------------------------------------------------------------------

// TestV4AnswerRouteNotRunningUsesStandardEnvelope: a work item no
// daemon holds answers the answer route with the standard DAEMON-*
// envelope and the correct status (green before AND after).
func TestV4AnswerRouteNotRunningUsesStandardEnvelope(t *testing.T) {
	inst := newV1Install(t)
	d := newV1Daemon(t, inst)
	d.start()
	status, body := v4PostDaemon(t, d.sock, "POST", "/jobs/wi_v4unknown/answer",
		`{"question_id":"q_x","answer_text":"x","answered_by":"usr_root"}`)
	if status != 404 {
		t.Fatalf("not-running answer: HTTP %d, want 404 (body %q)", status, body)
	}
	if code, detail := v4ErrCode(t, body); !strings.HasPrefix(code, "DAEMON-") {
		t.Fatalf("not-running answer: code %q (detail %q), want a DAEMON-* code", code, detail)
	}
}

// TestV4NeedsInputWaitNotCountedIntoT: the wall time a step spends
// PARKED on an owner question must not enter the step's cumulative
// runtime T (T counts agent running time only).
func TestV4NeedsInputWaitNotCountedIntoT(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "v4: the needs_input wait is not agent runtime", plan([]map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "needs_input",
			"question_id": "q_v4wait", "question": "should the wait count into T?"},
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
	}), nil)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	waitQuestion(t, d, wi, "q_v4wait", 60*time.Second)
	// The owner takes their time: a wait far longer than either of the
	// step's two agent dispatches.
	time.Sleep(6 * time.Second)
	answerQuestion(t, d, wi, "q_v4wait", "No — only agent runtime counts.")
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

	x := readExecution(t, capDir)
	wall, _ := x.Steps["grill"].BudgetConsumed["wall_ms"].(float64)
	if wall >= 5000 {
		t.Fatalf("SYMPTOM: grill booked %vms of wall — the 6s owner wait leaked into T (agent runtime is ~2s)", wall)
	}
}

// TestV4BundleFeatureFullLaneWraps: the shipped feature-full bundle
// (grill -> spec -> grill-review -> plan -> e2e-author -> e2e-baseline
// -> code -> e2e-run -> review -> verification) runs end to end through
// the real daemon and wraps — the only e2e run of that lane.
func TestV4BundleFeatureFullLaneWraps(t *testing.T) {
	inst := newV1Install(t)
	actions := []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "spec", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "grill-review", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "plan", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-author", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-baseline", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "code", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-run", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "review", "outcome": "completed"},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
	}
	wi := newWIWithBundle(t, inst, "v4: feature-full lane end to end", "feature-full", actions)
	d := newV1Daemon(t, inst)
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	inst.waitWIStatus(t, wi, "wrapped", 240*time.Second)
	x := readExecution(t, d.capsuleDir(wi))
	if x.WorkflowStatus != "completed" {
		t.Fatalf("workflow status %q, want completed", x.WorkflowStatus)
	}
	for _, step := range []string{"grill", "spec", "grill-review", "plan", "e2e-author", "e2e-baseline", "code", "e2e-run", "review", "verification"} {
		if st, ok := x.Steps[step]; !ok || st.Status != "done" {
			t.Fatalf("step %s not done: %+v", step, st)
		}
	}
}
