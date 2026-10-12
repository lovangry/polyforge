package driver

// spawn_test.go: the StepSpawnAdapter — prompt assembly (inherit vs
// fresh, handoff injection, declared inputs, rewind verdict, Q&A
// history, delivery contract), the SpawnParams mapping (model pin,
// resume session from the spawn journal, identity journal), and the
// stepRun's envelope collection (drop path, light fence checks, R14
// staging archive, teardown, exit-without-envelope).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/spawn"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// ---------------------------------------------------------------------------
// Fakes: a runtime.Spawner that records every SpawnParams and hands
// back a controllable AgentHandle.

type fakeAgent struct {
	mu   sync.Mutex
	done bool
	term bool
}

func (h *fakeAgent) PID() int          { return 4242 }
func (h *fakeAgent) StartedAt() string { return "2026-10-06T00:00:00Z" }
func (h *fakeAgent) Alive() bool       { h.mu.Lock(); defer h.mu.Unlock(); return !h.done }
func (h *fakeAgent) Done() bool        { h.mu.Lock(); defer h.mu.Unlock(); return h.done }
func (h *fakeAgent) Terminate(bound time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.done = true
	h.term = true
	return nil
}

func (h *fakeAgent) setDone() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.done = true
}

func (h *fakeAgent) terminated() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.term
}

type captureSpawner struct {
	mu      sync.Mutex
	params  []runtime.SpawnParams
	handles []*fakeAgent
}

func (s *captureSpawner) Spawn(ctx context.Context, p runtime.SpawnParams) (runtime.AgentHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := &fakeAgent{}
	s.params = append(s.params, p)
	s.handles = append(s.handles, h)
	return h, nil
}

func (s *captureSpawner) last() (runtime.SpawnParams, *fakeAgent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.params[len(s.params)-1], s.handles[len(s.handles)-1]
}

// ---------------------------------------------------------------------------
// Environment: a capsule with a materialized bundle (workflow.md with
// the extended frontmatter + steps/<id>.md definitions) and a session
// handle.

