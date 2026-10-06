// result.go: the step result envelope (R13) and its pre-commit
// validation — R14 steps 1–3, the STAGING half of the done commit
// point. The atomic execution.json acceptance (R14 step 6) belongs to
// transition.go; this file only answers "is this envelope acceptable".
//
// A result envelope is the one channel a step process uses to report
// its outcome. The driver trusts nothing but the envelope, and the
// envelope itself is trusted only after four gates, in order:
//
//  1. shape     — protocol version, UUID result_id, known outcome
//  2. binding   — wi_id/step_id identity and the spawn_n fence
//  3. once-only — the result_id idempotency key (R14 step 1)
//  4. substance — artifact files exist with matching hashes (R14
//     step 3) and output matches the step's declared
//     schema (R14 step 2)
package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ResultProtocolVersion is the result-envelope protocol this driver
// speaks (R13). Envelopes carrying any other version are rejected
// outright rather than best-effort parsed — a version this driver does
// not know is a contract it cannot validate.
const ResultProtocolVersion = 1

// Outcome is the closed result-verdict set of the envelope.
type Outcome string

const (
	OutcomeCompleted  Outcome = "completed"
	OutcomeGateFailed Outcome = "gate_failed"
	OutcomeNeedsInput Outcome = "needs_input"
	OutcomeIncomplete Outcome = "incomplete"
)

// Valid reports whether o belongs to the closed outcome set.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeCompleted, OutcomeGateFailed, OutcomeNeedsInput, OutcomeIncomplete:
		return true
	}
	return false
}

// StepResult is the result envelope (R13): the step process's complete
// and only report. Every field the driver needs to judge and archive
// the step rides here — the commit-point code (transition.go) never
// reaches back into the step process for more.
type StepResult struct {
	ProtocolVersion int               `json:"protocol_version"`
	ResultID        string            `json:"result_id"` // UUID, idempotency key
	WIID            string            `json:"wi_id"`
	StepID          string            `json:"step_id"`
	SpawnN          int64             `json:"spawn_n"` // fencing: only the current spawn's results count
	Outcome         Outcome           `json:"outcome"`
	Output          json.RawMessage   `json:"output"` // step-specific output JSON, schema-checked
	Artifacts       []ArtifactRef     `json:"artifacts"`
	HandoffRef      *ArtifactRef      `json:"handoff_ref"`           // required for completed inherit-source steps
	QuestionID      string            `json:"question_id,omitempty"` // set when Outcome == needs_input
	Question        string            `json:"question,omitempty"`    // the question text for needs_input
	CommandEvidence []CommandEvidence `json:"command_evidence"`
	SubmittedAt     time.Time         `json:"submitted_at"`
}

