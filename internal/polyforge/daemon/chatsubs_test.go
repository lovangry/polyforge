package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

func TestSubStorePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), chatSubsFile)
	s, err := newSubStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if ch, err := s.Subscribe("chat_a", "wi_1", 7); err != nil || !ch {
		t.Fatalf("subscribe: %v %v", ch, err)
	}
	if ch, _ := s.Subscribe("chat_a", "wi_1", 99); ch {
		t.Fatal("re-subscribing an active WI changes nothing and must not move the cursor")
	}
	if err := s.Ack("chat_a", protocol.ChatAck{WI: "wi_1", Cursor: 12, Notified: []string{"needs_input:q-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Subscribe("chat_b", "wi_2", 0); err != nil {
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
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("atomic write must not leave its temp file")
	}
	// a finished WI is re-armed by a new submit
	_ = s2.Ack("chat_a", protocol.ChatAck{WI: "wi_1", Cursor: 12, Done: true})
	if ch, _ := s2.Subscribe("chat_a", "wi_1", 0); !ch {
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
	_, _ = s.Subscribe("chat_a", "wi_1", 0)
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
	// (finished while the watcher was down)
	td.seedPendingQuestion(t, "wi_gone", "q-9", "s1", "x")
	snap, err := td.client.WatchSnapshot("wi_gone", 0)
	if err != nil || snap.Execution == nil || !snap.Known {
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
		items, err := td.attention(chat, owner, mark)
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
	_, _ = td.subs.Subscribe("chat_a", "wi_chat", 0)
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
