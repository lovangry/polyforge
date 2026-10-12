package daemon

// routes_test.go: full UDS round trips over a real unix socket, driven
// through the CLI's own DaemonClient so every response is asserted
// against the pinned CLI contract field by field (submit/pause/resume/
// wrap dual-path arbitration/ship/ops + the top-level api.Error
// envelope).

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

// TestSubmitAndStatus pins the claim round trip: SubmitResponse shape,
// the job going live, and every StatusResponse field.
func TestSubmitAndStatus(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_a")

	raw, err := td.client.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var st cli.StatusResponse
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("status undecodable: %v", err)
	}
	if st.MachineID != "m-test" || st.Auto || st.ShuttingDown {
		t.Fatalf("status head = %+v", st)
	}
	// #113: the status carries the mapped workspace SET.
	if len(st.Workspaces) != 1 || st.Workspaces[0] != td.cfg.Mapping.Workspaces()[0] {
		t.Fatalf("status workspaces = %v, want the mapped set %v", st.Workspaces, td.cfg.Mapping.Workspaces())
	}
	if len(st.Jobs) != 1 {
		t.Fatalf("jobs = %+v, want 1", st.Jobs)
	}
	j := st.Jobs[0]
	if j.WorkItemID != "wi_a" || j.Status != "running" || j.AgentPID != 0 || j.SpawnN == 0 || j.SessionFingerprint == "" {
		t.Fatalf("status job = %+v", j)
	}
	if st.Queue.Running != 1 || st.Queue.MaxConcurrent != 4 || st.Queue.Pending != 0 {
		t.Fatalf("queue stats = %+v", st.Queue)
	}
}

// TestObserveAnswerFlush walks the observe lane's own routes end to end
// through the daemon socket: JobView, a driver-persisted pending question
// projected from execution.json, and the answer route's refusal.
func TestObserveAnswerPending(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_b")
	td.waitMonitored(t, "wi_b")

	raw, err := td.client.Observe("wi_b", 0)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	var jv protocol.JobView
	if err := json.Unmarshal(raw, &jv); err != nil {
		t.Fatalf("observe undecodable: %v", err)
	}
	if jv.WorkItem != "wi_b" || jv.Project != "proj" || jv.Status != protocol.JobRunning {
		t.Fatalf("job view = %+v", jv)
	}
	if jv.SpawnN == 0 || len(jv.Events) == 0 {
		t.Fatalf("job view missing spawn/events: %+v", jv)
	}

	// The driver parks on a question: pending_question lands in
	// execution.json and the monitor's poll projects it.
	td.seedPendingQuestion(t, "wi_b", "q_abc123", "spec", "which flavor?")
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ = td.client.Observe("wi_b", 0)
		jv = protocol.JobView{}
		json.Unmarshal(raw, &jv)
		if jv.Pending != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if jv.Status != protocol.JobNeedsInput || jv.Pending == nil || jv.Pending.ID != "q_abc123" {
		t.Fatalf("needs_input projection = %s / %+v", jv.Status, jv.Pending)
	}

	// polyforge#106: the answer relay lane is deleted — the WI's engine
	// here is the scripted fake (no bridge Handle), so the answer
	// refuses fast with the typed no-bridge-handle 409 and NO relay
	// reaches the agent.
	araw, aerr := td.client.Answer("wi_b", cli.AnswerPayload{
		QuestionID: "q_abc123", AnswerID: "a_xy1234",
		AnswerText: "mint", AnsweredAt: "2026-01-01T00:00:00Z", AnsweredBy: "u_1",
	})
	if aerr == nil {
		t.Fatalf("answer with no live bridge handle was accepted: %s", araw)
	}
	var ae *api.Error
	if !errors.As(aerr, &ae) || ae.Code != api.ErrDaemonConflict || !strings.Contains(ae.Detail, "bridge") {
		t.Fatalf("answer error = %v, want the typed no-bridge-handle 409", aerr)
	}
}