func writeCapsuleFile(t *testing.T, capDir, rel, content string) {
	t.Helper()
	path := filepath.Join(capDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeArchivedResult(t *testing.T, capDir, stepID string, n int64, res *StepResult) {
	t.Helper()
	blob, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeCapsuleFile(t, capDir, fmt.Sprintf("artifacts/steps/%s/s%d/result.json", stepID, n), string(blob))
}

func spawnEnv(t *testing.T) (*StepSpawnAdapter, *captureSpawner, string) {
	t.Helper()
	capDir := t.TempDir()
	writeCapsuleFile(t, capDir, "workflows/quick-fix-x/workflow.md", "---\n"+strings.TrimPrefix(extExample, "\n")+"---\n")
	writeCapsuleFile(t, capDir, "workflows/quick-fix-x/steps/grill.md", "grill: ask the sharp questions")
	writeCapsuleFile(t, capDir, "workflows/quick-fix-x/steps/fix.md", "fix: implement the micro-spec")
	writeCapsuleFile(t, capDir, "workflows/quick-fix-x/steps/verification.md",
		"---\ntools: [go]\n---\n\nverification: build, lint, test")
	writeCapsuleFile(t, capDir, capsule.SessionJSON, `{"harness":"pi","session_id":"ab12cd34"}`)
	sp := &captureSpawner{}
	ad, err := NewStepSpawnAdapter(StepSpawnAdapter{
		WI:           WIInfo{ID: "wi_sp", CapsuleDir: capDir, BundleName: "quick-fix-x", BundleDigest: "dg"},
		Spawner:      sp,
		PollInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewStepSpawnAdapter: %v", err)
	}
	return ad, sp, capDir
}

// grillDone lays out the post-grill capsule: the handoff artifact, the
// declared fix inputs and grill's archived envelope carrying the
// handoff_ref.
func grillDone(t *testing.T, capDir string) {
	t.Helper()
	writeCapsuleFile(t, capDir, "artifacts/fix-memo.md", "the micro-spec body")
	writeCapsuleFile(t, capDir, "artifacts/grill-qa.md", "the q/a log")
	writeArchivedResult(t, capDir, "grill", 1, &StepResult{
		ProtocolVersion: 1,
		ResultID:        "319584c8-10e7-5703-8b0f-4af0a03f8657",
		WIID:            "wi_sp",
		StepID:          "grill",
		SpawnN:          1,
		Outcome:         OutcomeCompleted,
		Output:          json.RawMessage(`{}`),
		HandoffRef:      &ArtifactRef{Path: "artifacts/fix-memo.md", SHA256: hashOfFile(t, capDir, "artifacts/fix-memo.md"), Bytes: 19},
	})
}

// ---------------------------------------------------------------------------
// Prompt assembly.

func TestSpawnStepInheritPromptAssembly(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	grillDone(t, capDir)

	_, err := ad.SpawnStep(context.Background(), StepSpawn{
		StepID: "fix",
		SpawnN: 2,
		Model:  "prov/glm",
		RewindInput: &capsule.RewindInput{
			ToStep: "fix",
			Reason: "FAIL: tests red",
		},
		Answers: []capsule.ArchivedAnswer{
			{QuestionID: "q_1", Answer: "yes, vendored", AnsweredBy: "owner"},
		},
	})
	if err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	p, _ := sp.last()
	if p.Model != "prov/glm" {
		t.Fatalf("model pin: %q", p.Model)
	}
	if p.SpawnN != 2 || p.Resume {
		t.Fatalf("spawnN/resume: %d/%v", p.SpawnN, p.Resume)
	}
	if p.SessionHandle != "ab12cd34" {
		t.Fatalf("session handle: %q", p.SessionHandle)
	}
	if p.WorkItemID != "wi_sp" || p.CapsuleDir != capDir {
		t.Fatalf("identity: %q %q", p.WorkItemID, p.CapsuleDir)
	}
	prompt := p.InitialPrompt
	for _, want := range []string{
		"fix: implement the micro-spec", // step definition body
		"Handoff from step \"grill\"",   // handoff section
		"artifacts/fix-memo.md",         // handoff path
		"the micro-spec body",           // handoff content
		"the q/a log",                   // declared input content
		"FAIL: tests red",               // rewind verdict
		"q_1", "yes, vendored",          // Q&A history
		"runtime-state/pending-results/2.json", // delivery path
		"handoff_ref",                          // fix declares handoff
		"INHERIT",                              // context note
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "FRESH") {
		t.Fatalf("inherit prompt must not carry the fresh note\n%s", prompt)
	}
}

func TestSpawnStepFreshPromptOmitsHandoff(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	grillDone(t, capDir)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "the fix summary")

	_, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 3})
	if err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	p, _ := sp.last()
	prompt := p.InitialPrompt
	if !strings.Contains(prompt, "verification: build, lint, test") {
		t.Fatalf("prompt missing the step definition (frontmatter stripped)\n%s", prompt)
	}
	if strings.Contains(prompt, "tools: [go]") {
		t.Fatalf("step-definition frontmatter must be stripped\n%s", prompt)
	}
	if !strings.Contains(prompt, "the fix summary") {
		t.Fatalf("prompt missing declared input content\n%s", prompt)
	}
	if strings.Contains(prompt, "the micro-spec body") || strings.Contains(prompt, "Handoff from step") {
		t.Fatalf("fresh prompt must not carry a handoff\n%s", prompt)
	}
	if !strings.Contains(prompt, "FRESH") {
		t.Fatalf("prompt missing the fresh context note\n%s", prompt)
	}
	// verification declares no handoff: the contract must not demand one
	if strings.Contains(prompt, "DECLARES handoff") {
		t.Fatalf("fresh non-handoff step must not owe a handoff_ref\n%s", prompt)
	}
}

