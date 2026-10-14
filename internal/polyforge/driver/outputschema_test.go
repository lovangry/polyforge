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
)

// shippedStepDef reads one shipped step definition file.
func shippedStepDef(t *testing.T, bundle, step string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "workflows", bundle, "steps", step+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Every shipped step definition that declares an output schema compiles:
// a bundle whose contract cannot be checked must fail here, not validate
// silently against nothing at run time.
func TestShippedStepOutputSchemasCompile(t *testing.T) {
	defs, err := filepath.Glob(filepath.Join("..", "..", "..", "workflows", "*", "steps", "*.md"))
	if err != nil || len(defs) == 0 {
		t.Fatalf("no shipped step definitions found: %v", err)
	}
	for _, def := range defs {
		bundleDir := filepath.Dir(filepath.Dir(def))
		step := strings.TrimSuffix(filepath.Base(def), ".md")
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
	writeCapsuleFile(t, capDir, "workflows/quick-fix-x/steps/verification.md",
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
// with the SHIPPED quick-fix verification definition in the bundle.
func verificationEnv(t *testing.T) (*Driver, *fakeSpawner, string) {
	t.Helper()
	d, sp, execPath := runEnv(t, spec("fix"), gateSpec("verification", "fix"))
	writeCapsuleFile(t, d.wi.CapsuleDir, "workflows/b/steps/verification.md", shippedStepDef(t, "quick-fix", "verification"))
	return d, sp, execPath
}

func verificationDone(output string) fakeEntry {
	e := done(output)
	e.res.CommandEvidence = goodEvidence()
	return e
}

// H3: a verification "pass" carrying a failed check contradicts the
// declared schema (its oneOf). The driver no longer accepts it: the
// envelope is rejected in-process with the schema error as feedback, and
// the rewrite is accepted.
func TestRunRejectsOutputViolatingDeclaredSchema(t *testing.T) {
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
	exec := readExec(t, execPath)
	if exec.WorkflowStatus != StatusCompleted || exec.Steps["verification"].Output["checks"] == nil {
		t.Fatalf("the conforming rewrite must be accepted: %s %+v", exec.WorkflowStatus, exec.Steps["verification"])
	}
	var rejected bool
	for _, e := range d.log.(*eventLog).entries {
		rejected = rejected || e.kind == EvEnvelopeRejected
	}
	if !rejected {
		t.Fatal("the schema rejection must ride the envelope_rejected lane")
	}
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
			{"name":"unit","status":"pre_existing","detail":"TestFlaky fails on base abc123 too"}],
			"pre_existing_failures":["TestFlaky"]}`),
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
	if string(blob) != `["TestFlaky"]` {
		t.Fatalf("the pre-existing failures must stay reported: %s", blob)
	}
}
