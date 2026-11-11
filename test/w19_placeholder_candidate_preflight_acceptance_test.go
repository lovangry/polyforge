// carries: internal/polyforge/driver/*, internal/polyforge/daemon/*,
// internal/polyforge/task/*, internal/polyforge/summarize/*,
// internal/polyforge/modelclass/*, workflows/*, docs/reference/*

package test

// w19_placeholder_candidate_preflight_acceptance_test.go: the polyforge#19
// acceptance suite — the dispatch-time preflight skip of candidates whose
// model the harness cannot resolve (the step-level placeholder
// model_candidates that burn the first dispatch cycle of every step of an
// unmodified workspace), the shipped-bundle slimming behind it, and the
// loud refusal of an all-unresolvable manual `--models` list. Authored
// against the SPEC (the capsule's artifacts/spec.md — the ## Requirements
// scenario blocks + ## Acceptance criteria) BEFORE the implementation
// lands, through the project's real e2e surfaces only: the pf/aihub/fakepi
// binaries, the `pf` CLI, the daemon's UDS face, the workspace
// .polyforge.yaml, the shipped bundles under workflows/ and the daemon's
// <harness-root>/step-agent/models.json (the pi model-service config copy
// every spawned harness agent reads) — never implementation internals.
//
// Suite posture (artifacts/e2e-scenarios.md maps every scenario to its
// criteria and records the baseline run): every new-behaviour bar is RED
// on the pre-implementation tree at ASSERTION level — a journal record, an
// exit code, a spawn's pinned model — never a build red: this file
// compiles, vets and gofmts against the tree as it stands today.
//
// The one fact the preflight reads that the hermetic harness never
// controlled before: WHICH models the model service can resolve. w19Daemon
// boots its daemon with an explicit --operator-agent-dir (auth.json + a
// models.json declaring exactly the providers the resolvable candidates
// use) and a --harness-root whose step-agent copy carries the same
// models.json, so no candidate level or lane (step, task, summarize) can
// fall back to this host's ~/.pi/agent, and the resolvable/unresolvable
// split is the scenario's own in every world.
//
// Scenario map (titles match artifacts/e2e-scenarios.md):
//
//   - TestW19TemplatesCarryNoStepLevelModelCandidates: none of the six
//     shipped bundles carries a step-level model_candidates key (bundle
//     defaults keep theirs — they are NOT in scope), and the living docs
//     (README, docs/reference, docs/bundles.md, docs/extending.md, the
//     quickstarts) carry zero example-* references.
//   - TestW19DefaultModelsWorkspaceResolvesNoExampleCandidates: a workspace
//     setting only default.models resolves every step's chain from it —
//     no step-level example-* candidate anywhere in the dispatch.
//   - TestW19YamlStepListSkipsUnresolvableHeadCandidate: a yaml step list
//     whose head the model service cannot resolve is skipped before any
//     spawn, with no step death and an identifying event.
//   - TestW19BundlePlaceholderHeadSkippedWithoutSpawn: the same skip for a
//     bundle-placeholder head in an unmodified workspace (the bug's own
//     lane — bundle defaults level).
//   - TestW19ManualModelsListSkipsUnresolvableHeadCandidate: the same skip
//     for a manual `pf submit --models` head.
//   - TestW19AllUnresolvableManualModelsRefusedLoudly: an all-unresolvable
//     manual --models list is refused non-zero naming the candidates, and
//     no step is dispatched.
//   - TestW19AllUnresolvableStepBlocksThroughCandidatesExhausted: an
//     all-unresolvable non-manual step blocks through the existing
//     candidates-exhausted path, loudly, without burning death cycles.
//   - TestW19TaskLaneSkipsUnresolvableCandidate: the task lane runs the
//     resolvable candidate, never the unresolvable head.
//   - TestW19SummarizeLaneSkipsUnresolvableCandidate: the summarize lane
//     never dispatches the unresolvable head; it falls to the resolvable
//     candidate after the interval/transcript gates.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/workflow"
)

