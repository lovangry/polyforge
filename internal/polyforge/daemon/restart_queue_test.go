package daemon

// restart_queue_test.go pins v2-converge §5/§6 at the daemon seam:
//   - the persisted submit queue survives a restart and replays (only
//     plain claim intents, only while aihub still shows the WI queued);
//   - a dispatch-time Start failure lands as a durable
//     submit_rejected event instead of a stderr line;
//   - a mid-flight WI whose capsule is missing is blocked at aihub (nothing
//     left parked locally) while every other WI recovers and the daemon
//     starts, and every recovered WI carries a daemon_restarted event.

import (
	"bytes"
	"context"
	"encoding/json"
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

// rejectHub fails every claim — the dispatch-time Start refusal.
type rejectHub struct{ *fakeHub }

func (h *rejectHub) SessionClaim(api.SessionClaimRequest) (*api.SessionClaimResponse, error) {
	return nil, &api.Error{Code: "CORE-409", Detail: "NOT_CLAIMABLE: held elsewhere"}
}

func eventKinds(d *Daemon, wi string) []string {
	var kinds []string
	for _, e := range d.obs.Events().SinceAfter(wi, 0) {
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

// A submit's manual --models list is validated at the route and persisted
// in the queue file, survives a restart, and rides the replayed claim.
func TestSubmitModelsPersistedAndValidated(t *testing.T) {
	setGitIdentity(t)
	var data, hubBase string
	hub := newFakeHub()
	const wi = "wi_q_models"
	td1 := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		c.DispatchTick = time.Hour
		data, hubBase = c.DataDir, c.GitRemoteBase
	})
	mkBare(t, hubBase, wi)
	hub.status[wi] = api.StatusQueued
	for _, bad := range []string{`[]`, `["a",""]`, `["a","a"]`, `["a"," a "]`} {
		if code := postSubmit(t, td1.sock, "/jobs/"+wi+"/claim", `{"models":`+bad+`}`); code != 400 {
			t.Fatalf("models %s: status %d, want 400", bad, code)
		}
	}
	if code := postSubmit(t, td1.sock, "/jobs/"+wi+"/claim", `{"models":[" p/a ","p/b"]}`); code != 200 {
		t.Fatalf("submit status %d", code)
	}
	ents := readQueueFile(t, data)
	if len(ents) != 1 || strings.Join(ents[0].Models, ",") != "p/a,p/b" {
		t.Fatalf("persisted queue = %+v, want the trimmed models list", ents)
	}
	shutdownAndWait(t, td1)

	td2 := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		c.DataDir, c.GitRemoteBase = data, hubBase
		c.SocketPath = filepath.Join(filepath.Dir(data), "s2.sock")
	})
	td2.waitLive(t, wi)
	raw, err := os.ReadFile(filepath.Join(data, "manual-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"p/a"`) || !strings.Contains(string(raw), `"p/b"`) {
		t.Fatalf("replayed claim lost the manual list (daemon-side record): %s", raw)
	}
	ji, _ := td2.rt.Info(wi)
	if cj, err := os.ReadFile(filepath.Join(ji.CapsuleDir, "capsule.json")); err != nil || strings.Contains(string(cj), "manual_candidates") {
		t.Fatalf("capsule.json must not carry the manual list: %v %s", err, cj)
	}
}