// TestCloseArbitration is the load-bearing close arbitration: a CLI wrap
// holding the seal guard (its server-side end call parked) makes a
// concurrent wrap and a concurrent pause answer the typed DAEMON-409;
// releasing the park completes the first wrap as the one winner.
func TestCloseArbitration(t *testing.T) {
	td2 := newTestDaemon(t, nil)
	td2.hub.mu.Lock()
	td2.hub.blockEnd = make(chan struct{})
	td2.hub.mu.Unlock()
	td2.submitClaim(t, "wi_d")
	td2.waitMonitored(t, "wi_d")

	wrapDone := make(chan error, 1)
	go func() {
		_, err := td2.client.Wrap("wi_d", "wrap")
		wrapDone <- err
	}()
	waitState(t, td2, "wi_d", "sealing")
	for name, call := range map[string]func() error{
		"wrap":  func() error { _, err := td2.client.Wrap("wi_d", "wrap"); return err },
		"pause": func() error { _, err := td2.client.Pause("wi_d"); return err },
	} {
		err := call()
		if err == nil {
			t.Fatalf("concurrent %s during the CLI close succeeded; want DAEMON-409", name)
		}
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonConflict {
			t.Fatalf("concurrent %s error = %v, want DAEMON-409", name, err)
		}
	}
	close(td2.hub.blockEnd)
	if err := <-wrapDone; err != nil {
		t.Fatalf("CLI wrap winner failed: %v", err)
	}
	waitGone(t, td2, "wi_d")
	if td2.hub.endCount() != 1 {
		t.Fatalf("end-generation calls = %d, want 1", td2.hub.endCount())
	}
}

// TestWrapAlreadyTerminalGraceful: a wrap of an already-sealed work item
// answers gracefully (the idempotent already-terminal shape), not 404.
// TestResumeBlockedOwnerMove (polyforge#116): the §6.4 owner face on a
// workflow parked blocked — the route drives the hub's resume-blocked
// door (blocked → queued) once and then starts the WI live (the
// claim/start path's owner move resumes the parked capsule). A live or
// queued duplicate is refused, the submit-family way.
func TestResumeBlockedOwnerMove(t *testing.T) {
	td := newTestDaemon(t, nil)
	const wi = "wi_parked"
	// The hub names the WI blocked with no session (the bounded close's
	// shape — M2).
	// The CLI has already landed the hub's resume-blocked door with the
	// human's credential: the WI is queued when the daemon route runs.
	td.hub.mu.Lock()
	td.hub.status[wi] = api.StatusQueued
	td.hub.mu.Unlock()
	mkBare(t, td.cfg.GitRemoteBase, wi)

	raw, err := td.client.ResumeBlocked(wi)
	if err != nil {
		t.Fatalf("resume-blocked: %v", err)
	}
	var rr struct {
		Resumed    bool   `json:"resumed"`
		WorkItemID string `json:"work_item_id"`
	}
	if err := json.Unmarshal(raw, &rr); err != nil {
		t.Fatalf("resume-blocked answer undecodable: %v", err)
	}
	if !rr.Resumed || rr.WorkItemID != wi {
		t.Fatalf("resume-blocked answer = %+v, want resumed for %s", rr, wi)
	}
	td.waitLive(t, wi)
	// The daemon must NOT call the hub's resume door with its machine key.
	td.hub.mu.Lock()
	if len(td.hub.resumeBlockeds) != 0 {
		t.Fatalf("the daemon route fired the hub resume door %v; the human's CLI owns that call", td.hub.resumeBlockeds)
	}
	td.hub.mu.Unlock()

	// A live work item refuses the owner's verdict (it owns its starts).
	if _, err := td.client.ResumeBlocked(wi); err == nil {
		t.Fatal("resume-blocked on a live work item must be refused")
	}
}

