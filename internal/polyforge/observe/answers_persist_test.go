package observe

// answers_persist_test.go: the ledger's persistence AT HUB LEVEL — the
// daemon-wide answers.json is loaded in New() before anything can
// submit or re-deliver, shared by every monitor, quarantined when bad,
// and pruned when a work item finishes.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// newHubAt builds and runs an Observe hub over an EXISTING data
// directory — the restart seam: two hubs over the same dataDir are two
// daemon lifetimes.
func newHubAt(t *testing.T, dataDir string) (*Observe, chan Attach, func()) {
	t.Helper()
	rt := make(chan Attach, 8)
	ob := make(chan protocol.OBMsg, 64)
	o, err := New(Config{
		RT: rt, OB: ob, DataDir: dataDir,
		PollInterval:    5 * time.Millisecond,
		HangWindow:      time.Hour,
		IdleWindow:      time.Hour,
		AnswerAckWindow: 80 * time.Millisecond,
		CallTimeout:     time.Second,
		PushFunc: func(ctx context.Context, g gitops.Opts, message string) (string, bool, error) {
			return "sha_persist", true, nil
		},
	})
	if err != nil {
		t.Fatalf("hub over %s: %v", dataDir, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	t.Cleanup(cancel)
	// The returned stop func is the DAEMON KILL of the restart tests:
	// the hub and its in-memory ledger are gone, the data directory
	// stays.
	return o, rt, cancel
}

// seedLedgerFile writes a valid version-1 answers.json into dataDir.
func seedLedgerFile(t *testing.T, dataDir string, doc answerFileDoc) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAnswerFile(dataDir, doc); err != nil {
		t.Fatal(err)
	}
}

// awaitingRelay reads one restored relay by answer id ("" = absent).
func awaitingRelay(l *AnswerLedger, wi, answerID string) (AnswerRelay, bool) {
	for _, r := range l.Awaiting(wi) {
		if r.AnswerID == answerID {
			return r, true
		}
	}
	return AnswerRelay{}, false
}

// ledgerEvents lists the daemon-scope answer_ledger events (wi="").
func ledgerEvents(o *Observe) []protocol.EventEntry {
	var out []protocol.EventEntry
	for _, e := range o.Events().Since("") {
		if e.Kind == EvAnswerLedger {
			out = append(out, e)
		}
	}
	return out
}

func TestHubOverAnEmptyDirStartsWithAnEmptyLedger(t *testing.T) {
	dir := t.TempDir()
	o, _, _ := newHubAt(t, dir)
	if o.Ledger() == nil {
		t.Fatal("the hub must own a ledger")
	}
	if len(o.Ledger().Awaiting("wi_x")) != 0 {
		t.Fatalf("first boot must be empty: %+v", o.Ledger().Awaiting("wi_x"))
	}
	if len(ledgerEvents(o)) != 0 {
		t.Fatalf("a missing file is not an incident: %+v", ledgerEvents(o))
	}
}

func TestHubRestoresTheAwaitingSetBeforeAnyAttachment(t *testing.T) {
	dir := t.TempDir()
	relay := AnswerRelay{
		QuestionID: "q1", AnswerID: "a1", AnswerText: "go ahead",
		AnsweredAt: "2026-02-02T10:00:00Z", AnsweredBy: "alice",
	}
	doc := newAnswerFileDoc()
	doc.Awaiting["wi_x"] = map[string]AnswerRelay{"a1": relay}
	doc.Applied["a1"] = answerFileEntry{
		State: stateNameRetryable, ErrorCode: errCodeAckPending, WorkItem: "wi_x",
		SettledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	seedLedgerFile(t, dir, doc)

	// No RT attachment has been sent: the restored set must already be
	// there the instant New() returns.
	o, _, _ := newHubAt(t, dir)
	got, ok := awaitingRelay(o.Ledger(), "wi_x", "a1")
	if !ok {
		t.Fatalf("the awaiting set must be restored at startup: %+v", o.Ledger().Awaiting("wi_x"))
	}
	if got != relay {
		t.Fatalf("relay not verbatim:\n got %+v\nwant %+v", got, relay)
	}
	if !o.Ledger().AwaitingQuestion("wi_x", "q1") {
		t.Fatal("the restored entry must answer AwaitingQuestion")
	}
}

func TestHubQuarantinesACorruptLedgerAndStillStarts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(answerFilePath(dir), []byte("{\"version\":1,\"appl"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, _, _ := newHubAt(t, dir)
	if len(o.Ledger().Awaiting("wi_x")) != 0 {
		t.Fatalf("a corrupt file must yield an empty ledger: %+v", o.Ledger().Awaiting("wi_x"))
	}
	evs := ledgerEvents(o)
	if len(evs) != 1 {
		t.Fatalf("exactly one quarantine event: %+v", evs)
	}
	var body map[string]any
	if err := json.Unmarshal(evs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["detail"] != "quarantine" {
		t.Fatalf("event payload: %+v", body)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	quarantined := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), answerFileName+".corrupt-") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Fatalf("no answers.json.corrupt-<ts> in %v", entries)
	}
	// The hub is fully functional: a fresh submission persists cleanly.
	if _, err := o.Ledger().SubmitFor("wi_x", relayFor("q1", "a_new"), "alice",
		pendingAt("q1"), func(AnswerRelay) error { return nil }); err != nil {
		t.Fatalf("post-quarantine submission: %v", err)
	}
	if doc := readLedgerFile(t, dir); doc.Applied["a_new"].State != stateNameFinal {
		t.Fatalf("the rebuilt file must record the new answer: %+v", doc.Applied)
	}
}

func TestRTFinishedDropsTheWorkItemsPersistedEntries(t *testing.T) {
	dir := t.TempDir()
	doc := newAnswerFileDoc()
	doc.Awaiting["wi_done"] = map[string]AnswerRelay{"a1": relayFor("q1", "a1")}
	doc.Awaiting["wi_live"] = map[string]AnswerRelay{"b1": relayFor("q2", "b1")}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc.Applied["a1"] = answerFileEntry{State: stateNameRetryable, WorkItem: "wi_done", SettledAt: now}
	doc.Applied["b1"] = answerFileEntry{State: stateNameRetryable, WorkItem: "wi_live", SettledAt: now}
	seedLedgerFile(t, dir, doc)

	o, rt, _ := newHubAt(t, dir)
	rt <- Attach{Msg: protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: "wi_done"}}

	soon(t, 2*time.Second, "RTFinished never pruned the ledger", func() bool {
		return len(o.Ledger().Awaiting("wi_done")) == 0
	})
	soon(t, 2*time.Second, "RTFinished never rewrote the file", func() bool {
		raw, err := os.ReadFile(answerFilePath(dir))
		if err != nil {
			return false
		}
		var d answerFileDoc
		if json.Unmarshal(raw, &d) != nil {
			return false
		}
		d.normalize()
		_, appliedStillThere := d.Applied["a1"]
		return len(d.Awaiting["wi_done"]) == 0 && !appliedStillThere
	})
	// The live sibling is untouched, in memory and on disk.
	if len(o.Ledger().Awaiting("wi_live")) != 1 {
		t.Fatalf("a live work item must keep its entries: %+v", o.Ledger().Awaiting("wi_live"))
	}
	if d := readLedgerFile(t, dir); len(d.Awaiting["wi_live"]) != 1 || d.Applied["b1"].WorkItem != "wi_live" {
		t.Fatalf("the sibling must survive on disk: %+v / %+v", d.Awaiting, d.Applied)
	}
}

func TestHubSharesOneLedgerAcrossMonitors(t *testing.T) {
	dir := t.TempDir()
	o, rt, _ := newHubAt(t, dir)
	capsuleDir := filepath.Join(t.TempDir(), "capsule")
	if err := os.MkdirAll(filepath.Join(capsuleDir, "runtime-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newFakeHandle("att_1")
	rt <- Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: "wi_share", AttemptID: "att_1"},
		Handle: h,
		WI:     WIInfo{WorkItem: "wi_share", Project: "p", CapsuleDir: capsuleDir},
	}
	var m *Monitor
	soon(t, 2*time.Second, "monitor never registered", func() bool {
		m = o.lookup("wi_share")
		return m != nil
	})
	if m.ledger != o.Ledger() {
		t.Fatal("the monitor must use the hub's shared ledger, never a private one")
	}
}

// ---------------------------------------------------------------------------
// cross-restart behavior (plan step restart_equivalence_tests). A
// "restart" here is literal: the hub is stopped, its in-memory ledger
// goes with it, and a NEW hub is built over the SAME data directory and
// the SAME capsule. The capsule stays the authority for binding
// throughout — the persisted ledger only remembers delivery status.

// writeCapsuleExec writes a capsule execution.json whose current step
// carries pending question qid ("" = no open question), forcing a
// distinct mtime so the monitor's poll notices.
func writeCapsuleExec(t *testing.T, capsuleDir, qid string, version int64) {
	t.Helper()
	step := map[string]any{
		"status":          "in_progress",
		"attempts":        []any{},
		"answers":         []any{},
		"budget_consumed": map[string]any{"usd": 0.1},
	}
	if qid != "" {
		step["pending_question"] = map[string]any{
			"id": qid, "step": "code", "question": "pick one",
			"asked_at": time.Now().UTC().Format(time.RFC3339Nano),
		}
	}
	exec := map[string]any{
		"capsule_id": "cap", "work_item_id": "wi_r", "generation": int64(1),
		"current_step": "code", "workflow_status": "running",
		"steps":         map[string]any{"code": step},
		"review_rounds": map[string]int64{},
		"updated_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"version":       version,
	}
	raw, err := json.Marshal(exec)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(capsuleDir, "runtime-state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "execution.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = os.Chtimes(tmp, now, now)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// newCapsule makes a capsule directory with one open question.
func newCapsule(t *testing.T, qid string) string {
	t.Helper()
	capsuleDir := filepath.Join(t.TempDir(), "capsule")
	writeCapsuleExec(t, capsuleDir, qid, 1)
	return capsuleDir
}

// attachStart sends the RTStarted attachment a fresh daemon sends for a
// work item it picks up.
func attachStart(rt chan Attach, wi, capsuleDir string, h Handle) {
	rt <- Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: wi, AttemptID: h.AttemptID()},
		Handle: h,
		WI:     WIInfo{WorkItem: wi, Project: "proj", CapsuleDir: capsuleDir, BundleName: "b"},
	}
}

// waitMonitor waits for the hub to register the work item's monitor.
func waitMonitor(t *testing.T, o *Observe, wi string) *Monitor {
	t.Helper()
	var m *Monitor
	soon(t, 2*time.Second, "monitor never registered for "+wi, func() bool {
		m = o.lookup(wi)
		return m != nil
	})
	return m
}

// waitPending waits for the monitor to project the capsule's question.
func waitPending(t *testing.T, m *Monitor, qid string) {
	t.Helper()
	soon(t, 2*time.Second, "pending question never projected: "+qid, func() bool {
		m.stateMu.Lock()
		defer m.stateMu.Unlock()
		return m.pending != nil && m.pending.ID == qid
	})
}

// ackPending is a SendControl that delivers but never acks in time.
func ackPending() func(context.Context, protocol.ControlMsg) (protocol.ControlAck, error) {
	return func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		return protocol.ControlAck{}, ErrAnswerAckPending
	}
}

