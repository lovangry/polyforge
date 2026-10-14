// result.go: the step result envelope and its pre-commit validation —
// the STAGING half of the done commit point. The atomic execution.json
// acceptance belongs to transition.go; this file only answers "is this
// envelope acceptable".
//
// A result envelope is the one channel a step process uses to report
// its outcome. The driver trusts nothing but the envelope, and the
// envelope itself is trusted only after three gates, in order:
//
//  1. shape     — the outcome is in the closed set
//  2. binding   — wi_id/step_id identity and the spawn_n fence
//  3. substance — the needs_input question pair, the gate verdict
//     binding, the human-gate guarantee and the release/deploy verdict
//     binding
//
// The MINIMAL CONTRACT (polyforge#147 审计减法 A): an envelope is the
// outcome + output pair (needs_input adds question_id + question),
// written to the unchanged drop path; the driver judges only the
// fields above. There is no protocol version stamp, no envelope id,
// no artifact manifest, no handoff digest reference, and no hash or
// manifest verification anywhere — downstream and handoff readers work
// by path convention.
//
// For a step the workflow declares gate: true there is a further rule,
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
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

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

// EvHumanGateUnasked is the durable event kind recording that a
// requires_human_answer: if_asked step (quick-fix grill) completed with
// zero human answers (polyforge#164 H11). The completion is accepted —
// zero questions is a legitimate outcome — but never silent: the event
// is the record and the watcher's decision table notifies the owner.
const EvHumanGateUnasked = "human_gate_unasked"

// releaseStepID and deployStepID are the step ids of the `release`
// workflow bundle (the only place a release or deploy happens). They are
// the steps the release verdict binding (rule 3c below) judges: `release`
// owns the closed vocabulary [pass, fail] (a release WI with no procedure
// is an error, never a defer) and its pass must name the published
// artifacts; `deploy` owns [pass, defer, fail] — defer validates on it
// and nowhere else, because a project may release without deploying. They
// mirror the bundle's step declarations the same way verificationStepID
// does in observe.
const (
	releaseStepID = "release"
	deployStepID  = "deploy"
)

// maxEnvelopeRejects bounds the in-process re-dispatch lane: a step
// whose envelopes keep failing validation this many times CONSECUTIVELY
// goes BLOCKED with the result_invalid cause code (no driver relaunch,
// no recovery lane, no on_fail routing — rewind stays reserved for
// review FAIL verdicts). The #97/#107 history: hand-transcribed digest
// typos and missing handoff refs — the retries fix the author, the bound
// keeps a genuinely broken author from looping forever.
const maxEnvelopeRejects = 3

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

// StepResult is the result envelope: the step process's complete and
// only report. The MINIMAL CONTRACT (polyforge#147 审计减法 A): the
// outcome + output pair — needs_input adds the question pair — plus the
// identity/fence fields the driver judges the envelope against. There
// is no protocol version stamp, no envelope id, no artifact manifest
// and no handoff digest reference: downstream and handoff readers work
// by path convention, and an envelope missing any retired field is
// never judged invalid for its absence.
type StepResult struct {
	WIID       string          `json:"wi_id"`
	StepID     string          `json:"step_id"`
	SpawnN     int64           `json:"spawn_n"` // fencing: only the current spawn's results count
	Outcome    Outcome         `json:"outcome"`
	Output     json.RawMessage `json:"output"`                // step-specific output JSON, schema-checked
	QuestionID string          `json:"question_id,omitempty"` // set when Outcome == needs_input
	Question   string          `json:"question,omitempty"`    // the question text for needs_input
	// CommandEvidence is the V4 command-evidence list: required (and
	// checked) only on a step that owes it (ResultSpec.RequireCommandEvidence).
	CommandEvidence []CommandEvidence `json:"command_evidence,omitempty"`
	SubmittedAt     time.Time         `json:"submitted_at,omitempty"`

	// evidenceDropped records why the tolerant re-parse discarded
	// command_evidence entries (set by the collector, never serialized);
	// the evidence gate quotes it so a rejection names the real reason.
	evidenceDropped []string
	// rebaseline is set by ValidateStepResult when the step changed
	// authored test files with a human's approval (the authored-tests
	// gate): the accepted output is stamped with the new baseline.
	rebaseline bool
	// rebaselined names the authored test files a human-approved change
	// touched (the authored_tests_rebaselined event body).
	rebaselined []string
	// schemaViolation is the declared-output-schema validation error of
	// an accepted result while the schema lane records instead of
	// enforcing (enforceOutputSchemas false); "" = conforming.
	schemaViolation string
}

