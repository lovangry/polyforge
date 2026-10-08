package driver

// result_test.go: the R14 staging gates over the R13 envelope. The
// pinned vectors double as the contract documentation: fencing,
// once-only idempotency, declared-output enforcement, and hash-pinned
// artifact verification (including the capsule-jail guarantees — no
// absolute paths, no .. escapes, no symlink exits).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeArtifact materializes one artifact file under root and answers
// its verified manifest entry.
func writeArtifact(t *testing.T, root, rel, content string) ArtifactRef {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, n, err := HashFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	return ArtifactRef{Path: rel, SHA256: sum, Bytes: n}
}

// fixture is one assembled validation scene: a capsule root, a spec and
// a matching completed result with an output object, one declared
// artifact and (optionally) a handoff.
type fixture struct {
	root string
	spec ResultSpec
	res  *StepResult
	art  ArtifactRef
	hand ArtifactRef
}

func newFixture(t *testing.T, handoff bool) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{root: root}
	f.art = writeArtifact(t, root, "artifacts/fix-summary.md", "summary body\n")
	f.spec = ResultSpec{
		WIID:            "wi_17",
		StepID:          "fix",
		SpawnN:          3,
		AcceptedIDs:     map[string]bool{},
		Outputs:         []OutputSpec{{Path: "artifacts/fix-summary.md", Schema: "object"}},
		HandoffRequired: handoff,
		CapsuleRoot:     root,
	}
	f.res = &StepResult{
		ProtocolVersion: ResultProtocolVersion,
		ResultID:        "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		WIID:            "wi_17",
		StepID:          "fix",
		SpawnN:          3,
		Outcome:         OutcomeCompleted,
		Output:          json.RawMessage(`{"summary":"ok","lines":2}`),
		Artifacts:       []ArtifactRef{f.art},
		SubmittedAt:     time.Now().UTC(),
	}
	if handoff {
		f.hand = writeArtifact(t, root, "artifacts/handoff.md", "handoff body\n")
		f.res.HandoffRef = &f.hand
	}
	return f
}

func TestValidateStepResultCompleted(t *testing.T) {
	f := newFixture(t, true)
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("completed result with declared output and handoff must validate: %v", err)
	}
}

func TestValidateStepResultShape(t *testing.T) {
	f := newFixture(t, false)

	if err := ValidateStepResult(nil, f.spec); err == nil {
		t.Fatal("nil result must be rejected")
	}

	res := *f.res
	res.ProtocolVersion = 0
	err := ValidateStepResult(&res, f.spec)
	if !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("protocol 0 must fence with ErrProtocolMismatch, got %v", err)
	}
	res.ProtocolVersion = ResultProtocolVersion + 1
	if err := ValidateStepResult(&res, f.spec); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("future protocol must fence with ErrProtocolMismatch, got %v", err)
	}

	res = *f.res
	res.ResultID = "" // empty: the one shape still refused
	if err := ValidateStepResult(&res, f.spec); err == nil {
		t.Fatal("empty result_id must be rejected")
	}
	res.ResultID = strings.Repeat("x", 129)
	if err := ValidateStepResult(&res, f.spec); err == nil {
		t.Fatal("oversized result_id must be rejected")
	}
	res.ResultID = "has whitespace inside"
	if err := ValidateStepResult(&res, f.spec); err == nil {
		t.Fatal("result_id with inner whitespace must be rejected")
	}
	// Tolerant id (#97): a readable slug is a legal idempotency key —
	// the UUID shape is asked for, not enforced.
	res.ResultID = "grill-review-ask-batch-1"
	if err := ValidateStepResult(&res, f.spec); err != nil {
		t.Fatalf("readable result_id must validate: %v", err)
	}
	if res.ResultID != "grill-review-ask-batch-1" {
		t.Fatalf("tolerant id must survive verbatim, got %q", res.ResultID)
	}
	// surrounding whitespace is trimmed, not fatal
	res.ResultID = "  padded-id-9  "
	if err := ValidateStepResult(&res, f.spec); err != nil {
		t.Fatalf("padded result_id must validate after trim: %v", err)
	}
	if res.ResultID != "padded-id-9" {
		t.Fatalf("padded result_id must be trimmed in place, got %q", res.ResultID)
	}
	res.ResultID = "0f1e2d3c4b5a69788796a5b4c3d2e1f0" // 32 hex, no dashes: single-token, legal now
	if err := ValidateStepResult(&res, f.spec); err != nil {
		t.Fatalf("dashless hex result_id must validate: %v", err)
	}

	res = *f.res
	res.Outcome = Outcome("vibes")
	if err := ValidateStepResult(&res, f.spec); err == nil {
		t.Fatal("unknown outcome must be rejected")
	}
}

