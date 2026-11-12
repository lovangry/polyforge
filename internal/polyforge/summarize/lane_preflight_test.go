package summarize

// lane_preflight_test.go: the dispatch-time candidate preflight
// (polyforge#19) on the lane's walk — an unresolvable head is skipped
// before any dispatch, the call answers on the resolvable candidate, and
// an all-unresolvable list lands the skip as the lane's error naming
// every candidate, never a dispatch under a model that cannot resolve.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// preflightModelsJSON declares one known provider; every other provider
// is unresolvable (the e2e fixture's controlled view).
const preflightModelsJSON = `{
  "providers": {
    "w19p-good": {
      "baseUrl": "https://w19p.invalid",
      "apiKey": "w19p-fixture",
      "api": "openai-completions",
      "models": [{"id": "w19p-model", "name": "w19p-model"}]
    }
  }
}`

// newPreflightLane answers a lane on a fake dispatcher whose agent dir
// carries preflightModelsJSON (withModels false = no models.json: the
// fail-open shape).
func newPreflightLane(t *testing.T, d *fakeDispatcher, withModels bool) (*Lane, *transcript) {
	t.Helper()
	agentDir := t.TempDir()
	if withModels {
		if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(preflightModelsJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l := New(d, agentDir)
	tr := newTranscript(t)
	return l, tr
}

// The unresolvable head is skipped before any dispatch: the call answers
// on the resolvable candidate, and a following call (the interval and
// growth gates spaced) answers on it again — the skipped candidate never
// dispatches.
func TestPreflightSkipsUnresolvableHeadBeforeDispatch(t *testing.T) {
	c := &fakeClock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	d := &fakeDispatcher{}
	l, tr := newPreflightLane(t, d, true)
	l.now = c.Now
	models := []string{"pi:w19p-bad/w19p-x", "pi:w19p-good/w19p-model"}
	l.Marked("wi", models, "en", callInterval, tr.facts("t1"))
	awaitCalls(t, d, 1, 2*time.Second)
	if got := d.calls[0]; got != "pi:w19p-good/w19p-model" {
		t.Fatalf("the call must run on the resolvable candidate, got %q", got)
	}
	awaitHeld(t, l, "wi", "the agent is working", 2*time.Second)

	// a new attempt (a new dispatch token) with grown transcript, past
	// the interval: the next call is still on the resolvable candidate
	c.advance(callInterval)
	tr.grow(`{"t":"turn","text":"more"}`)
	l.Marked("wi", models, "en", callInterval, tr.facts("t2"))
	awaitCalls(t, d, 2, 2*time.Second)
	if got := d.calls[1]; got != "pi:w19p-good/w19p-model" {
		t.Fatalf("the post-growth call must stay on the resolvable candidate, got %q", got)
	}
	for _, c := range d.calls {
		if strings.Contains(c, "w19p-bad") {
			t.Fatalf("the unresolvable candidate was dispatched: %v", d.calls)
		}
	}
}

// An all-unresolvable list never dispatches: the lane lands the skip as
// its error, naming every skipped candidate.
func TestPreflightAllUnresolvableLandsTheSkipError(t *testing.T) {
	d := &fakeDispatcher{}
	l, tr := newPreflightLane(t, d, true)
	models := []string{"pi:w19p-bad/w19p-x", "pi:w19p-bad/w19p-y"}
	l.Marked("wi", models, "en", callInterval, tr.facts("t1"))
	err := awaitErr(t, l, "wi", "w19p-bad/w19p-x", 2*time.Second)
	if !strings.Contains(err, "w19p-bad/w19p-y") {
		t.Fatalf("the skip error must name every skipped candidate, got %q", err)
	}
	noMoreCalls(t, d, 0, 50*time.Millisecond)
}

// A failed call still advances along the kept list: the pointer walk
// skips the unresolvable entries, so a failure advances to the next
// KEPT candidate, never back to the skipped one.
func TestPreflightWalkAdvancesOnlyKeptCandidates(t *testing.T) {
	c := &fakeClock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	d := &fakeDispatcher{answer: func(candidate string) (string, error) {
		if candidate == "pi:w19p-good/w19p-model" {
			return "", errFake("dispatch failed: exit 1")
		}
		return "line", nil
	}}
	l, tr := newPreflightLane(t, d, true)
	l.now = c.Now
	models := []string{"pi:w19p-bad/w19p-x", "pi:w19p-good/w19p-model", "pi:w19p-good/w19p-model2"}
	l.Marked("wi", models, "en", callInterval, tr.facts("t1"))
	awaitCalls(t, d, 1, 2*time.Second)
	if got := d.calls[0]; got != "pi:w19p-good/w19p-model" {
		t.Fatalf("the first call must run on the kept head, got %q", got)
	}
	// the failure lands, advancing the pointer to the next KEPT entry
	awaitErr(t, l, "wi", "dispatch failed: exit 1", 2*time.Second)
	c.advance(callInterval)
	tr.grow(`{"t":"turn","text":"more"}`)
	l.Marked("wi", models, "en", callInterval, tr.facts("t2"))
	awaitCalls(t, d, 2, 2*time.Second)
	if got := d.calls[1]; got != "pi:w19p-good/w19p-model2" {
		t.Fatalf("the post-failure call must run on the next kept candidate, got %q (calls %v)", got, d.calls)
	}
	for _, c := range d.calls {
		if strings.Contains(c, "w19p-bad") {
			t.Fatalf("the unresolvable candidate was dispatched: %v", d.calls)
		}
	}
}

// Without a models.json nothing is proven unresolvable: the walk keeps
// today's behavior and the composed head dispatches.
func TestPreflightFailsOpenWithoutModelsJSON(t *testing.T) {
	d := &fakeDispatcher{}
	l, tr := newPreflightLane(t, d, false)
	models := []string{"pi:w19p-bad/w19p-x"}
	l.Marked("wi", models, "en", callInterval, tr.facts("t1"))
	awaitCalls(t, d, 1, 2*time.Second)
	if got := d.calls[0]; got != "pi:w19p-bad/w19p-x" {
		t.Fatalf("without a models.json the composed head dispatches, got %q", got)
	}
}