// verificationStepID is the conventional id of the V4 verification step:
// a completed result for it owes command evidence (ResultSpec.RequireCommandEvidence).
const verificationStepID = "verification"

// CommandEvidence is one recorded command the step process ran, kept in
// the envelope for audit (the operator face renders it). Validation of
// an evidence-owing step checks argv non-emptiness and duration_ms > 0
// (checkCommandEvidence); exit_code and started_at are not enforced.
type CommandEvidence struct {
	Argv       []string  `json:"argv"`
	Cwd        string    `json:"cwd"`
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at"`
	DurationMs int64     `json:"duration_ms"`
	LogPath    string    `json:"log_path"`
}

// Validation verdicts.
var (
	ErrStaleSpawn       = fmt.Errorf("driver: result fenced out (spawn_n mismatch): %w", ErrStaleFence)
	ErrIdentityMismatch = errors.New("driver: result bound to another work item/step")
)

// ResultSpec carries everything ValidateStepResult judges an envelope
// against: the binding and fence of the CURRENT spawn, the gate verdict
// binding and the human-gate guarantee. It is a read-only snapshot: the
// mutations happen at the commit point, never here.
type ResultSpec struct {
	WIID   string
	StepID string
	SpawnN int64
	// Gate mirrors the step's gate: true declaration — the verdict
	// binding applies (a completed envelope carrying a fail verdict is
	// re-bound to gate_failed, never accepted as a completed step).
	Gate bool
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
	// ReposDir is the capsule's task-repo root (<capsule>/repos). When
	// set, a completed result whose output declares a non-empty
	// test_files list is accepted only if every listed (repo-relative)
	// path exists in a task repo and is committed at HEAD in every task
	// repo where it exists (polyforge#156: a resume restores task repos
	// at their carrier ref, i.e. HEAD only, so an uncommitted authored
	// test would be lost).
	ReposDir string
	// OutputSchema is the step definition's declared output schema
	// (polyforge#164 H3); nil = none declared, nothing validated.
	OutputSchema *jsonschema.Schema
	// AuthoredTests is the authored-tests baseline of the done steps
	// before this one (polyforge#164 H4); nil = no authored tests.
	AuthoredTests *AuthoredTests
	// PreExisting is the pre-existing-failure boundary (polyforge#164
	// S6): the tests that pin the change itself — the authored test_files
	// of the done steps (e2e-author, repro) and bug-fix's regression_test
	// paths — and whether the repro reproduced the bug (checkPreExisting).
	PreExisting PreExistingRules
}