func TestValidateStepResultBindingAndFence(t *testing.T) {
	f := newFixture(t, false)

	res := *f.res
	res.WIID = "wi_999"
	if err := ValidateStepResult(&res, f.spec); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("foreign wi_id must fence with ErrIdentityMismatch, got %v", err)
	}
	res = *f.res
	res.StepID = "grill"
	if err := ValidateStepResult(&res, f.spec); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("foreign step_id must fence with ErrIdentityMismatch, got %v", err)
	}

	// The fence: a result from a superseded spawn (stale spawn_n) is
	// rejected even though everything else about it is perfect.
	res = *f.res
	res.SpawnN = f.spec.SpawnN - 1
	if err := ValidateStepResult(&res, f.spec); !errors.Is(err, ErrStaleSpawn) {
		t.Fatalf("stale spawn_n must fence with ErrStaleSpawn, got %v", err)
	}
	res.SpawnN = f.spec.SpawnN + 1
	if err := ValidateStepResult(&res, f.spec); !errors.Is(err, ErrStaleSpawn) {
		t.Fatalf("future spawn_n must fence with ErrStaleSpawn, got %v", err)
	}
}

func TestValidateStepResultIdempotency(t *testing.T) {
	f := newFixture(t, false)
	f.spec.AcceptedIDs[f.res.ResultID] = true
	err := ValidateStepResult(f.res, f.spec)
	if !errors.Is(err, ErrDuplicateResult) {
		t.Fatalf("already-committed result_id must answer ErrDuplicateResult, got %v", err)
	}
	if got := err.Error(); got != "driver: result_id already accepted: "+f.res.ResultID {
		t.Fatalf("duplicate verdict must name the id, got %q", got)
	}
	// A DIFFERENT id on the same spawn stays valid.
	res := *f.res
	res.ResultID = "11111111-2222-3333-4444-555555555555"
	if err := ValidateStepResult(&res, f.spec); err != nil {
		t.Fatalf("fresh result_id must validate: %v", err)
	}
}

func TestValidateStepResultNonCompletedOutcomes(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeGateFailed, OutcomeNeedsInput, OutcomeIncomplete} {
		f := newFixture(t, false)
		f.res.Outcome = outcome
		// Declared outputs and handoff are NOT enforced for
		// non-completed outcomes — and the envelope carries neither.
		f.res.Artifacts = nil
		f.res.Output = nil
		if err := ValidateStepResult(f.res, f.spec); err != nil {
			t.Fatalf("%s result without outputs must validate: %v", outcome, err)
		}
		// But an artifact reference it DOES carry is still verified.
		tampered := f.art
		tampered.SHA256 = "deadbeef"
		f.res.Artifacts = []ArtifactRef{tampered}
		if err := ValidateStepResult(f.res, f.spec); err == nil {
			t.Fatalf("%s result with a hash-mismatched artifact must be rejected", outcome)
		}
	}
}

