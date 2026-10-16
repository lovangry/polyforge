package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
)

func TestSubStorePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), chatSubsFile)
	s, err := newSubStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if ch, err := s.Subscribe("chat_a", "wi_1", "", 7); err != nil || !ch {
		t.Fatalf("subscribe: %v %v", ch, err)
	}
	if ch, _ := s.Subscribe("chat_a", "wi_1", "", 99); ch {
		t.Fatal("re-subscribing an active WI changes nothing and must not move the cursor")
	}
	// a re-subscribe that knows the slug fills a subscription recorded without one
	if _, err := s.Subscribe("chat_a", "wi_1", "proj#1", 99); err != nil {
		t.Fatal(err)
	}
	if subs := s.List("chat_a"); len(subs) != 1 || subs[0].Slug != "proj#1" || subs[0].Cursor != 7 {
		t.Fatalf("slug fill: %+v", subs)
	}
	if err := s.Ack("chat_a", protocol.ChatAck{WI: "wi_1", Cursor: 12, Notified: []string{"needs_input:q-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Subscribe("chat_b", "wi_2", "", 0); err != nil {
		t.Fatal(err)
	}
	// daemon restart: a brand-new store over the same file
	s2, err := newSubStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.List("chat_a")
	if len(got) != 1 || got[0].WI != "wi_1" || got[0].Cursor != 12 || len(got[0].Notified) != 1 || got[0].Notified[0] != "needs_input:q-1" {
		t.Fatalf("after restart: %+v", got)
	}
	if l := s2.List("chat_b"); len(l) != 1 || l[0].WI != "wi_2" {
		t.Fatalf("chat_b: %+v", l)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", st, err)
	}
	// a finished WI is re-armed by a new submit
	_ = s2.Ack("chat_a", protocol.ChatAck{WI: "wi_1", Cursor: 12, Done: true})
	if ch, _ := s2.Subscribe("chat_a", "wi_1", "", 0); !ch {
		t.Fatal("re-subscribe of a finished WI must re-arm it")
	}
	if l := s2.List("chat_a"); l[0].Done || l[0].Cursor != 12 {
		t.Fatalf("re-armed: %+v", l)
	}
	if ch, _ := s2.Unsubscribe("chat_a", "wi_1"); !ch || len(s2.List("chat_a")) != 0 {
		t.Fatal("unsubscribe")
	}
}

func TestSubStoreRefusesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), chatSubsFile)
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := newSubStore(path); err == nil {
		t.Fatal("a corrupt subscriptions file must refuse, not silently reset")
	}
}

func TestWatcherLivenessExpires(t *testing.T) {
	s, _ := newSubStore(filepath.Join(t.TempDir(), chatSubsFile))
	now := time.Now()
	s.now = func() time.Time { return now }
	_, _ = s.Subscribe("chat_a", "wi_1", "", 0)
	if s.Covered("wi_1") {
		t.Fatal("no heartbeat yet: not covered")
	}
	s.Heartbeat("chat_a")
	if !s.Covered("wi_1") {
		t.Fatal("fresh heartbeat covers")
	}
	now = now.Add(liveWatcherTTL + time.Second)
	if s.Covered("wi_1") {
		t.Fatal("expired heartbeat must not cover")
	}
}

func TestChatRoutesAndSubmitAutoSubscribe(t *testing.T) {
	td := newTestDaemon(t, nil)
	// explicit subscribe/list/unsubscribe through the UDS face
	if _, err := td.client.ChatSubscribe("chat_x", "wi_manual", true); err != nil {
		t.Fatal(err)
	}
	subs, err := td.client.ChatSubscriptions("chat_x")
	if err != nil || len(subs.Subscriptions) != 1 || subs.StatusPath != filepath.Join(td.cfg.DataDir, chatStatusDir, "chat_x.status") {
		t.Fatalf("subs = %+v err=%v", subs, err)
	}
	if _, err := td.client.ChatSubscriptions("bad/id"); err == nil {
		t.Fatal("an unsafe chat id must be refused")
	}
	// pf submit from a chat subscribes that chat
	td.client.Origin = &cli.SubmitOrigin{ChatID: "chat_x", Owner: "alice"}
	td.submitClaim(t, "wi_sub")
	subs, _ = td.client.ChatSubscriptions("chat_x")
	found := false
	for _, s := range subs.Subscriptions {
		found = found || s.WI == "wi_sub"
	}
	if !found {
		t.Fatalf("submit must auto-subscribe the submitting chat: %+v", subs)
	}
	// a submit without a chat subscribes nobody
	td.client.Origin = &cli.SubmitOrigin{Owner: "alice"}
	td.submitClaim(t, "wi_nochat")
	for _, s := range mustSubs(t, td, "chat_x") {
		if s.WI == "wi_nochat" {
			t.Fatal("a chat-less submit must not subscribe")
		}
	}
	if _, err := td.client.ChatSubscribe("chat_x", "wi_manual", false); err != nil {
		t.Fatal(err)
	}
}

