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
	"time"
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
	// wi is the OWNING work item. The in-memory idempotency map is
	// keyed by answer_id alone (ids are globally unique), but the
	// persisted file must be prunable per work item (spec A4:
	// RTFinished drops that work item's entries), so the owner is
	// carried here.
	wi string
	// relay is the submitted relay verbatim. It is what the serializer
	// projects an IN-FLIGHT reservation onto disk with (as retryable +
	// awaiting), so "a persisted entry exists before delivery starts"
	// holds without the file ever carrying an in-flight marker.
	relay AnswerRelay
	// settledAt is the moment this state was recorded — the load-time
	// age-out input (spec A4).
	settledAt time.Time
}

// answerStore is the ledger's optional durable backend: the daemon-wide
// answers.json under the observe data directory. nil ⇒ the ledger is
// memory-only and every persist is a no-op.
//
// LOCK ORDER IS ALWAYS store.mu → AnswerLedger.mu. persist takes the
// store lock first, snapshots under the ledger lock, then writes, so
// two concurrent settles can never land on disk out of order. persist
// is NEVER called while the ledger lock is held.
type answerStore struct {
	mu      sync.Mutex
	dataDir string
	rec     ledgerRecorder
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
	// Runtime's context injection). Since F9 the monitor closes the loop:
	// at every instance attachment (RTStarted/RTRecovered landing) the
	// entries whose question is still the open pending one are re-sent
	// to the new handle automatically (monitor.redeliverAwaiting), and
	// the manual CLI re-submit of the same answer_id re-delivers
	// idempotently on top.
	//
	// DURABILITY: both halves are mirrored to answers.json under the
	// daemon data directory (see answerfile.go) whenever the ledger
	// settles, so a daemon restart does not silently swallow an
	// unconsumed owner answer and does not turn every restart into an
	// idempotency hole. The AUTHORITY SPLIT is unchanged: the capsule's
	// execution.json still owns the open pending question and every
	// binding decision is still made against it — this file is only a
	// DELIVERY-STATUS CACHE recording what the daemon itself did. Losing
	// it costs at worst one owner re-submission, which is why no load
	// failure may block startup.
	awaiting map[string]map[string]AnswerRelay

	// store is the durable backend; nil = memory-only (the constructor
	// below, used by the unit tests and by any caller with no data dir).
	store *answerStore
}

// NewAnswerLedger creates a MEMORY-ONLY ledger (no backing file); every
// persist is a no-op. Production builds the daemon-wide ledger with
// NewAnswerLedgerAt instead.
func NewAnswerLedger() *AnswerLedger {
	return &AnswerLedger{
		applied:  map[string]answerOutcome{},
		awaiting: map[string]map[string]AnswerRelay{},
	}
}

// NewAnswerLedgerAt creates the DAEMON-WIDE ledger backed by
// <dataDir>/answers.json, loading whatever the previous daemon left
// there. record (may be nil) receives the ledger's own bookkeeping
// events; the hub binds it to the events log.
//
// It never fails: a missing file is first boot, and an unreadable,
// unparsable or unknown-version file is quarantined and replaced by an
// empty ledger (spec A3 — the capsule is the authority, so a bad cache
// must never stop the daemon from starting). An empty dataDir yields a
// memory-only ledger.
//
// The load also applies the A4 age-out; when it dropped anything the
// trimmed document is written straight back.
func NewAnswerLedgerAt(dataDir string, record func(kind string, body map[string]any)) *AnswerLedger {
	l := NewAnswerLedger()
	if dataDir == "" {
		return l
	}
	rec := ledgerRecorder(record)
	doc, pruned := loadAnswerFile(dataDir, rec, time.Now())
	l.store = &answerStore{dataDir: dataDir, rec: rec}
	l.hydrate(doc)
	if pruned {
		l.persist()
	}
	return l
}

// hydrate fills a fresh ledger from a loaded document. It can NEVER
// produce an answerInflight entry: the on-disk enum has no encoding for
// it, so a crash mid-delivery reloads as retryable + awaiting and the
// owner's re-submission is not refused with ErrAnswerInFlight.
func (l *AnswerLedger) hydrate(doc answerFileDoc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for wi, m := range doc.Awaiting {
		if wi == "" || len(m) == 0 {
			continue
		}
		cp := make(map[string]AnswerRelay, len(m))
		for id, r := range m {
			cp[id] = r
		}
		l.awaiting[wi] = cp
	}
	for id, e := range doc.Applied {
		if id == "" {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, e.SettledAt)
		out := answerOutcome{
			state:     decodeAnswerState(e.State),
			err:       decodeAnswerErr(e.ErrorCode, e.ErrorMessage),
			wi:        e.WorkItem,
			settledAt: ts,
		}
		if m := doc.Awaiting[e.WorkItem]; m != nil {
			out.relay = m[id]
		}
		l.applied[id] = out
	}
}

