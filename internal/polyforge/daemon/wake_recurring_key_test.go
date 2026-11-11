package daemon

// wake_recurring_key_test.go — review (rewind): the eligible
// chat's ack must land for the Passive owner-routed subscribers the way
// an ack lands in a shared record — the delivered keys MIRRORED, not
// unioned. The watcher acks only the still-pending keys, so a decision
// that cleared is pruned from the eligible chat's record and fires again
// when it recurs (blocked again, paused again, a needs_input with a
// reused question id). Had the outsiders kept every key ever delivered,
// a recurrence the eligible chat never delivered (it went quiet first)
// would be suppressed in every one of them under the empty-eligible
// fallback (design ruling 6), and the decision would wake no chat at all.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// gmi15ClearPendingQuestion plays the driver's commit after the question
// was answered: execution.json no longer carries a pending question.
func gmi15ClearPendingQuestion(t *testing.T, td *testDaemon, wi string) {
	t.Helper()
	td.hub.mu.Lock()
	name := api.PathName(td.hub.slugs[wi], wi)
	td.hub.mu.Unlock()
	path := filepath.Join(td.cfg.Mapping.Workspaces()[0], "capsules", name, filepath.FromSlash(capsule.ExecutionJSON))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var exec capsule.Execution
	if err := json.Unmarshal(raw, &exec); err != nil {
		t.Fatal(err)
	}
	step := exec.CurrentStep
	for _, st := range exec.Steps {
		st.PendingQuestion = nil
	}
	exec.Version++
	blob, err := json.Marshal(exec)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	td.sup.Link(wi).Fact(supervise.ExecCommitted{Version: exec.Version, Step: step})
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs, _ := td.statusJobs()
		cleared := true
		for _, j := range jobs {
			if j.WorkItemID == wi && j.NeedsInput {
				cleared = false
			}
		}
		if cleared {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job never cleared needs_input: %+v", jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRecurringDecisionReachesTheFallbackAudience — gone origin; the
// eligible chat delivers needs_input q-1; the question clears and the
// eligible chat acks the cleared state; it goes quiet past chatLiveFor
// (and so does the untagged chat, leaving the wsB chat the only live
// subscribed one — the single fallback target, polyforge#18); the same
// question id recurs. Nobody delivered the recurrence, so the fallback
// target must print it — and its ack must land in the Passive
// subscribers' records, so the next switch of the target (the untagged
// chat's watcher comes back) never replays it.
func TestRecurringDecisionReachesTheFallbackAudience(t *testing.T) {
	for _, originChat := range []bool{true, false} {
		s := gmi14ScopedWakeScene(t, originChat) // needs_input q-1 pending
		if out := gmi14RunChat(t, s.td, s.a2, 5*time.Second); !strings.Contains(out, "needs_input q-1") {
			t.Fatalf("the eligible chat must deliver q-1: %q", out)
		}
		gmi15ClearPendingQuestion(t, s.td, s.wi)
		// the eligible chat's next cycle acks the cleared state (no key)
		if out := gmi14RunChat(t, s.td, s.a2, 400*time.Millisecond); strings.Contains(out, "[polyforge]") {
			t.Fatalf("nothing to deliver once q-1 cleared: %q", out)
		}
		s.td.subs.mu.Lock()
		if r := s.td.subs.doc.Chats[s.a2].Subs[s.wi]; len(r.Notified) != 0 {
			s.td.subs.mu.Unlock()
			t.Fatalf("the eligible chat must have acked the cleared state: %+v", r)
		}
		// the eligible chat goes quiet (its session closed), and so does
		// the untagged chat: the wsB chat is the only live subscribed chat
		// left — the single fallback target
		s.td.subs.seen[s.a2] = s.td.subs.now().Add(-2 * chatLiveFor)
		s.td.subs.seen[s.untagged] = s.td.subs.now().Add(-2 * chatLiveFor)
		s.td.subs.mu.Unlock()
		gmi14SeedQ(t, s.td, s.wi, "q-1") // the same question id recurs
		if out := gmi14RunChat(t, s.td, s.b1, 5*time.Second); !strings.Contains(out, "needs_input q-1") {
			t.Fatalf("the recurrence nobody delivered must reach the single fallback target: %q", out)
		}
		// the fallback target's ack landed in the Passive records: a
		// later switch of the target (b1 quiet, the untagged chat back)
		// must not replay the recurrence
		s.td.subs.mu.Lock()
		s.td.subs.seen[s.b1] = s.td.subs.now().Add(-2 * chatLiveFor)
		s.td.subs.mu.Unlock()
		if out := gmi14RunChat(t, s.td, s.untagged, 400*time.Millisecond); strings.Contains(out, "[polyforge]") {
			t.Fatalf("the recurrence the fallback target delivered must not replay after the switch: %q", out)
		}
	}
}
