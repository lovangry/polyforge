package observe

// answers_test.go: the answer relay's idempotent THREE-STATE ledger —
// consumed / delivered-pending / redeliverable — driven through the
// monitor (the real path), plus the binding and identity refusals.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func armQuestion(t *testing.T, m *Monitor, qid string) {
	t.Helper()
	fc := FlushCall{Req: protocol.FlushRequest{Reason: protocol.FlushNeedsInput, QuestionID: qid, Question: "pick one"}, Resp: make(chan protocol.FlushAck, 1)}
	m.flushCh <- fc
	if ack := <-fc.Resp; !ack.OK {
		t.Fatalf("needs_input ack: %+v", ack)
	}
}

func submitAnswer(m *Monitor, relay AnswerRelay) error {
	ac := AnswerCall{Relay: relay, Caller: relay.AnsweredBy, Resp: make(chan error, 1)}
	m.answerCh <- ac
	select {
	case err := <-ac.Resp:
		return err
	case <-time.After(3 * time.Second):
		return errors.New("test: answer never settled")
	}
}

func relayFor(qid, aid string) AnswerRelay {
	return AnswerRelay{
		QuestionID: qid, AnswerID: aid, AnswerText: "do it",
		AnsweredAt: "2026-01-01T00:00:00Z", AnsweredBy: "alice",
	}
}

func TestAnswerRelayConsumedIsIdempotent(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("first submission: %v", err)
	}
	if h.sentCount() != 1 {
		t.Fatalf("deliveries: got %d, want 1", h.sentCount())
	}
	// A repeated answer_id replays the FIRST outcome verbatim and never
	// re-delivers.
	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if h.sentCount() != 1 {
		t.Fatalf("replay must not re-deliver: deliveries=%d", h.sentCount())
	}
	// Consumption observed → the pending projection cleared.
	m.stateMu.Lock()
	pending := m.pending
	m.stateMu.Unlock()
	if pending != nil {
		t.Fatalf("pending must clear after consumption, got %+v", pending)
	}
	if len(eventsOf(m, EvAnswered)) != 1 {
		t.Fatalf("answered events: got %d, want 1", len(eventsOf(m, EvAnswered)))
	}
}

func TestAnswerRelayAckTimeoutStaysPending(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	// The agent takes longer than the ack window: DELIVERED, consumption
	// NOT observed — the two-phase pending state, never an error.
	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		<-ctx.Done()
		return protocol.ControlAck{}, ErrAnswerAckPending
	})
	err := submitAnswer(m, relayFor("q1", "a1"))
	if !errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("outcome: got %v, want ErrAnswerAckPending", err)
	}
	// The entry stays redeliverable: a re-submit DELIVERS AGAIN.
	h.setSend(nil)
	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("re-delivery after pending: %v", err)
	}
	if h.sentCount() != 2 {
		t.Fatalf("deliveries: got %d, want 2 (pending never consumes the id)", h.sentCount())
	}
}

func TestAnswerRelayAgentDeadNeverConsumesID(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, errors.New("control path closed: agent exited")
	})
	err := submitAnswer(m, relayFor("q1", "a1"))
	if err == nil || errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("dead-agent delivery must fail, got %v", err)
	}
	// FAILURE NEVER CONSUMES THE ID: the same answer_id re-delivers to a
	// healthy successor (e.g. the recovered attempt).
	h.setSend(nil)
	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("re-delivery after agent death: %v", err)
	}
	if m.ledger.AwaitingQuestion("wi_test", "q1") {
		t.Fatal("awaiting set must clear on consumption")
	}
}

func TestAnswerRelayBindingAndIdentity(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	// Binding: a fresh question id never matches an open question.
	if err := submitAnswer(m, relayFor("q_other", "a_x")); !errors.Is(err, ErrAnswerConsumed) {
		t.Fatalf("binding refusal: got %v", err)
	}
	// A definitively refused id replays its refusal and never delivers.
	if err := submitAnswer(m, relayFor("q_other", "a_x")); !errors.Is(err, ErrAnswerConsumed) {
		t.Fatalf("refusal replay: got %v", err)
	}
	if h.sentCount() != 0 {
		t.Fatalf("no delivery may happen for a refused binding: %d", h.sentCount())
	}

	// Identity: answered_by must equal the caller.
	ac := AnswerCall{Relay: relayFor("q1", "a_y"), Caller: "mallory", Resp: make(chan error, 1)}
	m.answerCh <- ac
	if err := <-ac.Resp; !errors.Is(err, ErrAnswerIdentity) {
		t.Fatalf("identity refusal: got %v", err)
	}
}