func TestValidateStepResultDeclaredOutputs(t *testing.T) {
	f := newFixture(t, false)
	// The declared output artifact is missing from the envelope.
	f.res.Artifacts = nil
	if err := ValidateStepResult(f.res, f.spec); err == nil {
		t.Fatal("completed result missing its declared output artifact must be rejected")
	}

	// Present as an artifact but the output JSON violates the declared
	// object schema.
	f.res.Artifacts = []ArtifactRef{f.art}
	f.res.Output = json.RawMessage(`["not","an","object"]`)
	if err := ValidateStepResult(f.res, f.spec); err == nil {
		t.Fatal("completed result whose output violates the declared schema must be rejected")
	}

	// No declared outputs at all: unconstrained.
	f.spec.Outputs = nil
	f.res.Output = json.RawMessage(`"anything goes"`)
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("step without declared outputs must not constrain output: %v", err)
	}
}

func TestValidateStepResultHandoff(t *testing.T) {
	f := newFixture(t, true)
	if err := ValidateStepResult(f.res, f.spec); err != nil {
		t.Fatalf("handoff step with a valid handoff_ref must validate: %v", err)
	}
	// Missing handoff on a handoff-owing completed step.
	res := *f.res
	res.HandoffRef = nil
	if err := ValidateStepResult(&res, f.spec); err == nil {
		t.Fatal("completed handoff step without handoff_ref must be rejected")
	}
	// Handoff present but pointing at a nonexistent file.
	res = *f.res
	bogus := *res.HandoffRef
	bogus.Path = "artifacts/missing-handoff.md"
	res.HandoffRef = &bogus
	if err := ValidateStepResult(&res, f.spec); err == nil {
		t.Fatal("handoff_ref pointing at a missing file must be rejected")
	}
	// A non-handoff step never requires one.
	nf := newFixture(t, false)
	nf.spec.HandoffRequired = false
	nf.res.HandoffRef = nil
	if err := ValidateStepResult(nf.res, nf.spec); err != nil {
		t.Fatalf("non-handoff step without handoff_ref must validate: %v", err)
	}
}