// ValidateStepResult runs the staging gates over one envelope:
//
//  1. shape: the outcome is in the closed set;
//  2. binding: wi_id/step_id match and spawn_n equals the current
//     spawn (a result from a superseded spawn is fenced out);
//  3. substance, in order:
//     3a. a needs_input envelope owes its question pair (nameable
//     before the process is gone — an in-process rewrite, never a
//     recovery-lane error at the commit point);
//     3b. verdict binding (gate steps only): a completed envelope whose
//     output carries a top-level fail verdict is re-bound to
//     gate_failed BEFORE anything else is judged. The re-bind is
//     visible to the caller: res.Outcome differs from the delivered outcome after
//     this call, and the Run loop logs it as an EvGateVerdictBound
//     event;
//     3b'. the human-gate guarantee (#138): judged on the DELIVERED
//     outcome so a gate-fail verdict cannot dodge the human either;
//     3c. release verdict binding (the release bundle's `release` and
//     `deploy` steps only, judged on the DELIVERED outcome): a
//     completed envelope for one of them whose output carries a
//     top-level verdict is judged against that step's vocabulary. On
//     `release`: pass owes a non-empty version plus at least one
//     artifacts_published entry, fail owes a non-empty fail_reason,
//     and anything else (defer included) is outside the closed
//     [pass, fail] set. On `deploy`: pass owes a non-empty version
//     (the version now serving), fail owes a non-empty fail_reason,
//     defer (no deploy procedure in the capsule) owes neither, and
//     anything else is outside [pass, defer, fail]. A defer verdict on
//     any OTHER step is a validation error. The gate binding above
//     stays prefix-fail only, so a defer verdict clears the deploy
//     gate and the workflow proceeds, while a fail verdict (with its
//     fail_reason) still re-binds to gate_failed. A binding error is
//     an envelope rejection (the in-process retry lane), never a
//     silent reclassification;
//  4. the V4 command-evidence gate (RequireCommandEvidence steps): a
//     completed result must carry at least one command_evidence entry,
//     each with a non-empty argv and duration_ms > 0;
//  5. the committed-test-files gate (ReposDir set, any step): a
//     completed result whose output declares test_files must have every
//     listed path committed at HEAD in its task repo(s)
//     (checkTestFilesCommitted) — rejected like the evidence gate;
//  6. the declared output schema (OutputSchema set): a completed
//     result's output must validate against the step definition's
//     `output:` schema (polyforge#164 H3);
//  7. the authored-tests gate (ReposDir and AuthoredTests set): the
//     test files an earlier step authored must be unchanged since its
//     commit, unless a human answered in this attempt chain (then
//     res.rebaseline is set) (polyforge#164 H4).
//
// The minimal contract: an envelope missing the retired protocol
// fields is never judged invalid for its absence — there is no
// protocol version, no envelope id, no artifact manifest, no handoff
// digest reference and no hash or manifest verification anywhere.
//
// Non-completed outcomes (gate_failed, needs_input, incomplete) carry
// their disposition on other channels (the gate verdict, the pending
// question).
func ValidateStepResult(res *StepResult, spec ResultSpec) error {
	if res == nil {
		return errors.New("driver: nil step result")
	}
	// 1. shape.
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
	// 3c. the release verdict binding: judged on the DELIVERED outcome
	// (a completed envelope). A binding error is an envelope rejection
	// (the in-process retry lane re-dispatches the step with the typed
	// reason) — never a silent reclassification.
	if delivered == OutcomeCompleted {
		if err := releaseVerdictBinding(res); err != nil {
			return err
		}
	}
	// 4. the V4 command-evidence gate: a completed result (after the
	// verdict re-bind) of an evidence-owing step must carry well-formed
	// command_evidence.
	if res.Outcome == OutcomeCompleted && spec.RequireCommandEvidence {
		if err := checkCommandEvidence(res.CommandEvidence); err != nil {
			if len(res.evidenceDropped) > 0 {
				err = fmt.Errorf("%w (entries dropped while parsing the envelope: %s)", err, strings.Join(res.evidenceDropped, "; "))
			}
			return fmt.Errorf("driver: step %q completed without verification evidence: %w", spec.StepID, err)
		}
	}
	// 5. the committed-test-files gate (polyforge#156): declared test
	// files must survive a resume, which restores task repos at HEAD only.
	if res.Outcome == OutcomeCompleted && spec.ReposDir != "" {
		if err := checkTestFilesCommitted(spec.ReposDir, res.Output); err != nil {
			return fmt.Errorf("driver: step %q completed with %w", spec.StepID, err)
		}
	}
	// 6. the declared output schema (polyforge#164 H3): the schema the
	// prompt shows is the contract. While enforceOutputSchemas is false
	// a violation is recorded (res.schemaViolation -> the
	// output_schema_violation event), never rejected.
	if res.Outcome == OutcomeCompleted && spec.OutputSchema != nil {
		if err := checkOutputSchema(spec.OutputSchema, res.Output); err != nil {
			if enforceOutputSchemas {
				return fmt.Errorf("driver: step %q completed but its %w", spec.StepID, err)
			}
			res.schemaViolation = err.Error()
		}
	}
	// 6b. the pre-existing-failure boundary (polyforge#164 S6): the tests
	// that pin THIS change — the repro, the authored acceptance and
	// regression tests — can never be excused as failing on the base.
	if res.Outcome == OutcomeCompleted {
		if err := checkPreExisting(res.Output, spec.PreExisting, spec.StepID == verificationStepID); err != nil {
			return fmt.Errorf("driver: step %q completed with %w", spec.StepID, err)
		}
	}
	// 7. the authored-tests gate (polyforge#164 H4).
	if res.Outcome == OutcomeCompleted && spec.ReposDir != "" && spec.AuthoredTests != nil {
		changed, err := checkAuthoredTestsUnchanged(spec.ReposDir, spec.AuthoredTests, spec.HumanAnswers)
		if err != nil {
			return fmt.Errorf("driver: step %q completed with %w", spec.StepID, err)
		}
		res.rebaseline = len(changed) > 0
		res.rebaselined = changed
	}
	return nil
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
	// D11: the id is agent-chosen and travels into the human-facing
	// wake-up line, the answer verb and the event log — so its shape is
	// part of the envelope contract (the watcher's safeID filter stays as
	// defense in depth).
	if !questionIDShape.MatchString(id) {
		return id, text, fmt.Errorf("driver: needs_input question_id %q is malformed (must match %s)", id, questionIDShape)
	}
	return id, text, nil
}