func TestAnswerSurvivesAgentDeathForRecoveredAttempt(t *testing.T) {
	// The recovery projection (F9 semantics): an answer delivered but
	// never consumed before an agent death stays re-deliverable in the
	// ledger, and the monitor now re-sends it to the recovered handle
	// AUTOMATICALLY at the RTRecovered landing — the pending question
	// stays visible until that re-delivery is consumed. The manual CLI
	// re-submit path still works on top (idempotently).
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, nil)
	armQuestion(t, m, "q1")

	h.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, errors.New("agent died mid-delivery")
	})
	_ = submitAnswer(m, relayFor("q1", "a1")) // failure: stays redeliverable

	h2 := newFakeHandle("att_2")
	m.rtCh <- Attach{Msg: protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_test", AttemptID: "att_2"}, Handle: h2}
	soon(t, 500*time.Millisecond, "recovery adopted", func() bool { return m.currentAttempt() == "att_2" })

	// The awaiting answer was re-delivered to the NEW handle automatically
	// and consumed there: the pending question clears with it.
	soon(t, 2*time.Second, "auto re-delivery to the recovered handle", func() bool {
		return len(sentAnswers(h2)) == 1
	})
	if got := sentAnswers(h2); got[0].ID != "q1" {
		t.Fatalf("re-delivered relay bound to the wrong question: %+v", got[0])
	}
	soon(t, 2*time.Second, "re-delivered answer never settled consumed", func() bool {
		return len(m.ledger.Awaiting(m.wi.WorkItem)) == 0
	})
	view := m.JobView()
	if view.Pending != nil {
		t.Fatalf("pending must clear once the re-delivered answer is consumed: %+v", view.Pending)
	}

	// The manual re-submit path is unchanged: the finalized answer_id
	// replays its outcome verbatim and never re-delivers.
	if err := submitAnswer(m, relayFor("q1", "a1")); err != nil {
		t.Fatalf("manual re-submit of the finalized answer: %v", err)
	}
	if len(sentAnswers(h2)) != 1 {
		t.Fatalf("a finalized answer_id must never re-deliver")
	}
}

// ---------------------------------------------------------------------------
// the ledger's durable backend (plan step ledger_backend): a ledger
// built over a data directory mirrors BOTH halves into answers.json and
// a fresh ledger over the same directory behaves like the one that
// never left memory.

// pendingAt is a PendingSource fixed on one open question id.
func pendingAt(id string) PendingSource { return frozenPending{id: id, step: "code"} }

// recordingDeliver counts deliveries and returns a fixed outcome.
type recordingDeliver struct {
	mu   sync.Mutex
	sent []AnswerRelay
	err  error
}

func (d *recordingDeliver) fn() Deliver {
	return func(relay AnswerRelay) error {
		d.mu.Lock()
		d.sent = append(d.sent, relay)
		err := d.err
		d.mu.Unlock()
		return err
	}
}

func (d *recordingDeliver) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}

func readLedgerFile(t *testing.T, dir string) answerFileDoc {
	t.Helper()
	raw, err := os.ReadFile(answerFilePath(dir))
	if err != nil {
		t.Fatalf("read %s: %v", answerFilePath(dir), err)
	}
	var doc answerFileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", answerFilePath(dir), err)
	}
	doc.normalize()
	return doc
}

