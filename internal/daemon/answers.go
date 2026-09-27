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
//     result verbatim and never applies twice.
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
)

// answerOutcome is what a first submission settled to; replays of the same
// answer_id get exactly this back.
type answerOutcome struct {
	err error
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
	// Idempotency by answer_id BEFORE anything else: the same answer_id
	// never produces a second effect and returns the first outcome.
	l.mu.Lock()
	if prev, ok := l.applied[relay.AnswerID]; ok {
		l.mu.Unlock()
		return prev.err
	}
	// Reserve the slot BEFORE delivering so a crash mid-delivery does not
	// double-apply on retry (a retry sees the recorded outcome shape).
	l.applied[relay.AnswerID] = answerOutcome{}
	l.mu.Unlock()

	outcome := answerOutcome{}
	openQ, _ := src.OpenQuestion()
	if openQ == "" || openQ != relay.QuestionID {
		outcome.err = ErrAnswerConsumed
	} else if derr := deliver(relay); derr != nil {
		outcome.err = derr
	}
	l.mu.Lock()
	l.applied[relay.AnswerID] = outcome
	l.mu.Unlock()
	return outcome.err
}