// TestLatestJournalTranscriptFiltersByStep: the capsule journal
// interleaves every step's spawns; a resume of step X must never pick
// the higher-ordinal transcript of another step (the review-inside-
// e2e-run's-conversation bug), and lines without a step id never match.
func TestLatestJournalTranscriptFiltersByStep(t *testing.T) {
	ad, _, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/spawn-journal.jsonl",
		`{"spawn_n":1,"step_id":"review","transcript":"/t/review1.jsonl"}
{"spawn_n":2,"step_id":"code","transcript":"/t/code2.jsonl"}
{"spawn_n":3,"step_id":"e2e-run","transcript":"/t/e2e3.jsonl"}
{"spawn_n":4,"transcript":"/t/untagged4.jsonl"}
{"spawn_n":5,"step_id":"review","transcript":""}
`)
	for step, want := range map[string]string{"review": "/t/review1.jsonl", "code": "/t/code2.jsonl", "e2e-run": "/t/e2e3.jsonl"} {
		got, err := ad.latestJournalTranscript(step)
		if err != nil || got != want {
			t.Fatalf("step %s: got (%q, %v) want %q", step, got, err, want)
		}
	}
	if got, err := ad.latestJournalTranscript("polish"); err != nil || got != "" {
		t.Fatalf("a step with no journal line answers (\"\", nil), got (%q, %v)", got, err)
	}
}

func TestSpawnStepResumeUsesJournal(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "the fix summary")
	journal := `{"spawn_n":1,"step_id":"verification","transcript":"/old/t1.jsonl","work_item":"wi_sp"}
{"spawn_n":3,"step_id":"verification","transcript":"/live/t3.jsonl","work_item":"wi_sp"}
`
	liveT3 := filepath.Join(capDir, "session", "live-t3.jsonl")
	writeCapsuleFile(t, capDir, "session/live-t3.jsonl", `{"t":"turn"}`+"\n")
	journal = strings.ReplaceAll(journal, "/live/t3.jsonl", liveT3)
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/spawn-journal.jsonl", journal)

	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 4, Resume: true}); err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	p, _ := sp.last()
	if !p.Resume || p.TranscriptPath != liveT3 {
		t.Fatalf("resume mapping: resume=%v transcript=%q", p.Resume, p.TranscriptPath)
	}

	// a fresh derivation declares its own capsule-side session copy
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 5}); err != nil {
		t.Fatalf("SpawnStep fresh: %v", err)
	}
	p2, _ := sp.last()
	if p2.Resume {
		t.Fatal("fresh dispatch must not resume")
	}
	want := filepath.Join(capDir, capsule.DirSession, "session-ab12cd34#4.jsonl")
	if p2.TranscriptPath != want {
		t.Fatalf("fresh transcript: %q want %q", p2.TranscriptPath, want)
	}
}

func TestSpawnStepJournalCallback(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "s")
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 7}); err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	p, _ := sp.last()
	if p.Journal == nil {
		t.Fatal("SpawnParams must carry the journal callback")
	}
	if err := p.Journal(99, "2026-10-06T01:02:03Z"); err != nil {
		t.Fatalf("journal callback: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(capDir, capsule.DirRuntimeState, "spawn-journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &line); err != nil {
		t.Fatalf("journal line unparsable: %v\n%s", err, raw)
	}
	for key, want := range map[string]any{
		"work_item": "wi_sp", "pid": float64(99), "spawn_n": float64(7),
		"resume": false, "transcript": p.TranscriptPath, "session_handle": "ab12cd34",
	} {
		if line[key] != want {
			t.Fatalf("journal %s = %v, want %v", key, line[key], want)
		}
	}
	if line["spawn_token"] != "ab12cd34#6" {
		t.Fatalf("journal spawn_token = %v", line["spawn_token"])
	}
}

