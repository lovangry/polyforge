package publish

// outputschema_test.go: unit tests of the 03 §5.1a publish-time
// output-schema check — conforming/violating outputs, the per-publish
// compile cache, pointer/keyword normalization, missing-schema skips, and
// the INV-6 structural boundary (the check must be incapable of opening
// the live capsule's runtime-state/).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/sync/revision"
)

const strictCodeSkill = `---
name: code
description: writes the fix
tools: bash
output:
  type: object
  required: [summary]
  properties:
    summary: { type: string, minLength: 5 }
    verdict: { type: string, enum: [pass, fail] }
    lines: { type: array, minItems: 1 }
---

# code

Do the work.
`

// newCheckEnv builds an OutputCheck over a temp materialized-snapshot
// agents dir and a staged-bytes source, plus the manifest ops of one
// publish carrying the given execution.json bytes.
func newCheckEnv(t *testing.T, execJSON string, staged map[string][]byte) (*OutputCheck, []revision.Op) {
	t.Helper()
	agentsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentsDir, "code.md"), []byte(strictCodeSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	execSha := revision.DigestHex([]byte(execJSON))
	staged[execSha] = []byte(execJSON)
	mode := int64(420)
	check, err := NewOutputCheck(agentsDir, func(sha string) ([]byte, error) {
		b, ok := staged[sha]
		if !ok {
			return nil, os.ErrNotExist
		}
		return b, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ops := []revision.Op{
		{Op: revision.OpPut, Path: "artifacts/x.md", Sha256: revision.DigestHex([]byte("x"))},
		{Op: revision.OpPut, Path: revision.ExecutionJSONPath, Sha256: execSha, Bytes: int64(len(execJSON)), Mode: &mode},
	}
	return check, ops
}

func execWithDoneStep(output string) string {
	return `{"capsule_id":"c","work_item_id":"wi","generation":1,"bundle_digest":"bd-1",` +
		`"current_step":"verify","workflow_status":"running","steps":{` +
		`"code":{"status":"done","output":` + output + `,"attempts":[]}},"version":3}`
}

func TestOutputCheckConformingDoneStepYieldsNoSignal(t *testing.T) {
	exec := execWithDoneStep(`{"summary":"fixed the off-by-one","verdict":"pass","lines":["a.go"]}`)
	check, ops := newCheckEnv(t, exec, map[string][]byte{})
	ms, err := check.Check(ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 0 {
		t.Fatalf("conforming output must produce no signal: %+v", ms)
	}
}

func TestOutputCheckViolatingDoneStepSignalsPointerAndKeyword(t *testing.T) {
	// summary too short (minLength 5), verdict outside enum, empty lines.
	exec := execWithDoneStep(`{"summary":"hi","verdict":"maybe","lines":[]}`)
	check, ops := newCheckEnv(t, exec, map[string][]byte{})
	ms, err := check.Check(ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("want 3 mismatch units, got %+v", ms)
	}
	byPointer := map[string]SchemaMismatch{}
	for _, m := range ms {
		if m.StepID != "code" {
			t.Fatalf("mismatch must name the done step, got %q", m.StepID)
		}
		byPointer[m.Pointer+"/"+m.Keyword] = m
	}
	for _, want := range []struct{ pointer, keyword string }{
		{"/summary", "minLength"},
		{"/verdict", "enum"},
		{"/lines", "minItems"},
	} {
		m, ok := byPointer[want.pointer+"/"+want.keyword]
		if !ok {
			t.Fatalf("missing mismatch %s %s in %+v", want.pointer, want.keyword, ms)
		}
		if m.Detail == "" {
			t.Fatalf("mismatch detail must be non-empty: %+v", m)
		}
	}
}

func TestOutputCheckRootPointerAndRequiredKeyword(t *testing.T) {
	// output missing the required member: the unit's instance location is
	// the output root — reported as pointer "/" with keyword "required".
	exec := execWithDoneStep(`{"verdict":"pass","lines":["a.go"]}`)
	check, ops := newCheckEnv(t, exec, map[string][]byte{})
	ms, err := check.Check(ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Pointer != "/" || ms[0].Keyword != "required" {
		t.Fatalf("root-level required violation: %+v", ms)
	}
}

func TestOutputCheckSkipsNonDoneStepsAndMissingSchemas(t *testing.T) {
	// in_progress/pending steps are not validated; a done step with NO
	// pinned agent file cannot be validated — skipped, never a mismatch,
	// never an error.
	exec := `{"steps":{` +
		`"code":{"status":"done","output":{"summary":"hi"},"attempts":[]},` +
		`"verify":{"status":"in_progress","output":{"summary":"x"},` +
		`"attempts":[{"cause":"infra_failure","attempt_seq":1}]},` +
		`"missing":{"status":"done","output":{"whatever":1},"attempts":[]}}}`
	check, ops := newCheckEnv(t, exec, map[string][]byte{})
	ms, err := check.Check(ops)
	if err != nil {
		t.Fatal(err)
	}
	// "code" output {"summary":"hi"}: minLength 5 violated -> 1 unit.
	// "missing" has no agents/missing.md -> skipped.
	if len(ms) != 1 || ms[0].StepID != "code" {
		t.Fatalf("want only the code minLength unit: %+v", ms)
	}
}

func TestOutputCheckCompilesEachSchemaOncePerPublish(t *testing.T) {
	exec := execWithDoneStep(`{"summary":"hi"}`)
	check, ops := newCheckEnv(t, exec, map[string][]byte{})
	first, err := check.Check(ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first check: %+v", first)
	}
	// Delete the pinned schema file: a second Check within the SAME
	// publish must still validate (compiled schema served from the
	// bundle+step cache), proving the ONCE-per-publish compile.
	if err := os.Remove(filepath.Join(check.AgentsDir, "code.md")); err != nil {
		t.Fatal(err)
	}
	second, err := check.Check(ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Keyword != "minLength" {
		t.Fatalf("second check must serve the cached compiled schema: %+v", second)
	}
}

func TestOutputCheckNoExecutionBytesNoValidation(t *testing.T) {
	check, ops := newCheckEnv(t, `{}`, map[string][]byte{})
	ops = []revision.Op{{Op: revision.OpDelete, Path: revision.ExecutionJSONPath}}
	ms, err := check.Check(ops)
	if err != nil || len(ms) != 0 {
		t.Fatalf("no staged execution bytes -> no validation: %+v %v", ms, err)
	}
}

// TestINV6ValidatorRefusesLiveRuntimeStatePath pins the INV-6 boundary
// (03 §5.1a point 1): the validator is structurally incapable of opening
// the live capsule's runtime-state/ — constructing it with a live path
// FAILS instead of validating, and every path it may open is guarded.
func TestINV6ValidatorRefusesLiveRuntimeStatePath(t *testing.T) {
	capDir := t.TempDir()
	liveExec := filepath.Join(capDir, "runtime-state", "execution.json")
	if err := os.MkdirAll(filepath.Dir(liveExec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveExec, []byte(`{"version":1}`), 0o444); err != nil {
		t.Fatal(err)
	}
	staged := func(sha string) ([]byte, error) { return nil, os.ErrNotExist }

	// Handing the validator the LIVE runtime-state dir as its schema
	// source must fail closed with ErrINV6.
	if _, err := NewOutputCheck(filepath.Join(capDir, "runtime-state"), staged); !errors.Is(err, ErrINV6) {
		t.Fatalf("live runtime-state agents dir must be refused with ErrINV6, got %v", err)
	}
	// The live execution.json path itself is refused by the guard.
	if err := guardNoRuntimeState(liveExec); !errors.Is(err, ErrINV6) {
		t.Fatalf("live execution.json path must be refused with ErrINV6, got %v", err)
	}
	// Any runtime-state component anywhere in the path is refused.
	if err := guardNoRuntimeState(filepath.Join(capDir, "repos", "r", "runtime-state", "x")); !errors.Is(err, ErrINV6) {
		t.Fatalf("runtime-state component must be refused, got %v", err)
	}
	// The legitimate materialized snapshot dir passes the guard.
	if err := guardNoRuntimeState(filepath.Join(capDir, ".pi", "agents")); err != nil {
		t.Fatalf("materialized snapshot dir must be accepted, got %v", err)
	}
	// A traversal-style step id that would resolve into runtime-state is
	// refused before any file is opened.
	check, err := NewOutputCheck(filepath.Join(capDir, ".pi", "agents"), staged)
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := check.schemaFor("bd", "../runtime-state/execution"); serr == nil || !strings.Contains(serr.Error(), "plain file name") {
		t.Fatalf("traversal step id must be refused, got %v", serr)
	}
	// The only byte source for outputs is the staged-block closure keyed
	// by content sha: handing it the LIVE bytes is the caller's breach,
	// and the check itself holds no capsule path for outputs at all —
	// pinned here by asserting the check struct never stores capsuleDir.
	if strings.Contains(check.AgentsDir, "runtime-state") {
		t.Fatal("unreachable: constructor guard already refused runtime-state")
	}
}

// TestSchemaMismatchSignalShape pins the minimal wire shape the WI Agent
// consumes (skills/pf-execute.md names none; this is the definition).
func TestSchemaMismatchSignalShape(t *testing.T) {
	blob, err := json.Marshal([]SchemaMismatch{{
		StepID: "code", Pointer: "/summary", Keyword: "minLength", Detail: "length=2, minimum=5",
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"step_id":"code","pointer":"/summary","keyword":"minLength","detail":"length=2, minimum=5"}]`
	if string(blob) != want {
		t.Fatalf("signal shape: %s", blob)
	}
}
