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
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// newHubAt builds and runs an Observe hub over an EXISTING data
// directory — the restart seam: two hubs over the same dataDir are two
// daemon lifetimes.
func newHubAt(t *testing.T, dataDir string) (*Observe, chan Attach, chan protocol.OBMsg) {
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
	return o, rt, ob
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
