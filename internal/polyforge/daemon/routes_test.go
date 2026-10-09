package daemon

// routes_test.go: full UDS round trips over a real unix socket, driven
// through the CLI's own DaemonClient so every response is asserted
// against the pinned CLI contract field by field (submit/pause/resume/
// wrap dual-path arbitration/ship/ops + the top-level api.Error
// envelope).

import (
	"context"
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
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
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
	if st.MachineID != "m-test" || st.Mode != protocol.ModeInteractive || st.ShuttingDown {
		t.Fatalf("status head = %+v", st)
	}
	// #103: the status carries the active workspace (capsule base).
	if st.Workspace != td.cfg.Workspace {
		t.Fatalf("status workspace = %q, want the launched -workspace value %q", st.Workspace, td.cfg.Workspace)
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

// TestAdoptSessionCustody (#109): the adopt-session route verify-backs
// the handoff against the hub (only the LIVE holder's secret is
// adopted), journals the custody durably, and answers the PUBLIC
// fingerprint — never the secret.
func TestAdoptSessionCustody(t *testing.T) {
	td := newTestDaemon(t, nil)
	const wi = "wi_adopt"
	const secret = "abababababababababababababababababababababababababababababababab"
	// The hub names the presented secret as the live holder (the CLI
	// takeover's swap landed first — the production order of events).
	td.hub.mu.Lock()
	td.hub.held[wi] = secret
	td.hub.status[wi] = api.StatusRunning
	td.hub.mu.Unlock()

	raw, err := td.client.AdoptSession(wi, cli.AdoptSessionRequest{SessionID: secret, Owner: "op", Reason: "custody test"})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	var ar struct {
		Adopted            bool   `json:"adopted"`
		WorkItemID         string `json:"work_item_id"`
		SessionFingerprint string `json:"session_fingerprint"`
	}
	if err := json.Unmarshal(raw, &ar); err != nil {
		t.Fatalf("adopt answer undecodable: %v (%s)", err, raw)
	}
	if !ar.Adopted || ar.WorkItemID != wi || ar.SessionFingerprint != api.SessionFingerprint(secret) {
		t.Fatalf("adopt answer = %+v", ar)
	}
	// Custody is durable: the journal carries the secret (the anchor a
	// restart's reconciliation verifies against the hub).
	rec := readAdoptedJournal(t, td.cfg.DataDir, wi)
	if rec == nil || rec.SessionID != secret || rec.MachineID != "m-test" {
		t.Fatalf("custody journal = %+v, want the adopted secret under m-test", rec)
	}

	// A foreign secret is refused (verify-backed) and never replaces the
	// journalled custody.
	if _, err := td.client.AdoptSession(wi, cli.AdoptSessionRequest{SessionID: strings.Repeat("ff", 32)}); err == nil {
		t.Fatal("adopt of a non-holder secret must fail")
	}
	if rec := readAdoptedJournal(t, td.cfg.DataDir, wi); rec == nil || rec.SessionID != secret {
		t.Fatalf("refused adoption must not touch the journal: %+v", rec)
	}
}

// readAdoptedJournal reads one work item's custody entry from the
// daemon's adopted.json journal under dataDir (nil when absent). The
// journal shape is pinned here field by field (the runtime owns the
// type; this decode is the wire contract's guard).
func readAdoptedJournal(t *testing.T, dataDir, wi string) *adoptedJournalRec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "adopted.json"))
	if err != nil {
		t.Fatalf("adopted journal: %v", err)
	}
	var af struct {
		Items map[string]*adoptedJournalRec `json:"items"`
	}
	if err := json.Unmarshal(raw, &af); err != nil {
		t.Fatalf("adopted journal unparsable: %v", err)
	}
	return af.Items[wi]
}

type adoptedJournalRec struct {
	WorkItemID string `json:"work_item_id"`
	SessionID  string `json:"session_id"`
	MachineID  string `json:"machine_id"`
}

