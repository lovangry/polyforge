package drive

// Package drive implements the pure execution-state transitions of the
// infrastructure-failure lane (01-workflow-skills.md §6.3) that the
// publish-time output-schema signal (03-execution-capsule.md §5.1a)
// triggers. The WI Agent (pf-execute) is the ONLY writer of
// runtime-state/execution.json and performs these transitions through its
// single-writer tmp+fsync+rename channel; this package is the mechanical
// reference for the semantics — a pure function set over the
// capsule.Execution value — used by the executor skill's described
// behavior, by tests that exercise the signal's consumption path, and by
// any future drive implementation. It performs NO I/O and holds no state.
//
// Bounded candidate advance (INV-1g): the candidate pointer is NOT a new
// persisted field. It is recomputed from the attempts[] archive alone
// (01 §6.3 step 4), so a crash-recovered WI Agent derives the same
// ordinal as its predecessor without any process memory.
import (
	"fmt"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// Attempt causes (01 §6.3b): the closed two-value set of archived
// attempts. Q&A history never lands in attempts[].
const (
	CauseGateFailRewind = "gate_fail_rewind"
	CauseInfraFailure   = "infra_failure"
)

// AppendInfraFailure appends one archived interrupt record to the step's
// attempts[] (01 §6.3 step 2b): {attempt_seq, started_at, interrupted_at,
// cause: "infra_failure", detail, consumed_budget}. The caller persists
// the result in ONE atomic write together with whatever status transition
// accompanies it (the falsified-done rewrite below, or the in-place
// resume of an in_progress step). attempt_seq is strictly increasing per
// step (max existing + 1).
func AppendInfraFailure(st *capsule.Step, startedAt, interruptedAt, detail string, consumed *capsule.Budget) {
	seq := 0
	for _, a := range st.Attempts {
		if a.AttemptSeq > seq {
			seq = a.AttemptSeq
		}
	}
	st.Attempts = append(st.Attempts, capsule.Attempt{
		Cause:          CauseInfraFailure,
		AttemptSeq:     seq + 1,
		StartedAt:      startedAt,
		InterruptedAt:  interruptedAt,
		Detail:         detail,
		ConsumedBudget: consumed,
	})
}

// CandidateOrdinal recomputes the effective model-candidate ordinal for a
// step (01 §6.3 step 4): ordinal = 1 + the count of infra_failure entries
// in attempts[] after the last gate_fail_rewind entry (counting from the
// start when there is none), capped by min() against the candidate list
// length. The ordinal is 1-BASED: ordinal 1 is the list's first entry —
// the one the first dispatch uses. Advancing is bounded by the list
// length: once the ordinal reaches the length it stays there and
// re-dispatch keeps using the last candidate; exhausting candidates is
// NOT a separate terminal condition.
//
// listLen <= 0 encodes the no-candidate fallback (01 §6.3: both the step
// level and defaults.model_candidates absent): no switchable list exists,
// the formula does not apply, and the re-dispatch keeps the harness
// adapter's default model. The sentinel returned is 0.
func CandidateOrdinal(st *capsule.Step, listLen int) int {
	if listLen <= 0 {
		return 0 // no-candidate fallback: harness adapter default model
	}
	count := 0
	for _, a := range st.Attempts {
		switch a.Cause {
		case CauseGateFailRewind:
			count = 0 // counting restarts after the last rewind archive
		case CauseInfraFailure:
			count++
		}
	}
	ordinal := count + 1
	if ordinal > listLen {
		ordinal = listLen
	}
	return ordinal
}

// FalsifiedDoneRewrite is the consumption of one publish-time output-schema
// mismatch (03 §5.1a point 4 / 01 §6.3 step 5, the ONLY case where a done
// step is resumed): in ONE transition (the executor persists it in one
// atomic tmp+fsync+rename write) it
//
//   - rewrites the step's status from done to in_progress — NOT to
//     pending: the business-result lane is untouched, no gate FAIL
//     semantics, no terminal state is added;
//   - appends the infra_failure attempt archive (AppendInfraFailure);
//   - returns the recomputed effective candidate ordinal for the
//     re-dispatch, given the step's effective candidate list length.
//
// The prior consumed budget stays counted (01 §6.3 step 5); nothing is
// rolled back here — budget bookkeeping belongs to the executor's own
// write.
func FalsifiedDoneRewrite(exec *capsule.Execution, stepID, detail string, listLen int, now time.Time) (ordinal int, err error) {
	if exec == nil || exec.Steps == nil {
		return 0, fmt.Errorf("drive: execution has no steps")
	}
	st, ok := exec.Steps[stepID]
	if !ok || st == nil {
		return 0, fmt.Errorf("drive: step %q not found in execution.json", stepID)
	}
	if st.Status != capsule.StepDone {
		return 0, fmt.Errorf("drive: falsified-done rewrite requires status %q, step %q is %q",
			capsule.StepDone, stepID, st.Status)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	AppendInfraFailure(st, st.StepStartedAt, stamp, detail, nil)
	st.Status = capsule.StepInProgress
	return CandidateOrdinal(st, listLen), nil
}
