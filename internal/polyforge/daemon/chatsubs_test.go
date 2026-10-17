package daemon

import (
	"os"
	"path/filepath"
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

func TestChatRoutesAndSubmitAutoSubscribe(t *testing.T) {
	td := newTestDaemon(t, nil)
	// explicit subscribe/list/unsubscribe through the UDS face
	if _, err := td.client.ChatSubscribe("chat_x", "wi_manual", true); err != nil {
		t.Fatal(err)
	}
	subs, err := td.client.ChatSubscriptions("chat_x")
	if err != nil || len(subs.Subscriptions) != 1 {
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

// A watcher's start subscribes its chat to the held WIs the chat
// submitted or its owner owns (origin owner, else the reporter), once; a
// new subscription's cursor sits just before the WI's newest decision
// event, and a finished subscription is not re-armed.
func TestSubscribeOwnedRoutingAndOwnerFallback(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.hub.reporters = map[string]string{"wi_auto": "bob"}
	// wi_chat: submitted from chat_a by alice; wi_auto: no origin owner (reporter bob)
	td.client.Origin = &cli.SubmitOrigin{ChatID: "chat_a", Owner: "alice"}
	td.submitClaim(t, "wi_chat")
	td.client.Origin = nil
	td.submitClaim(t, "wi_auto")
	for _, wi := range []string{"wi_chat", "wi_auto"} {
		td.waitMonitored(t, wi)
	}
	ev := td.obs.Events()
	_ = ev.Append("wi_auto", "step_dead", map[string]any{})
	_ = ev.Append("wi_auto", "t_noise", nil)
	dead, _ := ev.Newest("wi_auto", func(k string) bool { return k == "step_dead" })
	deadSeq := dead.Seq

	owned := func(chat, owner string) map[string]int64 {
		t.Helper()
		added, err := td.client.ChatSubscribeOwned(chat, owner)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int64{}
		for _, s := range added {
			m[s.WI] = s.Cursor
		}
		return m
	}
	if got := owned("chat_z", ""); len(got) != 0 {
		t.Fatalf("a stranger chat with no owner: %v", got)
	}
	_, _ = td.subs.Unsubscribe("chat_a", "wi_chat") // the submit's own subscription
	before := ev.LastSeq("wi_chat")
	if got := owned("chat_a", ""); len(got) != 1 || got["wi_chat"] < before || got["wi_chat"] > ev.LastSeq("wi_chat") {
		t.Fatalf("origin chat: %v, want wi_chat from its current seq (%d)", got, before)
	}
	if got := owned("chat_y", "alice"); len(got) != 1 {
		t.Fatalf("owner alice: %v", got)
	}
	if got := owned("chat_z", "bob"); len(got) != 1 || got["wi_auto"] != deadSeq-1 {
		t.Fatalf("reporter fallback for bob: %v, want wi_auto just before its step_dead (seq %d)", got, deadSeq)
	}
	if got := owned("chat_z", "bob"); len(got) != 0 {
		t.Fatalf("a second start adds nothing: %v", got)
	}
	if err := td.subs.Ack("chat_y", protocol.ChatAck{WI: "wi_chat", Cursor: 1, Done: true}); err != nil {
		t.Fatal(err)
	}
	if got := owned("chat_y", "alice"); len(got) != 0 {
		t.Fatalf("a finished subscription must not be re-armed: %v", got)
	}
	if subs := mustSubs(t, td, "chat_z"); len(subs) != 1 || subs[0].WI != "wi_auto" || subs[0].Cursor != deadSeq-1 {
		t.Fatalf("chat_z subscriptions: %+v", subs)
	}
}

// Every watcher cycle calls subscribe-owned, so a repeated call costs no
// hub read: a WI's reporter is fetched once, and a WI the chat already
// holds a subscription for is skipped before any owner lookup.
func TestSubscribeOwnedRepeatedCallReadsTheHubOnce(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.hub.mu.Lock()
	td.hub.reporters = map[string]string{"wi_auto": "bob"}
	td.hub.mu.Unlock()
	td.submitClaim(t, "wi_auto") // no origin: the owner is the reporter
	td.waitMonitored(t, "wi_auto")
	gets := func() int {
		td.hub.mu.Lock()
		defer td.hub.mu.Unlock()
		return td.hub.gets["wi_auto"]
	}
	for _, owner := range []string{"carol", "carol", "bob"} {
		if _, err := td.client.ChatSubscribeOwned("chat_c", owner); err != nil {
			t.Fatal(err)
		}
	}
	base := gets()
	for i := 0; i < 3; i++ {
		for _, chat := range []string{"chat_c", "chat_d"} {
			if _, err := td.client.ChatSubscribeOwned(chat, "carol"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := gets(); n != base {
		t.Fatalf("repeated subscribe-owned read the hub %d more times, want 0", n-base)
	}
	if subs := mustSubs(t, td, "chat_c"); len(subs) != 1 || subs[0].WI != "wi_auto" {
		t.Fatalf("chat_c subscriptions: %+v", subs)
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

// TestLastSeqAndOwnedCursorPastHistoryCap (repo review 2026-10-16 B-5):
// a WI with more than HistoryCap events still reports its true last seq,
// and an owned subscription still finds its newest decision event.
func TestLastSeqAndOwnedCursorPastHistoryCap(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.client.Origin = &cli.SubmitOrigin{ChatID: "chat_long", Owner: "alice"}
	wi := "wi_" + "longlog"
	td.submitClaim(t, wi)
	td.waitMonitored(t, wi)
	_, _ = td.subs.Unsubscribe("chat_long", wi)
	ev := td.obs.Events()
	for i := 0; i < observe.HistoryCap+10; i++ {
		if err := ev.Append(wi, "t_event", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := ev.Append(wi, "step_dead", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	n := ev.LastSeq(wi)
	if got := td.lastSeq(wi); got != n || n <= observe.HistoryCap {
		t.Fatalf("lastSeq = %d, want %d (> HistoryCap)", got, n)
	}
	added, err := td.client.ChatSubscribeOwned("chat_long", "")
	if err != nil || len(added) != 1 || added[0].Cursor != n-1 {
		t.Fatalf("owned subscription %+v err=%v, want the cursor just before step_dead (%d)", added, err, n-1)
	}
}
