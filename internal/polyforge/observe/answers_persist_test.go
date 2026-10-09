package observe

// answers_persist_test.go: the answer ledger's hub-level FILE faces that
// survive the polyforge#106 relay deletion — the daemon-wide
// answers.json is loaded in New() before anything can attach, shared by
// every monitor, quarantined when bad (never fatal), and pruned per
// work item when the work item finishes (spec A1–A4).
//
// The relay's writer-side pins this file used to carry (the awaiting-set
// restore, the restart redelivery equivalence) died with the relay: the
// trimmed ledger records nothing new (the capsule is the authority).
// The durable QUESTION face those pins double-covered is pinned UNEDITED
// by the migrated s19 e2e suite on the capsule face
// (test/v1_s19_answer_ledger_persistence_test.go).

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
		PushFunc: func(ctx context.Context, g gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
			return "sha_persist", true, gitops.PushTimings{}, nil
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

// seedLedgerFile writes a valid answers.json into dataDir.
func seedLedgerFile(t *testing.T, dataDir string, doc answerFileDoc) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAnswerFile(dataDir, doc); err != nil {
		t.Fatal(err)
	}
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

// readLedgerFile reads and parses the daemon's answers.json.
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

func TestHubOverAnEmptyDirStartsWithAnEmptyLedger(t *testing.T) {
	dir := t.TempDir()
	o, _, _ := newHubAt(t, dir)
	if o.Ledger() == nil {
		t.Fatal("the hub must own a ledger")
	}
	if got := o.Ledger().snapshot(); len(got.Applied) != 0 {
		t.Fatalf("first boot must be empty: %+v", got.Applied)
	}
	if len(ledgerEvents(o)) != 0 {
		t.Fatalf("a missing file is not an incident: %+v", ledgerEvents(o))
	}
}

func TestHubQuarantinesACorruptLedgerAndStillStarts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(answerFilePath(dir), []byte("{\"version\":1,\"appl"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, _, _ := newHubAt(t, dir)
	if got := o.Ledger().snapshot(); len(got.Applied) != 0 {
		t.Fatalf("a corrupt file must yield an empty ledger: %+v", got.Applied)
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
	// The live path carries a fresh, valid, empty ledger: whatever the
	// boot left behind parses and records nothing.
	if raw, err := os.ReadFile(answerFilePath(dir)); err == nil {
		var doc answerFileDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("the rebuilt file must be valid: %v (%s)", err, raw)
		}
		doc.normalize()
		if len(doc.Applied) != 0 {
			t.Fatalf("the rebuilt file must be empty: %+v", doc.Applied)
		}
	}
}

func TestRTFinishedDropsTheWorkItemsPersistedEntries(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc := newAnswerFileDoc()
	doc.Applied["a1"] = answerFileEntry{State: stateNameFinal, WorkItem: "wi_done", SettledAt: now}
	doc.Applied["b1"] = answerFileEntry{State: stateNameFinal, WorkItem: "wi_live", SettledAt: now}
	seedLedgerFile(t, dir, doc)

	o, rt, _ := newHubAt(t, dir)
	rt <- Attach{Msg: protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: "wi_done"}}

	soon(t, 2*time.Second, "RTFinished never pruned the ledger", func() bool {
		got := o.Ledger().snapshot()
		_, stillThere := got.Applied["a1"]
		return !stillThere
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
		return !appliedStillThere
	})
	// The live sibling is untouched, in memory and on disk.
	if got := o.Ledger().snapshot(); got.Applied["b1"].WorkItem != "wi_live" {
		t.Fatalf("a live work item must keep its entries: %+v", got.Applied)
	}
	if d := readLedgerFile(t, dir); d.Applied["b1"].WorkItem != "wi_live" {
		t.Fatalf("the sibling must survive on disk: %+v", d.Applied)
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
		Msg:    protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: "wi_share", SpawnN: 1},
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
