package daemon

// Repro test for polyforge#18: with the WI's origin chat missing (a
// daemon auto-claim or a chat-less `pf submit` recorded no origin) and
// the eligible set empty (no subscribed chat tagged with the WI's
// workspace — the reporting machine's reality: 230 of 236 chats carry
// no workspace tag), the fallback is owner-wide: EVERY subscribed chat
// is a wake target of its own record, none Passive. One decision line
// then fans out to every online watcher — 35 resident `pf watch --chat
// <id>` processes on the reporting machine — and each printed line is
// a sendMessage(..., { triggerTurn: true }) in
// skills/polyforge/extensions/pf-chat.ts, so one decision wakes every
// pi session and the personal key's 60 RPM dies on the first few
// decisions (429 code 102503 in every session panel).
//
// Expected behavior per the work item: the empty-eligible fallback
// converges to a SINGLE wake target; every other subscriber is Passive.
//
// Before the fix this test fails at the "REPORTED FAILURE" assertion:
// all of the owner-routed subscribers are non-Passive wake targets.

import (
	"path/filepath"
	"testing"
)

// A WI without an origin chat and without an eligible chat (no
// workspace-tagged live subscriber) wakes exactly ONE subscribed chat;
// the rest are Passive.
func TestSubStoreEmptyEligibleFallbackWakesOneChat(t *testing.T) {
	const wi = "wi_p18"
	s, err := newSubStore(filepath.Join(t.TempDir(), chatSubsFile))
	if err != nil {
		t.Fatal(err)
	}
	s.scope = wakeScope{
		originChat: func(string) string { return "" },           // no origin chat: the machine's 195 origin-less WIs
		workspace:  func(string) string { return "/ws/p18_18" }, // a mapped workspace...
	}
	// ...that no subscriber is tagged with: the eligible set is empty.
	// Five owner-routed subscribers (the real app-releases#34 snapshot
	// holds 36), every watcher live — a resident `pf watch` polls
	// every 2s and calls subscribe-owned each cycle.
	audience := []string{"chat_a", "chat_b", "chat_c", "chat_d", "chat_e"}
	for _, chat := range audience {
		if _, err := s.SubscribeNew(chat, wi, "app-releases#34", "", 0); err != nil {
			t.Fatal(err)
		}
		s.Seen(chat)
	}
	targets := map[string]bool{}
	for _, chat := range audience {
		for _, sub := range s.List(chat) {
			if sub.WI == wi && !sub.Passive {
				targets[chat] = true
			}
		}
	}
	if len(targets) != 1 {
		t.Fatalf("REPORTED FAILURE (polyforge#18): a decision on a WI with no origin chat and no eligible chat woke %d of %d subscribed chats (each a non-Passive wake target of its own record — one triggerTurn per online session, the 60 RPM 429 fanout); expected exactly 1: %v",
			len(targets), len(audience), targets)
	}
	for _, chat := range audience {
		if targets[chat] {
			continue
		}
		for _, sub := range s.List(chat) {
			if sub.WI == wi && !sub.Passive {
				t.Fatalf("%s must be Passive under the single wake target: %+v", chat, sub)
			}
		}
	}
}
