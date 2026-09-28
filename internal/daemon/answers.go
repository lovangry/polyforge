package daemon

// answers.go: the Interactive answer relay (00 §3.4a, the single authority
// for the relay schema and dedup/expiry rules; 04 §5.1 only references it).
//
//   - The relay payload is forwarded VERBATIM (never parsed or rewritten by
//     the Daemon beyond the binding checks below).
//   - Binding: question_id must equal the current step's persisted
//     pending_question (03 §4.2 — the ONLY persistent comparison basis).
//     Mismatch or already-consumed → DAEMON-409; the answer is never
//     applied to a later question (question ids are freshly generated).
//   - Idempotency: a repeated answer_id returns the FIRST submission's
//     result verbatim and never applies twice — but ONLY once the first
//     submission actually SETTLED. A submission whose delivery FAILED
//     (00 §3.4a: the pf/answer ack is the durability signal, so an agent
//     dying before its ack is a delivery failure) stays REDELIVERABLE: the
//     same answer_id may be re-submitted and re-delivered, matching the
//     supervisor's failure-keeps-redeliverable behavior (SK-4/CR-3). The
//     idempotency entry is finalized/consumed only by a SUCCESSFUL durable
//     delivery (or a definitive binding refusal); a delivery error is
//     recorded as retryable, never as the permanent outcome.
//   - Identity: answered_by must equal the authenticated caller →
//     DAEMON-403 on mismatch.

import (
	"errors"
	"sync"
)

// AnswerRelay is the 00 §3.4a answer message (five fields, closed set).
type AnswerRelay struct {
	QuestionID string `json:"question_id"`
	AnswerID   string `json:"answer_id"`
	AnswerText string `json:"answer_text"`
	AnsweredAt string `json:"answered_at"`
	AnsweredBy string `json:"answered_by"`
}

// Validate checks the relay's closed-set field constraints (presence only;
// answer_text is deliberately NOT structurally parsed — the pf-execute main
// loop reads natural language).
func (a *AnswerRelay) Validate() error {
	if a.QuestionID == "" || a.AnswerID == "" || a.AnswerText == "" || a.AnsweredAt == "" || a.AnsweredBy == "" {
		return errors.New("daemon: answer relay requires all five fields non-empty")
	}
	return nil
}

// Answer error shapes (04 §5.2): 409 for question binding/expiry, 403 for
// identity mismatch.
var (
	ErrAnswerConsumed = errors.New("daemon: question_id does not match an open pending_question (DAEMON-409)")
	ErrAnswerIdentity = errors.New("daemon: answered_by does not match the authenticated caller (DAEMON-403)")
	// ErrAnswerInFlight is the transient refusal for a CONCURRENT duplicate
	// of an answer_id whose delivery is still running: the first delivery
	// has not settled, so there is no outcome to replay and re-entering the
	// deliver call would risk a double-apply. The caller retries.
	ErrAnswerInFlight = errors.New("daemon: answer delivery already in progress for this answer_id (DAEMON-409)")
)

// answerState is the lifecycle of one answer_id in the ledger.
type answerState int

const (
	// answerInflight: reserved, the delivery is running right now.
	answerInflight answerState = iota
	// answerFinal: settled. Replays return the recorded outcome verbatim
	// and never re-deliver (success, or a definitive binding refusal).
	answerFinal
	// answerRetryable: the delivery FAILED. The entry is not consumed:
	// the same answer_id may be re-submitted and re-delivered (00 §3.4a
	// failure-keeps-redeliverable). The recorded error is returned to the
	// submitter but never becomes the permanent outcome.
	answerRetryable
)

// answerOutcome is what a submission settled to.
type answerOutcome struct {
	state answerState
	err   error
}

// AnswerLedger holds the per-daemon applied-answer idempotency map.
type AnswerLedger struct {
	mu      sync.Mutex
	applied map[string]answerOutcome
}

// NewAnswerLedger creates the ledger.
func NewAnswerLedger() *AnswerLedger { return &AnswerLedger{applied: map[string]answerOutcome{}} }

// PendingSource is the persisted comparison basis: the current step's
// pending_question (or nil when the step is not waiting).
type PendingSource interface {
	// OpenQuestion returns the persisted pending question's id, or "" when
	// no question is open (step not in needs_input, or already consumed).
	OpenQuestion() (questionID, stepID string)
}

// Deliver hands a first-time answer to the WI Agent (which atomically
// writes pending_answer and resumes the step per 03 §4.2/D9).
type Deliver func(relay AnswerRelay) error

// Submit applies one answer relay. callerID is the authenticated identity
// of the submitter (must equal answered_by).
func (l *AnswerLedger) Submit(relay AnswerRelay, callerID string, src PendingSource, deliver Deliver) error {
	if err := relay.Validate(); err != nil {
		return err
	}
	if relay.AnsweredBy != callerID {
		return ErrAnswerIdentity
	}
	// Idempotency by answer_id BEFORE anything else: a FINALIZED answer_id
	// never produces a second effect and returns the first outcome verbatim.
	// A RETRYABLE entry (a previously FAILED delivery, 00 §3.4a) falls
	// through and is re-delivered; an IN-FLIGHT entry is a concurrent
	// duplicate of a delivery that has not settled yet — refuse transiently
	// so the delivery cannot be entered twice at once.
	l.mu.Lock()
	if prev, ok := l.applied[relay.AnswerID]; ok {
		if prev.state == answerFinal {
			l.mu.Unlock()
			return prev.err
		}
		if prev.state == answerInflight {
			l.mu.Unlock()
			return ErrAnswerInFlight
		}
		// answerRetryable: re-attempt below, re-reserving the slot.
	}
	// Reserve the slot BEFORE delivering so a concurrent duplicate of the
	// same answer_id cannot double-deliver while this call is in flight.
	l.applied[relay.AnswerID] = answerOutcome{state: answerInflight}
	l.mu.Unlock()

	outcome := answerOutcome{state: answerFinal}
	openQ, _ := src.OpenQuestion()
	if openQ == "" || openQ != relay.QuestionID {
		// A binding refusal is definitive: question ids are freshly
		// generated, so this answer can never match a later question.
		outcome.err = ErrAnswerConsumed
	} else if derr := deliver(relay); derr != nil {
		// Delivery failure (agent dead pre-ack, control path lost, ...):
		// the answer was NOT durably applied, so the entry stays
		// REDELIVERABLE — the error is reported to this submitter but the
		// same answer_id can be re-submitted and re-delivered later
		// (e.g. after agent re-derivation). Finalizing the entry here would
		// permanently lose the answer behind the idempotency map.
		outcome = answerOutcome{state: answerRetryable, err: derr}
	}
	l.mu.Lock()
	l.applied[relay.AnswerID] = outcome
	l.mu.Unlock()
	return outcome.err
}
