package driver

// preflight_dispatch_test.go: the dispatch-time preflight skip
// (polyforge#19) at the driver's compose/prepare boundary — the
// placeholder-candidate chains a step composes are filtered BEFORE any
// spawn: no death cycle, one identifying event per skipped candidate, and
// an all-unresolvable step blocks through the same candidates-exhausted
// door a spent chain does.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// preflightModelsJSON mirrors the e2e fixture's controlled view: one
// known provider with one model; every other provider is unresolvable.
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

// newPreflightDriver builds one driver over a seeded in_progress step
// whose composed list is list, with a step-agent dir carrying
// preflightModelsJSON (noDir = no models.json at all).
func newPreflightDriver(t *testing.T, list []string, withModels bool) (*Driver, *eventLog) {
	t.Helper()
	dir := t.TempDir()
	execPath := filepath.Join(dir, capsule.ExecutionJSON)
	seedExecution(t, execPath, "fix", capsule.StepInProgress, nil)
	agentDir := filepath.Join(dir, "step-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if withModels {
		if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(preflightModelsJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	log := &eventLog{}
	d, err := New(Config{
		WI:           WIInfo{ID: "wi_run", CapsuleDir: dir, BundleName: "b"},
		ExecPath:     execPath,
		Workflow:     &Workflow{Steps: []StepSpec{{ID: "fix", ContextPolicy: "fresh"}}, MaxTotal: 4, MaxPerStep: 2},
		Spawner:      &fakeSpawner{},
		Log:          log,
		Compose:      composeOf(map[string][]string{"fix": list}),
		StepAgentDir: agentDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, log
}

// The unresolvable head is skipped before any spawn: the dispatch runs
// the next resolvable candidate, one identifying event names the skipped
// one, and no step failure is recorded.
func TestPreflightSkipsUnresolvableHeadBeforeSpawn(t *testing.T) {
	d, log := newPreflightDriver(t, []string{"pi:w19p-bad/w19p-x", "pi:w19p-good/w19p-model"}, true)
	if blocked, err := d.composeDispatch("fix"); err != nil || blocked {
		t.Fatalf("composeDispatch: blocked=%v err=%v — a mixed list must dispatch", blocked, err)
	}
	spawn, _, err := d.prepareDispatch("fix")
	if err != nil {
		t.Fatalf("prepareDispatch: %v", err)
	}
	if spawn.Model != "w19p-good/w19p-model" {
		t.Fatalf("model %q, want the resolvable fall-to w19p-good/w19p-model", spawn.Model)
	}
	if got := d.effectiveCandidates("fix"); len(got) != 1 || got[0] != "pi:w19p-good/w19p-model" {
		t.Fatalf("the kept chain must be the resolvable candidate alone, got %v", got)
	}
	if !log.has("candidate_skipped", "candidate", "pi:w19p-bad/w19p-x") {
		t.Fatalf("no candidate_skipped event names the skipped head; kinds=%v", log.kinds())
	}
	if e := log.count("candidate_skipped"); e != 1 {
		t.Fatalf("want exactly one skip event, got %d", e)
	}
	if n := log.count("step_failed"); n != 0 {
		t.Fatalf("a skipped candidate must leave no step death, got %d step_failed events", n)
	}
}

// The skip is observable with its reason — the classification-shaped
// diagnostic that names the candidate.
func TestPreflightSkipEventCarriesTheReason(t *testing.T) {
	d, log := newPreflightDriver(t, []string{"pi:w19p-bad/w19p-x", "pi:w19p-good/w19p-model"}, true)
	d.composeDispatch("fix")
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, e := range log.entries {
		if e.kind != "candidate_skipped" {
			continue
		}
		if reason, _ := e.body["reason"].(string); !strings.Contains(reason, "w19p-bad/w19p-x") || !strings.Contains(reason, "not found") {
			t.Fatalf("the skip reason must name the candidate and the resolution failure, got %q", reason)
		}
		return
	}
	t.Fatal("no candidate_skipped event")
}

// An all-unresolvable step blocks through the candidates-exhausted door:
// no spawn, no death cycle, the block reason and the events naming every
// skipped candidate.
func TestPreflightAllUnresolvableBlocksExhausted(t *testing.T) {
	d, log := newPreflightDriver(t, []string{"pi:w19p-bad/w19p-x", "pi:w19p-bad/w19p-y"}, true)
	blocked, err := d.composeDispatch("fix")
	if err != nil {
		t.Fatalf("composeDispatch: %v", err)
	}
	if !blocked {
		t.Fatal("an all-unresolvable list must block the dispatch")
	}
	if s := d.Snapshot().WorkflowStatus; s != StatusBlocked {
		t.Fatalf("workflow status %q, want blocked", s)
	}
	if len(d.Snapshot().BlockedReason) == 0 || !strings.Contains(d.Snapshot().BlockedReason, "w19p-bad/w19p-x") || !strings.Contains(d.Snapshot().BlockedReason, "w19p-bad/w19p-y") {
		t.Fatalf("the block reason must name every skipped candidate, got %q", d.Snapshot().BlockedReason)
	}
	if n := log.count("candidate_skipped"); n != 2 {
		t.Fatalf("want one skip event per candidate, got %d", n)
	}
	if n := log.count("candidates_exhausted"); n != 1 {
		t.Fatalf("the block must ride the candidates_exhausted door, got %d events", n)
	}
	if n := log.count("blocked"); n != 1 {
		t.Fatalf("want the blocked event, got %d", n)
	}
	if n := log.count("step_failed"); n != 0 {
		t.Fatalf("no death cycle: want no step_failed events, got %d", n)
	}
}

// The fail-open shapes: no models.json at all, and no agent dir, keep the
// composed list untouched — nothing is proven unresolvable.
func TestPreflightFailsOpenWithoutAReadableModelsFile(t *testing.T) {
	for name, withModels := range map[string]bool{"no models.json": false} {
		t.Run(name, func(t *testing.T) {
			d, log := newPreflightDriver(t, []string{"pi:w19p-bad/w19p-x", "pi:w19p-good/w19p-model"}, withModels)
			if blocked, err := d.composeDispatch("fix"); err != nil || blocked {
				t.Fatalf("composeDispatch: blocked=%v err=%v", blocked, err)
			}
			spawn, _, err := d.prepareDispatch("fix")
			if err != nil {
				t.Fatalf("prepareDispatch: %v", err)
			}
			if spawn.Model != "w19p-bad/w19p-x" {
				t.Fatalf("without a models.json the composed head stays, got %q", spawn.Model)
			}
			if n := log.count("candidate_skipped"); n != 0 {
				t.Fatalf("no skip without a models.json, got %d", n)
			}
		})
	}
	d, _ := newPreflightDriver(t, []string{"pi:w19p-bad/w19p-x"}, true)
	dir := t.TempDir()
	d.cfg.StepAgentDir = filepath.Join(dir, "nowhere")
	if blocked, err := d.composeDispatch("fix"); err != nil || blocked {
		t.Fatalf("composeDispatch: blocked=%v err=%v", blocked, err)
	}
	if got := d.effectiveCandidates("fix"); len(got) != 1 || got[0] != "pi:w19p-bad/w19p-x" {
		t.Fatalf("an absent agent dir must keep the list, got %v", got)
	}
}

// A skipped candidate never re-enters the chain of the dispatch: after
// the skip the failure decision walks the kept list alone (the
// resolvable candidate is its end — a death there is a same-model
// retry with exhaustion reported, never a switch back to the skipped
// placeholder).
func TestPreflightSkipRemovesTheCandidateFromTheChain(t *testing.T) {
	d, log := newPreflightDriver(t, []string{"pi:w19p-bad/w19p-x", "pi:w19p-good/w19p-model"}, true)
	d.composeDispatch("fix")
	// the death of the resolvable candidate over the kept chain
	d.mu.Lock()
	st := d.exec.Steps["fix"]
	st.Candidate = "pi:w19p-good/w19p-model"
	dec := d.decideFailureLocked("fix", st, &StepFailure{Cause: FailExitModel, Detail: "quota"})
	d.mu.Unlock()
	if dec.Switch || dec.To != "" {
		t.Fatalf("a death on the kept chain's end must not switch back to the skipped placeholder (dec=%+v)", dec)
	}
	if !dec.Exhausted {
		t.Fatalf("the kept chain's end death is exhaustion (dec=%+v)", dec)
	}
	if !log.has("candidate_skipped", "candidate", "pi:w19p-bad/w19p-x") {
		t.Fatal("the skip event must still name the skipped candidate")
	}
}

// The preflight judges only the dispatch's own step: another step's list
// in the same composed map is untouched until its dispatch recomposes.
func TestPreflightJudgesOnlyTheDispatchStep(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, capsule.ExecutionJSON)
	seedExecution(t, execPath, "fix", capsule.StepInProgress, nil)
	agentDir := filepath.Join(dir, "step-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(preflightModelsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	log := &eventLog{}
	d, err := New(Config{
		WI:       WIInfo{ID: "wi_run", CapsuleDir: dir, BundleName: "b"},
		ExecPath: execPath,
		Workflow: &Workflow{Steps: []StepSpec{
			{ID: "fix", ContextPolicy: "fresh"},
			{ID: "other", ContextPolicy: "fresh"},
		}, MaxTotal: 4, MaxPerStep: 2},
		Spawner:      &fakeSpawner{},
		Log:          log,
		StepAgentDir: agentDir,
		Compose: composeOf(map[string][]string{
			"fix":   {"pi:w19p-bad/w19p-x", "pi:w19p-good/w19p-model"},
			"other": {"pi:w19p-bad/w19p-x"},
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blocked, err := d.composeDispatch("fix")
	if err != nil || blocked {
		t.Fatalf("composeDispatch: blocked=%v err=%v", blocked, err)
	}
	if got := d.cands["other"]; len(got) != 1 || got[0] != "pi:w19p-bad/w19p-x" {
		t.Fatalf("the und ispatched step's list must stay untouched until its own dispatch, got %v", got)
	}
	if n := log.count("candidate_skipped"); n != 1 {
		t.Fatalf("only the dispatch's own step skips, got %d events", n)
	}
}
