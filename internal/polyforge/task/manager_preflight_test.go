package task

// manager_preflight_test.go: the candidate walk's dispatch-time preflight
// (polyforge#19) — a candidate pi would refuse at startup is skipped
// BEFORE the runner: no dispatch under an unresolvable model, the task
// records the candidate that actually ran, and an all-unresolvable list
// fails loudly naming every skipped candidate.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// newPreflightManager answers a manager whose resolved candidates are
// cands and whose agent dir carries preflightModelsJSON.
func newPreflightManager(t *testing.T, r Runner, cands []string) (*Manager, func() int) {
	t.Helper()
	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(preflightModelsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var terms []Task
	m := NewManager(Config{
		Store:    NewStore(t.TempDir()),
		Runner:   r,
		Resolve:  func(string) ([]string, string, error) { return cands, "steps.p.task.models", nil },
		WorkDir:  func(string) (string, error) { return t.TempDir(), nil },
		AgentDir: agentDir,
		OnTerminal: func(t Task) {
			mu.Lock()
			terms = append(terms, t)
			mu.Unlock()
		},
	})
	t.Cleanup(m.Close)
	return m, func() int {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(terms)
			mu.Unlock()
			if n > 0 {
				return n
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 0
	}
}

// The unresolvable head is skipped before the runner: the task completes
// on the resolvable candidate, records it as the model it ran, and the
// report never names the skipped one.
func TestPreflightSkipsUnresolvableCandidateBeforeRunner(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	m, terms := newPreflightManager(t, fakeRunner{func(_ context.Context, s RunSpec) (RunResult, error) {
		mu.Lock()
		ran = append(ran, s.Candidate.String())
		mu.Unlock()
		return RunResult{Report: "model=" + s.Candidate.Model, Tokens: 10}, nil
	}}, []string{"pi:w19p-bad/w19p-x", "pi:w19p-good/w19p-model"})
	tk, err := m.Spawn(Spec{Project: "p", Prompt: "w19", ReadOnlyTools: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	final := waitState(t, m, tk.ID, StateCompleted)
	if terms() != 1 {
		t.Fatalf("want one terminal task, got %d", terms())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || ran[0] != "pi:w19p-good/w19p-model" {
		t.Fatalf("the runner must see only the resolvable candidate, ran %v", ran)
	}
	if final.Candidate != "pi:w19p-good/w19p-model" {
		t.Fatalf("the task must record the candidate it ran, got %q", final.Candidate)
	}
	if strings.Contains(final.Report, "w19p-bad") {
		t.Fatalf("the report must never name the skipped candidate: %q", final.Report)
	}
}

// An all-unresolvable list fails loudly: the runner never runs, and the
// error names every skipped candidate.
func TestPreflightAllUnresolvableFailsNamingCandidates(t *testing.T) {
	var ran int
	m, terms := newPreflightManager(t, fakeRunner{func(_ context.Context, s RunSpec) (RunResult, error) {
		ran++
		return RunResult{Report: "should not run"}, nil
	}}, []string{"pi:w19p-bad/w19p-x", "pi:w19p-bad/w19p-y"})
	tk, err := m.Spawn(Spec{Project: "p", Prompt: "w19", ReadOnlyTools: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	final := waitState(t, m, tk.ID, StateFailed)
	if terms() != 1 {
		t.Fatalf("want one terminal task, got %d", terms())
	}
	if ran != 0 {
		t.Fatalf("the runner must never see an unresolvable candidate, ran %d", ran)
	}
	if !strings.Contains(final.Error, "w19p-bad/w19p-x") || !strings.Contains(final.Error, "w19p-bad/w19p-y") {
		t.Fatalf("the failure must name every skipped candidate, got %q", final.Error)
	}
}

// Without a models.json (or with an agent dir that proves nothing) the
// walk keeps today's behavior: every composed candidate runs in order.
func TestPreflightFailsOpenWithoutModelsJSON(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	m, _ := newPreflightManager(t, fakeRunner{func(_ context.Context, s RunSpec) (RunResult, error) {
		mu.Lock()
		ran = append(ran, s.Candidate.String())
		mu.Unlock()
		return RunResult{Report: "done", Tokens: 1}, nil
	}}, []string{"pi:w19p-bad/w19p-x"})
	// wipe the models.json: nothing is proven unresolvable
	if err := os.Remove(filepath.Join(m.cfg.AgentDir, "models.json")); err != nil {
		t.Fatal(err)
	}
	tk, err := m.Spawn(Spec{Project: "p", Prompt: "w19", ReadOnlyTools: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitState(t, m, tk.ID, StateCompleted)
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || ran[0] != "pi:w19p-bad/w19p-x" {
		t.Fatalf("without a models.json the composed candidate runs, ran %v", ran)
	}
}