func TestSpawnStepInputFormErrors(t *testing.T) {
	ad, _, capDir := spawnEnv(t)
	grillDone(t, capDir)

	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "nope", SpawnN: 1}); err == nil {
		t.Fatal("unknown step must fail")
	}
	// missing declared input (grill-qa.md removed)
	if err := os.Remove(filepath.Join(capDir, "artifacts", "grill-qa.md")); err != nil {
		t.Fatal(err)
	}
	_, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "fix", SpawnN: 2})
	if err == nil || !strings.Contains(err.Error(), "grill-qa.md") {
		t.Fatalf("missing declared input must fail naming it, got %v", err)
	}

	// missing step definition
	if err := os.Remove(filepath.Join(capDir, "workflows", "quick-fix-x", "steps", "fix.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "fix", SpawnN: 2}); err == nil ||
		!strings.Contains(err.Error(), "step definition") {
		t.Fatalf("missing step definition must fail, got %v", err)
	}
}

func TestSpawnStepInheritWithoutProducerEnvelopeFails(t *testing.T) {
	ad, _, capDir := spawnEnv(t)
	// fix is inherit; grill declares handoff but nothing is archived
	// (the inputs exist, the envelope does not)
	writeCapsuleFile(t, capDir, "artifacts/fix-memo.md", "m")
	writeCapsuleFile(t, capDir, "artifacts/grill-qa.md", "q")
	_, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "fix", SpawnN: 2})
	if err == nil || !strings.Contains(err.Error(), "handoff") {
		t.Fatalf("missing producer envelope must fail loudly, got %v", err)
	}
}

func TestSpawnStepFirstStepHasNoHandoffSection(t *testing.T) {
	ad, sp, _ := spawnEnv(t)
	// grill is the first step: no earlier step exists, so the prompt
	// simply carries no handoff section (nothing to inherit).
	_, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "grill", SpawnN: 1})
	if err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	p, _ := sp.last()
	if strings.Contains(p.InitialPrompt, "Handoff from step") {
		t.Fatalf("first step must not carry a handoff\n%s", p.InitialPrompt)
	}
}

// TestSpawnStepGatePromptStatesVerdictBinding: the instruction half of
// the #106 fix — a gate step's prompt must state that a fail verdict
// REQUIRES outcome gate_failed (the driver's only rewind trigger) and
// that a fail verdict under completed is re-bound fail-closed; a
// non-gate step's prompt must not carry the gate paragraph.
func TestSpawnStepGatePromptStatesVerdictBinding(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	// verification declares artifacts/fix-summary.md as an input.
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "the fix summary")
	// verification is the extExample bundle's gate: true step.
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 2}); err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	p, _ := sp.last()
	prompt := p.InitialPrompt
	for _, want := range []string{
		"Gate verdict binding",         // the binding paragraph is present
		"this step is a GATE",          // the step knows it is a gate
		`outcome: "gate_failed"`,       // the required outcome spelling
		"only rewind trigger",          // why the outcome matters
		"re-binds it to `gate_failed`", // the fail-closed backstop
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("gate prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}
	// grill is no gate (fresh, first step): its prompt must not state
	// the gate duty.
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "grill", SpawnN: 3}); err != nil {
		t.Fatalf("SpawnStep grill: %v", err)
	}
	p, _ = sp.last()
	if strings.Contains(p.InitialPrompt, "Gate verdict binding") {
		t.Fatalf("non-gate prompt must not carry the gate paragraph\n%s", p.InitialPrompt)
	}
}

// ---------------------------------------------------------------------------
// stepRun: envelope collection.

func validEnvelope(wi, step string, n int64) []byte {
	res := &StepResult{
		ProtocolVersion: 1,
		ResultID:        "162bc0dd-9635-5323-b170-99044b484a69",
		WIID:            wi,
		StepID:          step,
		SpawnN:          n,
		Outcome:         OutcomeCompleted,
		Output:          json.RawMessage(`{"summary":"done"}`),
		Artifacts:       []ArtifactRef{},
	}
	blob, _ := json.MarshalIndent(res, "", "  ")
	return blob
}