func TestRestartRedeliversUnconsumedAnswerToTheNextInstance(t *testing.T) {
	dir := t.TempDir()
	capsuleDir := newCapsule(t, "q1")
	relay := relayFor("q1", "a1")

	// --- daemon lifetime 1: the answer is delivered but never consumed.
	o1, rt1, kill1 := newHubAt(t, dir)
	h1 := newFakeHandle("att_1")
	h1.setSend(ackPending())
	attachStart(rt1, "wi_r", capsuleDir, h1)
	m1 := waitMonitor(t, o1, "wi_r")
	waitPending(t, m1, "q1")
	if err := submitAnswer(m1, relay); err == nil || err.Error() != ErrAnswerAckPending.Error() {
		t.Fatalf("first submission must settle ack-pending, got %v", err)
	}
	if doc := readLedgerFile(t, dir); len(doc.Awaiting["wi_r"]) != 1 {
		t.Fatalf("the unconsumed answer must be on disk: %+v", doc.Awaiting)
	}
	kill1()

	// --- daemon lifetime 2: the restored entry rides the attachment.
	o2, rt2, _ := newHubAt(t, dir)
	got, ok := awaitingRelay(o2.Ledger(), "wi_r", "a1")
	if !ok || got != relay {
		t.Fatalf("the awaiting entry must be restored before any attachment: ok=%v %+v", ok, got)
	}
	h2 := newFakeHandle("att_2")
	attachStart(rt2, "wi_r", capsuleDir, h2)
	soon(t, 3*time.Second, "the restored answer was never re-delivered after the restart", func() bool {
		return len(sentAnswers(h2)) == 1
	})
	if sent := sentAnswers(h2); sent[0].ID != "q1" || sent[0].Answer != relay.AnswerText {
		t.Fatalf("the re-delivered relay must be verbatim: %+v", sent[0])
	}
	// Consumed on the new instance: the entry finalizes in memory AND
	// leaves the persisted awaiting half.
	soon(t, 3*time.Second, "the re-delivered answer never finalized in memory", func() bool {
		return len(o2.Ledger().Awaiting("wi_r")) == 0
	})
	soon(t, 3*time.Second, "the re-delivered answer never finalized on disk", func() bool {
		raw, err := os.ReadFile(answerFilePath(dir))
		if err != nil {
			return false
		}
		var d answerFileDoc
		if json.Unmarshal(raw, &d) != nil {
			return false
		}
		d.normalize()
		return len(d.Awaiting["wi_r"]) == 0 && d.Applied["a1"].State == stateNameFinal
	})
}

