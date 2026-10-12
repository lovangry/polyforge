// candidates.go: the v2 model chain (R4 — unified) as a PURE function
// set. The candidate pointer is not a persisted field: it is recomputed
// from the step's archived attempts alone, so a crash-recovered driver
// derives the same ordinal as its predecessor without any process
// memory (the mirror rule of v0/internal/workflow/drive/drive.go (git history)
// CandidateOrdinal, extended with the v2 fault classes: only a
// PROVIDER-class infra failure moves the pointer).
//
// Advancement policy per fault class (the A-chain runtime advancement
// in runtime/recover.go advanceCandidate is the same rule statement —
// the count advances on provider failures alone):
//
//	provider  → advance candidate (bounded: capped at the list end,
//	            exhausting the list is NOT a terminal condition)
//	host      → do NOT advance, report blocked (disk full, tool
//	            missing — a different model will not fix the machine)
//	operator  → do NOT advance, not a failure (an interruption is a
//	            stop, not a model verdict)
//	schema    → do NOT advance, report to owner (a result-contract
//	            violation is a bundle/step authoring problem)
package driver

import (
	"context"
	"errors"
	"strings"
)

// FaultClass is the classification of one step-attempt failure.
type FaultClass string

const (
	FaultProvider FaultClass = "provider"
	FaultHost     FaultClass = "host"
	FaultOperator FaultClass = "operator"
	FaultSchema   FaultClass = "schema"
)

// Attempt causes (the closed two-value set of archived attempts, v0
// mirror): Q&A history never lands in attempts[].
const ()

// StepAttempt is the attempts[] entry the B-chain recomputation reads —
// capsule.Attempt reduced to the two fields the model chain needs: why
// the attempt ended (cause) and, for infra failures, the classified
// fault class recorded at archive time.
type StepAttempt struct {
	Cause      string     `json:"cause"`
	FaultClass FaultClass `json:"fault_class,omitempty"`
}

// CandidatePointer answers the 0-based index into candidates of the
// step's current effective model, recomputed purely from the archived
// attempts (R4, the B-chain formula):
//
//	index = count of PROVIDER-class infra failures recorded after the
//	        most recent gate_fail_rewind entry (from the start when
//	        there is none), capped at len(candidates)-1
//
// Host, operator and schema failures NEVER move the pointer; a
// gate-fail rewind RESETS the count (the re-run starts over at the
// chain's head). Advancing is bounded, never wrapping: once the count
// reaches the last candidate it stays pinned there — exhausting the
// list is not a terminal condition (the storm guard owns terminality,
// not the candidate list).
//
// An empty candidate list answers -1: the no-candidate fallback (both
// the step level and defaults.model_candidates absent) — no switchable
// chain exists and the harness adapter's default model stays in effect.
func CandidatePointer(attempts []StepAttempt, candidates []string) int {
	if len(candidates) == 0 {
		return -1
	}
	count := 0
	for _, a := range attempts {
		switch a.Cause {
		case CauseGateFailRewind:
			count = 0 // counting restarts after the last rewind archive
		case CauseInfraFailure:
			// Provider-class faults advance; host/operator/schema do not.
			// An empty FaultClass (unclassified) does NOT advance —
			// classification is the caller's (adapter) duty.
			if a.FaultClass == FaultProvider {
				count++
			}
		}
		// unknown causes are ignored, not fatal: the pointer is a
		// recomputation over history, and history may predate a cause
		// rename
	}
	if count > len(candidates)-1 {
		count = len(candidates) - 1
	}
	return count
}

// EffectiveModel answers the model id the next dispatch of the step
// must be pinned to ("" when no switchable chain exists — the harness
// adapter's default). The runtime mirror is job.effectiveModel.
func EffectiveModel(attempts []StepAttempt, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	return candidates[CandidatePointer(attempts, candidates)]
}

// CandidatePolicy is the advancement disposition of one fault class:
// whether a failed attempt of that class moves the candidate pointer
// (Advance), whether the step reports blocked pending host/operator
// intervention (Block), and whether the owner must be notified
// (Report).
type CandidatePolicy struct {
	Advance bool
	Block   bool
	Report  bool
}

// PolicyFor answers the advancement policy of one fault class:
//
//	provider → advance (bounded)
//	host     → block + report: the machine, not the model, is broken
//	operator → nothing: an interruption is not a failure
//	schema   → report: the result contract, not the model, is broken
//
// An unclassified value answers the all-false policy — never advance on
// an unknown class.
func PolicyFor(class FaultClass) CandidatePolicy {
	switch class {
	case FaultProvider:
		return CandidatePolicy{Advance: true}
	case FaultHost:
		return CandidatePolicy{Block: true, Report: true}
	case FaultOperator:
		return CandidatePolicy{}
	case FaultSchema:
		return CandidatePolicy{Report: true}
	default:
		return CandidatePolicy{}
	}
}

// faultNeedles is the substring classification table, checked in order:
// operator first (an explicit stop must never be re-read as an infra
// failure), then schema (result-contract violations name their own
// domain), then host (resource/environment strings), then provider.
// Substring matching is deliberate — the harnesses and the OS carry
// free text here (the frozen observe/stream.go precedent: the harness
// has no dedicated error record; failures ride ordinary messages as
// free text) — so every needle is lowercase and matched case-blind.
var faultNeedles = []struct {
	class   FaultClass
	needles []string
}{
	{FaultOperator, []string{
		"interrupt", "cancel", "operator", "stop notice", "paused",
		"sigint", "sigterm", "shutdown",
	}},
	{FaultSchema, []string{
		"schema", "validat", "artifact", "declared output", "hash mismatch",
		"output mismatch", "manifest",
	}},
	{FaultHost, []string{
		"disk full", "no space left", "enoent", "no such file", "exec format",
		"permission denied", "out of memory", "cannot allocate", "too many open files",
		"cgroup", "fork",
	}},
	{FaultProvider, []string{
		"rate limit", "quota", "overloaded", "429", "502", "503", "504",
		"timeout", "timed out", "deadline", "api error", "provider",
		"upstream", "model", "unauthorized", "invalid api key", "authentication",
		"billing", "credit", "connection reset", "connection refused", "eof",
	}},
}

// ClassifyFault classifies one step-attempt error into the four fault
// classes. Sentinel context errors are recognized first (wrapped or
// not): a cancellation is an operator interruption, a deadline is a
// timeout on the model call (provider). An unmatched message defaults
// to provider — the bounded side of the policy: advancing the candidate
// is capped by the list length and never terminal, whereas defaulting
// to host would report blocked on an unknown free-text string. A nil
// error answers "" (nothing to classify).
func ClassifyFault(err error) FaultClass {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return FaultOperator
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FaultProvider
	}
	msg := strings.ToLower(err.Error())
	for _, grp := range faultNeedles {
		for _, needle := range grp.needles {
			if strings.Contains(msg, needle) {
				return grp.class
			}
		}
	}
	return FaultProvider
}