func TestStepRunWaitCollectsAndArchivesEnvelope(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "s")
	run, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 5})
	if err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	// simulate the step process: deliver the envelope to the drop path
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/pending-results/5.json", string(validEnvelope("wi_sp", "verification", 5)))

	res, err := run.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Outcome != OutcomeCompleted || res.StepID != "verification" || res.SpawnN != 5 {
		t.Fatalf("collected envelope: %+v", res)
	}
	// archived at the R14 staging path, byte-faithful
	raw, err := os.ReadFile(filepath.Join(capDir, "artifacts", "steps", "verification", "s5", "result.json"))
	if err != nil {
		t.Fatalf("archived envelope missing: %v", err)
	}
	var archived StepResult
	if err := json.Unmarshal(raw, &archived); err != nil {
		t.Fatalf("archived envelope unparsable: %v", err)
	}
	if archived.ResultID != res.ResultID {
		t.Fatalf("archived result_id %q != %q", archived.ResultID, res.ResultID)
	}
	// the process was torn down after collection
	_, h := sp.last()
	if !h.terminated() {
		t.Fatal("handle must be terminated after envelope collection")
	}
	// the drop is CONSUMED (#97): a collected envelope must not sit in
	// pending-results for the next same-ordinal dispatch to re-collect
	if _, err := os.Stat(filepath.Join(capDir, capsule.DirRuntimeState, "pending-results", "5.json")); !os.IsNotExist(err) {
		t.Fatalf("drop file must be removed after collection, stat err=%v", err)
	}
}

func TestStepRunWaitFillsOmittedIdentity(t *testing.T) {
	ad, _, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "s")
	run, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 2})
	if err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	// a well-meaning process omitted step_id/wi_id/spawn_n
	blob := `{"protocol_version":1,"result_id":"33333333-3333-3333-3333-333333333333","outcome":"completed","output":{}}`
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/pending-results/2.json", blob)
	res, err := run.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.StepID != "verification" || res.WIID != "wi_sp" || res.SpawnN != 2 {
		t.Fatalf("identity fill: %+v", res)
	}
}

func TestStepRunWaitExitWithoutEnvelope(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "s")
	run, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 6})
	if err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	_, h := sp.last()
	h.setDone() // the process died before delivering anything
	_, err = run.Wait(context.Background())
	var ee *ExitedError
	if err == nil || !errors.As(err, &ee) {
		t.Fatalf("exit without envelope must fail with an ExitedError, got %v", err)
	}
	if ee.Step != "verification" || ee.SpawnN != 6 {
		t.Fatalf("exit error identity: %+v", ee)
	}
}

func TestStepRunWaitStaleFenceRejected(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "s")
	run, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 7})
	if err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	// a stale envelope (spawn 4) under this derivation's drop path
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/pending-results/7.json", string(validEnvelope("wi_sp", "verification", 4)))
	_, h := sp.last()
	h.setDone()
	_, err = run.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), ErrStaleSpawn.Error()) {
		t.Fatalf("stale-fenced envelope must fail as such, got %v", err)
	}
	// and it must NOT have been archived
	if _, serr := os.Stat(filepath.Join(capDir, "artifacts", "steps", "verification", "s7", "result.json")); serr == nil {
		t.Fatal("a rejected envelope must not be archived")
	}
}

func TestStepRunWaitTornWriteHeals(t *testing.T) {
	ad, _, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "s")
	run, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 8})
	if err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	drop := pendingResultPath(capDir, 8)
	// a write in flight: torn JSON first, the complete envelope after
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/pending-results/8.json", `{"protocol_version":1`)
	go func() {
		time.Sleep(8 * time.Millisecond)
		if err := os.WriteFile(drop, validEnvelope("wi_sp", "verification", 8), 0o644); err != nil {
			t.Errorf("rewrite: %v", err)
		}
	}()
	res, err := run.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.ResultID != "162bc0dd-9635-5323-b170-99044b484a69" {
		t.Fatalf("healed envelope: %+v", res)
	}
}

func TestStepRunWaitContextCancel(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "s")
	run, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 9})
	if err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(6 * time.Millisecond)
		cancel()
	}()
	if _, err := run.Wait(ctx); err == nil {
		t.Fatal("canceled Wait must fail")
	}
	_, h := sp.last()
	if h.terminated() {
		t.Fatal("Wait cancel must not tear the handle down (Run's error path owns that)")
	}
}

func TestSpawnAdapterConfigValidation(t *testing.T) {
	capDir := t.TempDir()
	if _, err := NewStepSpawnAdapter(StepSpawnAdapter{}); err == nil {
		t.Fatal("empty config must fail")
	}
	if _, err := NewStepSpawnAdapter(StepSpawnAdapter{WI: WIInfo{ID: "w", CapsuleDir: capDir}}); err == nil {
		t.Fatal("config without a spawner must fail")
	}
	// no bundle name/dir and no workflow: unresolvable
	if _, err := NewStepSpawnAdapter(StepSpawnAdapter{
		WI:      WIInfo{ID: "w", CapsuleDir: capDir},
		Spawner: &captureSpawner{},
	}); err == nil {
		t.Fatal("config without a bundle name/dir must fail")
	}
}