func TestLedgerPersistsBothHalvesAcrossAFreshLedger(t *testing.T) {
	dir := t.TempDir()
	l := NewAnswerLedgerAt(dir, nil)

	// a0: a definitive binding refusal — final, never awaiting.
	refused := relayFor("q_old", "a0")
	if _, err := l.SubmitFor("wi_a", refused, "alice", pendingAt("q1"), (&recordingDeliver{}).fn()); !errors.Is(err, ErrAnswerConsumed) {
		t.Fatalf("binding refusal: %v", err)
	}
	// a1: delivered, consumption never observed — retryable + awaiting.
	pending := relayFor("q1", "a1")
	d1 := &recordingDeliver{err: ErrAnswerAckPending}
	if _, err := l.SubmitFor("wi_a", pending, "alice", pendingAt("q1"), d1.fn()); !errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("ack-pending submission: %v", err)
	}

	// A FRESH ledger over the same directory — the restart.
	l2 := NewAnswerLedgerAt(dir, nil)
	awaiting := l2.Awaiting("wi_a")
	if len(awaiting) != 1 {
		t.Fatalf("awaiting half must survive: %+v", awaiting)
	}
	if awaiting[0] != pending {
		t.Fatalf("relay not verbatim across the reload:\n got %+v\nwant %+v", awaiting[0], pending)
	}
	if !l2.AwaitingQuestion("wi_a", "q1") {
		t.Fatal("the needs_input exemption companion must see the restored relay")
	}
	// The refused id replays its recorded outcome verbatim and never
	// delivers.
	d2 := &recordingDeliver{}
	delivered, err := l2.SubmitFor("wi_a", refused, "alice", pendingAt("q1"), d2.fn())
	if delivered || !errors.Is(err, ErrAnswerConsumed) {
		t.Fatalf("refusal replay after reload: delivered=%v err=%v", delivered, err)
	}
	if d2.count() != 0 {
		t.Fatalf("a replayed refusal must never deliver: %d", d2.count())
	}
	// The retryable id re-delivers.
	d3 := &recordingDeliver{}
	delivered, err = l2.SubmitFor("wi_a", pending, "alice", pendingAt("q1"), d3.fn())
	if !delivered || err != nil {
		t.Fatalf("retryable re-delivery after reload: delivered=%v err=%v", delivered, err)
	}
	if d3.count() != 1 {
		t.Fatalf("the restored retryable entry must re-deliver exactly once: %d", d3.count())
	}
	// Consumption observed: the awaiting half empties on disk too.
	if doc := readLedgerFile(t, dir); len(doc.Awaiting["wi_a"]) != 0 {
		t.Fatalf("the consumed relay must leave the persisted awaiting half: %+v", doc.Awaiting)
	}
}

func TestLedgerReplaysFinalNilOutcomeAfterReload(t *testing.T) {
	dir := t.TempDir()
	l := NewAnswerLedgerAt(dir, nil)
	relay := relayFor("q1", "a1")
	d := &recordingDeliver{}
	if delivered, err := l.SubmitFor("wi_a", relay, "alice", pendingAt("q1"), d.fn()); !delivered || err != nil {
		t.Fatalf("first submission: delivered=%v err=%v", delivered, err)
	}
	l2 := NewAnswerLedgerAt(dir, nil)
	d2 := &recordingDeliver{}
	delivered, err := l2.SubmitFor("wi_a", relay, "alice", pendingAt("q1"), d2.fn())
	if delivered || err != nil {
		t.Fatalf("post-reload replay must be (false, nil): delivered=%v err=%v", delivered, err)
	}
	if d2.count() != 0 {
		t.Fatalf("a finalized answer_id must never re-deliver after a reload: %d", d2.count())
	}
}