// ArtifactRef pins one artifact file: capsule-relative path plus the
// content hash and size it must verify against (R14 step 3). The driver
// never hashes "whatever is there" — it hashes what is there and
// compares to this manifest entry.
type ArtifactRef struct {
	Path   string `json:"path"` // capsule-relative
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// CommandEvidence is one recorded command the step process ran, kept in
// the envelope for audit (the operator face renders it; validation
// never branches on it).
type CommandEvidence struct {
	Argv       []string  `json:"argv"`
	Cwd        string    `json:"cwd"`
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at"`
	DurationMs int64     `json:"duration_ms"`
	LogPath    string    `json:"log_path"`
}

// OutputSpec is one entry of a step's declared outputs (the workflow
// frontmatter extension): the artifact path the completed step must
// deliver plus the JSON type the step's result output must carry.
type OutputSpec struct {
	Path   string `json:"path"`
	Schema string `json:"schema"` // "", "any", "object", "array", "string", "number", "boolean"
}

// Validation verdicts. ErrDuplicateResult is not an envelope failure:
// it answers "this result_id is already committed" and the caller
// treats the resubmission as an idempotent no-op (R14 step 1) instead
// of re-applying the result.
var (
	ErrProtocolMismatch = errors.New("driver: result protocol version mismatch")
	ErrStaleSpawn       = fmt.Errorf("driver: result fenced out (spawn_n mismatch): %w", ErrStaleFence)
	ErrIdentityMismatch = errors.New("driver: result bound to another work item/step")
	ErrDuplicateResult  = errors.New("driver: result_id already accepted")
)

// ResultSpec carries everything ValidateStepResult judges an envelope
// against: the binding and fence of the CURRENT spawn, the set of
// already-committed result ids (idempotency), the step's declared
// outputs and handoff duty, and the capsule root artifact paths resolve
// under. It is a read-only snapshot: the mutation that adds a result_id
// to AcceptedIDs happens at the commit point (R14 step 6), never here.
type ResultSpec struct {
	WIID   string
	StepID string
	SpawnN int64
	// AcceptedIDs holds the result ids already committed for this step.
	AcceptedIDs map[string]bool
	// Outputs are the step's declared outputs (frontmatter outputs:);
	// presence is enforced for completed results only.
	Outputs []OutputSpec
	// HandoffRequired mirrors the step's handoff: true declaration —
	// a completed handoff step must carry a verifiable handoff_ref.
	HandoffRequired bool
	// CapsuleRoot is the directory capsule-relative artifact paths
	// resolve against (verification is jailed inside it).
	CapsuleRoot string
}

// ValidateStepResult runs the R14 staging gates over one envelope:
//
//  1. shape: protocol version, UUID result_id, known outcome;
//  2. binding: wi_id/step_id match and spawn_n equals the current
//     spawn (a result from a superseded spawn is fenced out);
//  3. once-only: a result_id already in AcceptedIDs answers
//     ErrDuplicateResult (wrap with errors.Is to detect);
//  4. substance: every artifact the envelope declares must exist under
//     the capsule with matching size and SHA-256, and — for completed
//     results — every declared output must be delivered and the output
//     JSON must match the declared schema, with the handoff_ref present
//     when the step declares handoff.
//
// Non-completed outcomes (gate_failed, needs_input, incomplete) skip
// the declared-output and handoff enforcement: their disposition rides
// other channels (the gate verdict, the pending question), but any
// artifact reference they DO carry is still verified — a wrong hash is
// a broken envelope whatever the outcome.
func ValidateStepResult(res *StepResult, spec ResultSpec) error {
	if res == nil {
		return errors.New("driver: nil step result")
	}
	// 1. shape.
	if res.ProtocolVersion != ResultProtocolVersion {
		return fmt.Errorf("%w: envelope speaks %d, driver speaks %d",
			ErrProtocolMismatch, res.ProtocolVersion, ResultProtocolVersion)
	}
	if !validUUID(res.ResultID) {
		return fmt.Errorf("driver: result_id %q is not a UUID", res.ResultID)
	}
	if !res.Outcome.Valid() {
		return fmt.Errorf("driver: unknown outcome %q", res.Outcome)
	}
	// 2. binding: identity first, then the fence.
	if res.WIID != spec.WIID || res.StepID != spec.StepID {
		return fmt.Errorf("%w: envelope binds %s/%s, expected %s/%s",
			ErrIdentityMismatch, res.WIID, res.StepID, spec.WIID, spec.StepID)
	}
	if res.SpawnN != spec.SpawnN {
		return fmt.Errorf("%w: envelope carries spawn_n %d, current spawn is %d",
			ErrStaleSpawn, res.SpawnN, spec.SpawnN)
	}
	// 3. once-only.
	if spec.AcceptedIDs[res.ResultID] {
		return fmt.Errorf("%w: %s", ErrDuplicateResult, res.ResultID)
	}
	// 4. substance: verify everything the envelope names, whether or
	// not the outcome demands it.
	for i, a := range res.Artifacts {
		if err := VerifyArtifact(spec.CapsuleRoot, a); err != nil {
			return fmt.Errorf("driver: artifact[%d] %q: %w", i, a.Path, err)
		}
	}
	if res.HandoffRef != nil {
		if err := VerifyArtifact(spec.CapsuleRoot, *res.HandoffRef); err != nil {
			return fmt.Errorf("driver: handoff_ref %q: %w", res.HandoffRef.Path, err)
		}
	}
	if res.Outcome != OutcomeCompleted {
		return nil
	}
	// Completed: every declared output delivered, output schema-clean,
	// handoff present when the step owes one.
	for _, out := range spec.Outputs {
		if !hasArtifact(res.Artifacts, out.Path) {
			return fmt.Errorf("driver: step %q declares output %q but the result carries no artifact for it",
				spec.StepID, out.Path)
		}
		if err := ValidateOutputSchema(res.Output, out.Schema); err != nil {
			return fmt.Errorf("driver: output for declared %q: %w", out.Path, err)
		}
	}
	if spec.HandoffRequired && res.HandoffRef == nil {
		return fmt.Errorf("driver: step %q declares handoff but the completed result carries no handoff_ref",
			spec.StepID)
	}
	return nil
}

// ValidateOutputSchema checks one result output against a declared
// schema type name. The v2 schema surface is deliberately a closed
// JSON-type set (the frontmatter extension): object|array|string|
// number|boolean, plus "" / "any" for unconstrained. An unknown name is
// an error, not a pass — a typo in a bundle must fail loudly at the
// envelope, never slip through as "unconstrained".
func ValidateOutputSchema(output json.RawMessage, schema string) error {
	switch schema {
	case "", "any":
		if len(output) == 0 {
			return nil
		}
		var probe any
		if err := json.Unmarshal(output, &probe); err != nil {
			return fmt.Errorf("driver: output is not valid JSON: %w", err)
		}
		return nil
	case "object":
		return checkJSONType(output, "object", func(v any) bool { _, ok := v.(map[string]any); return ok })
	case "array":
		return checkJSONType(output, "array", func(v any) bool { _, ok := v.([]any); return ok })
	case "string":
		return checkJSONType(output, "string", func(v any) bool { _, ok := v.(string); return ok })
	case "number":
		return checkJSONType(output, "number", func(v any) bool { _, ok := v.(float64); return ok })
	case "boolean":
		return checkJSONType(output, "boolean", func(v any) bool { _, ok := v.(bool); return ok })
	default:
		return fmt.Errorf("driver: unknown output schema %q", schema)
	}
}

// checkJSONType unmarshals output once and asserts its dynamic JSON
// type; null never satisfies a typed schema.
func checkJSONType(output json.RawMessage, want string, ok func(any) bool) error {
	if len(output) == 0 {
		return fmt.Errorf("driver: output is empty, schema wants %s", want)
	}
	var probe any
	if err := json.Unmarshal(output, &probe); err != nil {
		return fmt.Errorf("driver: output is not valid JSON: %w", err)
	}
	if probe == nil || !ok(probe) {
		return fmt.Errorf("driver: output schema wants %s, got JSON %s", want, jsonTypeName(probe))
	}
	return nil
}

// jsonTypeName answers the JSON type name of one decoded value for
// error messages.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// VerifyArtifact checks one artifact reference against the capsule
// (R14 step 3): the path must be capsule-relative, the file must exist
// as a regular file inside the capsule, and its byte length and
// SHA-256 must match the manifest entry. The open goes through
// os.OpenRoot, so a symlink planted inside the capsule cannot make the
// driver hash a file beyond the capsule boundary.
func VerifyArtifact(capsuleRoot string, ref ArtifactRef) error {
	if strings.TrimSpace(ref.Path) == "" {
		return errors.New("empty artifact path")
	}
	root, err := os.OpenRoot(capsuleRoot)
	if err != nil {
		return fmt.Errorf("open capsule root: %w", err)
	}
	defer root.Close()
	f, err := root.Open(ref.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if info.Size() != ref.Bytes {
		return fmt.Errorf("size mismatch: manifest says %d bytes, file has %d", ref.Bytes, info.Size())
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(sum, ref.SHA256) {
		return fmt.Errorf("sha256 mismatch: manifest says %s, file hashes %s", ref.SHA256, sum)
	}
	return nil
}

// HashFile answers the lowercase-hex SHA-256 and byte length of one
// file. The staging side uses it to build manifest entries; the
// verification side (VerifyArtifact) recomputes and compares.
func HashFile(path string) (sha256Hex string, bytes int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// hasArtifact answers whether the envelope declares an artifact at rel.
func hasArtifact(refs []ArtifactRef, rel string) bool {
	for _, a := range refs {
		if a.Path == rel {
			return true
		}
	}
	return false
}

// validUUID answers whether s is shaped like a UUID (8-4-4-4-12 hex).
// The result_id is the idempotency key: a malformed key is rejected at
// the envelope rather than discovered later as a set-entry collision.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexDigit(c) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
