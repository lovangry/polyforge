package daemon

// restart_queue_test.go pins v2-converge §5/§6 at the daemon seam:
//   - the persisted submit queue survives a restart and replays (only
//     plain claim intents, only while aihub still shows the WI queued);
//   - a dispatch-time StartIntent failure lands as a durable
//     submit_rejected event instead of a stderr line;
//   - a mid-flight WI whose capsule is missing parks repair_pending while
//     every other WI recovers, the daemon starts, `pf resume` after the
//     capsule is restored continues the SAME session in place, and every
//     recovered/parked WI carries a daemon_restarted event.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// rejectHub fails every claim — the dispatch-time StartIntent refusal.
type rejectHub struct{ *fakeHub }

func (h *rejectHub) SessionClaim(api.SessionClaimRequest) (*api.SessionClaimResponse, error) {
	return nil, &api.Error{Code: "CORE-409", Detail: "NOT_CLAIMABLE: held elsewhere"}
}

func eventKinds(d *Daemon, wi string) []string {
	var kinds []string
	for _, e := range d.obs.Events().Since(wi) {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func hasKind(kinds []string, k string) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

func readQueueFile(t *testing.T, data string) []qEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(data, submitQueueFile))
	if err != nil {
		t.Fatalf("queue file: %v", err)
	}
	var st submitQueueState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st.Entries
}

func postSubmit(t *testing.T, sock, path string, body string) int {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := hc.Post("http://pf"+path, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func shutdownAndWait(t *testing.T, td *testDaemon) {
	t.Helper()
	_ = td.Shutdown(context.Background(), DirectShutdownCause())
	select {
	case <-td.runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

// An accepted-but-undispatched submit is on disk (submitted_at + origin),
// survives a daemon restart, and replays into a real claim.
func TestSubmitQueueSurvivesRestartAndReplays(t *testing.T) {
	setGitIdentity(t)
	var data, hubBase string
	hub := newFakeHub()
	const wi = "wi_q_replay"
	td1 := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		c.DispatchTick = time.Hour // the submit stays pending
		data, hubBase = c.DataDir, c.GitRemoteBase
	})
	mkBare(t, hubBase, wi)
	hub.status[wi] = api.StatusQueued
	if code := postSubmit(t, td1.sock, "/jobs/"+wi+"/claim", `{"priority":"high","origin":{"chat_id":"chat-9","owner":"usr_1"}}`); code != 200 {
		t.Fatalf("submit status %d", code)
	}
	ents := readQueueFile(t, data)
	if len(ents) != 1 || ents[0].WorkItemID != wi || ents[0].Intent != "claim" || ents[0].SubmittedAt.IsZero() {
		t.Fatalf("persisted queue = %+v", ents)
	}
	if ents[0].Origin != (runtime.Origin{Kind: runtime.OriginChat, ChatID: "chat-9", Owner: "usr_1"}) {
		t.Fatalf("origin/chat not carried: %+v", ents[0].Origin)
	}
	shutdownAndWait(t, td1)

	td2 := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		c.DataDir, c.GitRemoteBase = data, hubBase
		c.SocketPath = filepath.Join(filepath.Dir(data), "s2.sock")
	})
	td2.waitLive(t, wi)
	if hub.claimCount() != 1 {
		t.Fatalf("replayed submit must claim exactly once, got %d", hub.claimCount())
	}
	if ji, ok := td2.rt.Info(wi); !ok || ji.Origin == nil || *ji.Origin != (runtime.Origin{Kind: runtime.OriginChat, ChatID: "chat-9", Owner: "usr_1"}) {
		t.Fatalf("replayed claim must hand the persisted origin to the job record, got %+v", ji.Origin)
	}
	// the dequeue is persisted too
	waitFor(t, "queue file drained", 5*time.Second, func() bool { return len(readQueueFile(t, data)) == 0 })
}