func TestValidateOutputSchema(t *testing.T) {
	cases := []struct {
		name   string
		output string
		schema string
		ok     bool
	}{
		{"object yes", `{"a":1}`, "object", true},
		{"object no-array", `[1]`, "object", false},
		{"object no-string", `"x"`, "object", false},
		{"object no-null", `null`, "object", false},
		{"object no-empty", ``, "object", false},
		{"array yes", `[1,2]`, "array", true},
		{"array no", `{"a":1}`, "array", false},
		{"string yes", `"text"`, "string", true},
		{"string no", `1`, "string", false},
		{"number int yes", `42`, "number", true},
		{"number float yes", `42.5`, "number", true},
		{"number no-quoted", `"42"`, "number", false},
		{"boolean yes", `true`, "boolean", true},
		{"boolean no", `"true"`, "boolean", false},
		{"any json", `[{"x":1}]`, "any", true},
		{"empty unconstrained", ``, "", true},
		{"unconstrained valid json", `{}`, "", true},
		{"unconstrained invalid json", `{[`, "", false},
		{"typo rejected", `{}`, "objects", false},
	}
	for _, tc := range cases {
		err := ValidateOutputSchema(json.RawMessage(tc.output), tc.schema)
		if tc.ok && err != nil {
			t.Errorf("%s: schema %q over %s: unexpected error %v", tc.name, tc.schema, tc.output, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: schema %q over %s: must be rejected", tc.name, tc.schema, tc.output)
		}
	}
}

func TestVerifyArtifact(t *testing.T) {
	root := t.TempDir()
	ref := writeArtifact(t, root, "artifacts/a.txt", "hello\n")

	if err := VerifyArtifact(root, ref); err != nil {
		t.Fatalf("intact artifact must verify: %v", err)
	}

	// Size mismatch.
	bad := ref
	bad.Bytes++
	if err := VerifyArtifact(root, bad); err == nil {
		t.Fatal("size mismatch must be rejected")
	}

	// Hash mismatch (uppercase hex of the true hash must still pass —
	// hex case is not part of the contract).
	bad = ref
	bad.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := VerifyArtifact(root, bad); err == nil {
		t.Fatal("sha256 mismatch must be rejected")
	}
	upper := ref
	upper.SHA256 = toUpperHex(ref.SHA256)
	if err := VerifyArtifact(root, upper); err != nil {
		t.Fatalf("uppercase hex of the true hash must verify: %v", err)
	}

	// Missing file.
	missing := ref
	missing.Path = "artifacts/gone.txt"
	if err := VerifyArtifact(root, missing); err == nil {
		t.Fatal("missing artifact must be rejected")
	}

	// Directory is not a regular file.
	if err := os.MkdirAll(filepath.Join(root, "artifacts/dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirRef := ref
	dirRef.Path = "artifacts/dir"
	if err := VerifyArtifact(root, dirRef); err == nil {
		t.Fatal("directory must be rejected as an artifact")
	}

	// Capsule jail: absolute path, .. escape and symlink exit must all
	// be rejected without reading anything outside the root.
	abs := ref
	abs.Path = "/etc/passwd"
	if err := VerifyArtifact(root, abs); err == nil {
		t.Fatal("absolute artifact path must be rejected")
	}
	esc := ref
	esc.Path = "../outside.txt"
	if err := VerifyArtifact(root, esc); err == nil {
		t.Fatal(".. escape must be rejected")
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape-link")); err != nil {
		t.Fatal(err)
	}
	link, _, err := HashFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	sym := ArtifactRef{Path: "escape-link", SHA256: link, Bytes: st.Size()}
	if err := VerifyArtifact(root, sym); err == nil {
		t.Fatal("symlink exit out of the capsule must be rejected")
	}

	// Empty path.
	if err := VerifyArtifact(root, ArtifactRef{}); err == nil {
		t.Fatal("empty artifact path must be rejected")
	}
}

func toUpperHex(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vec.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, n, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Pinned vector: sha256("abc").
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if sum != want {
		t.Fatalf("sha256 vector: got %s want %s", sum, want)
	}
	if n != 3 {
		t.Fatalf("byte length: got %d want 3", n)
	}
}

func TestStepResultJSONRoundTrip(t *testing.T) {
	// Pin the envelope's wire tags: the staging side persists result
	// envelopes verbatim, so a renamed tag is a protocol break.
	f := newFixture(t, true)
	f.res.CommandEvidence = []CommandEvidence{{
		Argv:       []string{"go", "test", "./..."},
		Cwd:        "/w/repo",
		ExitCode:   0,
		StartedAt:  time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
		DurationMs: 4242,
		LogPath:    "logs/fix.log",
	}}
	raw, err := json.Marshal(f.res)
	if err != nil {
		t.Fatal(err)
	}
	var back StepResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ProtocolVersion != f.res.ProtocolVersion || back.ResultID != f.res.ResultID ||
		back.WIID != f.res.WIID || back.StepID != f.res.StepID || back.SpawnN != f.res.SpawnN ||
		back.Outcome != f.res.Outcome {
		t.Fatalf("round trip lost envelope binding: %+v", back)
	}
	if back.HandoffRef == nil || back.HandoffRef.Path != f.hand.Path ||
		back.HandoffRef.SHA256 != f.hand.SHA256 || back.HandoffRef.Bytes != f.hand.Bytes {
		t.Fatalf("round trip lost handoff ref: %+v", back.HandoffRef)
	}
	if len(back.CommandEvidence) != 1 || back.CommandEvidence[0].DurationMs != 4242 ||
		len(back.CommandEvidence[0].Argv) != 3 {
		t.Fatalf("round trip lost command evidence: %+v", back.CommandEvidence)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"protocol_version", "result_id", "wi_id", "step_id",
		"spawn_n", "outcome", "output", "artifacts", "handoff_ref", "command_evidence", "submitted_at"} {
		if _, ok := probe[tag]; !ok {
			t.Fatalf("envelope JSON must carry tag %q", tag)
		}
	}
}