// The scenario's candidate pair: the head the model service cannot
// resolve (provider w19p-bad is in no models.json) and the resolvable
// fall-to candidate (provider w19p-good declares w19p-model).
const (
	w19BadPinned  = "w19p-bad/w19p-x"
	w19BadPinned2 = "w19p-bad/w19p-y"
	w19GoodModel  = "w19p-good/w19p-model"
	w19GoodCand   = "pi:" + w19GoodModel
)

// w19ModelsJSON is the pi model-service config the daemon's step-agent
// copy carries (the same file shape ~/.pi/agent/models.json uses): the
// providers the resolvable candidates of this suite name — w19p-good (the
// yaml/manual/task/summarize scenarios) and example-claude (the bundle
// defaults fallback after example-gateway is skipped). example-gateway,
// example-openai and w19p-bad are in no provider list: a candidate naming
// them is exactly the class the harness's model resolution fails on.
const w19ModelsJSON = `{
  "providers": {
    "w19p-good": {
      "baseUrl": "https://w19p.invalid",
      "apiKey": "w19p-fixture",
      "api": "openai-completions",
      "models": [{"id": "w19p-model", "name": "w19p-model"}]
    },
    "example-claude": {
      "baseUrl": "https://w19p.invalid",
      "apiKey": "w19p-fixture",
      "api": "openai-completions",
      "models": [{"id": "claude-opus-5", "name": "claude-opus-5"}]
    }
  }
}`