// TestTerminateBlockedOwnerVerdict (polyforge#116): the §6.4 terminate
// verdict's face — a close that finds no local job because the workflow
// is parked blocked lands the owner's kill verdict at the hub
// (the terminate-blocked door) instead of DAEMON-404; a wrap (success)
// and a pause of a parked workflow answer the owner-move refusal — there
// is nothing to seal.
func TestTerminateBlockedOwnerVerdict(t *testing.T) {
	td := newTestDaemon(t, nil)
	const wi = "wi_parked"
	td.hub.mu.Lock()
	td.hub.status[wi] = api.StatusBlocked
	td.hub.mu.Unlock()

	// `--fail` on a parked workflow is landed by the CLI with the human's
	// key; the daemon route refuses with a hint and never calls the door.
	if _, err := td.client.Wrap(wi, "fail"); err == nil || !strings.Contains(err.Error(), "your own credential") {
		t.Fatalf("daemon wrap --fail on a parked workflow must refuse with the CLI hint, got %v", err)
	}
	td.hub.mu.Lock()
	if len(td.hub.terminateBlockeds) != 0 {
		t.Fatalf("the daemon fired the hub terminate door %v with its machine key", td.hub.terminateBlockeds)
	}
	td.hub.mu.Unlock()

	// A wrap (success) or a pause of a STILL-parked workflow has nothing
	// to seal — the owner's moves are the §6.4 face, not a runtime close.
	// (After the terminate verdict the WI is terminal, and a close answers
	// the graceful already_terminal shape — the arbitration's idempotency.)
	const wi2 = "wi_parked2"
	td.hub.mu.Lock()
	td.hub.status[wi2] = api.StatusBlocked
	td.hub.mu.Unlock()
	if _, err := td.client.Wrap(wi2, "wrap"); err == nil {
		t.Fatal("wrap (success) on a parked workflow must be refused")
	}
	// A pause of a parked workflow: same refusal — the owner's moves are
	// the §6.4 face, not a runtime close.
	if _, err := td.client.Pause(wi2); err == nil {
		t.Fatal("pause on a parked workflow must be refused")
	}
}

func TestWrapAlreadyTerminalGraceful(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_e")
	td.waitMonitored(t, "wi_e")
	if _, err := td.client.Wrap("wi_e", "wrap"); err != nil {
		t.Fatalf("first wrap: %v", err)
	}
	waitGone(t, td, "wi_e")
	td.hub.mu.Lock()
	td.hub.status["wi_e"] = api.StatusWrapped
	td.hub.mu.Unlock()
	raw, err := td.client.Wrap("wi_e", "wrap")
	if err != nil {
		t.Fatalf("second wrap: %v", err)
	}
	var cr closeResponse
	if err := json.Unmarshal(raw, &cr); err != nil || cr.Outcome != "already_terminal" {
		t.Fatalf("second wrap answer = %s (%v)", raw, err)
	}
}

