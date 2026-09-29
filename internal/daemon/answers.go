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

// TWO-PHASE DELIVERY (polyforge#48, folded from #49/#50). The relay has
// always had two distinct moments and only one word for them. They are
// separated now:
//
//   - DELIVERY: the relay reached the agent's control channel. Confirmed
//     synchronously by the write succeeding.
//   - CONSUMPTION: the agent persisted pending_answer and acked. That is
//     a whole model turn away — the pilot measured 45-90 seconds, and a
//     large context makes it longer.
//
// The ack window is therefore turn-scale, and its expiry is reported as
// ErrAnswerAckPending: delivered, consumption pending, NOT an error. The
// ledger entry stays redeliverable so a crash continuation re-delivers
// the SAME answer_id, and the agent's own answer_id/question_id binding
// makes that re-delivery idempotent — the daemon needs no second durable
// record of its own (00 §3.4a: the capsule already owns the data).

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
	// awaiting holds, per WORK ITEM, the relays that were DELIVERED but
	// whose consumption the daemon never observed (ack pending, or the
	// delivery failed outright). It is the re-delivery source for a crash
	// continuation / new derivation: the pilot of 2026-09-29 01:20
	// measured the relay NOT being re-armed for a re-derived agent, so a
	// hang during consumption lost the answer permanently even though the
	// three-state ledger said "retryable".
	//
	// In-memory ONLY, and deliberately so: a daemon restart has the
	// capsule's own pending_question/pending_answer to reason from, and a
	// second durable record of data the capsule owns is exactly the shape
	// 00 §3.4a rules out. This closes the RUNTIME gap (the agent died, the
	// daemon did not), which is the measured one.
	awaiting map[string]map[string]AnswerRelay
}

// NewAnswerLedger creates the ledger.
func NewAnswerLedger() *AnswerLedger {
	return &AnswerLedger{
		applied:  map[string]answerOutcome{},
		awaiting: map[string]map[string]AnswerRelay{},
	}
}

// noteAwaiting records a relay whose consumption is not yet observed.
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

// clearAwaiting forgets one relay (its consumption was observed, or the
// question it binds is gone).
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
// to this work item's agent and its consumption never observed.
//
// This is the daemon's own half of the polyforge#48/#50 exemption
// narrowing, and it covers the window the capsule cannot: between the
// relay reaching the agent and the agent persisting pending_answer, the
// capsule still shows a bare open question — indistinguishable from
// "waiting for a human" — while the truth is "the answer is in the
// agent's hands and it is working". The pilot's hang landed exactly
// there. The daemon knows, because it did the delivering.
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
// daemon has not observed, for re-delivery after a re-derivation.
func (l *AnswerLedger) Awaiting(wi string) []AnswerRelay {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AnswerRelay, 0, len(l.awaiting[wi]))
	for _, r := range l.awaiting[wi] {
		out = append(out, r)
	}
	return out
}

// markRedeliverable re-opens one answer_id for another delivery attempt
// (a re-derivation is a new consumer; the previous attempt's outcome
// must not block it behind the idempotency map).
func (l *AnswerLedger) markRedeliverable(answerID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if prev, ok := l.applied[answerID]; ok && prev.state == answerFinal && prev.err == nil {
		// A CONSUMED answer is never re-delivered: that is the whole
		// point of the idempotency map.
		return
	}
	l.applied[answerID] = answerOutcome{state: answerRetryable}
}

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
// of the submitter (must equal answered_by). Kept for callers that have
// no work item id to hand (unit tests); SubmitFor is the production
// entry point.
func (l *AnswerLedger) Submit(relay AnswerRelay, callerID string, src PendingSource, deliver Deliver) error {
	return l.SubmitFor("", relay, callerID, src, deliver)
}

// SubmitFor is Submit bound to a work item, so a delivery whose
// CONSUMPTION was never observed can be re-delivered to a re-derived
// agent (polyforge#48 item 5).
func (l *AnswerLedger) SubmitFor(wi string, relay AnswerRelay, callerID string, src PendingSource, deliver Deliver) error {
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
	switch {
	case openQ == "" || openQ != relay.QuestionID:
		// A binding refusal is definitive: question ids are freshly
		// generated, so this answer can never match a later question.
		outcome.err = ErrAnswerConsumed
	default:
		derr := deliver(relay)
		switch {
		case derr == nil:
			// Delivered AND acked: consumption observed.
			l.clearAwaiting(wi, relay.AnswerID)
		case errors.Is(derr, ErrAnswerAckPending):
			// PHASE ONE ONLY (polyforge#48/#49): the relay reached the
			// agent, the ack window expired first. This is NOT a failure
			// — the measured case consumed the answer 45-90s later — so
			// the outcome is reported as the pending sentinel, and the
			// entry stays redeliverable in case the agent dies before it
			// finishes consuming.
			outcome = answerOutcome{state: answerRetryable, err: derr}
			l.noteAwaiting(wi, relay)
		default:
			// Delivery failure (agent dead pre-ack, control path lost,
			// ...): the answer was NOT durably applied, so the entry stays
			// REDELIVERABLE — the error is reported to this submitter but
			// the same answer_id can be re-submitted and re-delivered
			// later (e.g. after agent re-derivation). Finalizing the entry
			// here would permanently lose the answer behind the
			// idempotency map.
			outcome = answerOutcome{state: answerRetryable, err: derr}
			l.noteAwaiting(wi, relay)
		}
	}
	l.mu.Lock()
	l.applied[relay.AnswerID] = outcome
	l.mu.Unlock()
	return outcome.err
}