// snapshot renders the current ledger as the on-disk document. The
// IN-FLIGHT PROJECTION lives here: a reserved-but-unsettled slot is
// written as retryable plus its awaiting relay — the file records
// settled states only, and that is exactly the state a crash
// mid-delivery must reload as.
func (l *AnswerLedger) snapshot() answerFileDoc {
	l.mu.Lock()
	defer l.mu.Unlock()
	doc := newAnswerFileDoc()
	for wi, m := range l.awaiting {
		cp := make(map[string]AnswerRelay, len(m))
		for id, r := range m {
			cp[id] = r
		}
		doc.Awaiting[wi] = cp
	}
	now := time.Now()
	for id, o := range l.applied {
		settled := o.settledAt
		if settled.IsZero() {
			settled = now
		}
		code, msg := encodeAnswerErr(o.err)
		doc.Applied[id] = answerFileEntry{
			State:        encodeAnswerState(o.state),
			ErrorCode:    code,
			ErrorMessage: msg,
			WorkItem:     o.wi,
			SettledAt:    settled.UTC().Format(time.RFC3339Nano),
		}
		if o.state != answerInflight || o.wi == "" || o.relay.AnswerID == "" {
			continue
		}
		// The in-flight half of the projection: the accepted answer is on
		// disk as re-deliverable before its delivery even begins.
		if doc.Awaiting[o.wi] == nil {
			doc.Awaiting[o.wi] = map[string]AnswerRelay{}
		}
		if _, ok := doc.Awaiting[o.wi][id]; !ok {
			doc.Awaiting[o.wi][id] = o.relay
		}
	}
	return doc
}

// persist writes the whole ledger to its backing file. BEST EFFORT by
// design: a write failure is recorded as one answer_ledger event and
// never fails the submission that triggered it — the capsule remains
// the authority, so a stale cache is a recoverable annoyance while a
// refused answer is a lost one. Must NOT be called with l.mu held.
func (l *AnswerLedger) persist() {
	s := l.store
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := l.snapshot()
	if err := writeAnswerFile(s.dataDir, doc); err != nil {
		s.rec.record(map[string]any{
			"detail": "write_failed",
			"error":  err.Error(),
			"path":   answerFilePath(s.dataDir),
		})
	}
}

// ForgetWorkItem drops every trace of one work item (spec A4: the
// RTFinished path). Both halves go — a finished work item can neither
// receive a re-delivery nor be re-answered, so nothing is lost and the
// file cannot grow without bound.
func (l *AnswerLedger) ForgetWorkItem(wi string) {
	if wi == "" {
		return
	}
	l.mu.Lock()
	_, hadAwaiting := l.awaiting[wi]
	delete(l.awaiting, wi)
	dropped := hadAwaiting
	for id, o := range l.applied {
		if o.wi == wi {
			delete(l.applied, id)
			dropped = true
		}
	}
	l.mu.Unlock()
	if dropped {
		l.persist()
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
	if m := l.awaiting[wi]; m != nil {
		delete(m, answerID)
		if len(m) == 0 {
			delete(l.awaiting, wi)
		}
	}
	l.mu.Unlock()
	l.persist()
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
	m := l.awaiting[wi]
	for id, r := range m {
		if r.QuestionID == questionID {
			delete(m, id)
		}
	}
	if len(m) == 0 {
		delete(l.awaiting, wi)
	}
	l.mu.Unlock()
	l.persist()
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
	// cannot double-deliver while this call is in flight. The owner and
	// the verbatim relay ride the reservation so the serializer can
	// project it onto disk as retryable + awaiting.
	l.applied[relay.AnswerID] = answerOutcome{
		state: answerInflight, wi: wi, relay: relay, settledAt: time.Now(),
	}
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
	outcome.wi = wi
	outcome.relay = relay
	outcome.settledAt = time.Now()
	l.applied[relay.AnswerID] = outcome
	l.mu.Unlock()
	l.persist()
	return delivered, outcome.err
}