func hashOfFile(t *testing.T, capDir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(capDir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Journal seeding + torn-tail repair (astra r2 #2/#3 regressions).

// TestJournalMaxSeedsFromLedger: the ordinal ledger answers its max;
// absence answers zero (a fresh capsule), unparsable lines are skipped.
func TestJournalMaxSeedsFromLedger(t *testing.T) {
	dir := t.TempDir()
	if got, err := JournalMax(dir); got != 0 || err != nil {
		t.Fatalf("absent journal must answer (0, nil), got (%d, %v)", got, err)
	}
	writeCapsuleFile(t, dir, capsule.DirRuntimeState+"/spawn-journal.jsonl",
		"{\"spawn_n\":1}\n{\"spawn_n\":3}\n{\"spawn_n\":2}\n")
	if got, err := JournalMax(dir); got != 3 || err != nil {
		t.Fatalf("journal max must be 3, got (%d, %v)", got, err)
	}
}

// TestJournalMaxTornTailTolerated: a crash-torn tail (half a JSON line)
// is skipped by the loose reader — but a READABLE existing line before
// it still seeds the max.
func TestJournalMaxTornTailTolerated(t *testing.T) {
	dir := t.TempDir()
	writeCapsuleFile(t, dir, capsule.DirRuntimeState+"/spawn-journal.jsonl",
		"{\"spawn_n\":7}\n{\"spawn_n\":8, \"trans") // torn tail, no newline
	if got, err := JournalMax(dir); got != 7 || err != nil {
		t.Fatalf("torn tail must seed the last parsable ordinal 7, got (%d, %v)", got, err)
	}
}

// TestAppendSpawnJournalRepairsTornTail is the astra-r2 #2 regression:
// a torn tail must never GLUE onto the next record — the repair isolates
// the half-line as its own line and the appended record stays parsable,
// so the next JournalMax sees it and the relaunch does not re-derive
// the same ordinal.
func TestAppendSpawnJournalRepairsTornTail(t *testing.T) {
	ad, _, capDir := spawnEnv(t)
	jpath := capDir + "/" + capsule.DirRuntimeState + "/spawn-journal.jsonl"
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/spawn-journal.jsonl",
		"{\"spawn_n\":7}\n{\"spawn_n\":8, \"trans") // torn: no trailing newline

	p := spawn.Params{WorkItemID: "wi_sp", SpawnN: 9, TranscriptPath: "t"}
	if err := ad.appendSpawnJournal(p, "code", "s9", 4242, nowStampX()); err != nil {
		t.Fatalf("appendSpawnJournal: %v", err)
	}

	raw, err := os.ReadFile(jpath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("torn tail + repair + record must yield 3 lines, got %d: %q", len(lines), string(raw))
	}
	// line 1 must survive the repair UNTOUCHED (an offset-0 clobber would
	// eat its opening brace)
	if lines[0] != `{"spawn_n":7}` {
		t.Fatalf("the first record must survive the repair verbatim, got %q", lines[0])
	}
	// the appended record must be its OWN line (not glued onto the tail)
	var rec struct {
		SpawnN int64 `json:"spawn_n"`
	}
	if json.Unmarshal([]byte(lines[2]), &rec) != nil || rec.SpawnN != 9 {
		t.Fatalf("the appended record must be its own parsable line with spawn 9, got %q", lines[2])
	}
	// and the ledger now seeds past the crashed dispatch's ordinal
	if got, err := JournalMax(capDir); got != 9 || err != nil {
		t.Fatalf("after repair the ledger must seed 9, got (%d, %v)", got, err)
	}
}

func nowStampX() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// TestSpawnStepFreshRetryCarriesPreviousTranscriptAsReference pins the
// 2nd+ retry shape (v2-converge 2.3): a fresh conversation — no resume —
// with the previous transcript, when readable, handed over as read-only
// reference in the prompt; a missing/empty transcript is simply omitted.
func TestSpawnStepFreshRetryCarriesPreviousTranscriptAsReference(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "the fix summary")
	prev := filepath.Join(capDir, "session", "prev.jsonl")
	writeCapsuleFile(t, capDir, "session/prev.jsonl", `{"t":"turn"}`+"\n")
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/spawn-journal.jsonl",
		`{"spawn_n":3,"step_id":"verification","transcript":"`+prev+`","work_item":"wi_sp"}`+"\n")

	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 4, Fresh: true}); err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	p, _ := sp.last()
	if p.Resume {
		t.Fatal("a fresh retry must not resume the old conversation")
	}
	if p.TranscriptPath == prev {
		t.Fatal("a fresh retry declares its own new transcript")
	}
	if !strings.Contains(p.InitialPrompt, "Previous attempt (read-only reference)") || !strings.Contains(p.InitialPrompt, prev) {
		t.Fatalf("the prompt must carry the previous transcript as reference:\n%s", p.InitialPrompt)
	}

	// an unreadable previous transcript is omitted, never an error
	if err := os.Remove(prev); err != nil {
		t.Fatal(err)
	}
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 5, Fresh: true}); err != nil {
		t.Fatalf("SpawnStep (no transcript): %v", err)
	}
	p2, _ := sp.last()
	if strings.Contains(p2.InitialPrompt, "read-only reference") {
		t.Fatalf("no reference may be named when the transcript is gone:\n%s", p2.InitialPrompt)
	}
}