func mustSubs(t *testing.T, td *testDaemon, chat string) []protocol.ChatSub {
	t.Helper()
	s, err := td.client.ChatSubscriptions(chat)
	if err != nil {
		t.Fatal(err)
	}
	return s.Subscriptions
}

func TestWatchSnapshotRoute(t *testing.T) {
	td := newTestDaemon(t, nil)
	// not registered yet: a normal, empty answer (the watcher shows queued/preparing)
	snap, err := td.client.WatchSnapshot("wi_nothing", 0)
	if err != nil || snap.Known || snap.Execution != nil || snap.Job != nil {
		t.Fatalf("unknown WI: %+v %v", snap, err)
	}
	td.submitClaim(t, "wi_snap")
	td.waitMonitored(t, "wi_snap")
	td.seedPendingQuestion(t, "wi_snap", "q-1", "plan", "Which database?")
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err = td.client.WatchSnapshot("wi_snap", 0)
		if err == nil && snap.Execution != nil && snap.Execution.PendingQuestion != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no execution layer: %+v %v", snap, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap.Execution.PendingQuestion.ID != "q-1" || snap.Execution.CurrentStep != "plan" || !snap.Known || snap.Job == nil {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, err := td.client.WatchSnapshot("wi_snap", -1); err == nil {
		t.Fatal("a negative cursor is bad input")
	}
}

func TestWatchSnapshotReadsFinishedWIFromTheCapsule(t *testing.T) {
	td := newTestDaemon(t, nil)
	// a WI the daemon holds no job for, but whose capsule is on disk
	// (finished while the watcher was down): its hub record names the
	// capsule, capsules/<project>-<seq> (polyforge#144)
	td.hub.slugs = map[string]string{"wi_gone": "proj#9"}
	td.seedPendingQuestion(t, "wi_gone", "q-9", "s1", "x")
	snap, err := td.client.WatchSnapshot("wi_gone", 0)
	if err != nil || snap.Execution == nil || !snap.Known || snap.Slug != "proj#9" {
		t.Fatalf("capsule fallback: %+v %v", snap, err)
	}
}

func TestAttentionRoutingAndOwnerFallback(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.hub.reporters = map[string]string{"wi_auto": "bob"}
	// wi_chat: submitted from chat_a by alice; wi_auto: no origin owner (reporter bob)
	td.client.Origin = &cli.SubmitOrigin{ChatID: "chat_a", Owner: "alice"}
	td.submitClaim(t, "wi_chat")
	td.client.Origin = nil
	td.submitClaim(t, "wi_auto")
	for _, wi := range []string{"wi_chat", "wi_auto"} {
		td.waitMonitored(t, wi)
		td.seedPendingQuestion(t, wi, "q-"+wi, "s1", "?")
	}
	waitNeedsInput(t, td, "wi_chat", "wi_auto")

	ids := func(chat, owner string, mark bool) map[string]bool {
		mode := attnPeek
		if mark {
			mode = attnMark
		}
		items, err := td.attention(chat, owner, mode)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, it := range items {
			m[it.WI] = true
		}
		return m
	}
	// a stranger chat with no owner sees nothing
	if got := ids("chat_z", "", false); len(got) != 0 {
		t.Fatalf("stranger: %v", got)
	}
	// the origin chat's next watcher is told (chat_a has no live watcher yet)
	if got := ids("chat_a", "", false); !got["wi_chat"] || got["wi_auto"] {
		t.Fatalf("origin chat: %v", got)
	}
	// owner routing: alice (origin owner) and bob (reporter fallback), from any chat
	if got := ids("chat_z", "alice", false); !got["wi_chat"] || got["wi_auto"] {
		t.Fatalf("owner alice: %v", got)
	}
	if got := ids("chat_z", "bob", false); !got["wi_auto"] || got["wi_chat"] {
		t.Fatalf("reporter fallback for bob: %v", got)
	}
	// a live subscribed watcher covers the WI: nobody else is nagged
	_, _ = td.subs.Subscribe("chat_a", "wi_chat", "", 0)
	td.subs.Heartbeat("chat_a")
	if got := ids("chat_z", "alice", false); got["wi_chat"] {
		t.Fatalf("covered WI must not be listed: %v", got)
	}
	// shown once per chat
	if got := ids("chat_z", "bob", true); !got["wi_auto"] {
		t.Fatalf("first: %v", got)
	}
	if got := ids("chat_z", "bob", true); len(got) != 0 {
		t.Fatalf("second call must not repeat the notice: %v", got)
	}
	if got := ids("chat_y", "bob", true); !got["wi_auto"] {
		t.Fatalf("another chat of the owner is still told: %v", got)
	}
}

