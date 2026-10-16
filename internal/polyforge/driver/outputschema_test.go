package driver

// outputschema_test.go: the declared step output schema (polyforge#164
// H3) — shown in the prompt, enforced on completed results through the
// envelope-rejection retry lane — and verification's pre-existing
// failure lane (S6) riding the same shipped schema.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/workflow"
)

// shippedStepDef reads one shipped step definition file.
func shippedStepDef(t *testing.T, bundle, step string) string {
	t.Helper()
	raw, err := os.ReadFile(workflow.StepDefinitionPath(filepath.Join("..", "..", "..", "workflows", bundle), step))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Every shipped step definition that declares an output schema compiles:
// a bundle whose contract cannot be checked must fail here, not validate
// silently against nothing at run time.
func TestShippedStepOutputSchemasCompile(t *testing.T) {
	for _, bs := range shippedBundleSteps(t) {
		bundleDir, step := bs[0], bs[1]
		def := bs[0] + "/" + step
		s, err := loadStepOutputSchema(bundleDir, step)
		if err != nil {
			t.Fatalf("%s: %v", def, err)
		}
		if s == nil || s.Schema == nil || s.Text == "" {
			t.Fatalf("%s declares no output schema", def)
		}
	}
}

// The prompt carries the declared schema ahead of the step body (the
// body says "per the schema above"); the rest of the frontmatter stays
// stripped.
func TestSpawnStepPromptShowsDeclaredOutputSchema(t *testing.T) {
	ad, sp, capDir := spawnEnv(t)
	writeCapsuleFile(t, capDir, "workflows/autopilot-x/steps/verification.md",
		"---\ntools: [go]\noutput:\n  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n---\n\nverification: build, lint, test")
	if _, err := ad.SpawnStep(context.Background(), StepSpawn{StepID: "verification", SpawnN: 1}); err != nil {
		t.Fatalf("SpawnStep: %v", err)
	}
	p, _ := sp.last()
	prompt := p.InitialPrompt
	i, j := strings.Index(prompt, "# Output schema"), strings.Index(prompt, "# Step definition")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("the output schema must precede the step definition\n%s", prompt)
	}
	if !strings.Contains(prompt[i:j], "required:") || !strings.Contains(prompt[i:j], "enum: [pass, fail]") {
		t.Fatalf("the schema section must carry the declared schema\n%s", prompt[i:j])
	}
	if strings.Contains(prompt, "tools: [go]") {
		t.Fatalf("the non-schema frontmatter stays stripped\n%s", prompt)
	}
}

// verificationEnv runs fix -> verification (a gate rewinding to fix)
// with the SHIPPED autopilot verification definition in the bundle.
func verificationEnv(t *testing.T) (*Driver, *fakeSpawner, string) {
	t.Helper()
	d, sp, execPath := runEnv(t, spec("fix"), gateSpec("verification", "fix"))
	writeCapsuleFile(t, d.wi.CapsuleDir, "workflows/b/steps/verification.md", shippedStepDef(t, "autopilot", "verification"))
	return d, sp, execPath
}

func verificationDone(output string) fakeEntry {
	e := done(output)
	e.res.CommandEvidence = goodEvidence()
	return e
}