// w19Daemon boots one daemon on the suite's model-resolution fixture: an
// explicit --operator-agent-dir (auth.json + models.json — an explicit
// source without readable credentials would refuse the start) and a
// --harness-root whose step-agent copy is pre-planted with the same
// models.json, so the lanes that never spawn a step (task, summarize)
// read the same controlled view, and the per-spawn refresh that copies
// the operator dir in cannot change it. yaml ("" = none) is written
// before the daemon starts. The daemon is NOT started — a scenario may
// still add argv (e.g. w224Summarize) before d.start().
func w19Daemon(t *testing.T, inst *v1Install, yaml string) *v1Daemon {
	t.Helper()
	op := t.TempDir()
	if err := os.WriteFile(filepath.Join(op, "auth.json"), []byte(`{"w19p": "fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(op, "models.json"), []byte(w19ModelsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sa := filepath.Join(root, "step-agent")
	if err := os.MkdirAll(sa, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sa, "models.json"), []byte(w19ModelsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newV1Daemon(t, inst, w224OptArgs("--harness-root", root, "--operator-agent-dir", op))
	if yaml != "" {
		w33Yaml(t, d.workspace, yaml)
	}
	return d
}

// w19Plan is the three-step autopilot happy path (the harness's workflows
// dir strips deliver): grill, fix, verification all complete.
func w19Plan() []map[string]any {
	return []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
		// Ordinal 4 pads the pre-change world, where the scenario's
		// unresolvable head burns one dead dispatch (a spawn that still
		// consumes its ordinal but never reaches an action).
		{"do": "envelope", "id": "verification", "outcome": "completed"},
	}
}

// w19AssertNoBadDispatch fails on any model_fail record in the fake
// agent's journal (a spawn that died at birth because the harness could
// not resolve its pinned model — the exact cycle this change removes) and
// on any spawn whose pinned model contains one of bad.
func w19AssertNoBadDispatch(t *testing.T, capDir string, bad ...string) {
	t.Helper()
	for _, rec := range fakeJournal(t, capDir) {
		if rec["type"] == "model_fail" {
			t.Fatalf("a dispatch spawned the unresolvable candidate %q and it died at birth (model_fail record %+v): the preflight must skip it BEFORE any spawn, recording no step death", rec["model"], rec)
		}
		if rec["type"] != "spawn" {
			continue
		}
		m, _ := rec["model"].(string)
		for _, b := range bad {
			if strings.Contains(m, b) {
				t.Fatalf("a process was spawned for the unresolvable candidate (spawn record model %q): the preflight must skip it before the spawn", m)
			}
		}
	}
}

// w19AwaitFirstRecord waits until the fake agent's journal holds any
// record — the pre-change and post-change worlds both dispatch (the bad
// head dies at birth in the former; the fall-to candidate runs in both).
func w19AwaitFirstRecord(t *testing.T, capDir string) {
	t.Helper()
	waitUntil(t, 60*time.Second, "no dispatch ever reached the fake harness (journal empty)", func() bool {
		return len(fakeJournal(t, capDir)) > 0
	})
}

// TestW19TemplatesCarryNoStepLevelModelCandidates pins the shipped-bundle
// half of the fix: none of the six bundles may carry a step-level
// model_candidates key (the placeholder source), while the bundle
// defaults.model_candidates stay (they are explicitly NOT in scope), and
// the living docs carry zero example-* references. RED today: bug-fix,
// release, feature-full, chore and autopilot pin step-level candidates.
func TestW19TemplatesCarryNoStepLevelModelCandidates(t *testing.T) {
	for _, b := range []string{"bug-fix", "release", "feature-full", "chore", "micro", "autopilot"} {
		fm, err := workflow.ReadFrontmatter(filepath.Join("..", "workflows", b, "workflow.md"))
		if err != nil || fm == nil {
			t.Fatalf("workflows/%s/workflow.md: unreadable frontmatter (%v, %v)", b, fm, err)
		}
		for _, st := range fm.Steps {
			if len(st.ModelCandidates) > 0 {
				t.Fatalf("workflows/%s/workflow.md: step %q still carries a step-level model_candidates key %v — the templates must carry no step-level model choice (spec: Shipped bundles MUST NOT carry step-level model_candidates)", b, st.ID, st.ModelCandidates)
			}
		}
		if fm.Defaults == nil || len(fm.Defaults.ModelCandidates) == 0 {
			t.Fatalf("workflows/%s/workflow.md: bundle defaults.model_candidates must stay present (the example-* documentation placeholders are explicitly NOT in scope of the removal)", b)
		}
	}

	// The living docs: zero example-* references (docs/upgrade.md is the
	// append-only history and deliberately not in this grep).
	var files []string
	for _, p := range []string{"README.md", "docs/bundles.md", "docs/extending.md", "docs/quickstart-developers.md", "docs/quickstart-macos.md"} {
		files = append(files, p)
	}
	_ = filepath.WalkDir(filepath.Join("..", "docs", "reference"), func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	for _, p := range files {
		raw, err := os.ReadFile(filepath.Join("..", p))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if i := strings.Index(string(raw), "example-"); i >= 0 {
			line := 1 + strings.Count(string(raw)[:i], "\n")
			t.Fatalf("%s:%d carries an example-* model reference — the living docs must carry zero (spec: Docs grep after the change)", p, line)
		}
	}
}

// TestW19DefaultModelsWorkspaceResolvesNoExampleCandidates pins the
// spec's "Unmodified workspace with default.models": a workspace setting
// only default.models composes every step's chain from it (falling to
// bundle defaults if unset) with no step-level example-* candidate
// anywhere. RED today: the autopilot verification step's own step-level
// list outranks default.models, so its dispatch runs under
// example-claude/claude-opus-5 after example-gateway/model-a dies.
func TestW19DefaultModelsWorkspaceResolvesNoExampleCandidates(t *testing.T) {
	inst := newV1Install(t)
	d := w19Daemon(t, inst, "steps:\n  e2e:\n    default: { models: ["+w19GoodCand+"] }\n")
	d.start()
	wi := inst.newWI(t, "w19: default.models workspace resolves no example-* candidates", w19Plan(), nil)
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)
	w19AwaitFirstRecord(t, capDir)
	w113WaitCompleted(t, capDir, 120*time.Second)

	ms := spawnModels(t, capDir)
	if len(ms) != 3 {
		t.Fatalf("want exactly one dispatch per step (grill, fix, verification), got %v", ms)
	}
	for _, m := range ms {
		if strings.Contains(m, "example-") {
			t.Fatalf("a step dispatched under the placeholder model %q — a default.models-only workspace must resolve no step-level example-* candidate anywhere in the chain", m)
		}
		if m != w19GoodModel {
			t.Fatalf("a step dispatched under %q, want the default.models candidate %q", m, w19GoodModel)
		}
	}
	for _, st := range []string{"grill", "fix", "verification"} {
		if m := readExecution(t, capDir).Steps[st].model(); m != w19GoodModel {
			t.Fatalf("step %s finished on model %q, want %q", st, m, w19GoodModel)
		}
	}
}

// TestW19YamlStepListSkipsUnresolvableHeadCandidate pins the preflight at
// the yaml step-list level: every step's list starts with a candidate the
// model service cannot resolve; the daemon must skip it before any spawn
// (no model_fail, no step death, no death count) and dispatch the
// fall-to candidate, leaving an identifying event for the skipped one.
func TestW19YamlStepListSkipsUnresolvableHeadCandidate(t *testing.T) {
	inst := newV1Install(t)
	list := "pi:" + w19BadPinned + ", " + w19GoodCand
	d := w19Daemon(t, inst, w33StepYaml(list))
	d.start()
	wi := inst.newWI(t, "w19: yaml step-list head is skipped before any spawn", w19Plan(), nil)
	writePlanExtra(t, inst, wi, w19Plan(), nil, map[string]any{"bad_models": []string{w19BadPinned}})
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	w19AwaitFirstRecord(t, capDir)
	w19AssertNoBadDispatch(t, capDir, w19BadPinned)
	w113WaitCompleted(t, capDir, 120*time.Second)
	w19AssertNoBadDispatch(t, capDir, w19BadPinned)

	ms := spawnModels(t, capDir)
	if len(ms) != 3 || ms[0] != w19GoodModel || ms[1] != w19GoodModel || ms[2] != w19GoodModel {
		t.Fatalf("want exactly one dispatch per step, all on %q, got %v", w19GoodModel, ms)
	}
	for _, f := range stepFailures(t, d, wi) {
		t.Fatalf("step %q recorded a failure (cause %s, from %q): a preflight-skipped candidate must leave no step death", f.Step, f.Cause, f.From)
	}
	if !strings.Contains(eventsRaw(t, d.dataDir), w19BadPinned) {
		t.Fatalf("no event identifies the skipped candidate %q — a skipped candidate must be observable without counting as a death", w19BadPinned)
	}
}

// TestW19BundlePlaceholderHeadSkippedWithoutSpawn pins the preflight at
// the bundle level in the bug's own lane: an unmodified workspace (no
// model keys at all) whose steps resolve the shipped bundles' placeholder
// chains. The example-gateway/model-a head must be skipped before any
// spawn — no death cycle — and the step runs on the next candidate.
func TestW19BundlePlaceholderHeadSkippedWithoutSpawn(t *testing.T) {
	inst := newV1Install(t)
	d := w19Daemon(t, inst, "") // unmodified: no workspace model keys
	d.start()
	wi := inst.newWI(t, "w19: bundle placeholder head is skipped before any spawn", w19Plan(), nil)
	writePlanExtra(t, inst, wi, w19Plan(), nil, map[string]any{"bad_models": []string{"example-gateway/model-a"}})
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	w19AwaitFirstRecord(t, capDir)
	w19AssertNoBadDispatch(t, capDir, "example-gateway/model-a")
	w113WaitCompleted(t, capDir, 120*time.Second)
	w19AssertNoBadDispatch(t, capDir, "example-gateway/model-a")

	ms := spawnModels(t, capDir)
	if len(ms) != 3 {
		t.Fatalf("want exactly one dispatch per step (grill, fix, verification), got %v", ms)
	}
	for _, m := range ms {
		if m != "example-claude/claude-opus-5" {
			t.Fatalf("a step dispatched under %q, want the bundle-defaults fall-to candidate example-claude/claude-opus-5", m)
		}
	}
	for _, f := range stepFailures(t, d, wi) {
		t.Fatalf("step %q recorded a failure (cause %s, from %q): a preflight-skipped placeholder must leave no step death", f.Step, f.Cause, f.From)
	}
	if !strings.Contains(eventsRaw(t, d.dataDir), "example-gateway/model-a") {
		t.Fatalf("no event identifies the skipped placeholder example-gateway/model-a — a skipped candidate must be observable without counting as a death")
	}
}

// TestW19ManualModelsListSkipsUnresolvableHeadCandidate pins the preflight
// at the manual `pf submit --models` level: the manual list outranks the
// yaml step lists, so its unresolvable head must be skipped before any
// spawn exactly like a composed one.
func TestW19ManualModelsListSkipsUnresolvableHeadCandidate(t *testing.T) {
	inst := newV1Install(t)
	d := w19Daemon(t, inst, w33StepYaml(w19GoodCand))
	d.start()
	wi := inst.newWI(t, "w19: manual --models head is skipped before any spawn", w19Plan(), nil)
	writePlanExtra(t, inst, wi, w19Plan(), nil, map[string]any{"bad_models": []string{w19BadPinned}})
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi, "--models", "pi:"+w19BadPinned+","+w19GoodCand)
	capDir := d.capsuleDir(wi)

	w19AwaitFirstRecord(t, capDir)
	w19AssertNoBadDispatch(t, capDir, w19BadPinned)
	w113WaitCompleted(t, capDir, 120*time.Second)
	w19AssertNoBadDispatch(t, capDir, w19BadPinned)

	ms := spawnModels(t, capDir)
	if len(ms) != 3 || ms[0] != w19GoodModel || ms[1] != w19GoodModel || ms[2] != w19GoodModel {
		t.Fatalf("want exactly one dispatch per step, all on the manual list's resolvable candidate %q, got %v", w19GoodModel, ms)
	}
	for _, f := range stepFailures(t, d, wi) {
		t.Fatalf("step %q recorded a failure (cause %s, from %q): a preflight-skipped manual candidate must leave no step death", f.Step, f.Cause, f.From)
	}
	if !strings.Contains(eventsRaw(t, d.dataDir), w19BadPinned) {
		t.Fatalf("no event identifies the skipped manual candidate %q", w19BadPinned)
	}
}

// TestW19AllUnresolvableManualModelsRefusedLoudly pins the loud refusal:
// a manual --models list whose every candidate fails the classification
// must be refused non-zero naming the candidates, with no step
// dispatched — never silently passing through to an empty candidate set.
func TestW19AllUnresolvableManualModelsRefusedLoudly(t *testing.T) {
	inst := newV1Install(t)
	d := w19Daemon(t, inst, w33StepYaml(w19GoodCand))
	d.start()
	wi := inst.newWI(t, "w19: all-unresolvable manual --models list refused loudly", w19Plan(), nil)
	t.Cleanup(func() { killOrphans(wi) })

	out, code, _ := w224Pf(t, inst, d.sock, "submit", wi, "--models", "pi:"+w19BadPinned+",pi:"+w19BadPinned2)
	if code == 0 {
		t.Fatalf("an all-unresolvable --models list must be refused; it submitted: %q", out)
	}
	if strings.Contains(out, "unknown command") {
		t.Fatalf("the submit verb family must be recognized; got %q", out)
	}
	for _, m := range []string{w19BadPinned, w19BadPinned2} {
		if !strings.Contains(out, m) {
			t.Fatalf("the refusal must name the unresolvable candidate %s; got %q", m, out)
		}
	}

	// No step dispatched: no capsule, no event, the work item stays
	// unclaimed for the daemon.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(d.capsuleDir(wi)); err == nil {
			t.Fatalf("a step was dispatched for the refused submission (capsule at %s)", d.capsuleDir(wi))
		}
		if evs := d.eventsOf(t, wi); len(evs) > 0 {
			t.Fatalf("the refused submission dispatched work (events: %+v)", evs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestW19AllUnresolvableStepBlocksThroughCandidatesExhausted pins the
// non-manual exhaustion path: a step whose every composed candidate fails
// the classification must block through the existing candidates-exhausted
// path — loudly, naming the candidates — without burning a death cycle
// per candidate first.
func TestW19AllUnresolvableStepBlocksThroughCandidatesExhausted(t *testing.T) {
	inst := newV1Install(t)
	d := w19Daemon(t, inst, "steps:\n  e2e:\n"+
		"    grill: { models: ["+w19GoodCand+"] }\n"+
		"    fix: { models: [pi:"+w19BadPinned+", pi:"+w19BadPinned2+"] }\n"+
		"    verification: { models: ["+w19GoodCand+"] }\n")
	d.start()
	wi := inst.newWI(t, "w19: all-unresolvable step blocks loudly, no death cycle", w19Plan(), nil)
	writePlanExtra(t, inst, wi, w19Plan(), nil, map[string]any{"bad_models": []string{w19BadPinned, w19BadPinned2}})
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	capDir := d.capsuleDir(wi)

	waitExecution(t, capDir, 180*time.Second, "the work item never blocked on its all-unresolvable fix step",
		func(x *executionView) bool { return x.WorkflowStatus == "blocked" })
	w19AssertNoBadDispatch(t, capDir, w19BadPinned, w19BadPinned2)

	for _, f := range stepFailures(t, d, wi) {
		if f.Disposition == "switch_model" {
			t.Fatalf("the exhausted fix step burned a death cycle before blocking (%+v): every candidate must be skipped before any spawn", f)
		}
	}
	if len(w33EventsOf(t, d.dataDir, wi, "candidates_exhausted")) == 0 {
		t.Fatalf("no candidates_exhausted event fired for the all-unresolvable step — the block must ride the existing loud path")
	}
	for _, m := range []string{w19BadPinned, w19BadPinned2} {
		if !strings.Contains(eventsRaw(t, d.dataDir), m) {
			t.Fatalf("the block never names the skipped candidate %s", m)
		}
	}
	if ms := spawnModels(t, capDir); len(ms) != 1 || ms[0] != w19GoodModel {
		t.Fatalf("want exactly one dispatch (grill on %q); the all-unresolvable fix must spawn nothing, got %v", w19GoodModel, ms)
	}
}

// w19TaskView is the pf task show --json shape this scenario pins (the
// task record's state, report, current candidate, error).
type w19TaskView struct {
	State     string `json:"state"`
	Report    string `json:"report"`
	Candidate string `json:"candidate"`
	Error     string `json:"error"`
}

// TestW19TaskLaneSkipsUnresolvableCandidate pins the preflight on the task
// lane: a spawn whose task models resolve [unresolvable, resolvable] must
// complete on the resolvable candidate with the unresolvable one never
// run — no report, no candidate switch on it.
func TestW19TaskLaneSkipsUnresolvableCandidate(t *testing.T) {
	inst := newV1Install(t)
	d := w19Daemon(t, inst, w40TaskYaml("task: { models: [pi:"+w19BadPinned+", "+w19GoodCand+"] }"))
	d.start()

	out, code, _ := w224Pf(t, inst, d.sock, "task", "spawn", "--project", "e2e", "w19: run under the resolvable model")
	if code != 0 {
		t.Fatalf("`pf task spawn` must succeed and print a task id; exit %d output %q", code, out)
	}
	id := w40TaskIDRe.FindString(out)
	if id == "" {
		t.Fatalf("`pf task spawn` must print a task_<16hex> id; output %q", out)
	}

	var final w19TaskView
	waitUntil(t, 60*time.Second, "the spawned task never reached a terminal state", func() bool {
		so, sc, _ := w224Pf(t, inst, d.sock, "task", "show", id, "--json")
		if sc != 0 {
			return false
		}
		return json.Unmarshal([]byte(so), &final) == nil && (final.State == "completed" || final.State == "failed" || final.State == "capped" || final.State == "interrupted")
	})
	if final.State != "completed" {
		t.Fatalf("the task must complete on the resolvable candidate; state %q error %q", final.State, final.Error)
	}
	if final.Candidate != w19GoodCand {
		t.Fatalf("the task must record the resolvable candidate %q as the model it ran, got %q", w19GoodCand, final.Candidate)
	}
	if !strings.Contains(final.Report, "model="+w19GoodModel) {
		t.Fatalf("the task report must name the model it ran (%q); report %q", w19GoodModel, final.Report)
	}
	if strings.Contains(final.Report, w19BadPinned) {
		t.Fatalf("the unresolvable candidate %q was run by the task lane (report names it): the preflight must skip it before the runner", w19BadPinned)
	}
}

// TestW19SummarizeLaneSkipsUnresolvableCandidate pins the preflight on the
// summarize lane: the lane's first candidate is unresolvable, so its
// demand must either land an error naming the skipped candidate or answer
// directly on the resolvable one — never a dispatch under the bad model
// (the fake's held line names the model it ran) — and after the interval
// and transcript gates the next call answers on the resolvable candidate.
func TestW19SummarizeLaneSkipsUnresolvableCandidate(t *testing.T) {
	inst := newV1Install(t)
	d := w19Daemon(t, inst, "")
	w224Summarize(t, d, "pi:"+w19BadPinned, w19GoodCand)
	d.start()
	wi := w224NewWI(t, inst, "w19: summarize lane skips the unresolvable candidate", func(wi string) []map[string]any {
		return []map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "run": []string{w224GrowCmd(75)}, "ms": 30000},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
		}
	})
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	waitStepStatus(t, d, wi, "fix", "in_progress", 30*time.Second)
	capDir := d.capsuleDir(wi)

	w224Demand(t, inst, d, wi)
	s := w224AwaitSummary(t, inst, d, wi, 20*time.Second)
	if s.Summary != nil && strings.Contains(*s.Summary, w19BadPinned) {
		t.Fatalf("the summarize lane dispatched the unresolvable candidate %q (its run answered): %q", w19BadPinned, w224Str(s.Summary))
	}
	if s.Summary == nil {
		if s.SummaryError == nil || !strings.Contains(*s.SummaryError, w19BadPinned) {
			t.Fatalf("the first summarize fact after the skip must either be a call on %q or an error naming the skipped candidate; got error %q", w19GoodCand, w224Str(s.SummaryError))
		}
	} else if !strings.Contains(*s.Summary, "model="+w19GoodModel) {
		t.Fatalf("a summarize call ran under neither the resolvable model %q nor an error naming the skipped one: %q", w19GoodModel, w224Str(s.Summary))
	}

	// The interval and the transcript-growth gates space the next call;
	// the grown transcript arms it and it answers on the resolvable model.
	w224AwaitRun(t, capDir, "PI_CODING_AGENT_SESSION_DIR", 90*time.Second)
	w224Demand(t, inst, d, wi)
	s2 := w224AwaitSummary(t, inst, d, wi, 20*time.Second)
	if s2.Summary == nil {
		t.Fatalf("the post-growth call never answered on the resolvable candidate; error %q", w224Str(s2.SummaryError))
	}
	if !strings.Contains(*s2.Summary, "model="+w19GoodModel) {
		t.Fatalf("the post-growth summarize call must run on the resolvable model %q: %q", w19GoodModel, w224Str(s2.Summary))
	}
	if strings.Contains(*s2.Summary, w19BadPinned) {
		t.Fatalf("the unresolvable candidate %q was dispatched by the summarize lane: %q", w19BadPinned, w224Str(s2.Summary))
	}
}
