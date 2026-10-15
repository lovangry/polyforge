package driver

// harness_test.go: polyforge#159 in the driver — the per-step harness
// (claim-time map > bundle choice > pi), the candidate list FILTERED to
// the ids the step's harness serves (switch and exhaustion walk the
// filtered list, so incompatible candidates are skipped), and the spawn
// adapter's harness-aware resume/fresh choice.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/harness"
)

func harnessDriver(t *testing.T, stepHarness map[string]string, bundleHarness string, list []string, attempts []capsule.Attempt) *Driver {
	t.Helper()
	dir := t.TempDir()
	execPath := filepath.Join(dir, capsule.ExecutionJSON)
	seedExecution(t, execPath, "review", capsule.StepInProgress, attempts)
	d, err := New(Config{
		WI:       WIInfo{ID: "wi_run", CapsuleDir: dir, BundleName: "b"},
		ExecPath: execPath,
		Workflow: &Workflow{Steps: []StepSpec{
			{ID: "review", ContextPolicy: "fresh", Harness: bundleHarness, ModelCandidates: []string{"bundle/x"}},
		}, MaxTotal: 4, MaxPerStep: 2},
		Spawner:        &fakeSpawner{},
		Log:            &eventLog{},
		StepCandidates: map[string][]string{"review": list},
		StepHarness:    stepHarness,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestHarnessFiltersCandidates(t *testing.T) {
	mixed := []string{"g/glm", "claude-opus-5-5", "g/flash", "claude-sonnet-5"}
	model := infraAttempt(1, FailExitModel)
	cases := []struct {
		name        string
		stepHarness map[string]string
		bundle      string
		attempts    []capsule.Attempt
		wantH       string
		wantModel   string
	}{
		{"claude step takes the first claude id", map[string]string{"review": "claude"}, "", nil, "claude", "claude-opus-5-5"},
		{"claude step switches to the next claude id (pi ids skipped)", map[string]string{"review": "claude"}, "", []capsule.Attempt{model}, "claude", "claude-sonnet-5"},
		{"claude step caps at its last claude id", map[string]string{"review": "claude"}, "", []capsule.Attempt{model, infraAttempt(2, FailExitModel)}, "claude", "claude-sonnet-5"},
		{"pi step takes the first pi id", map[string]string{"review": "pi"}, "", nil, "pi", "g/glm"},
		{"pi step switches to the next pi id (claude ids skipped)", map[string]string{"review": "pi"}, "", []capsule.Attempt{model}, "pi", "g/flash"},
		{"no map: the bundle's own harness", nil, "claude", nil, "claude", "claude-opus-5-5"},
		{"nothing chosen: pi", nil, "", nil, "pi", "g/glm"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := harnessDriver(t, c.stepHarness, c.bundle, mixed, c.attempts)
			sp, _, err := d.prepareDispatch("review")
			if err != nil {
				t.Fatal(err)
			}
			if sp.Harness != c.wantH || sp.Model != c.wantModel {
				t.Fatalf("dispatch harness %q model %q, want %q %q", sp.Harness, sp.Model, c.wantH, c.wantModel)
			}
		})
	}
}

// The switch decision walks the filtered list: on a claude step with one
// claude candidate a model-class failure is an exhaustion, never a
// switch onto a pi id.
func TestHarnessSwitchSkipsIncompatible(t *testing.T) {
	d := harnessDriver(t, map[string]string{"review": "claude"}, "", []string{"claude-opus-5-5", "g/glm"}, nil)
	d.mu.Lock()
	dec := d.decideFailureLocked("review", d.exec.Steps["review"], &StepFailure{Cause: FailExitModel})
	d.mu.Unlock()
	if dec.Switch || !dec.Exhausted {
		t.Fatalf("decision %+v, want exhausted (the only claude candidate is in use)", dec)
	}
	d = harnessDriver(t, map[string]string{"review": "claude"}, "", []string{"claude-opus-5-5", "g/glm", "claude-sonnet-5"}, nil)
	d.mu.Lock()
	dec = d.decideFailureLocked("review", d.exec.Steps["review"], &StepFailure{Cause: FailExitModel})
	d.mu.Unlock()
	if !dec.Switch || dec.From != "claude-opus-5-5" || dec.To != "claude-sonnet-5" {
		t.Fatalf("decision %+v, want a switch opus → sonnet", dec)
	}
}

// The adapter: a claude dispatch gets a minted session uuid and its own
// capsule transcript; a resume continues the previous claude session
// only when the native file exists; a harness change between dispatches
// never resumes.
func TestAdapterClaudeSessionAndResume(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	ad, sp, capDir := spawnEnv(t)

	run, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "grill", WIID: "wi_sp", SpawnN: 1, Harness: "claude", Model: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := sp.last()
	if p.Harness != "claude" || !harness.ValidSessionID(p.HarnessSession) || p.Resume {
		t.Fatalf("fresh claude params %+v", p)
	}
	first := p.HarnessSession
	if m := run.(*stepRun).Meta(); m.Harness != "claude" || m.HarnessSession != first {
		t.Fatalf("meta %+v", m)
	}
	entries, _ := capsule.ReadSpawnJournal(capDir)
	if len(entries) != 0 {
		// the capture spawner never calls Journal; write the line the real
		// spawner would, so the resume lookup has its ledger
		t.Fatalf("unexpected journal %+v", entries)
	}
	if err := ad.appendSpawnJournal(p, "grill", "ab12cd34", 1, "2026-10-15T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	// resume without the native file: fresh, a NEW session
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "grill", WIID: "wi_sp", SpawnN: 2, Harness: "claude", Resume: true}); err != nil {
		t.Fatal(err)
	}
	p, _ = sp.last()
	if p.Resume || p.HarnessSession == first {
		t.Fatalf("resume without a native session must start fresh: %+v", p)
	}

	// the native file appears: the resume continues the SAME session, on
	// a new capsule transcript of its own
	native := filepath.Join(cfgDir, "projects", "-cap", first+".jsonl")
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "grill", WIID: "wi_sp", SpawnN: 3, Harness: "claude", Resume: true}); err != nil {
		t.Fatal(err)
	}
	p, _ = sp.last()
	if !p.Resume || p.HarnessSession != first {
		t.Fatalf("resume must continue session %s: %+v", first, p)
	}
	if p.TranscriptPath == entries0Transcript(t, capDir) {
		t.Fatalf("a claude resume writes its own capsule transcript, got the previous one %s", p.TranscriptPath)
	}

	// the step switched to pi: a pi resume never picks up the claude line
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "grill", WIID: "wi_sp", SpawnN: 4, Harness: "pi", Resume: true}); err != nil {
		t.Fatal(err)
	}
	p, _ = sp.last()
	if p.Resume || p.HarnessSession != "" {
		t.Fatalf("a pi dispatch after a claude one must start fresh: %+v", p)
	}
}

func entries0Transcript(t *testing.T, capDir string) string {
	t.Helper()
	entries, err := capsule.ReadSpawnJournal(capDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("journal %v %v", entries, err)
	}
	return entries[0].Transcript
}

func TestWorkflowHarnessParse(t *testing.T) {
	wf, err := parseWorkflow([]byte("defaults:\n  harness: claude\nsteps:\n  - { id: a }\n  - { id: b, harness: pi }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if wf.Steps[0].Harness != "claude" || wf.Steps[1].Harness != "pi" {
		t.Fatalf("harness %q %q", wf.Steps[0].Harness, wf.Steps[1].Harness)
	}
	// a bundle without the field (every pre-#159 snapshot) declares none
	wf, _ = parseWorkflow([]byte("steps:\n  - { id: a }\n"))
	if wf.Steps[0].Harness != "" || validateWorkflow(wf) != nil {
		t.Fatalf("old bundle %+v", wf.Steps[0])
	}
	wf, _ = parseWorkflow([]byte("steps:\n  - { id: a, harness: claude-code }\n"))
	if err := validateWorkflow(wf); err == nil {
		t.Fatal("unknown harness accepted at load")
	}
}