// H3, record-only (v2.5.0): a verification "pass" carrying a failed
// check contradicts the declared schema (its oneOf). The result is
// accepted, and the violation is a durable output_schema_violation event.
func TestRunRecordsOutputViolatingDeclaredSchema(t *testing.T) {
	d, sp, execPath := verificationEnv(t)
	sp.script = []fakeEntry{
		done(`{}`), // fix
		verificationDone(`{"verdict":"pass","checks":[{"name":"unit","status":"fail"}]}`),
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sp.dispatched(); len(got) != 2 {
		t.Fatalf("record-only mode must not re-dispatch, got %v", got)
	}
	if exec := readExec(t, execPath); exec.WorkflowStatus != StatusCompleted {
		t.Fatalf("record-only mode accepts the result: %s", exec.WorkflowStatus)
	}
	var violation map[string]any
	for _, e := range d.log.(*eventLog).entries {
		if e.kind == EvEnvelopeRejected {
			t.Fatalf("record-only mode must not reject: %+v", e)
		}
		if e.kind == EvOutputSchemaViolation {
			violation = e.body
		}
	}
	if violation == nil || violation["step"] != "verification" || !strings.Contains(violation["schema"].(string), "verification.md") ||
		!strings.Contains(violation["errors"].(string), "declared output schema") {
		t.Fatalf("the violation must be recorded with step, schema path and errors: %+v", violation)
	}
}

// H3, enforcing (the one switch flipped): the same output is rejected
// in-process with the schema error as feedback, and the rewrite accepted.
func TestRunEnforcedSchemaRejectsViolation(t *testing.T) {
	enforceOutputSchemas = true
	defer func() { enforceOutputSchemas = false }()
	d, sp, execPath := verificationEnv(t)
	sp.script = []fakeEntry{
		done(`{}`), // fix
		verificationDone(`{"verdict":"pass","checks":[{"name":"unit","status":"fail"}]}`),
		verificationDone(`{"verdict":"pass","checks":[{"name":"unit","status":"pass"}]}`),
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sp.dispatched(); len(got) != 3 {
		t.Fatalf("the contradictory pass must be re-dispatched once (no rewind), got %v", got)
	}
	if fb := sp.spawnAt(2).PriorRejection; !strings.Contains(fb, "declared output schema") {
		t.Fatalf("feedback must name the schema violation: %q", fb)
	}
	if exec := readExec(t, execPath); exec.WorkflowStatus != StatusCompleted {
		t.Fatalf("the conforming rewrite must be accepted: %s", exec.WorkflowStatus)
	}
}

// null on an optional property means absent: a pass with
// "fail_reason": null conforms.
func TestOutputSchemaTreatsNullAsAbsent(t *testing.T) {
	s, err := loadStepOutputSchema(filepath.Join("..", "..", "..", "workflows", "autopilot"), "verification")
	if err != nil || s == nil {
		t.Fatal(err)
	}
	if err := checkOutputSchema(s.Schema, json.RawMessage(`{"verdict":"pass","checks":[{"name":"b","status":"pass","detail":null}],"fail_reason":null}`)); err != nil {
		t.Fatalf("null optional properties must validate as absent: %v", err)
	}
}

// N3: a declared schema that does not load is loud — a durable event
// (and stderr), never a silent nil.
func TestUnloadableOutputSchemaIsRecorded(t *testing.T) {
	d, sp, _ := runEnv(t, spec("fix"))
	writeCapsuleFile(t, d.wi.CapsuleDir, "workflows/b/steps/fix.md", "---\noutput:\n  type: object\n  required: 7\n---\nfix")
	sp.script = []fakeEntry{done(`{}`)}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, e := range d.log.(*eventLog).entries {
		if e.kind == EvOutputSchemaUnloadable && e.body["step"] == "fix" {
			return
		}
	}
	t.Fatal("an unloadable schema must be recorded as output_schema_unloadable")
}

// S6: a test that also fails on the base commit is reported as
// pre_existing; the verification passes, nothing rewinds to fix, and the
// failures stay listed in the accepted output.
func TestRunVerificationPreExistingFailureDoesNotRewind(t *testing.T) {
	d, sp, execPath := verificationEnv(t)
	sp.script = []fakeEntry{
		done(`{}`), // fix
		verificationDone(`{"verdict":"pass","checks":[
			{"name":"build","status":"pass"},
			{"name":"unit","status":"pre_existing","detail":"TestFlaky fails on base abc1234 too"}],
			"pre_existing_failures":["internal/x/flaky_test.go: TestFlaky"]}`),
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sp.dispatched(); len(got) != 2 {
		t.Fatalf("a pre-existing failure must neither rewind nor be rejected, got %v", got)
	}
	exec := readExec(t, execPath)
	if exec.WorkflowStatus != StatusCompleted || exec.Steps["fix"].Status != capsule.StepDone {
		t.Fatalf("workflow must complete without a rewind: %s", exec.WorkflowStatus)
	}
	blob, _ := json.Marshal(exec.Steps["verification"].Output["pre_existing_failures"])
	if string(blob) != `["internal/x/flaky_test.go: TestFlaky"]` {
		t.Fatalf("the pre-existing failures must stay reported: %s", blob)
	}
}

// deliverSample is the known-good output of the deliver step every
// delivery bundle ships (one shared step document): one repo pushed and
// merged under the merge rule, one with nothing to deliver.
const deliverSample = `{"verdict":"pass","repos":[{"name":"app","default_branch":"main","branch":"polyforge/app-7","pr_url":"https://github.com/o/app/pull/9","merged":true,"merge_commit":"def5678","llm_resolved_conflict":false,"tests_passed":true},{"name":"docs","skipped_reason":"no commits beyond the claim baseline"}],"delivery_note":"app: polyforge/app-7 merged as def5678 (PR #9); docs: nothing to deliver"}`

// shippedSamples is one known-good completed output per shipped step,
// written from each step document's own instructions (the shape a
// conforming agent emits; `null` on an optional property included where
// agents write it). Every shipped step must have one.
var shippedSamples = map[string]string{
	"bug-fix/repro":             `{"repro_path":"artifacts/repro.md","reproduces":true,"test_files":["internal/x/x_test.go"],"summary":"reproduced"}`,
	"bug-fix/root-cause":        `{"verdict":"pass","root_cause":"nil map write","evidence":[{"kind":"code","ref":"internal/x/x.go:42","detail":"write before init"}],"fail_reason":null}`,
	"bug-fix/fix":               `{"commits":[{"sha":"abc1234","message":"fix: init map"}],"build_status":"green","root_cause_addressed":true,"regression_test":"internal/x/x_test.go","summary":"init before write"}`,
	"bug-fix/verification":      `{"verdict":"pass","checks":[{"name":"build","status":"pass"},{"name":"repro","status":"pass"}],"fail_reason":null,"test_bar":[{"repo":"app","command":"go build ./..."},{"repo":"app","command":"go test ./..."}]}`,
	"bug-fix/review":            `{"verdict":"pass","findings":[{"severity":"non-blocking","detail":"nit"}],"fail_reason":null}`,
	"chore/plan":                `{"plan_path":"artifacts/plan.md","step_ids":["s1"],"touched_files":[{"path":"a.go","intent":"write"}]}`,
	"chore/code":                `{"commits":[{"sha":"abc1234","message":"chore: drop dead code"}],"build_status":"green"}`,
	"chore/review":              `{"verdict":"pass","findings":[]}`,
	"chore/verification":        `{"verdict":"pass","checks":[{"name":"build","status":"pass"},{"name":"unit","status":"pre_existing","detail":"TestFlaky fails on base abc1234 too"}],"pre_existing_failures":["internal/x/flaky_test.go: TestFlaky"],"test_bar":[{"repo":"app","command":"go build ./..."},{"repo":"app","command":"go test ./..."}]}`,
	"feature-full/grill":        `{"qa_log":"artifacts/grill-qa.md","ready_for_spec":true}`,
	"feature-full/spec":         `{"spec_path":"artifacts/spec.md","requirement_titles":["R1"],"non_goals":[]}`,
	"feature-full/grill-review": `{"verdict":"pass","qa_log":"artifacts/grill-qa.md","fail_reason":null}`,
	"feature-full/plan":         `{"plan_path":"artifacts/plan.md","step_ids":["s1","s2"],"touched_files":[{"path":"a.go","intent":"write"},{"path":"b.go","intent":"read"}]}`,
	"feature-full/e2e-author":   `{"scenarios_path":"artifacts/e2e-scenarios.md","scenario_titles":["A","B"],"test_files":["test/a_test.go"],"baseline":[{"title":"A","status":"fail","evidence":"want 1 got 0"},{"title":"B","status":"fail","evidence":"404"}],"already_satisfied":["C"],"added_test_lines":512,"size_justification":"each scenario drives its own daemon lifecycle"}`,
	"feature-full/code":         `{"commits":[{"step_id":"s1","sha":"abc1234","message":"feat: s1"}],"build_status":"green","summary":"done"}`,
	"feature-full/review":       `{"verdict":"pass","findings":[{"severity":"non-blocking","detail":"nit"}]}`,
	"feature-full/verification": `{"verdict":"pass","checks":[{"name":"build","status":"pass"},{"name":"tests","status":"skipped","detail":"no tests"},{"name":"authored-scenarios","status":"pass"}],"authored_titles":["A","B"],"scenarios":[{"title":"A","status":"pass"},{"title":"B","status":"skipped","detail":"needs GPU"}],"test_bar":[{"repo":"app","command":"go build ./..."},{"repo":"app","command":"go test ./..."},{"repo":"app","command":"npm run test:e2e -- test/e2e/a.spec.ts"}]}`,
	"autopilot/grill":           `{"qa_log":"artifacts/grill-qa.md","memo_path":"artifacts/fix-memo.md","scope_points":["fix the typo"],"non_goals":[]}`,
	"autopilot/fix":             `{"commits":[{"sha":"abc1234","message":"fix: typo"}],"build_status":"green"}`,
	"autopilot/verification":    `{"verdict":"pass","checks":[{"name":"build","status":"pass"}],"test_bar":[{"repo":"app","command":"go build ./..."},{"repo":"app","command":"go test ./..."}]}`,
	"bug-fix/deliver":           deliverSample,
	"chore/deliver":             deliverSample,
	"feature-full/deliver":      deliverSample,
	"autopilot/deliver":         deliverSample,
	"release/release":           `{"verdict":"pass","version":"v2.5.0","commit":"abc1234","notes":"docs/release-notes/v2.5.0.md","artifacts_published":["pf-linux-amd64"],"fail_reason":null}`,
	"release/verify":            `{"verdict":"pass","version":"v2.5.0","commit":"abc1234","checks":[{"name":"release.sh --verify","status":"pass"}]}`,
	"release/deploy":            `{"verdict":"defer","state":"none"}`,
}

// Every shipped step's known-good output validates against its declared
// schema (the compile test proves the schema loads; this one proves it
// accepts what the step document asks for).
func TestShippedStepSchemasAcceptKnownGoodOutputs(t *testing.T) {
	for _, bs := range shippedBundleSteps(t) {
		bundleDir, step := bs[0], bs[1]
		key := filepath.Base(bundleDir) + "/" + step
		sample, ok := shippedSamples[key]
		if !ok {
			t.Fatalf("%s has no known-good sample output", key)
		}
		s, err := loadStepOutputSchema(bundleDir, step)
		if err != nil || s == nil {
			t.Fatalf("%s: %v", key, err)
		}
		if err := checkOutputSchema(s.Schema, json.RawMessage(sample)); err != nil {
			t.Fatalf("%s: known-good output rejected: %v", key, err)
		}
	}
}

// TestFeatureFullMergedStepSchemasRejectContradictions (polyforge#165):
// e2e-author's baseline admits only proven reds, and feature-full's
// verification (which absorbed e2e-run) cannot pass over a red authored
// scenario or omit the per-scenario results.
func TestFeatureFullMergedStepSchemasRejectContradictions(t *testing.T) {
	bundle := filepath.Join("..", "..", "..", "workflows", "feature-full")
	for step, bad := range map[string]string{
		"e2e-author":   `{"scenarios_path":"artifacts/e2e-scenarios.md","scenario_titles":["A"],"baseline":[{"title":"A","status":"pass","evidence":"ok"}]}`,
		"verification": `{"verdict":"pass","checks":[{"name":"build","status":"pass"}],"authored_titles":["A"],"scenarios":[{"title":"A","status":"fail"}]}`,
	} {
		s, err := loadStepOutputSchema(bundle, step)
		if err != nil || s == nil {
			t.Fatalf("%s: %v", step, err)
		}
		if err := checkOutputSchema(s.Schema, json.RawMessage(bad)); err == nil {
			t.Fatalf("%s: contradictory output accepted: %s", step, bad)
		}
	}
	s, _ := loadStepOutputSchema(bundle, "verification")
	if err := checkOutputSchema(s.Schema, json.RawMessage(`{"verdict":"pass","checks":[{"name":"build","status":"pass"}]}`)); err == nil {
		t.Fatal("verification without authored_titles/scenarios accepted")
	}
}