// TestPauseResume (Finding 2, v1.5 review): the operator pause RETAINS
// the job record — the hub hears SessionEnd(paused, clean_pause=true)
// and the local job transitions to the paused state instead of being
// dropped (the old contract destroyed the secret locally, so the M3
// attach could never fire). A repeat pause answers the graceful
// already-paused shape. The resume flow itself (clear clean_pause,
// holder daemon attaches on its next start) is pinned by the runtime
// restart tests and the s03 e2e.
func TestPauseResume(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_f")
	if _, err := td.client.Pause("wi_f"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	waitState(t, td, "wi_f", "paused")
	if st := td.hub.lastEnd().Status; st != "paused" {
		t.Fatalf("pause end status = %q, want paused", st)
	}
	if !td.hub.lastEnd().CleanPause {
		t.Fatal("the operator pause must carry clean_pause=true")
	}
	// A repeat pause is the graceful idempotency answer, not a conflict.
	raw, err := td.client.Pause("wi_f")
	if err != nil {
		t.Fatalf("second pause: %v", err)
	}
	var cr closeResponse
	if err := json.Unmarshal(raw, &cr); err != nil || cr.Outcome != "already_paused" {
		t.Fatalf("second pause answer = %s (%v)", raw, err)
	}
}

// TestErrorEnvelope: unknown targets answer the top-level api.Error
// envelope (DAEMON-404), which the CLI client decodes as a typed error.
func TestErrorEnvelope(t *testing.T) {
	td := newTestDaemon(t, nil)
	// Typed 404 for an unknown work item (the envelope shape the CLI
	// decodes): no local job + aihub lookup fails.
	td.hub.mu.Lock()
	td.hub.status["wi_nope"] = "gone-broken"
	td.hub.mu.Unlock()
	if _, err := td.client.Wrap("wi_nope", "wrap"); err == nil {
		t.Fatal("wrap of unknown wi succeeded")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonNotFound {
			t.Fatalf("wrap unknown error = %v, want DAEMON-404", err)
		}
	}
}

// TestOpsAutoAndMax: the auto switch and the max number over UDS; manual
// submits stay legal while auto is on; bad input and shutdown refusals.
func TestOpsAutoAndMax(t *testing.T) {
	td := newTestDaemon(t, nil)
	on, off, two := true, false, 2
	raw, err := td.client.OpsConfig(cli.OpsConfigRequest{Auto: &on, Max: &two})
	if err != nil {
		t.Fatalf("ops config: %v", err)
	}
	var r opsConfigResponse
	if err := json.Unmarshal(raw, &r); err != nil || !r.Auto || r.Max != 2 {
		t.Fatalf("ops config answer = %s (%v)", raw, err)
	}
	if !td.modes.Auto() || td.q.MaxConcurrent() != 2 {
		t.Fatalf("state not applied: auto=%v max=%d", td.modes.Auto(), td.q.MaxConcurrent())
	}
	// A manual submit is ALLOWED while auto is on (no DAEMON-409).
	if _, err := td.client.Claim("wi_z", "normal"); err != nil {
		t.Fatalf("claim while auto on: %v", err)
	}
	if _, err := td.client.OpsConfig(cli.OpsConfigRequest{Auto: &off}); err != nil {
		t.Fatalf("auto off: %v", err)
	}
	if td.modes.Auto() {
		t.Fatal("auto still on")
	}
	// Bad input refuses with DAEMON-400.
	zero := 0
	for _, bad := range []cli.OpsConfigRequest{{}, {Max: &zero}} {
		_, err := td.client.OpsConfig(bad)
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonBadInput {
			t.Fatalf("bad ops config %+v error = %v, want DAEMON-400", bad, err)
		}
	}
	// A submit and an auto switch during shutdown answer DAEMON-410.
	td.modes.SetShuttingDown()
	if _, err := td.client.Claim("wi_y", "normal"); err == nil {
		t.Fatal("claim during shutdown succeeded")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonGone {
			t.Fatalf("shutdown claim error = %v, want DAEMON-410", err)
		}
	}
	if _, err := td.client.OpsConfig(cli.OpsConfigRequest{Auto: &on}); err == nil {
		t.Fatal("auto switch during shutdown succeeded")
	}
}

// TestSubmitDispatchBackfill: submits beyond the concurrency cap queue
// and backfill as slots open (04 §12 ruling 0).
func TestSubmitDispatchBackfill(t *testing.T) {
	td := newTestDaemon(t, func(c *Config) { c.MaxConcurrent = 1 })
	for _, wi := range []string{"wi_p", "wi_q", "wi_r"} {
		mkBare(t, td.cfg.GitRemoteBase, wi)
		if _, err := td.client.Claim(wi, "normal"); err != nil {
			t.Fatalf("claim %s: %v", wi, err)
		}
	}
	// With max=1 exactly one job runs; the rest stay pending.
	td.waitLive(t, "wi_p")
	time.Sleep(100 * time.Millisecond)
	raw, _ := td.client.Status()
	var st cli.StatusResponse
	json.Unmarshal(raw, &st)
	if st.Queue.Running != 1 || st.Queue.Pending != 2 {
		t.Fatalf("queue after one slot = %+v, want 1 running / 2 pending", st.Queue)
	}
	// Wrapping the running job frees its slot; the next pending entry
	// backfills without any new submission.
	if _, err := td.client.Wrap("wi_p", "wrap"); err != nil {
		t.Fatalf("wrap wi_p: %v", err)
	}
	td.waitLive(t, "wi_q")
}

// TestShip: the wrap→git delivery bridge against a real sealed capsule
// holding one task-repo worktree with its own origin.
func TestShip(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_s")
	capDir := filepath.Join(td.cfg.Mapping.Workspaces()[0], "capsules", "wi_s")

	// A task repo inside the capsule, with its own origin remote and a
	// commit (the "inventory" the agent would have checked out).
	origin := filepath.Join(t.TempDir(), "task-origin.git")
	if out, err := exec.Command("git", "init", "--bare", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare origin: %v: %s", err, out)
	}
	repo := filepath.Join(capDir, "repos", "app")
	if out, err := exec.Command("git", "clone", origin, repo).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "work"}, {"push", "origin", "HEAD:refs/heads/main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if _, err := td.client.Ship("wi_s"); err == nil {
		t.Fatal("ship of a still-active work item succeeded; want DAEMON-409")
	}
	if _, err := td.client.Wrap("wi_s", "wrap"); err != nil {
		t.Fatalf("wrap: %v", err)
	}
	waitGone(t, td, "wi_s")

	raw, err := td.client.Ship("wi_s")
	if err != nil {
		t.Fatalf("ship: %v", err)
	}
	var sr cli.ShipResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		t.Fatalf("ship undecodable: %v", err)
	}
	if sr.WorkItemID != "wi_s" || sr.Branch != "polyforge/wi_s" || len(sr.Repos) != 1 {
		t.Fatalf("ship response = %+v", sr)
	}
	oc := sr.Repos[0]
	if oc.Name != "app" || oc.Branch != "polyforge/wi_s" || !oc.Pushed || oc.Reused {
		t.Fatalf("ship repo outcome = %+v", oc)
	}
	if oc.Head == "" || oc.OriginURL == "" {
		t.Fatalf("ship repo outcome missing head/origin: %+v", oc)
	}
	// The branch exists at the sealed commit on the origin.
	out, err := exec.Command("git", "--git-dir", origin, "rev-parse", "refs/heads/polyforge/wi_s").Output()
	if err != nil || len(out) == 0 {
		t.Fatalf("origin has no polyforge branch: %v %q", err, out)
	}

	// Idempotent re-ship: the branch is reused, nothing pushed.
	raw, err = td.client.Ship("wi_s")
	if err != nil {
		t.Fatalf("re-ship: %v", err)
	}
	json.Unmarshal(raw, &sr)
	if !sr.Repos[0].Reused || sr.Repos[0].Pushed {
		t.Fatalf("re-ship outcome = %+v, want reused", sr.Repos[0])
	}

	// An unknown work item answers DAEMON-404.
	if _, err := td.client.Ship("wi_ghost"); err == nil {
		t.Fatal("ship of unknown wi succeeded")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonNotFound {
			t.Fatalf("ship unknown error = %v, want DAEMON-404", err)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers

func waitGone(t *testing.T, td *testDaemon, wi string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := td.client.Status()
		if err == nil {
			var st cli.StatusResponse
			if json.Unmarshal(raw, &st) == nil {
				found := false
				for _, j := range st.Jobs {
					if j.WorkItemID == wi {
						found = true
					}
				}
				if !found {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never left the job list", wi)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitState(t *testing.T, td *testDaemon, wi, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := td.client.Status()
		if err == nil {
			var st cli.StatusResponse
			if json.Unmarshal(raw, &st) == nil {
				for _, j := range st.Jobs {
					if j.WorkItemID == wi && j.Status == state {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached state %s", wi, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func strp(s string) *string { return &s }

// dialWorks reports whether the daemon socket still accepts connections.
func dialWorks(sock string) bool {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestStatusShowsNeedsInput(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_askst")
	td.waitMonitored(t, "wi_askst")

	// before the question: plain running bookkeeping, no QUESTION cell
	st := td.statusOf(t)
	if j := statusJobOf(&st, "wi_askst"); j == nil || j.Status != "running" || j.NeedsInput {
		t.Fatalf("pre-question status = %+v", j)
	}

	// the driver parks on a human question (pending_question persisted)
	td.seedPendingQuestion(t, "wi_askst", "q_d6001", "spec", "pick one")

	// /status presents needs_input + the question id (the monitor's
	// projection is applied on the read, so a small poll window is legal)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := td.statusOf(t)
		j := statusJobOf(&st, "wi_askst")
		if j != nil && j.NeedsInput && j.Status == protocol.JobNeedsInput && j.QuestionID == "q_d6001" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("needs_input never surfaced on /status: %+v", st.Jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// statusOf reads GET /status once, decoded.
func (td *testDaemon) statusOf(t *testing.T) cli.StatusResponse {
	t.Helper()
	raw, err := td.client.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var st cli.StatusResponse
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("status undecodable: %v", err)
	}
	return st
}

// statusJobOf finds one jobs[] entry by work item id.
func statusJobOf(s *cli.StatusResponse, wi string) *cli.StatusJob {
	for i := range s.Jobs {
		if s.Jobs[i].WorkItemID == wi {
			return &s.Jobs[i]
		}
	}
	return nil
}
