package observe

// answers.go: the interactive answer relay — the single authority for the
// relay schema and dedup/expiry rules (00 §3.4a semantics, v1 protocol:
// the delivery rides the stdio control channel as pf/answer).
//
//   - The relay payload is forwarded VERBATIM (never parsed or rewritten
//     beyond the binding checks below).
//   - Binding: question_id must equal the currently open pending
//     question (the only persistent comparison basis — the monitor's
//     pending projection, seeded from flush(needs_input) and reconciled
//     against the capsule's execution.json pending_question). Mismatch
//     or already-consumed → binding refusal (definitive); the answer is
//     never applied to a later question (ids are freshly generated).
//   - Idempotency: a repeated answer_id returns the FIRST submission's
//     result verbatim and never applies twice — but ONLY once the first
//     submission actually SETTLED as consumed. A submission whose
//     delivery FAILED (the pf/answer ack is the durability signal, so
//     an agent dying before its ack is a delivery failure) stays
//     REDELIVERABLE: the same answer_id may be re-submitted and
//     re-delivered. The idempotency entry is finalized/consumed only by
//     a SUCCESSFUL durable delivery (or a definitive binding refusal).
//     A FAILURE NEVER CONSUMES THE ID.
//
// TWO-PHASE DELIVERY: the relay has two distinct moments.
//
//   - DELIVERY: the relay reached the agent's control channel, confirmed
//     synchronously by the write succeeding.
//   - CONSUMPTION: the agent persisted the answer and acked — a whole
//     model turn away (the frozen v0 pilot measured 45-90 seconds on a
//     real agent, and a large context makes it longer).
//
// Ack-window expiry is reported as ErrAnswerAckPending: delivered,
// consumption pending, NOT an error. The ledger entry stays
// redeliverable so the same answer_id can be re-submitted; the agent's
// own answer_id/question_id binding makes the re-delivery idempotent.

import (
	"errors"
	"sync"
)

// ErrAnswerAckPending is the two-phase outcome: delivery confirmed,
// consumption ack still pending. Not an error-grade outcome.
var ErrAnswerAckPending = errors.New("observe: answer delivered, agent consumption ack still pending (not an error)")

// AnswerRelay is the 00 §3.4a answer message (five fields, closed set).
type AnswerRelay struct {
	QuestionID string `json:"question_id"`
	AnswerID   string `json:"answer_id"`
	AnswerText string `json:"answer_text"`
	AnsweredAt string `json:"answered_at"`
	AnsweredBy string `json:"answered_by"`
}

// Validate checks the relay's closed-set field constraints (presence
// only; answer_text is deliberately NOT structurally parsed — the agent
// reads natural language).
func (a AnswerRelay) Validate() error {
	if a.QuestionID == "" || a.AnswerID == "" || a.AnswerText == "" || a.AnsweredAt == "" || a.AnsweredBy == "" {
		return errors.New("observe: answer relay requires all five fields non-empty")
	}
	return nil
}

// Answer error shapes: 409 for binding/expiry, 403 for identity.
var (
	ErrAnswerConsumed = errors.New("observe: question_id does not match an open pending question (DAEMON-409)")
	ErrAnswerIdentity = errors.New("observe: answered_by does not match the authenticated caller (DAEMON-403)")
	// ErrAnswerInFlight is the transient refusal for a CONCURRENT
	// duplicate of an answer_id whose delivery is still running: the
	// first delivery has not settled, so re-entering deliver would risk
	// a double-apply. The caller retries.
	ErrAnswerInFlight = errors.New("observe: answer delivery already in progress for this answer_id (DAEMON-409)")
)

// answerState is the lifecycle of one answer_id in the ledger.
type answerState int

