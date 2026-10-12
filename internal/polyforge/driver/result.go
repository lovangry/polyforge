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
//
// For a step the workflow declares gate: true there is a fifth rule,
// the VERDICT BINDING (the #106 lesson): a gate's business verdict —
// the top-level "verdict" string in the output JSON — and the
// envelope's routing outcome must agree in the fail direction. A
// completed envelope carrying a fail verdict is a mislabeled gate-fail
// report: it is never accepted as a completed step. ValidateStepResult
// re-binds it to gate_failed — the only outcome whose routing (the
// gate's on_fail policy) the driver honors — so a gate's fail verdict
// can never advance a workflow, whatever the envelope's outcome field
// says. Fail-closed by construction.
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

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// ResultProtocolVersion is the result-envelope protocol this driver
// speaks (R13). Envelopes carrying any other version are rejected
// outright rather than best-effort parsed — a version this driver does
// not know is a contract it cannot validate.
const ResultProtocolVersion = 1

// EvEnvelopeRejected is the durable event kind for the in-process
// envelope-rejection retry lane (one event per rejection, carrying the
// validation error verbatim — the operator sees exactly what the step
// author got wrong).
const EvEnvelopeRejected = "envelope_rejected"

// EvGateVerdictBound is the durable event kind for the verdict binding
// (the #106 lesson): one event per envelope the driver re-classified
// from completed to gate_failed because a workflow-declared gate step
// delivered a fail verdict under a completed outcome. The delivered
// envelope is archived verbatim where it always is (the R14 staging
// result.json); this event is the news that its ROUTING was bound to
// the verdict.
const EvGateVerdictBound = "gate_verdict_bound"

// releaseStepID is the step id whose output schema carries the extended
// verdict vocabulary [pass, defer, fail] (polyforge#123): the only step a
// defer verdict validates on, and the only step the release verdict
// binding (rule 3c below) judges. It mirrors the workflow bundles' step
// declaration the same way verificationStepID does in observe.
const releaseStepID = "release"

// verificationStepID is the conventional id of the V4 verification step:
// a completed result for it owes command evidence (ResultSpec.RequireCommandEvidence).
const verificationStepID = "verification"

// maxEnvelopeRejects bounds the in-process re-dispatch lane: a step
// whose envelopes keep failing validation this many times CONSECUTIVELY
// escalates to the recovery lane (the storm guard bounds whatever
// follows). The #97/#107 history: hand-transcribed digest typos and
// missing handoff refs — the retries fix the author, the bound keeps a
// genuinely broken author from looping forever.
const maxEnvelopeRejects = 3

// maxRejectRelaunches bounds the driver relaunches a persistently
// rejected envelope may cost (persisted per step in execution.json,
// because the in-process counter resets on every relaunch): the third
// escalation blocks the step instead of relaunching again.
const maxRejectRelaunches = 3

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

	// evidenceDropped records why the tolerant re-parse discarded
	// command_evidence entries (set by the collector, never serialized);
	// the evidence gate quotes it so a rejection names the real reason.
	evidenceDropped []string
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
// the envelope for audit (the operator face renders it). Validation of
// a verification step checks argv non-emptiness and duration_ms > 0
// (checkCommandEvidence); exit_code and started_at are not enforced.
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
	// Gate mirrors the step's gate: true declaration — the verdict
	// binding applies (a completed envelope carrying a fail verdict is
	// re-bound to gate_failed, never accepted as a completed step).
	Gate bool
	// CapsuleRoot is the directory capsule-relative artifact paths
	// resolve against (verification is jailed inside it).
	CapsuleRoot string
	// RequiresHumanAnswer mirrors the step's requires_human_answer: true
	// declaration: a delivered completed result is rejected unless
	// HumanAnswers > 0 (the human-gate guarantee, polyforge#138).
	RequiresHumanAnswer bool
	// HumanAnswers is the number of human answers recorded in the step's
	// CURRENT attempt chain: st.Answers[st.AnswerBase:], where AnswerBase
	// is advanced to len(Answers) by every gate-fail rewind that archives
	// the step (see currentChainAnswers). Answers only enter the record
	// through the answer transition, so every entry is a human's.
	HumanAnswers int
	// RequireCommandEvidence mirrors requires_command_evidence: true (or
	// the step id "verification"): an accepted completed result must carry
	// at least one command_evidence entry, each with a non-empty argv and
	// duration_ms > 0 (the V4 gate; exit_code presence is not checkable).
	RequireCommandEvidence bool
}