// Replay drops what must not run: a WI aihub shows claimed elsewhere, and
// takeover/resume intents — each with a submit_dropped event.
func TestSubmitReplayDropsClaimedAndNonClaimIntents(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	const (
		wiOK    = "wi_r_ok"
		wiTaken = "wi_r_claimed_elsewhere"
		wiTake  = "wi_r_takeover"
		wiRes   = "wi_r_resume"
	)
	hub.status[wiOK] = api.StatusQueued
	hub.status[wiTaken] = api.StatusRunning
	hub.status[wiTake] = api.StatusQueued
	hub.status[wiRes] = api.StatusQueued
	td := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		now := time.Now().UTC()
		var es []qEntry
		for _, e := range []struct{ wi, intent string }{{wiOK, "claim"}, {wiTaken, "claim"}, {wiTake, "takeover"}, {wiRes, "resume"}} {
			es = append(es, qEntry{WorkItemID: e.wi, Intent: e.intent, SubmittedAt: now, EnqueuedAt: now})
		}
		blob, _ := json.Marshal(submitQueueState{Version: 1, Entries: es})
		if err := os.WriteFile(filepath.Join(c.DataDir, submitQueueFile), blob, 0o600); err != nil {
			t.Fatal(err)
		}
		mkBare(t, c.GitRemoteBase, wiOK)
	})
	td.waitLive(t, wiOK)
	for _, w := range []string{wiTaken, wiTake, wiRes} {
		if !hasKind(eventKinds(td.Daemon, w), observe.EvSubmitDropped) {
			t.Errorf("%s: want a submit_dropped event, got %v", w, eventKinds(td.Daemon, w))
		}
		if _, live := td.rt.Info(w); live {
			t.Errorf("%s must not be started", w)
		}
	}
	if hub.claimCount() != 1 || len(hub.takeovers) != 0 {
		t.Fatalf("only the plain queued claim may run (claims=%d takeovers=%d)", hub.claimCount(), len(hub.takeovers))
	}
}

