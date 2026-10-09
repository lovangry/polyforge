package observe

// answers.go: the answer ledger's surviving core (polyforge#106 trim).
//
// The legacy WI-Agent answer relay — the two-phase DELIVERY/CONSUMPTION
// machinery with its ack-window expiry surface, the awaiting set and
// the attach-time re-delivery lane — is deleted with the engine that
// spoke it: on the driver lane the CONSUMER of an answer is the DRIVER
// (the D9 archive-and-clear write on the capsule's execution.json), and
// the capsule is the answer's persistent authority. What remains here
// is the daemon-side delivery-status CACHE file (spec A1–A4): the
// answers.json document is loaded at startup (corrupt files are
// quarantined, never fatal), entries age out, and a finished work
// item's entries are dropped wholesale (ForgetWorkItem, the RTFinished
// retention rule). Losing the cache costs nothing the capsule does not
// already hold.
//
// AUTHORITY SPLIT (unchanged): the capsule's runtime-state/execution.json
// stays the persistent authority for the open pending question and the
// archived answers; this file only remembers what a previous daemon
// process recorded. No load failure may ever block daemon startup.

import (
	"errors"
	"sync"
	"time"
)

// Answer error classes a recorded (pre-trim or bound-refusal) entry can
// carry. The relay that produced them over the live control channel is
// deleted; the sentinels survive only as the recorded history's
// identity (the codec's closed code set).
var (
	ErrAnswerConsumed = errors.New("observe: question_id does not match an open pending question (DAEMON-409)")
	ErrAnswerIdentity = errors.New("observe: answered_by does not match the authenticated caller (DAEMON-403)")
)

// answerFileEntry (see answerfile.go) is the only record shape; the
// in-memory ledger keeps the settled outcomes it loaded and prunes them
// by age, but nothing writes new entries anymore — the relay that
// produced them is gone.

// answerOutcome is one settled record loaded from the cache file.
type answerOutcome struct {
	err       error
	wi        string
	settledAt time.Time
}

// answerStore is the ledger's optional durable backend: the daemon-wide
// answers.json under the observe data directory. nil ⇒ the ledger is
// memory-only and every persist is a no-op.
type answerStore struct {
	mu      sync.Mutex
	dataDir string
	rec     ledgerRecorder
}

// AnswerLedger holds the settled records the cache file carried at
// startup (the idempotency-replay and retention faces). Nothing appends
// to it anymore; it exists so a restarted daemon still honors the
// retention rule on what earlier processes wrote and so the file
// machinery (quarantine, age-out, per-WI forget) keeps its contract.
type AnswerLedger struct {
	mu      sync.Mutex
	applied map[string]answerOutcome

	// store is the durable backend; nil = memory-only.
	store *answerStore
}

// NewAnswerLedger creates a MEMORY-ONLY ledger (no backing file); every
// persist is a no-op.
func NewAnswerLedger() *AnswerLedger {
	return &AnswerLedger{
		applied: map[string]answerOutcome{},
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

// hydrate fills a fresh ledger from a loaded document. Any state the
// on-disk enum carried loads as a settled record — the relay that could
// still act on an unsettled one is gone, so every entry is history.
func (l *AnswerLedger) hydrate(doc answerFileDoc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, e := range doc.Applied {
		if id == "" {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, e.SettledAt)
		l.applied[id] = answerOutcome{
			err:       decodeAnswerErr(e.ErrorCode, e.ErrorMessage),
			wi:        e.WorkItem,
			settledAt: ts,
		}
	}
}

// snapshot renders the current ledger as the on-disk document.
func (l *AnswerLedger) snapshot() answerFileDoc {
	l.mu.Lock()
	defer l.mu.Unlock()
	doc := newAnswerFileDoc()
	now := time.Now()
	for id, o := range l.applied {
		settled := o.settledAt
		if settled.IsZero() {
			settled = now
		}
		code, msg := encodeAnswerErr(o.err)
		doc.Applied[id] = answerFileEntry{
			State:        stateNameFinal,
			ErrorCode:    code,
			ErrorMessage: msg,
			WorkItem:     o.wi,
			SettledAt:    settled.UTC().Format(time.RFC3339Nano),
		}
	}
	return doc
}

// persist writes the whole ledger to its backing file. BEST EFFORT by
// design: a write failure is recorded as one answer_ledger event and
// never fails the caller. Must NOT be called with l.mu held.
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
// RTFinished path). A finished work item can neither receive nor be
// re-answered, so its entries go from memory and from the file.
func (l *AnswerLedger) ForgetWorkItem(wi string) {
	if wi == "" {
		return
	}
	l.mu.Lock()
	dropped := false
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