func TestLedgerLoadNeverYieldsInflight(t *testing.T) {
	dir := t.TempDir()
	// Hand-write a file that TRIES to carry an in-flight-looking state:
	// the loader must still produce a re-deliverable entry, never one
	// that refuses the owner's re-submission.
	doc := newAnswerFileDoc()
	doc.Awaiting["wi_a"] = map[string]AnswerRelay{"a1": relayFor("q1", "a1")}
	doc.Applied["a1"] = answerFileEntry{State: "inflight", WorkItem: "wi_a",
		SettledAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeAnswerFile(dir, doc); err != nil {
		t.Fatal(err)
	}
	l := NewAnswerLedgerAt(dir, nil)
	l.mu.Lock()
	state := l.applied["a1"].state
	l.mu.Unlock()
	if state == answerInflight {
		t.Fatal("no loaded entry may be in-flight")
	}
	d := &recordingDeliver{}
	delivered, err := l.SubmitFor("wi_a", relayFor("q1", "a1"), "alice", pendingAt("q1"), d.fn())
	if errors.Is(err, ErrAnswerInFlight) {
		t.Fatal("a loaded entry must never refuse a re-submission as in-flight")
	}
	if !delivered || err != nil {
		t.Fatalf("the loaded entry must re-deliver: delivered=%v err=%v", delivered, err)
	}
}

func TestLedgerForgetWorkItemEmptiesTheFileForThatWorkItem(t *testing.T) {
	dir := t.TempDir()
	l := NewAnswerLedgerAt(dir, nil)
	dA := &recordingDeliver{err: ErrAnswerAckPending}
	if _, err := l.SubmitFor("wi_a", relayFor("q1", "a1"), "alice", pendingAt("q1"), dA.fn()); !errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("wi_a submission: %v", err)
	}
	dB := &recordingDeliver{err: ErrAnswerAckPending}
	if _, err := l.SubmitFor("wi_b", relayFor("q2", "b1"), "alice", pendingAt("q2"), dB.fn()); !errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("wi_b submission: %v", err)
	}

	l.ForgetWorkItem("wi_a")

	doc := readLedgerFile(t, dir)
	if len(doc.Awaiting["wi_a"]) != 0 {
		t.Fatalf("the finished work item's awaiting half must be gone: %+v", doc.Awaiting)
	}
	if _, ok := doc.Applied["a1"]; ok {
		t.Fatalf("the finished work item's applied half must be gone: %+v", doc.Applied)
	}
	// The OTHER work item is untouched.
	if len(doc.Awaiting["wi_b"]) != 1 {
		t.Fatalf("a sibling work item must survive: %+v", doc.Awaiting)
	}
	if _, ok := doc.Applied["b1"]; !ok {
		t.Fatalf("a sibling applied entry must survive: %+v", doc.Applied)
	}
	// And the reload agrees.
	l2 := NewAnswerLedgerAt(dir, nil)
	if len(l2.Awaiting("wi_a")) != 0 || len(l2.Awaiting("wi_b")) != 1 {
		t.Fatalf("reload after ForgetWorkItem: wi_a=%+v wi_b=%+v", l2.Awaiting("wi_a"), l2.Awaiting("wi_b"))
	}
}

func TestMemoryOnlyLedgerWritesNothing(t *testing.T) {
	dir := t.TempDir()
	l := NewAnswerLedger() // no backend: every persist is a no-op
	d := &recordingDeliver{err: ErrAnswerAckPending}
	if _, err := l.SubmitFor("wi_a", relayFor("q1", "a1"), "alice", pendingAt("q1"), d.fn()); !errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("submission: %v", err)
	}
	if len(l.Awaiting("wi_a")) != 1 {
		t.Fatal("the in-memory half must still work")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("a memory-only ledger must write nothing: %v %v", entries, err)
	}
}

// ---------------------------------------------------------------------------
// the persist point (plan step persist_before_delivery): the entry is
// on disk as retryable + awaiting BEFORE the delivery function is
// entered. Everything below asserts from INSIDE deliver, which is the
// only place that can observe the invariant.

func TestPersistedEntryExistsBeforeDeliveryStarts(t *testing.T) {
	dir := t.TempDir()
	l := NewAnswerLedgerAt(dir, nil)
	relay := relayFor("q1", "a1")

	var (
		sawEntry  bool
		sawState  string
		sawRelay  AnswerRelay
		sawErrors []string
	)
	deliver := func(r AnswerRelay) error {
		// The first thing the delivery does is read the file.
		raw, err := os.ReadFile(answerFilePath(dir))
		if err != nil {
			sawErrors = append(sawErrors, err.Error())
			return nil
		}
		var doc answerFileDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			sawErrors = append(sawErrors, err.Error())
			return nil
		}
		doc.normalize()
		e, ok := doc.Applied[r.AnswerID]
		sawEntry = ok
		sawState = e.State
		sawRelay = doc.Awaiting["wi_a"][r.AnswerID]
		// No in-flight marker may appear anywhere in the file.
		if strings.Contains(string(raw), "inflight") || strings.Contains(string(raw), "in_flight") {
			sawErrors = append(sawErrors, "file carries an in-flight marker: "+string(raw))
		}
		return nil
	}
	if _, err := l.SubmitFor("wi_a", relay, "alice", pendingAt("q1"), deliver); err != nil {
		t.Fatalf("submission: %v", err)
	}
	if len(sawErrors) != 0 {
		t.Fatalf("delivery-entry observations: %v", sawErrors)
	}
	if !sawEntry {
		t.Fatal("a persisted entry MUST exist before delivery starts")
	}
	if sawState != stateNameRetryable {
		t.Fatalf("the pre-delivery entry must be retryable, got %q", sawState)
	}
	if sawRelay != relay {
		t.Fatalf("the pre-delivery awaiting relay must be the verbatim submission:\n got %+v\nwant %+v", sawRelay, relay)
	}
}

func TestConcurrentDuplicateIsInflightInMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	l := NewAnswerLedgerAt(dir, nil)
	relay := relayFor("q1", "a1")

	release := make(chan struct{})
	var dupErr error
	var fileRaw []byte
	deliver := func(r AnswerRelay) error {
		// A concurrent duplicate lands while this delivery is running.
		_, dupErr = l.SubmitFor("wi_a", relay, "alice", pendingAt("q1"), func(AnswerRelay) error {
			t.Error("the concurrent duplicate must never enter delivery")
			return nil
		})
		fileRaw, _ = os.ReadFile(answerFilePath(dir))
		close(release)
		return nil
	}
	if _, err := l.SubmitFor("wi_a", relay, "alice", pendingAt("q1"), deliver); err != nil {
		t.Fatalf("submission: %v", err)
	}
	<-release
	if !errors.Is(dupErr, ErrAnswerInFlight) {
		t.Fatalf("the concurrent duplicate must be refused in-flight FROM MEMORY, got %v", dupErr)
	}
	var doc answerFileDoc
	if err := json.Unmarshal(fileRaw, &doc); err != nil {
		t.Fatalf("parse mid-delivery file: %v", err)
	}
	doc.normalize()
	if e := doc.Applied["a1"]; e.State != stateNameRetryable {
		t.Fatalf("the file must show retryable, never an in-flight marker: %+v", e)
	}
}

func TestBindingRefusalPersistsFinalOnlyNeverAwaiting(t *testing.T) {
	dir := t.TempDir()
	l := NewAnswerLedgerAt(dir, nil)
	d := &recordingDeliver{}
	if _, err := l.SubmitFor("wi_a", relayFor("q_old", "a_x"), "alice", pendingAt("q1"), d.fn()); !errors.Is(err, ErrAnswerConsumed) {
		t.Fatalf("binding refusal: %v", err)
	}
	if d.count() != 0 {
		t.Fatalf("a refused binding must never deliver: %d", d.count())
	}
	doc := readLedgerFile(t, dir)
	if len(doc.Awaiting) != 0 {
		t.Fatalf("a refusal must persist NO awaiting relay: %+v", doc.Awaiting)
	}
	e, ok := doc.Applied["a_x"]
	if !ok || e.State != stateNameFinal || e.ErrorCode != errCodeConsumed {
		t.Fatalf("a refusal must persist exactly one final outcome: ok=%v %+v", ok, e)
	}
}

func TestValidationAndIdentityFailuresPersistNothing(t *testing.T) {
	dir := t.TempDir()
	l := NewAnswerLedgerAt(dir, nil)
	d := &recordingDeliver{}

	bad := relayFor("q1", "a1")
	bad.AnswerText = ""
	if _, err := l.SubmitFor("wi_a", bad, "alice", pendingAt("q1"), d.fn()); err == nil {
		t.Fatal("a malformed relay must be refused")
	}
	if _, err := l.SubmitFor("wi_a", relayFor("q1", "a2"), "mallory", pendingAt("q1"), d.fn()); !errors.Is(err, ErrAnswerIdentity) {
		t.Fatal("an identity mismatch must be refused")
	}
	if d.count() != 0 {
		t.Fatalf("neither refusal may deliver: %d", d.count())
	}
	// Both return BEFORE the slot reservation, so nothing was ever
	// written — the file does not even exist yet.
	if _, err := os.Stat(answerFilePath(dir)); !os.IsNotExist(err) {
		doc := readLedgerFile(t, dir)
		t.Fatalf("a pre-reservation refusal must persist nothing, file holds %+v", doc)
	}
}