func waitNeedsInput(t *testing.T, td *testDaemon, wis ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs, _ := td.statusJobs()
		n := 0
		for _, j := range jobs {
			for _, wi := range wis {
				if j.WorkItemID == wi && j.NeedsInput {
					n++
				}
			}
		}
		if n == len(wis) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs never presented needs_input: %+v", jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatchProgressProjection(t *testing.T) {
	if p := watchProgress(observe.LiveProgress{InDispatch: true, Step: "s"}); p != nil {
		t.Fatalf("an empty dispatch has nothing to show: %+v", p)
	}
	p := watchProgress(observe.LiveProgress{InDispatch: true, Command: "go test ./...", CommandAgeSec: 42,
		RecentFiles: []string{"a.go", "b.go"}, ToolCalls: 7})
	if p == nil || p.Command != "go test ./..." || p.CommandAgeSec == nil || *p.CommandAgeSec != 42 ||
		len(p.RecentFiles) != 2 || p.ToolCalls == nil || *p.ToolCalls != 7 {
		t.Fatalf("projection: %+v", p)
	}
	p = watchProgress(observe.LiveProgress{InDispatch: true, RecentFiles: []string{"a.go"}})
	if p == nil || p.Command != "" || p.CommandAgeSec != nil || p.ToolCalls != nil {
		t.Fatalf("files-only projection: %+v", p)
	}
}