// questionIDShape is the accepted needs_input question id: 1–64 chars of
// letters, digits, '.', '_' and '-'.
var questionIDShape = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

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
// bundle's verdict contract. It reads only the top-level scalar fields the
// output schemas name and mirrors the failVerdictInOutput tolerance: an
// output that is not a JSON object, or whose verdict is absent/non-string,
// is not this binding's business (the declared-schema lane stays its
// authority). `release`: closed vocabulary [pass, fail]; pass owes a
// non-empty version plus at least one artifacts_published entry. `deploy`:
// [pass, defer, fail]; pass owes a non-empty version, defer owes neither
// version nor artifacts_published. fail owes a non-empty fail_reason on
// both, and defer is illegal on every other step.
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
	isDeploy := res.StepID == deployStepID
	bound := isRelease || isDeploy
	switch {
	case verdict == "defer":
		if !isDeploy {
			return fmt.Errorf("driver: step %q output carries verdict %q — defer validates nowhere but the %s step's schema",
				res.StepID, v, deployStepID)
		}
		if nonEmptyJSONString(probe.Version) || len(probe.ArtifactsPublished) > 0 {
			return fmt.Errorf("driver: deploy output verdict %q owes neither version nor artifacts_published — a defer deploys nothing; report pass with the deployed version or defer with none", v)
		}
		return nil
	case strings.HasPrefix(verdict, "fail"):
		if !bound {
			return nil // the gate binding's business, not this one
		}
		if !nonEmptyJSONString(probe.FailReason) {
			return fmt.Errorf("driver: %s output verdict %q requires a non-empty fail_reason — the failure must say what went wrong", res.StepID, v)
		}
		return nil
	case verdict == "pass":
		switch {
		case isRelease:
			if !nonEmptyJSONString(probe.Version) || len(probe.ArtifactsPublished) == 0 {
				return fmt.Errorf("driver: release output verdict %q requires a non-empty version and at least one artifacts_published entry — pass means something was actually published", v)
			}
		case isDeploy:
			if !nonEmptyJSONString(probe.Version) {
				return fmt.Errorf("driver: deploy output verdict %q requires a non-empty version — pass means that version is now serving", v)
			}
		}
		return nil
	default:
		if isRelease {
			return fmt.Errorf("driver: release output verdict %q is outside the closed release vocabulary [pass, fail]", v)
		}
		if isDeploy {
			return fmt.Errorf("driver: deploy output verdict %q is outside the closed deploy vocabulary [pass, defer, fail]", v)
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