// ValidateStepResult runs the R14 staging gates over one envelope:
//
//  1. shape: protocol version, UUID result_id, known outcome;
//  2. binding: wi_id/step_id match and spawn_n equals the current
//     spawn (a result from a superseded spawn is fenced out);
//  3. once-only: a result_id already in AcceptedIDs answers
//     ErrDuplicateResult (wrap with errors.Is to detect);
//     3b. verdict binding (gate steps only): a completed envelope whose
//     output carries a top-level fail verdict is re-bound to
//     gate_failed BEFORE substance is judged — a gate-fail report owes
//     no completed duties (declared outputs, handoff_ref). The
//     re-bind is visible to the caller: res.Outcome differs from the
//     delivered outcome after this call, and the Run loop logs it as
//     an EvGateVerdictBound event;
//     3c. release verdict binding (the release step only, polyforge#123):
//     a completed envelope for the release step whose output carries a
//     top-level verdict is judged against the extended release verdict
//     vocabulary — pass owes a non-empty version plus at least one
//     artifacts_published entry, fail owes a non-empty fail_reason,
//     defer owes neither version nor artifacts_published, and anything
//     else is outside the closed [pass, defer, fail] set. A defer
//     verdict on any OTHER step is a validation error: defer validates
//     nowhere but the release step. The gate binding above stays
//     prefix-fail only, so a defer verdict clears the release gate and
//     the workflow proceeds to wrap, while a fail verdict (with its
//     fail_reason) still re-binds to gate_failed and rewinds.
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
	if id := strings.TrimSpace(res.ResultID); id == "" {
		return fmt.Errorf("driver: result_id is empty")
	} else if len(id) > 128 {
		return fmt.Errorf("driver: result_id %q exceeds 128 characters", id)
	} else if strings.ContainsAny(id, " \t\r\n") {
		return fmt.Errorf("driver: result_id %q contains whitespace", id)
	} else {
		// Tolerant id (the #97 lesson): a well-meaning step agent minted
		// a readable slug ("grill-review-ask-batch-1") where the UUID
		// contract asked for a random one. The id's whole duty is to be a
		// non-empty, collision-honest idempotency key — a stable readable
		// slug serves it as well as a UUID, and rejecting it sent the
		// driver into a reject/relaunch death loop (nothing consumed the
		// drop, so every restart re-collected the same envelope). Accept
		// any single-token id; the prompt still asks for a fresh UUID.
		res.ResultID = id
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
	// 3a. needs_input envelope shape: the question must be nameable
	// BEFORE the process is gone, so a missing question_id/question is an
	// envelope rejection (in-process rewrite), never a recovery-lane error
	// at the commit point.
	if res.Outcome == OutcomeNeedsInput {
		if _, _, err := needsInputQuestion(res); err != nil {
			return err
		}
	}
	// 3b. the verdict binding (the #106 lesson): for a gate step the
	// output's top-level fail verdict IS the routing. A completed
	// envelope carrying one is a mislabeled gate-fail report — re-bind
	// it to gate_failed so the gate's on_fail policy fires and the
	// verdict can never advance a workflow. The caller logs the bind;
	// the delivered envelope stays archived verbatim.
	delivered := res.Outcome
	if spec.Gate && delivered == OutcomeCompleted {
		if _, fail := failVerdictInOutput(res.Output); fail {
			res.Outcome = OutcomeGateFailed
		}
	}
	// 3b'. the human-gate guarantee (#138): judged on the DELIVERED
	// outcome so a gate-fail verdict cannot dodge the human either.
	if delivered == OutcomeCompleted && spec.RequiresHumanAnswer && spec.HumanAnswers <= 0 {
		return fmt.Errorf("driver: step %q requires a human answer: return needs_input first (a completed result is refused until a human has answered a question in this attempt)",
			spec.StepID)
	}
	// 3c. the release verdict binding (polyforge#123): judged on the
	// DELIVERED outcome (a completed envelope), before substance. A
	// binding error is an envelope rejection (the retry lane re-dispatches
	// the step with the typed reason) — never a silent reclassification.
	if delivered == OutcomeCompleted {
		if err := releaseVerdictBinding(res); err != nil {
			return err
		}
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
	if spec.RequireCommandEvidence {
		if err := checkCommandEvidence(res.CommandEvidence); err != nil {
			if len(res.evidenceDropped) > 0 {
				err = fmt.Errorf("%w (entries dropped while parsing the envelope: %s)", err, strings.Join(res.evidenceDropped, "; "))
			}
			return fmt.Errorf("driver: step %q completed without verification evidence: %w", spec.StepID, err)
		}
	}
	return nil
}

// needsInputQuestion extracts the question of a needs_input envelope: it
// may live at envelope level (question_id/question) or embedded in the
// output object (step agents naturally put it there). Each half is taken
// from the envelope first, then the output. An error names what is missing.
func needsInputQuestion(res *StepResult) (id, text string, err error) {
	id, text = strings.TrimSpace(res.QuestionID), strings.TrimSpace(res.Question)
	if (id == "" || text == "") && len(res.Output) > 0 {
		var embedded struct {
			QuestionID string `json:"question_id"`
			Question   string `json:"question"`
		}
		if json.Unmarshal(res.Output, &embedded) == nil {
			if id == "" {
				id = strings.TrimSpace(embedded.QuestionID)
			}
			if text == "" {
				text = strings.TrimSpace(embedded.Question)
			}
		}
	}
	var missing []string
	if id == "" {
		missing = append(missing, "question_id")
	}
	if text == "" {
		missing = append(missing, "question")
	}
	if len(missing) > 0 {
		return id, text, fmt.Errorf("driver: needs_input result is missing %s (set them on the envelope or in the output object)",
			strings.Join(missing, " and "))
	}
	return id, text, nil
}

// checkCommandEvidence enforces the V4 evidence shape: at least one entry,
// every entry with a non-empty argv and duration_ms > 0. exit_code's
// zero value is legitimate and its presence cannot be checked on the
// decoded struct, so it is not enforced. An entry the tolerant re-parse
// could not decode never reaches here as a struct (see evidenceDropped).
func checkCommandEvidence(ev []CommandEvidence) error {
	if len(ev) == 0 {
		return errors.New("command_evidence is empty — a verification step must carry at least one entry, each an OBJECT {\"argv\": [...], \"exit_code\": N, \"duration_ms\": N} (duration_ms > 0)")
	}
	for i, e := range ev {
		if len(e.Argv) == 0 || strings.TrimSpace(strings.Join(e.Argv, "")) == "" {
			return fmt.Errorf("command_evidence[%d] has an empty argv", i)
		}
		if e.DurationMs <= 0 {
			return fmt.Errorf("command_evidence[%d] (%s) has no duration_ms (must be > 0)", i, e.Argv[0])
		}
	}
	return nil
}

// currentChainAnswers counts the human answers of a step's current attempt
// chain: answers recorded since the step's last gate-fail rewind
// (st.Answers[st.AnswerBase:]). A step never rewound counts all of them.
func currentChainAnswers(st *capsule.Step) int {
	base := st.AnswerBase
	if base < 0 || base > len(st.Answers) {
		base = len(st.Answers)
	}
	return len(st.Answers) - base
}

// failVerdictInOutput reports whether one result output carries a
// top-level "verdict" field whose STRING value asserts failure. The
// match is a case- and space-insensitive prefix on "fail" — the
// verdict vocabulary the gate steps declare (enum [pass, fail]) plus
// the prose-decorated shape the in-repo gate fixtures use ("FAIL:
// tests red"). Only the top-level scalar is read: a nested verdict or
// a non-string one is not a fail verdict this binding can interpret,
// and the declared-schema lane stays its authority.
func failVerdictInOutput(output json.RawMessage) (string, bool) {
	if len(output) == 0 {
		return "", false
	}
	var probe struct {
		Verdict any `json:"verdict"`
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		return "", false
	}
	s, ok := probe.Verdict.(string)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "fail") {
		return s, true
	}
	return "", false
}