func TestRestartDoesNotRedeliverAnswersOfASupersededQuestion(t *testing.T) {
	dir := t.TempDir()
	capsuleDir := newCapsule(t, "q1") // the capsule's open question is q1
	old := relayFor("q_old", "a_old")
	doc := newAnswerFileDoc()
	doc.Awaiting["wi_r"] = map[string]AnswerRelay{"a_old": old}
	doc.Applied["a_old"] = answerFileEntry{
		State: stateNameRetryable, ErrorCode: errCodeAckPending, WorkItem: "wi_r",
		SettledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	seedLedgerFile(t, dir, doc)

	o, rt, _ := newHubAt(t, dir)
	h := newFakeHandle("att_1")
	attachStart(rt, "wi_r", capsuleDir, h)
	waitMonitor(t, o, "wi_r")
	// Give the attach-time hook every chance to (wrongly) fire.
	time.Sleep(200 * time.Millisecond)
	if n := len(sentAnswers(h)); n != 0 {
		t.Fatalf("an answer bound to a superseded question must not be re-delivered: %d", n)
	}
	if got, ok := awaitingRelay(o.Ledger(), "wi_r", "a_old"); !ok || got != old {
		t.Fatalf("the superseded entry must stay untouched in the ledger: ok=%v %+v", ok, got)
	}
	if d := readLedgerFile(t, dir); len(d.Awaiting["wi_r"]) != 1 {
		t.Fatalf("the superseded entry must stay in the file: %+v", d.Awaiting)
	}
}

func TestRestartReplaysFinalOutcomesWithoutRedelivering(t *testing.T) {
	dir := t.TempDir()
	capsuleDir := newCapsule(t, "q1")
	consumed := relayFor("q1", "a1")
	refused := relayFor("q_gone", "a2")

	o1, rt1, kill1 := newHubAt(t, dir)
	h1 := newFakeHandle("att_1")
	attachStart(rt1, "wi_r", capsuleDir, h1)
	m1 := waitMonitor(t, o1, "wi_r")
	waitPending(t, m1, "q1")
	if err := submitAnswer(m1, consumed); err != nil {
		t.Fatalf("a1 first submission: %v", err)
	}
	if err := submitAnswer(m1, refused); err == nil || err.Error() != ErrAnswerConsumed.Error() {
		t.Fatalf("a2 must be a definitive binding refusal, got %v", err)
	}
	kill1()

	o2, rt2, _ := newHubAt(t, dir)
	h2 := newFakeHandle("att_2")
	attachStart(rt2, "wi_r", capsuleDir, h2)
	m2 := waitMonitor(t, o2, "wi_r")
	// a1 replays the recorded nil outcome, sends no pf/answer.
	if err := submitAnswer(m2, consumed); err != nil {
		t.Fatalf("post-restart replay of a consumed answer_id: %v", err)
	}
	// a2 replays the recorded refusal verbatim.
	if err := submitAnswer(m2, refused); err == nil || err.Error() != ErrAnswerConsumed.Error() {
		t.Fatalf("post-restart replay of a refused answer_id: %v", err)
	}
	if n := len(sentAnswers(h2)); n != 0 {
		t.Fatalf("a final answer_id must send no pf/answer after a restart: %d", n)
	}
	if len(eventsOf(m2, EvAnswered)) != 0 {
		t.Fatalf("a replay must not re-announce the answer")
	}
}

func TestCrashMidDeliveryReloadsRedeliverableAndNotInflight(t *testing.T) {
	dir := t.TempDir()
	crashDir := t.TempDir() // the snapshot the killed daemon left behind
	capsuleDir := newCapsule(t, "q1")
	relay := relayFor("q1", "a1")

	o1, rt1, kill1 := newHubAt(t, dir)
	h1 := newFakeHandle("att_1")
	blocked := make(chan struct{})
	snapshotted := make(chan struct{})
	h1.setSend(func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
		if msg.Method == protocol.ControlAnswer {
			// THE CRASH MOMENT: the delivery is running. Copy the file
			// exactly as the dying daemon left it.
			raw, err := os.ReadFile(answerFilePath(dir))
			if err == nil {
				_ = os.WriteFile(answerFilePath(crashDir), raw, answerFileMode)
			}
			close(snapshotted)
			// The killed process NEVER returns from this delivery — that
			// is the whole point of the scenario, so this goroutine is
			// deliberately parked for the rest of the test binary and
			// nothing else ever writes into dir.
			<-blocked
			return protocol.ControlAck{}, ErrAnswerAckPending
		}
		return protocol.ControlAck{Method: msg.Method, Seq: msg.Seq, OK: true}, nil
	})
	attachStart(rt1, "wi_r", capsuleDir, h1)
	m1 := waitMonitor(t, o1, "wi_r")
	waitPending(t, m1, "q1")
	ac := AnswerCall{Relay: relay, Caller: relay.AnsweredBy, Resp: make(chan error, 1)}
	m1.answerCh <- ac
	select {
	case <-snapshotted:
	case <-time.After(3 * time.Second):
		t.Fatal("the delivery never started")
	}
	kill1()

	// --- the restarted daemon reads what the crash left.
	o2, rt2, _ := newHubAt(t, crashDir)
	got, ok := awaitingRelay(o2.Ledger(), "wi_r", "a1")
	if !ok || got != relay {
		t.Fatalf("a crash mid-delivery must reload the answer as awaiting: ok=%v %+v", ok, got)
	}
	o2.Ledger().mu.Lock()
	state := o2.Ledger().applied["a1"].state
	o2.Ledger().mu.Unlock()
	if state != answerRetryable {
		t.Fatalf("the reloaded entry must be retryable, got %v", state)
	}
	// The next attachment re-delivers it automatically.
	h2 := newFakeHandle("att_2")
	h2.setSend(ackPending())
	attachStart(rt2, "wi_r", capsuleDir, h2)
	m2 := waitMonitor(t, o2, "wi_r")
	soon(t, 3*time.Second, "no automatic re-delivery after the mid-delivery crash", func() bool {
		return len(sentAnswers(h2)) >= 1
	})
	// And the owner's manual re-submission is NOT refused as in-flight
	// BY THE RELOADED ENTRY. (A transient ErrAnswerInFlight from the
	// automatic re-delivery that is still running right now is the
	// documented concurrent-duplicate refusal and is retried, exactly as
	// it would be without a restart.)
	h2.setSend(nil)
	waitPending(t, m2, "q1") // the binding basis is the capsule's open question
	var lastErr error
	soon(t, 3*time.Second, "the manual re-submission never got past the in-flight refusal", func() bool {
		lastErr = submitAnswer(m2, relay)
		return lastErr == nil || lastErr.Error() != ErrAnswerInFlight.Error()
	})
	if lastErr != nil {
		t.Fatalf("the manual re-submission after the crash must succeed, got %v", lastErr)
	}
	// Settle the whole scenario on disk before the test's temp dirs go:
	// no write may outlive the test.
	soon(t, 3*time.Second, "the manual re-delivery never finalized on disk", func() bool {
		raw, err := os.ReadFile(answerFilePath(crashDir))
		if err != nil {
			return false
		}
		var d answerFileDoc
		if json.Unmarshal(raw, &d) != nil {
			return false
		}
		d.normalize()
		return d.Applied["a1"].State == stateNameFinal && len(d.Awaiting["wi_r"]) == 0
	})
}