// TestAttentionAllDecisionClasses (D9): the attention class derivation
// covers every decision class — not only needs_input — through the
// state the daemon holds: the status-backed classes (needs_input,
// paused, blocked, completed, failed), the ring-derived classes status
// alone cannot distinguish (step_dead, candidates_exhausted,
// submit_rejected, submit_dropped, daemon_restarted), and canceled —
// the hub-side class — for a record the daemon still holds.
func TestAttentionAllDecisionClasses(t *testing.T) {
	td := newTestDaemon(t, nil)
	wi := "wi_" + "attcls"
	td.hub.reporters = map[string]string{wi: "bob"}

	// running jobs with no ring events present no decision
	if got := td.attentionClass(statusJob{WorkItemID: "wi_plain", Status: "running"}); got != "" {
		t.Fatalf("a plain running job must present no decision, got %q", got)
	}

	cases := []struct {
		name   string
		job    statusJob
		events []string // ring kinds appended before derivation, oldest first
		hub    string   // hub status seeded for the hub-canceled arm
		want   string
	}{
		{"needs_input", statusJob{WorkItemID: wi, Status: "running", NeedsInput: true, QuestionID: "q-1"}, nil, "", "needs_input"},
		{"paused", statusJob{WorkItemID: wi, Status: "paused"}, nil, "", "paused"},
		{"blocked (end_pending close)", statusJob{WorkItemID: wi, Status: "end_pending"}, nil, "", "blocked"},
		{"completed (wrap close)", statusJob{WorkItemID: wi, Status: "sealing", StopReason: protocol.FinishWrapped}, nil, "", "completed"},
		{"failed (fail close)", statusJob{WorkItemID: wi, Status: "sealing", StopReason: protocol.FinishFailed}, nil, "", "failed"},
		{"step_dead", statusJob{WorkItemID: wi, Status: "running"}, []string{"step_dead"}, "", "step_dead"},
		{"candidates_exhausted", statusJob{WorkItemID: wi, Status: "running"}, []string{"candidates_exhausted"}, "", "candidates_exhausted"},
		{"submit_rejected", statusJob{WorkItemID: wi, Status: "running"}, []string{observe.EvSubmitRejected}, "", "submit_rejected"},
		{"submit_dropped", statusJob{WorkItemID: wi, Status: "running"}, []string{observe.EvSubmitDropped}, "", "submit_dropped"},
		{"daemon_restarted", statusJob{WorkItemID: wi, Status: "running"}, []string{observe.EvDaemonRestarted}, "", "daemon_restarted"},
		{"canceled (hub says so)", statusJob{WorkItemID: wi, Status: "paused"}, nil, "canceled", "canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range tc.events {
				if err := td.obs.Events().Append(wi, k, map[string]any{}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.hub != "" {
				td.hub.status[wi] = tc.hub
			}
			if got := td.attentionClass(tc.job); got != tc.want && tc.hub == "" {
				t.Fatalf("class = %q, want %q", got, tc.want)
			}
			if tc.hub != "" {
				if got := td.attentionClassHubCanceled(tc.job); got != tc.want {
					t.Fatalf("hub class = %q, want %q", got, tc.want)
				}
			}
		})
	}

	// the newest decision-class event wins over older ones
	if err := td.obs.Events().Append("wi_two", "step_dead", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := td.obs.Events().Append("wi_two", observe.EvDaemonRestarted, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := td.attentionClass(statusJob{WorkItemID: "wi_two", Status: "running"}); got != observe.EvDaemonRestarted {
		t.Fatalf("newest decision event must win: %q", got)
	}

	// the whole hub-canceled class flows through the attention face: a
	// real retained paused job whose WI was canceled at the hub reaches
	// the owner (reporter-fallback routing)
	wiHold := "wi_holdcancel"
	td.client.Origin = nil
	td.submitClaim(t, wiHold)
	td.waitMonitored(t, wiHold)
	if _, err := td.client.Pause(wiHold); err != nil {
		t.Fatal(err)
	}
	waitState(t, td, wiHold, "paused")
	td.hub.reporters[wiHold] = "bob"
	td.hub.status[wiHold] = "canceled"
	items, err := td.attention("chat_z", "bob", attnPeek)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		found = found || (it.WI == wiHold && it.Reason == "canceled")
	}
	if !found {
		t.Fatalf("the hub-canceled held record must reach the owner: %+v", items)
	}
}

// TestLastSeqAndAttentionPastHistoryCap (repo review 2026-10-16 B-5):
// a WI with more than HistoryCap events still reports its true last seq
// and its newest decision-class event — the old path answered both from
// the durable files, truncated at HistoryCap (oldest first).
func TestLastSeqAndAttentionPastHistoryCap(t *testing.T) {
	td := newTestDaemon(t, nil)
	wi := "wi_" + "longlog"
	ev := td.obs.Events()
	n := observe.HistoryCap + 10
	for i := 0; i < n-1; i++ {
		if err := ev.Append(wi, "t_event", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := ev.Append(wi, "step_dead", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := td.lastSeq(wi); got != int64(n) {
		t.Fatalf("lastSeq = %d, want %d", got, n)
	}
	if got := td.attentionClass(statusJob{WorkItemID: wi, Status: "running"}); got != "step_dead" {
		t.Fatalf("attention class = %q, want the newest event step_dead", got)
	}
}

// repo review 2026-10-16 B-25 (WA-4), fetch-then-ack: a delivered attention
// notice keeps being listed until the watcher acks it; the ack (and the
// pending delivery) survive a daemon restart; acks are idempotent and
// unknown keys change nothing; the legacy mark-at-fetch stays as it was.
func TestSubStoreAttentionFetchThenAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), chatSubsFile)
	s, err := newSubStore(path)
	if err != nil {
		t.Fatal(err)
	}
	const k = "wi_1|blocked|"
	fresh := func(s *subStore, mode attnMode) bool {
		t.Helper()
		f, err := s.MarkShown("chat_a", k, mode)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if !fresh(s, attnDeliver) || !fresh(s, attnDeliver) {
		t.Fatal("an un-acked delivery must be re-delivered")
	}
	// the watcher crashed; the daemon restarts before anyone acks
	s, _ = newSubStore(path)
	if !fresh(s, attnDeliver) {
		t.Fatal("still un-acked after a daemon restart: re-delivered")
	}
	if n, err := s.AckShown("chat_a", []string{"wi_x|nope|", "garbage"}); err != nil || n != 0 {
		t.Fatalf("unknown keys: n=%d err=%v", n, err)
	}
	if n, err := s.AckShown("chat_nobody", []string{k}); err != nil || n != 0 {
		t.Fatalf("unknown chat: n=%d err=%v", n, err)
	}
	if !fresh(s, attnDeliver) {
		t.Fatal("an unknown-key ack must not ack the real notice")
	}
	if n, err := s.AckShown("chat_a", []string{k}); err != nil || n != 1 {
		t.Fatalf("ack: n=%d err=%v", n, err)
	}
	if n, err := s.AckShown("chat_a", []string{k}); err != nil || n != 0 {
		t.Fatalf("a repeated ack is a no-op: n=%d err=%v", n, err)
	}
	if fresh(s, attnDeliver) || fresh(s, attnPeek) {
		t.Fatal("an acked notice must not be listed again")
	}
	s, _ = newSubStore(path)
	if fresh(s, attnDeliver) {
		t.Fatal("the ack must survive a daemon restart")
	}
	if c := s.doc.Chats["chat_a"]; len(c.AttentionPending) != 0 || len(c.AttentionShown) != 1 {
		t.Fatalf("store after ack: %+v", c)
	}
	// legacy (an older watcher: mark=1, no ack=1): shown at fetch
	if f, _ := s.MarkShown("chat_b", k, attnMark); !f {
		t.Fatal("legacy first fetch lists it")
	}
	if f, _ := s.MarkShown("chat_b", k, attnMark); f {
		t.Fatal("legacy second fetch must not repeat it")
	}
}

// udsDo speaks to the test daemon's socket directly (the wire a
// watcher of another version would send).
func udsDo(t *testing.T, sock, method, path, body string) []byte {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequest(method, "http://daemon"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	return raw
}

func TestAttentionRouteCompat(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.client.Origin = &cli.SubmitOrigin{ChatID: "chat_a", Owner: "alice"}
	td.submitClaim(t, "wi_c")
	td.waitMonitored(t, "wi_c")
	td.seedPendingQuestion(t, "wi_c", "q-1", "s1", "?")
	waitNeedsInput(t, td, "wi_c")

	list := func(chat, query string) []protocol.ChatAttentionItem {
		t.Helper()
		var a protocol.ChatAttention
		if err := json.Unmarshal(udsDo(t, td.sock, "GET", "/chats/"+chat+"/attention?owner=alice"+query, ""), &a); err != nil {
			t.Fatal(err)
		}
		return a.Items
	}
	// an older watcher (mark=1 only): marked at fetch, as before
	if got := list("chat_old", "&mark=1"); len(got) != 1 || got[0].WI != "wi_c" {
		t.Fatalf("legacy first fetch: %+v", got)
	}
	if got := list("chat_old", "&mark=1"); len(got) != 0 {
		t.Fatalf("legacy second fetch must not repeat: %+v", got)
	}
	// a fetch-then-ack watcher (mark=1&ack=1): listed until acked
	got := list("chat_new", "&mark=1&ack=1")
	if len(got) != 1 || got[0].Key == "" {
		t.Fatalf("fetch-then-ack items carry a key: %+v", got)
	}
	if again := list("chat_new", "&mark=1&ack=1"); len(again) != 1 {
		t.Fatalf("un-acked: re-delivered: %+v", again)
	}
	udsDo(t, td.sock, "POST", "/chats/chat_new/attention/ack", `{"keys":["bogus|x|"]}`)
	if again := list("chat_new", "&mark=1&ack=1"); len(again) != 1 {
		t.Fatalf("an unknown-key ack acks nothing: %+v", again)
	}
	for i := 0; i < 2; i++ { // idempotent
		udsDo(t, td.sock, "POST", "/chats/chat_new/attention/ack", `{"keys":["`+got[0].Key+`"]}`)
	}
	if again := list("chat_new", "&mark=1&ack=1"); len(again) != 0 {
		t.Fatalf("acked: not listed again: %+v", again)
	}
	udsDo(t, td.sock, "POST", "/chats/chat_none/attention/ack", `{}`) // empty ack is harmless
}