// releaseVerdictBinding judges one completed envelope against the release
// step's extended verdict contract (polyforge#123). It reads only the
// top-level scalar fields the release output schema names and mirrors the
// failVerdictInOutput tolerance: an output that is not a JSON object, or
// whose verdict is absent/non-string, is not this binding's business (the
// declared-schema lane stays its authority). On the release step the
// vocabulary is closed — pass owes a non-empty version plus at least one
// artifacts_published entry, fail owes a non-empty fail_reason, defer owes
// neither version nor artifacts_published — and defer is illegal on every
// other step.
func releaseVerdictBinding(res *StepResult) error {
	if len(res.Output) == 0 {
		return nil
	}
	var probe struct {
		Verdict            any   `json:"verdict"`
		Version            any   `json:"version"`
		ArtifactsPublished []any `json:"artifacts_published"`
		FailReason         any   `json:"fail_reason"`
	}
	if err := json.Unmarshal(res.Output, &probe); err != nil {
		return nil // not a decodable output object: not this binding's business
	}
	v, ok := probe.Verdict.(string)
	if !ok {
		return nil
	}
	verdict := strings.ToLower(strings.TrimSpace(v))
	isRelease := res.StepID == releaseStepID
	switch {
	case verdict == "defer":
		if !isRelease {
			return fmt.Errorf("driver: step %q output carries verdict %q — defer validates nowhere but the %s step's schema",
				res.StepID, v, releaseStepID)
		}
		if nonEmptyJSONString(probe.Version) || len(probe.ArtifactsPublished) > 0 {
			return fmt.Errorf("driver: release output verdict %q owes neither version nor artifacts_published — a defer releases nothing; report pass with the produced version/artifacts or defer with none", v)
		}
		return nil
	case strings.HasPrefix(verdict, "fail"):
		if !isRelease {
			return nil // the gate binding's business, not this one
		}
		if !nonEmptyJSONString(probe.FailReason) {
			return fmt.Errorf("driver: release output verdict %q requires a non-empty fail_reason — the on_fail rewind input must say what went wrong", v)
		}
		return nil
	case verdict == "pass":
		if !isRelease {
			return nil
		}
		if !nonEmptyJSONString(probe.Version) || len(probe.ArtifactsPublished) == 0 {
			return fmt.Errorf("driver: release output verdict %q requires a non-empty version and at least one artifacts_published entry — pass means something was actually published", v)
		}
		return nil
	default:
		if isRelease {
			return fmt.Errorf("driver: release output verdict %q is outside the closed release vocabulary [pass, defer, fail]", v)
		}
		return nil
	}
}

// nonEmptyJSONString answers whether one decoded JSON field carries a
// non-blank string value (absent, non-string and blank all count as
// empty — the release schema's minLength: 1 discipline).
func nonEmptyJSONString(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
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