// TestSpawnStepNeverResumesOrReferencesAnotherStepsTranscript: with a
// journal whose highest ordinal belongs to a different step, a resume
// of "verification" attaches its OWN transcript and a fresh dispatch
// references only its own — never the foreign one.
func TestSpawnStepNeverResumesOrReferencesAnotherStepsTranscript(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "the fix summary")
	own := filepath.Join(capDir, "session", "own.jsonl")
	foreign := filepath.Join(capDir, "session", "foreign.jsonl")
	writeCapsuleFile(t, capDir, "session/own.jsonl", `{"t":"turn"}`+"\n")
	writeCapsuleFile(t, capDir, "session/foreign.jsonl", `{"t":"turn"}`+"\n")
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/spawn-journal.jsonl",
		`{"spawn_n":1,"step_id":"verification","transcript":"`+own+`"}`+"\n"+
			`{"spawn_n":2,"step_id":"other","transcript":"`+foreign+`"}`+"\n")

	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 3, Resume: true}); err != nil {
		t.Fatalf("SpawnStep resume: %v", err)
	}
	p, _ := sp.last()
	if !p.Resume || p.TranscriptPath != own {
		t.Fatalf("resume must use the step's own transcript: resume=%v transcript=%q", p.Resume, p.TranscriptPath)
	}
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 4, Fresh: true}); err != nil {
		t.Fatalf("SpawnStep fresh: %v", err)
	}
	p, _ = sp.last()
	if p.Resume || p.TranscriptPath == foreign || strings.Contains(p.InitialPrompt, foreign) || !strings.Contains(p.InitialPrompt, own) {
		t.Fatalf("fresh dispatch must reference only its own transcript: %+v\n%s", p, p.InitialPrompt)
	}
}

// TestSpawnStepResumeWithoutReadableTranscriptDegradesToFresh: a resume
// whose declared transcript was never written (the dispatch died before
// the harness created its session) starts the conversation fresh instead
// of handing the harness a path it cannot resume.
func TestSpawnStepResumeWithoutReadableTranscriptDegradesToFresh(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "s")
	writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/spawn-journal.jsonl",
		`{"spawn_n":3,"step_id":"verification","transcript":"/does/not/exist.jsonl","work_item":"wi_sp"}`+"\n")
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 4, Resume: true}); err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	p, _ := sp.last()
	if p.Resume {
		t.Fatalf("an unreadable transcript cannot be resumed: %+v", p)
	}
}