const (
	// answerInflight: reserved, the delivery is running right now.
	answerInflight answerState = iota
	// answerFinal: settled as consumed (or a definitive binding
	// refusal). Replays return the recorded outcome verbatim and never
	// re-deliver.
	answerFinal
	// answerRetryable: the delivery FAILED (agent dead pre-ack, control
	// path lost) or its consumption was never observed (ack pending).
	// The entry is NOT consumed: the same answer_id may be re-submitted.
	answerRetryable
)

type answerOutcome struct {
	state answerState
	err   error
}

// AnswerLedger holds the per-WI applied-answer idempotency map plus the
// delivered-but-unconsumed set (see SubmitFor).
type AnswerLedger struct {
	mu      sync.Mutex
	applied map[string]answerOutcome
	// awaiting holds, per WORK ITEM, the relays that were DELIVERED but
	// whose consumption the monitor never observed (ack pending, or the
	// delivery failed outright). It is the daemon's own half of the
	// recovery projection: between the relay reaching the agent and the
	// agent persisting the answer, the capsule still shows a bare open
	// question, indistinguishable from "waiting for a human", while the
	// truth is "the answer is in the agent's hands" — or, after an
	// agent death, "the answer never landed".
	//
	// PENDING-QUESTION PROJECTION ACROSS RECOVERY: when the agent dies
	// with an answer in flight, the entry stays here and the pending
	// question stays visible in the JobView (execution.json carries
	// pending_question, which the NEW attempt's first turn sees through
	// Runtime's context injection — the new agent reads the capsule's
	// execution.json, so no daemon-side re-delivery is needed or
	// performed; the CLI may re-submit the same answer_id and this
	// ledger re-delivers it idempotently). In-memory only, deliberately:
	// the capsule already owns the durable data; a second durable
	// record of what the capsule owns is exactly the shape 00 §3.4a
	// rules out.
	awaiting map[string]map[string]AnswerRelay
}

// NewAnswerLedger creates the ledger.
func NewAnswerLedger() *AnswerLedger {
	return &AnswerLedger{
		applied:  map[string]answerOutcome{},
		awaiting: map[string]map[string]AnswerRelay{},
	}
}

func (l *AnswerLedger) noteAwaiting(wi string, relay AnswerRelay) {
	if wi == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.awaiting[wi] == nil {
		l.awaiting[wi] = map[string]AnswerRelay{}
	}
	l.awaiting[wi][relay.AnswerID] = relay
}

func (l *AnswerLedger) clearAwaiting(wi, answerID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m := l.awaiting[wi]; m != nil {
		delete(m, answerID)
		if len(m) == 0 {
			delete(l.awaiting, wi)
		}
	}
}

// AwaitingQuestion reports whether a relay for questionID was DELIVERED
// to this work item's agent and its consumption never observed (the
// monitor's needs_input exemption companion: an agent working on a
// delivered answer is not "waiting for a human").
func (l *AnswerLedger) AwaitingQuestion(wi, questionID string) bool {
	if wi == "" || questionID == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.awaiting[wi] {
		if r.QuestionID == questionID {
			return true
		}
	}
	return false
}

// Awaiting lists the relays of one work item whose consumption the
// monitor has not observed (diagnostic/observe face).
func (l *AnswerLedger) Awaiting(wi string) []AnswerRelay {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AnswerRelay, 0, len(l.awaiting[wi]))
	for _, r := range l.awaiting[wi] {
		out = append(out, r)
	}
	return out
}

// clearAwaitingQuestion forgets every delivered-but-unconsumed relay
// bound to one question (the agent consumed the question itself, e.g.
// it resolved the step without the human's answer landing).
func (l *AnswerLedger) clearAwaitingQuestion(wi, questionID string) {
	if wi == "" || questionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.awaiting[wi]
	for id, r := range m {
		if r.QuestionID == questionID {
			delete(m, id)
		}
	}
	if len(m) == 0 {
		delete(l.awaiting, wi)
	}
}

// PendingSource is the persisted comparison basis: the currently open
// pending question (or "" when no question is open).
type PendingSource interface {
	OpenQuestion() (questionID, stepID string)
}