func TestRestoredEntryWhoseCapsuleQuestionIsGoneSettlesConsumed(t *testing.T) {
	dir := t.TempDir()
	// The capsule has NO open pending question any more.
	capsuleDir := newCapsule(t, "")
	relay := relayFor("q1", "a1")
	doc := newAnswerFileDoc()
	doc.Awaiting["wi_r"] = map[string]AnswerRelay{"a1": relay}
	doc.Applied["a1"] = answerFileEntry{
		State: stateNameRetryable, ErrorCode: errCodeAckPending, WorkItem: "wi_r",
		SettledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	seedLedgerFile(t, dir, doc)

	o, rt, _ := newHubAt(t, dir)
	h := newFakeHandle("att_1")
	attachStart(rt, "wi_r", capsuleDir, h)
	m := waitMonitor(t, o, "wi_r")
	time.Sleep(200 * time.Millisecond)
	if n := len(sentAnswers(h)); n != 0 {
		t.Fatalf("no re-delivery may happen with no open capsule question: %d", n)
	}
	// Binding is decided against the CAPSULE, not against the file: a
	// fresh submission is the definitive refusal, exactly as it would be
	// without a restart.
	if err := submitAnswer(m, relay); err == nil || err.Error() != ErrAnswerConsumed.Error() {
		t.Fatalf("a submission with no open capsule question must settle ErrAnswerConsumed, got %v", err)
	}
}

// restartObservation is the comparable trace of one run: the returned
// errors, whether each submission delivered, the re-delivery sends the
// successor instance saw, and the Awaiting contents at each checkpoint.
type restartObservation struct {
	FirstErr      string
	AwaitingAfter []string
	Redeliveries  int
	AwaitingEnd   []string
	ReplayErr     string
	ReplayDeliver bool
	SendsEnd      int
}

// runAnswerScenario plays the identical sequence once continuously and
// once with a daemon restart inserted after the first delivery settles.
func runAnswerScenario(t *testing.T, restart bool) restartObservation {
	t.Helper()
	dir := t.TempDir()
	capsuleDir := newCapsule(t, "q1")
	relay := relayFor("q1", "a1")
	var obs restartObservation

	o, rt, kill := newHubAt(t, dir)
	h1 := newFakeHandle("att_1")
	h1.setSend(ackPending())
	attachStart(rt, "wi_r", capsuleDir, h1)
	m := waitMonitor(t, o, "wi_r")
	waitPending(t, m, "q1")

	if err := submitAnswer(m, relay); err != nil {
		obs.FirstErr = err.Error()
	}
	obs.AwaitingAfter = awaitingIDs(o.Ledger(), "wi_r")

	if restart {
		// THE RESTART: the hub and its in-memory ledger are gone; only
		// the data directory survives into the next lifetime.
		kill()
		o, rt, _ = newHubAt(t, dir)
	}

	h2 := newFakeHandle("att_2")
	if restart {
		attachStart(rt, "wi_r", capsuleDir, h2)
	} else {
		rt <- Attach{
			Msg:    protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_r", AttemptID: "att_2"},
			Handle: h2,
			WI:     WIInfo{WorkItem: "wi_r", Project: "proj", CapsuleDir: capsuleDir, BundleName: "b"},
		}
	}
	m = waitMonitor(t, o, "wi_r")
	soon(t, 3*time.Second, "the successor never received the re-delivery", func() bool {
		return len(sentAnswers(h2)) >= 1
	})
	obs.Redeliveries = len(sentAnswers(h2))
	soon(t, 3*time.Second, "the re-delivery never settled consumed", func() bool {
		return len(o.Ledger().Awaiting("wi_r")) == 0
	})
	obs.AwaitingEnd = awaitingIDs(o.Ledger(), "wi_r")
	// The consumption must also have REACHED DISK before the run ends,
	// so the replay below is the last write and nothing outlives the
	// test's temp directory.
	soon(t, 3*time.Second, "the consumption never reached disk", func() bool {
		raw, err := os.ReadFile(answerFilePath(dir))
		if err != nil {
			return false
		}
		var d answerFileDoc
		if json.Unmarshal(raw, &d) != nil {
			return false
		}
		d.normalize()
		return d.Applied["a1"].State == stateNameFinal && len(d.Awaiting["wi_r"]) == 0
	})

	before := len(sentAnswers(h2))
	if err := submitAnswer(m, relay); err != nil {
		obs.ReplayErr = err.Error()
	}
	obs.ReplayDeliver = len(sentAnswers(h2)) > before
	obs.SendsEnd = len(sentAnswers(h2))
	return obs
}

func awaitingIDs(l *AnswerLedger, wi string) []string {
	out := []string{}
	for _, r := range l.Awaiting(wi) {
		out = append(out, r.AnswerID)
	}
	return out
}

func TestPostRestartBehaviorMatchesNoRestartBehavior(t *testing.T) {
	continuous := runAnswerScenario(t, false)
	restarted := runAnswerScenario(t, true)
	if !reflect.DeepEqual(continuous, restarted) {
		t.Fatalf("a restart must be unobservable:\n continuous %+v\n restarted  %+v", continuous, restarted)
	}
	// Sanity: the scenario really exercised the path it claims to.
	if continuous.FirstErr != ErrAnswerAckPending.Error() {
		t.Fatalf("the scenario must settle ack-pending first: %+v", continuous)
	}
	if len(continuous.AwaitingAfter) != 1 || len(continuous.AwaitingEnd) != 0 {
		t.Fatalf("the scenario must go awaiting -> consumed: %+v", continuous)
	}
	if continuous.Redeliveries != 1 || continuous.ReplayDeliver {
		t.Fatalf("the scenario must re-deliver once and never replay-deliver: %+v", continuous)
	}
}