// TestSpawnStepResumeWithoutJournalLineDegradesToFresh: a resume for a
// step with no spawn-journal line of its own (other steps' lines, or no
// journal at all) is a plain fresh dispatch, not an error.
func TestSpawnStepResumeWithoutJournalLineDegradesToFresh(t *testing.T) {
	for name, journal := range map[string]string{
		"other-step-only": `{"spawn_n":3,"step_id":"other","transcript":"/t/o.jsonl"}` + "\n" + `{"spawn_n":4,"transcript":"/t/untagged.jsonl"}` + "\n",
		"no-journal":      "",
	} {
		t.Run(name, func(t *testing.T) {
			ad, sp, capDir := spawnEnv(t)
			writeCapsuleFile(t, capDir, "artifacts/fix-summary.md", "s")
			if journal != "" {
				writeCapsuleFile(t, capDir, capsule.DirRuntimeState+"/spawn-journal.jsonl", journal)
			}
			if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 4, Resume: true}); err != nil {
				t.Fatalf("resume without a journal line must degrade, not fail: %v", err)
			}
			p, _ := sp.last()
			if p.Resume || strings.Contains(p.TranscriptPath, "/t/") {
				t.Fatalf("expected a fresh dispatch: %+v", p)
			}
		})
	}
}

// TestSpawnStepWiresHarnessTranslatorOntoTheLink: the step process's
// stdout records reach the dispatch's tracker through the harness
// translator, and the process exit (code + stderr tail) is announced.
func TestSpawnStepWiresHarnessTranslatorOntoTheLink(t *testing.T) {
	capDir := t.TempDir()
	writeCapsuleFile(t, capDir, "workflows/quick-fix-x/workflow.md", "---\n"+strings.TrimPrefix(extExample, "\n")+"---\n")
	writeCapsuleFile(t, capDir, "workflows/quick-fix-x/steps/grill.md", "grill")
	writeCapsuleFile(t, capDir, capsule.SessionJSON, `{"harness":"pi","session_id":"ab12cd34"}`)
	sp := &captureSpawner{}
	link := supervise.NewLink("wi_sp")
	ad, err := NewStepSpawnAdapter(StepSpawnAdapter{
		WI:      WIInfo{ID: "wi_sp", CapsuleDir: capDir, BundleName: "quick-fix-x"},
		Spawner: sp, Link: link, PollInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "grill", SpawnN: 1})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := sp.last()
	if p.OnRecord == nil || p.OnExit == nil {
		t.Fatal("the dispatch must hand the spawner its record tap and exit callback")
	}
	meta := run.(MetaRun).Meta()
	if meta.Harness != "pi" || meta.Granularity != supervise.GranularityToken || meta.PID != 4242 {
		t.Fatalf("run meta %+v", meta)
	}
	tr := link.Tracker(1)
	p.OnRecord(`{"type":"extension_ui_request"}`)
	if s := tr.Snapshot(time.Now()); s.WorkSignals != 0 || s.NoiseCount != 1 {
		t.Fatalf("UI noise must not count as work: %+v", s)
	}
	p.OnRecord(`{"type":"turn_start"}`)
	p.OnRecord(`{"type":"agent_end","messages":[{"role":"assistant","stopReason":"error","errorMessage":"401 unauthorized"}]}`)
	p.OnExit(spawn.ExitInfo{Code: 1, StderrTail: "fatal: auth failed"})
	s := tr.Snapshot(time.Now())
	if !s.TurnError || s.Exit == nil || s.Exit.Code != 1 {
		t.Fatalf("the tracker must hold the error turn and the exit: %+v", s)
	}
	// the driver's classification reads both: the last signal was an error turn
	_, h := sp.last()
	h.setDone()
	_, werr := run.Wait(context.Background())
	var ee *ExitedError
	if !errors.As(werr, &ee) || !ee.TurnError || !strings.Contains(ee.LastError, "401") {
		t.Fatalf("exit error %+v, want the error turn + its text", werr)
	}
	if cause, _, _ := classifyWaitFailure(werr); cause != FailExitModel {
		t.Fatalf("an error-turn exit is model-class, got %q", cause)
	}
}