// frozenPending freezes one PendingView snapshot for a delivery that runs
// outside the monitor goroutine (the binding basis is captured at
// submission time and cannot race a concurrent state change).
type frozenPending struct {
	id, step string
}

func (f frozenPending) OpenQuestion() (string, string) { return f.id, f.step }

// Deliver hands a first-time answer to the agent over the stdio control
// channel (pf/answer) and waits for the ack, bounded by the ack window.
// ErrAnswerAckPending means delivered-but-unconsumed.
type Deliver func(relay AnswerRelay) error

// SubmitFor applies one answer relay bound to a work item. callerID is
// the authenticated identity of the submitter (must equal answered_by).
// It returns whether THIS call actually delivered to the agent (false on
// an idempotent replay, a binding refusal or an identity refusal) plus
// the settled outcome: a replay returns the FIRST submission's outcome
// verbatim, delivered=false — the monitor must not re-announce it.
func (l *AnswerLedger) SubmitFor(wi string, relay AnswerRelay, callerID string, src PendingSource, deliver Deliver) (delivered bool, err error) {
	if err := relay.Validate(); err != nil {
		return false, err
	}
	if relay.AnsweredBy != callerID {
		return false, ErrAnswerIdentity
	}
	// Idempotency by answer_id BEFORE anything else: a FINALIZED id
	// never produces a second effect and returns the first outcome
	// verbatim; a RETRYABLE entry (previous delivery failed or its
	// consumption was never observed) falls through and is re-delivered;
	// an IN-FLIGHT entry is a concurrent duplicate of an unsettled
	// delivery — refuse transiently so the delivery cannot be entered
	// twice at once.
	l.mu.Lock()
	if prev, ok := l.applied[relay.AnswerID]; ok {
		if prev.state == answerFinal {
			l.mu.Unlock()
			return false, prev.err
		}
		if prev.state == answerInflight {
			l.mu.Unlock()
			return false, ErrAnswerInFlight
		}
		// answerRetryable: re-attempt below, re-reserving the slot.
	}
	// Reserve the slot BEFORE delivering so a concurrent duplicate
	// cannot double-deliver while this call is in flight.
	l.applied[relay.AnswerID] = answerOutcome{state: answerInflight}
	l.mu.Unlock()

	outcome := answerOutcome{state: answerFinal}
	openQ, _ := src.OpenQuestion()
	switch {
	case openQ == "" || openQ != relay.QuestionID:
		// A binding refusal is definitive: question ids are freshly
		// generated, so this answer can never match a later question.
		outcome.err = ErrAnswerConsumed
	default:
		delivered = true
		derr := deliver(relay)
		switch {
		case derr == nil:
			// Delivered AND acked: consumption observed.
			l.clearAwaiting(wi, relay.AnswerID)
		case errors.Is(derr, ErrAnswerAckPending):
			// PHASE ONE ONLY: the relay reached the agent, the ack
			// window expired first. NOT a failure — the entry stays
			// redeliverable in case the agent dies before finishing.
			delivered = false // delivery happened, consumption did not
			outcome = answerOutcome{state: answerRetryable, err: derr}
			l.noteAwaiting(wi, relay)
		default:
			// Delivery failure (agent dead pre-ack, control path lost,
			// ...): the answer was NOT durably applied, so the entry
			// stays REDELIVERABLE — the error is reported to this
			// submitter but the same answer_id can be re-submitted
			// later (e.g. after agent recovery). Finalizing here would
			// permanently lose the answer behind the idempotency map.
			delivered = false
			outcome = answerOutcome{state: answerRetryable, err: derr}
			l.noteAwaiting(wi, relay)
		}
	}
	l.mu.Lock()
	l.applied[relay.AnswerID] = outcome
	l.mu.Unlock()
	return delivered, outcome.err
}