// TestObserveAnswerFlush walks the observe lane's own routes end to end
// through the daemon socket: JobView, flush(needs_input), the pending
// projection, the answer relay consumed by the (fake) agent.
func TestObserveAnswerFlush(t *testing.T) {
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

	// The agent's needs_input lifeline (ruling C2: UDS proxy).
	fraw, err := td.client.Flush("wi_b", protocol.FlushRequest{
		WorkItem: "wi_b", SpawnN: jv.SpawnN,
		Reason: protocol.FlushNeedsInput, QuestionID: "q_abc123",
		Question: "which flavor?", Step: "spec",
	})
	if err != nil {
		t.Fatalf("flush(needs_input): %v", err)
	}
	var ack protocol.FlushAck
	if err := json.Unmarshal(fraw, &ack); err != nil || !ack.OK {
		t.Fatalf("flush(needs_input) ack = %s (%v)", fraw, err)
	}

	raw, _ = td.client.Observe("wi_b", 0)
	json.Unmarshal(raw, &jv)
	if jv.Status != protocol.JobNeedsInput || jv.Pending == nil || jv.Pending.ID != "q_abc123" {
		t.Fatalf("needs_input projection = %s / %+v", jv.Status, jv.Pending)
	}

	araw, err := td.client.Answer("wi_b", cli.AnswerPayload{
		QuestionID: "q_abc123", AnswerID: "a_xy1234",
		AnswerText: "mint", AnsweredAt: "2026-01-01T00:00:00Z", AnsweredBy: "u_1",
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	var out struct {
		Outcome  string `json:"outcome"`
		AnswerID string `json:"answer_id"`
	}
	if err := json.Unmarshal(araw, &out); err != nil || out.Outcome != "consumed" {
		t.Fatalf("answer outcome = %s (%v)", araw, err)
	}
	// The relay reached the agent over the stdio control channel.
	if !td.spawner.agentOf(0).sentAnswer("q_abc123", "mint") {
		t.Fatalf("agent never consumed the answer: %+v", td.spawner.agentOf(0).sent)
	}

	// Idempotent replay of the same answer_id: consumed again, never
	// re-delivered.
	araw, err = td.client.Answer("wi_b", cli.AnswerPayload{
		QuestionID: "q_abc123", AnswerID: "a_xy1234",
		AnswerText: "mint", AnsweredAt: "2026-01-01T00:00:00Z", AnsweredBy: "u_1",
	})
	if err != nil {
		t.Fatalf("answer replay: %v", err)
	}
	json.Unmarshal(araw, &out)
	if out.Outcome != "consumed" {
		t.Fatalf("replay outcome = %s", araw)
	}
}

// TestWrapDualPathArbitration is the load-bearing close arbitration:
// the CLI wrap (Runtime.Stop) and the agent's flush(wrap) (Observe →
// OBCompleted → Runtime) racing on one work item must produce exactly
// one winner; the late arrival answers the typed DAEMON-409.
func TestWrapDualPathArbitration(t *testing.T) {
	// (a) flush(wrap) first, its commit+push deliberately slow so the
	// seal guard is observably held; the CLI wrap must get DAEMON-409.
	td := newTestDaemon(t, func(c *Config) {
		c.PushFunc = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, gitops.PushTimings, error) {
			time.Sleep(400 * time.Millisecond)
			return gitops.CommitPush(o, msg)
		}
	})
	td.submitClaim(t, "wi_c")
	td.waitMonitored(t, "wi_c")
	// The flush face is attempt-fenced (#8): play the agent with its
	// CURRENT attempt id, as the live one would.
	craw, err := td.client.Observe("wi_c", 0)
	if err != nil {
		t.Fatalf("observe wi_c: %v", err)
	}
	var cjv protocol.JobView
	if err := json.Unmarshal(craw, &cjv); err != nil || cjv.SpawnN == 0 {
		t.Fatalf("wi_c view undecodable: %v (%s)", err, craw)
	}

	flushDone := make(chan error, 1)
	go func() {
		_, err := td.client.Flush("wi_c", protocol.FlushRequest{
			WorkItem: "wi_c", SpawnN: cjv.SpawnN,
			Reason: protocol.FlushWrap, FinalStatus: "wrapped",
		})
		flushDone <- err
	}()
	// Let the flush claim the guard (its first action on the route).
	time.Sleep(120 * time.Millisecond)
	if _, err := td.client.Wrap("wi_c", "wrap"); err == nil {
		t.Fatal("concurrent CLI wrap succeeded; want DAEMON-409")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonConflict {
			t.Fatalf("CLI wrap error = %v, want DAEMON-409", err)
		}
	}
	if err := <-flushDone; err != nil {
		t.Fatalf("flush(wrap) winner failed: %v", err)
	}
	// The winner sealed exactly once: one end-generation, terminal.
	waitGone(t, td, "wi_c")
	if td.hub.endCount() != 1 {
		t.Fatalf("end-generation calls = %d, want 1", td.hub.endCount())
	}
	if st := td.hub.lastEnd().Status; st != protocol.FinishWrapped {
		t.Fatalf("end status = %q, want wrapped", st)
	}

	// (b) the reverse: CLI wrap first with the server-side end call
	// parked, so the wrap route holds the guard in sealing; the agent's
	// flush(wrap) must get DAEMON-409; releasing the park completes the
	// wrap as the winner.
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
	draw, err := td2.client.Observe("wi_d", 0)
	if err != nil {
		t.Fatalf("observe wi_d: %v", err)
	}
	var djv protocol.JobView
	if err := json.Unmarshal(draw, &djv); err != nil || djv.SpawnN == 0 {
		t.Fatalf("wi_d view undecodable: %v (%s)", err, draw)
	}
	if _, err := td2.client.Flush("wi_d", protocol.FlushRequest{
		WorkItem: "wi_d", SpawnN: djv.SpawnN,
		Reason: protocol.FlushWrap, FinalStatus: "wrapped",
	}); err == nil {
		t.Fatal("flush(wrap) during the CLI close succeeded; want DAEMON-409")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonConflict {
			t.Fatalf("flush(wrap) error = %v, want DAEMON-409", err)
		}
	}
	close(td2.hub.blockEnd)
	if err := <-wrapDone; err != nil {
		t.Fatalf("CLI wrap winner failed: %v", err)
	}
	waitGone(t, td2, "wi_d")
}

// TestWrapAlreadyTerminalGraceful: a wrap of an already-sealed work item
// answers gracefully (the idempotent already-terminal shape), not 404.
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

// TestTakeoverAndErrorEnvelope: the takeover intent reaches the claim
// call; unknown targets answer the top-level api.Error envelope
// (DAEMON-404), which the CLI client decodes as a typed error.
func TestTakeoverAndErrorEnvelope(t *testing.T) {
	td := newTestDaemon(t, nil)
	mkBare(t, td.cfg.GitRemoteBase, "wi_g")
	// The takeover intent reaches the aihub claim call.
	if _, err := td.client.Takeover("wi_g", "normal"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	td.waitLive(t, "wi_g")
	// v1.5: the takeover intent reaches the ForceTakeover endpoint with a
	// freshly minted session secret and a reason.
	td.hub.mu.Lock()
	var last api.SessionClaimRequest
	if len(td.hub.takeovers) > 0 {
		last = td.hub.takeovers[len(td.hub.takeovers)-1]
	}
	nTakeovers := len(td.hub.takeovers)
	td.hub.mu.Unlock()
	if nTakeovers != 1 || last.Reason == "" || len(last.SessionID) != 64 {
		t.Fatalf("takeover must reach ForceTakeover with a reason and a fresh 64-hex secret, got %d calls %+v", nTakeovers, last)
	}

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

// TestOpsModesAndGetRecommendation: mode switching over UDS, the batch
// recommendation shape, mode-exclusion refusals.
func TestOpsModesAndGetRecommendation(t *testing.T) {
	td := newTestDaemon(t, nil)
	// get_recommendation: {items:[...]} — every field of api.WorkItem.
	want := []api.WorkItem{{
		ID: "wi_1", Slug: "wi-1", ProjectName: "proj", Goal: "do a thing",
		Status: api.StatusQueued, Priority: api.PriorityHigh,
		Labels: []string{"x"}, BundleName: strp("demo-bundle"),
	}}
	td.hub.mu.Lock()
	td.hub.queues = want
	td.hub.mu.Unlock()
	raw, err := td.client.Mode(cli.ModeRequest{Action: "get_recommendation"})
	if err != nil {
		t.Fatalf("get_recommendation: %v", err)
	}
	var rec struct {
		Items []api.WorkItem `json:"items"`
		Total int            `json:"total"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("recommendation undecodable: %v", err)
	}
	if rec.Total != 1 || len(rec.Items) != 1 {
		t.Fatalf("recommendation = %+v", rec)
	}
	got := rec.Items[0]
	if got.ID != want[0].ID || got.Slug != want[0].Slug || got.Goal != want[0].Goal ||
		got.Status != want[0].Status || got.Priority != want[0].Priority ||
		got.ProjectName != want[0].ProjectName || len(got.Labels) != 1 ||
		got.BundleName == nil || *got.BundleName != "demo-bundle" {
		t.Fatalf("recommendation item = %+v, want %+v", got, want[0])
	}

	// mode set / drain / abort-drain round trips.
	raw, err = td.client.Mode(cli.ModeRequest{Action: "set", Mode: protocol.ModeBatch})
	if err != nil {
		t.Fatalf("mode set batch: %v", err)
	}
	var mr modeResponse
	json.Unmarshal(raw, &mr)
	if mr.Mode != protocol.ModeBatch {
		t.Fatalf("mode set answer = %s", raw)
	}
	if _, err := td.client.Mode(cli.ModeRequest{Action: "drain"}); err != nil {
		t.Fatalf("mode drain: %v", err)
	}
	// Single submits are refused in drain (the loop is the only entry).
	if _, err := td.client.Claim("wi_z", "normal"); err == nil {
		t.Fatal("claim in drain succeeded")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonConflict {
			t.Fatalf("drain claim error = %v, want DAEMON-409", err)
		}
	}
	// batch_submit is refused in drain as well.
	if _, err := td.client.BatchSubmit([]string{"wi_z"}); err == nil {
		t.Fatal("batch_submit in drain succeeded")
	}
	if _, err := td.client.Mode(cli.ModeRequest{Action: "abort-drain"}); err != nil {
		t.Fatalf("abort-drain: %v", err)
	}
	// Bad mode values refuse with DAEMON-400.
	if _, err := td.client.Mode(cli.ModeRequest{Action: "set", Mode: "turbo"}); err == nil {
		t.Fatal("bad mode accepted")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonBadInput {
			t.Fatalf("bad mode error = %v, want DAEMON-400", err)
		}
	}
	// A submit during shutdown answers DAEMON-410.
	td.modes.SetShuttingDown()
	if _, err := td.client.Claim("wi_y", "normal"); err == nil {
		t.Fatal("claim during shutdown succeeded")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonGone {
			t.Fatalf("shutdown claim error = %v, want DAEMON-410", err)
		}
	}
}

// TestBatchSubmitDispatch: a confirmed wave enqueues beyond the
// concurrency cap and backfills as slots open (04 §12 ruling 0).
func TestBatchSubmitDispatch(t *testing.T) {
	td := newTestDaemon(t, func(c *Config) { c.MaxConcurrent = 1 })
	for _, wi := range []string{"wi_p", "wi_q", "wi_r"} {
		mkBare(t, td.cfg.GitRemoteBase, wi)
	}
	raw, err := td.client.BatchSubmit([]string{"wi_p", "wi_q", "wi_r"})
	if err != nil {
		t.Fatalf("batch_submit: %v", err)
	}
	var br batchResponse
	if err := json.Unmarshal(raw, &br); err != nil || br.Submitted != 3 {
		t.Fatalf("batch answer = %s (%v)", raw, err)
	}
	// With max=1 exactly one job runs; the rest stay pending.
	td.waitLive(t, "wi_p")
	time.Sleep(100 * time.Millisecond)
	raw, _ = td.client.Status()
	var st cli.StatusResponse
	json.Unmarshal(raw, &st)
	if st.Queue.Running != 1 || st.Queue.Pending != 2 {
		t.Fatalf("queue after one slot = %+v, want 1 running / 2 pending", st.Queue)
	}
	// Wrapping the running job frees its slot; the next pending entry
	// backfills without any new confirmation.
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
	capDir := filepath.Join(td.cfg.Workspace, "capsules", "wi_s")

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

// TestFlushStaleAttemptRefused (#8): through the daemon's own socket — a
// flush not naming the monitor's current instance answers DAEMON-409
// STALE_ATTEMPT without sealing anything; the live attempt stays closable.
func TestFlushStaleAttemptRefused(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_f")
	td.waitMonitored(t, "wi_f")
	fraw, err := td.client.Observe("wi_f", 0)
	if err != nil {
		t.Fatalf("observe wi_f: %v", err)
	}
	var fjv protocol.JobView
	if err := json.Unmarshal(fraw, &fjv); err != nil || fjv.SpawnN == 0 {
		t.Fatalf("wi_f view undecodable: %v (%s)", err, fraw)
	}

	// A zombie's wrap flush (a stale derivation counter): refused at the fence.
	_, err = td.client.Flush("wi_f", protocol.FlushRequest{
		WorkItem: "wi_f", SpawnN: fjv.SpawnN + 1,
		Reason: protocol.FlushWrap, FinalStatus: "wrapped",
	})
	if err == nil {
		t.Fatal("stale flush(wrap) succeeded; want DAEMON-409 STALE_SPAWN")
	}
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrDaemonConflict || !strings.Contains(ae.Detail, "STALE_SPAWN") {
		t.Fatalf("stale flush error = %v, want DAEMON-409 STALE_SPAWN", err)
	}

	// The live attempt was untouched: the CLI close still wins.
	if _, err := td.client.Wrap("wi_f", "wrap"); err != nil {
		t.Fatalf("wrap after a stale flush: %v", err)
	}
	waitGone(t, td, "wi_f")
	if td.hub.endCount() != 1 {
		t.Fatalf("end-generation calls = %d, want 1", td.hub.endCount())
	}
}

// TestStatusShowsNeedsInput pins the D6 owner face: a job the runtime
// state machine calls running while its monitor holds a pending question
// is presented as needs_input on GET /status, carrying the question id —
// the operator's one-glance surface for "the agent is blocked on you".
func TestStatusShowsNeedsInput(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_askst")
	td.waitMonitored(t, "wi_askst")

	// before the question: plain running bookkeeping, no QUESTION cell
	st := td.statusOf(t)
	if j := statusJobOf(&st, "wi_askst"); j == nil || j.Status != "running" || j.NeedsInput {
		t.Fatalf("pre-question status = %+v", j)
	}

	// the agent blocks on a human (ruling C2 lifeline)
	raw, err := td.client.Observe("wi_askst", 0)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	var jv protocol.JobView
	if err := json.Unmarshal(raw, &jv); err != nil {
		t.Fatalf("observe undecodable: %v", err)
	}
	if _, err := td.client.Flush("wi_askst", protocol.FlushRequest{
		WorkItem: "wi_askst", SpawnN: jv.SpawnN,
		Reason: protocol.FlushNeedsInput, QuestionID: "q_d6001",
		Question: "pick one", Step: "spec",
	}); err != nil {
		t.Fatalf("flush(needs_input): %v", err)
	}

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