// A refused start of an accepted submit is a durable submit_rejected event
// on the WI (replayed entry here; the live dispatch shares the path).
func TestSubmitRejectedRecordedAsEvent(t *testing.T) {
	setGitIdentity(t)
	hub := &rejectHub{newFakeHub()}
	const wi = "wi_q_rejected"
	hub.status[wi] = api.StatusQueued
	td := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		now := time.Now().UTC()
		blob, _ := json.Marshal(submitQueueState{Version: 1, Entries: []qEntry{{WorkItemID: wi, Intent: "claim", SubmittedAt: now, EnqueuedAt: now}}})
		_ = os.WriteFile(filepath.Join(c.DataDir, submitQueueFile), blob, 0o600)
	})
	waitFor(t, "submit_rejected event", 10*time.Second, func() bool {
		return hasKind(eventKinds(td.Daemon, wi), observe.EvSubmitRejected)
	})
	for _, e := range td.obs.Events().Since(wi) {
		if e.Kind == observe.EvSubmitRejected && !strings.Contains(string(mustJSON(e)), "NOT_CLAIMABLE") {
			t.Errorf("event must carry the error: %s", mustJSON(e))
		}
	}
	waitFor(t, "queue file drained", 5*time.Second, func() bool { return len(readQueueFile(t, td.cfg.DataDir)) == 0 })
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// A mid-flight WI with a missing capsule parks repair_pending (session
// kept) while the daemon starts and another WI recovers; `pf resume`
// refuses with a typed error until the capsule is restored, then resumes
// the same session in place. Every recovered/parked WI gets daemon_restarted.
func TestRestartMissingCapsuleParksOnlyThatWI(t *testing.T) {
	setGitIdentity(t)
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	data := filepath.Join(dir, "data")
	hubBase := filepath.Join(dir, "hub")
	for _, p := range []string{ws, data, hubBase} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const bad, good = "wi_bad_capsule", "wi_good_capsule"
	bareBad, bareGood := mkBare(t, hubBase, bad), mkBare(t, hubBase, good)
	capGood := filepath.Join(ws, "capsules", good)
	gitGood := filepath.Join(data, "git", good+".git")
	seedCapsuleGit(t, capGood, gitGood, bareGood)
	seedTranscript(t, capGood, apiSessionFingerprint(e2eSession(good)))
	capBad := filepath.Join(ws, "capsules", bad)
	gitBad := filepath.Join(data, "git", bad+".git")
	seedStateJobs(t, data, "m-e2e",
		e2eRecord(bad, capBad, gitBad, bareBad),
		e2eRecord(good, capGood, gitGood, bareGood),
	)
	hub := newFakeHub()
	hub.hold(bad, e2eSession(bad), api.StatusRunning)
	hub.hold(good, e2eSession(good), api.StatusRunning)
	sp := &fakeSpawner{}
	sock := filepath.Join(dir, "d.sock")
	cfg := e2eConfig(sock, ws, data, hubBase, hub, sp)
	// a prior graceful shutdown left its attribution (#110)
	if err := os.WriteFile(filepath.Join(data, lastShutdownFile), []byte(`{"kind":"signal","detail":"terminated (signal 15)","at":"2026-10-12T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(context.Background()) }()
	t.Cleanup(func() {
		_ = d.Shutdown(context.Background(), DirectShutdownCause())
		select {
		case <-runErr:
		case <-time.After(15 * time.Second):
		}
	})
	client := cli.NewDaemonClient(sock)
	var st cli.StatusResponse
	waitFor(t, "daemon serving with the bad WI parked", 20*time.Second, func() bool {
		raw, serr := client.Status()
		if serr != nil || json.Unmarshal(raw, &st) != nil {
			return false
		}
		return len(st.Jobs) == 2
	})
	byWI := map[string]cli.StatusJob{}
	for _, j := range st.Jobs {
		byWI[j.WorkItemID] = j
	}
	if byWI[bad].Status != "repair_pending" || byWI[bad].RepairReason != "capsule_missing" {
		t.Fatalf("pf ops status must show %s repair_pending/capsule_missing, got %+v", bad, byWI[bad])
	}
	if byWI[good].Status == "repair_pending" {
		t.Fatalf("the good WI must recover normally, got %+v", byWI[good])
	}
	waitFor(t, "good WI recovered (one derivation)", 10*time.Second, func() bool { return sp.callCount() == 1 })
	if hub.endCount() != 0 {
		t.Fatalf("parking must not end the session (ends=%d)", hub.endCount())
	}
	if _, serr := os.Stat(capBad); !os.IsNotExist(serr) {
		t.Fatal("the capsule must not be rebuilt")
	}
	// durable events
	if !hasKind(eventKinds(d, bad), "capsule_missing") {
		t.Errorf("want capsule_missing on %s, got %v", bad, eventKinds(d, bad))
	}
	for _, w := range []string{bad, good} {
		if !hasKind(eventKinds(d, w), observe.EvDaemonRestarted) {
			t.Errorf("want daemon_restarted on %s, got %v", w, eventKinds(d, w))
		}
	}
	for _, e := range d.obs.Events().Since(good) {
		if e.Kind == observe.EvDaemonRestarted && !strings.Contains(string(mustJSON(e)), "signal") {
			t.Errorf("daemon_restarted must carry the previous shutdown cause: %s", mustJSON(e))
		}
	}

	// still broken: typed error, still parked
	_, rerr := client.Resume(bad, "normal")
	var ae *api.Error
	if rerr == nil || !errors.As(rerr, &ae) || ae.Code != api.ErrDaemonRepairPending || !strings.Contains(ae.Detail, capBad) {
		t.Fatalf("resume with the capsule still missing must answer %s naming the path, got %v", api.ErrDaemonRepairPending, rerr)
	}
	// the owner restores the capsule; resume continues the same session
	seedCapsuleGit(t, capBad, gitBad, bareBad)
	seedTranscript(t, capBad, apiSessionFingerprint(e2eSession(bad)))
	if _, rerr := client.Resume(bad, "normal"); rerr != nil {
		t.Fatalf("resume after restoring the capsule: %v", rerr)
	}
	waitFor(t, "bad WI resumed in place", 10*time.Second, func() bool { return sp.callCount() == 2 })
	ji, ok := d.rt.Info(bad)
	if !ok || ji.RepairPending != "" || ji.SessionID != e2eSession(bad) {
		t.Fatalf("same session must continue, got ok=%v %+v", ok, ji)
	}
	if hub.endCount() != 0 || hub.claimCount() != 0 {
		t.Fatalf("in-place resume must not end/re-claim (ends=%d claims=%d)", hub.endCount(), hub.claimCount())
	}
}

func (h *fakeHub) claimCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.claims)
}

// hold records sess as the WI's session holder at the given status.
func (h *fakeHub) hold(wi, sess, status string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held[wi] = sess
	h.status[wi] = status
}

// Abandon (v2 §6): a repair_pending WI is given up through the daemon
// route — the daemon ends the kept session with the verdict, drops the
// parked record and writes repair_abandoned. The plain wrap route refuses a
// parked WI with a pointer to the owner moves; an unparked WI is DAEMON-404.
func TestAbandonRepairPendingRoute(t *testing.T) {
	setGitIdentity(t)
	for _, verdict := range []string{"canceled", "failed"} {
		t.Run(verdict, func(t *testing.T) {
			dir := t.TempDir()
			ws := filepath.Join(dir, "ws")
			data := filepath.Join(dir, "data")
			hubBase := filepath.Join(dir, "hub")
			for _, p := range []string{ws, data, hubBase} {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			const bad = "wi_abandon_capsule"
			bareBad := mkBare(t, hubBase, bad)
			capBad := filepath.Join(ws, "capsules", bad)
			gitBad := filepath.Join(data, "git", bad+".git")
			seedStateJobs(t, data, "m-e2e", e2eRecord(bad, capBad, gitBad, bareBad))
			hub := newFakeHub()
			hub.hold(bad, e2eSession(bad), api.StatusRunning)
			sock := filepath.Join(dir, "d.sock")
			d, err := New(e2eConfig(sock, ws, data, hubBase, hub, &fakeSpawner{}))
			if err != nil {
				t.Fatal(err)
			}
			runErr := make(chan error, 1)
			go func() { runErr <- d.Run(context.Background()) }()
			t.Cleanup(func() {
				_ = d.Shutdown(context.Background(), DirectShutdownCause())
				select {
				case <-runErr:
				case <-time.After(15 * time.Second):
				}
			})
			client := cli.NewDaemonClient(sock)
			waitFor(t, "the WI parked repair_pending", 20*time.Second, func() bool {
				var st cli.StatusResponse
				raw, serr := client.Status()
				return serr == nil && json.Unmarshal(raw, &st) == nil && len(st.Jobs) == 1 && st.Jobs[0].Status == "repair_pending"
			})

			// the plain close route must not touch a parked WI
			_, werr := client.Wrap(bad, "fail")
			var ae *api.Error
			if werr == nil || !errors.As(werr, &ae) || ae.Code != api.ErrDaemonConflict {
				t.Fatalf("wrap on a parked WI must answer %s, got %v", api.ErrDaemonConflict, werr)
			}
			if hub.endCount() != 0 {
				t.Fatalf("no SessionEnd before the abandon (ends=%d)", hub.endCount())
			}
			if _, aerr := client.AbandonRepair("wi_not_parked", verdict); aerr == nil || !errors.As(aerr, &ae) || ae.Code != api.ErrDaemonNotFound {
				t.Fatalf("an unparked WI must answer %s, got %v", api.ErrDaemonNotFound, aerr)
			}
			if _, aerr := client.AbandonRepair(bad, "wrapped"); aerr == nil || !errors.As(aerr, &ae) || ae.Code != api.ErrDaemonBadInput {
				t.Fatalf("a non-abandon verdict must answer %s, got %v", api.ErrDaemonBadInput, aerr)
			}

			if _, aerr := client.AbandonRepair(bad, verdict); aerr != nil {
				t.Fatalf("abandon: %v", aerr)
			}
			if hub.endCount() != 1 {
				t.Fatalf("want exactly one SessionEnd, got %d", hub.endCount())
			}
			end := hub.ends[0]
			if end.WI != bad || end.Req.Status != verdict || end.Req.SessionID != e2eSession(bad) {
				t.Fatalf("SessionEnd = %+v", end)
			}
			if _, ok := d.rt.Info(bad); ok {
				t.Fatal("the parked record must be dropped")
			}
			if !hasKind(eventKinds(d, bad), "repair_abandoned") {
				t.Errorf("want repair_abandoned, got %v", eventKinds(d, bad))
			}
			var st cli.StatusResponse
			raw, _ := client.Status()
			_ = json.Unmarshal(raw, &st)
			if len(st.Jobs) != 0 {
				t.Errorf("status must no longer list the WI: %+v", st.Jobs)
			}
		})
	}
}