// Replay drops what must not run: a WI aihub shows claimed elsewhere, and
// resume and unknown intents — each with a submit_dropped event.
func TestSubmitReplayDropsClaimedAndNonClaimIntents(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	const (
		wiOK    = "wi_r_ok"
		wiTaken = "wi_r_claimed_elsewhere"
		wiTake  = "wi_r_unknown_intent"
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
		for _, e := range []struct{ wi, intent string }{{wiOK, "claim"}, {wiTaken, "claim"}, {wiTake, "bogus"}, {wiRes, "resume"}} {
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
	if hub.claimCount() != 1 {
		t.Fatalf("only the plain queued claim may run (claims=%d)", hub.claimCount())
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
	for _, e := range td.obs.Events().SinceAfter(wi, 0) {
		if e.Kind == observe.EvSubmitRejected && !strings.Contains(string(mustJSON(e)), "NOT_CLAIMABLE") {
			t.Errorf("event must carry the error: %s", mustJSON(e))
		}
	}
	waitFor(t, "queue file drained", 5*time.Second, func() bool { return len(readQueueFile(t, td.cfg.DataDir)) == 0 })
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// Replay-time auto discard (wi_seWTgHeKtaa8's untested gap): the
// persisted queue's origin=auto entries must not re-enter a machine the
// operator turned auto OFF on — the same rule the live auto-off purge
// applies (v2-converge §5), replayed at restart (restartsignal.go).
// An explicit (cli) entry replays as before.
func TestSubmitReplayDropsAutoOriginEntriesWhenAutoOff(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	const (
		wiAuto = "wi_r_auto"
		wiCLI  = "wi_r_cli"
	)
	hub.status[wiAuto] = api.StatusQueued
	hub.status[wiCLI] = api.StatusQueued
	td := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		now := time.Now().UTC()
		// a persisted queue from the previous, auto-ON process: one auto
		// entry and one explicit entry, both still queued at aihub.
		blob, _ := json.Marshal(submitQueueState{Version: 1, Entries: []qEntry{
			{WorkItemID: wiAuto, Intent: "claim", SubmittedAt: now, EnqueuedAt: now, Origin: runtime.Origin{Kind: runtime.OriginAuto}},
			{WorkItemID: wiCLI, Intent: "claim", SubmittedAt: now, EnqueuedAt: now, Origin: runtime.Origin{Kind: runtime.OriginCLI}},
		}})
		if err := os.WriteFile(filepath.Join(c.DataDir, submitQueueFile), blob, 0o600); err != nil {
			t.Fatal(err)
		}
		mkBare(t, c.GitRemoteBase, wiCLI)
	})
	// no persisted mode file: the fresh daemon defaults to auto OFF, so
	// the startup replay must drop the auto entry and replay the
	// explicit one (the path the live auto-off purge mirrors).
	if _, live := td.rt.Info(wiAuto); live {
		t.Errorf("the origin=auto entry must not replay on an auto-off machine")
	}
	if !hasKind(eventKinds(td.Daemon, wiAuto), observe.EvSubmitDropped) {
		t.Errorf("want a submit_dropped event for the auto entry, got %v", eventKinds(td.Daemon, wiAuto))
	}
	td.waitLive(t, wiCLI)
	if hub.claimCount() != 1 {
		t.Fatalf("only the explicit claim may run (claims=%d)", hub.claimCount())
	}
}

// A mid-flight WI with a missing capsule is blocked at aihub (SessionEnd
// blocked naming the path, no local record) while the daemon starts and
// another WI recovers. Every recovered WI gets daemon_restarted.
func TestRestartMissingCapsuleBlocksOnlyThatWI(t *testing.T) {
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
	waitFor(t, "daemon serving with the bad WI blocked", 20*time.Second, func() bool {
		raw, serr := client.Status()
		if serr != nil || json.Unmarshal(raw, &st) != nil {
			return false
		}
		return len(st.Jobs) == 1 && hub.endCount() == 1
	})
	if st.Jobs[0].WorkItemID != good {
		t.Fatalf("only the good WI may stay local (the bad one is blocked at aihub), got %+v", st.Jobs)
	}
	hub.mu.Lock()
	end := hub.ends[0]
	badStatus := hub.status[bad]
	hub.mu.Unlock()
	if end.WI != bad || end.Req.Status != api.StatusBlocked || end.Req.SessionID != e2eSession(bad) ||
		!strings.Contains(end.Req.Note, "capsule missing at "+capBad) || badStatus != api.StatusBlocked {
		t.Fatalf("the bad WI must be blocked at the hub naming the path, got %+v (status %q)", end, badStatus)
	}
	waitFor(t, "good WI recovered (one derivation)", 10*time.Second, func() bool { return sp.callCount() == 1 })
	if hub.endCount() != 1 {
		t.Fatalf("only the bad WI is ended (ends=%d)", hub.endCount())
	}
	if _, serr := os.Stat(capBad); !os.IsNotExist(serr) {
		t.Fatal("the capsule must not be rebuilt")
	}
	// durable events
	if !hasKind(eventKinds(d, bad), "capsule_missing") {
		t.Errorf("want capsule_missing on %s, got %v", bad, eventKinds(d, bad))
	}
	if !hasKind(eventKinds(d, good), observe.EvDaemonRestarted) {
		t.Errorf("want daemon_restarted on %s, got %v", good, eventKinds(d, good))
	}
	for _, e := range d.obs.Events().SinceAfter(good, 0) {
		if e.Kind == observe.EvDaemonRestarted && !strings.Contains(string(mustJSON(e)), "signal") {
			t.Errorf("daemon_restarted must carry the previous shutdown cause: %s", mustJSON(e))
		}
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
