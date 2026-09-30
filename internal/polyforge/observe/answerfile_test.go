package observe

// answerfile_test.go: the on-disk answer-ledger format in isolation —
// atomic write, mode, round-trip of BOTH halves, the A3 missing/corrupt
// tolerance and the A4 load-time retention rule. Nothing here touches a
// monitor or a hub; the wiring tests live in answers_persist_test.go.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// collectRecorder captures the loader's one-event callback.
func collectRecorder() (ledgerRecorder, *[]map[string]any) {
	var got []map[string]any
	return func(kind string, body map[string]any) {
		body["__kind"] = kind
		got = append(got, body)
	}, &got
}

func seedDoc() answerFileDoc {
	doc := newAnswerFileDoc()
	doc.Awaiting["wi_a"] = map[string]AnswerRelay{
		"a1": {
			QuestionID: "q1", AnswerID: "a1", AnswerText: "ship it",
			AnsweredAt: "2026-01-01T00:00:00Z", AnsweredBy: "alice",
		},
	}
	doc.Applied["a1"] = answerFileEntry{
		State: stateNameRetryable, ErrorCode: errCodeAckPending,
		WorkItem: "wi_a", SettledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	doc.Applied["a0"] = answerFileEntry{
		State: stateNameFinal, ErrorCode: errCodeConsumed,
		WorkItem: "wi_a", SettledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	return doc
}

func TestAnswerFileRoundTripsBothHalves(t *testing.T) {
	dir := t.TempDir()
	want := seedDoc()
	if err := writeAnswerFile(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	rec, events := collectRecorder()
	got, pruned := loadAnswerFile(dir, rec, time.Now())
	if pruned {
		t.Fatalf("a fresh document must not prune")
	}
	if len(*events) != 0 {
		t.Fatalf("a clean load records no event: %+v", *events)
	}
	if got.Version != answerFileVersion {
		t.Fatalf("version: got %d want %d", got.Version, answerFileVersion)
	}
	// The awaiting half keeps all five relay fields verbatim.
	gotRelay, ok := got.Awaiting["wi_a"]["a1"]
	if !ok {
		t.Fatalf("awaiting half lost the relay: %+v", got.Awaiting)
	}
	if gotRelay != want.Awaiting["wi_a"]["a1"] {
		t.Fatalf("relay not verbatim:\n got %+v\nwant %+v", gotRelay, want.Awaiting["wi_a"]["a1"])
	}
	// The applied half keeps state + error identity + owner + settle time.
	if e := got.Applied["a1"]; e.State != stateNameRetryable || e.ErrorCode != errCodeAckPending || e.WorkItem != "wi_a" {
		t.Fatalf("applied a1 round-trip: %+v", e)
	}
	if e := got.Applied["a0"]; e.State != stateNameFinal || e.ErrorCode != errCodeConsumed {
		t.Fatalf("applied a0 round-trip: %+v", e)
	}
	// The recorded error identity replays as the SENTINEL, not a copy.
	if err := decodeAnswerErr(got.Applied["a0"].ErrorCode, got.Applied["a0"].ErrorMessage); !errors.Is(err, ErrAnswerConsumed) {
		t.Fatalf("final outcome must replay ErrAnswerConsumed, got %v", err)
	}
	if err := decodeAnswerErr(got.Applied["a1"].ErrorCode, ""); !errors.Is(err, ErrAnswerAckPending) {
		t.Fatalf("retryable outcome must replay ErrAnswerAckPending, got %v", err)
	}
}

func TestAnswerFileErrorCodecIsAClosedSet(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{nil, errCodeNone},
		{ErrAnswerConsumed, errCodeConsumed},
		{ErrAnswerAckPending, errCodeAckPending},
		{ErrAnswerIdentity, errCodeIdentity},
	}
	for _, c := range cases {
		code, msg := encodeAnswerErr(c.err)
		if code != c.code || msg != "" {
			t.Fatalf("encode %v: got (%q,%q) want (%q,\"\")", c.err, code, msg, c.code)
		}
		back := decodeAnswerErr(code, msg)
		if c.err == nil {
			if back != nil {
				t.Fatalf("nil outcome must decode to nil, got %v", back)
			}
			continue
		}
		if !errors.Is(back, c.err) {
			t.Fatalf("decode %q: got %v want %v", code, back, c.err)
		}
	}
	// A non-sentinel delivery failure keeps its message under `other`.
	code, msg := encodeAnswerErr(errors.New("control path closed"))
	if code != errCodeOther || msg != "control path closed" {
		t.Fatalf("other encoding: (%q,%q)", code, msg)
	}
	if got := decodeAnswerErr(code, msg); got == nil || got.Error() != "control path closed" {
		t.Fatalf("other decoding: %v", got)
	}
}

func TestAnswerFileStateCodecNeverEncodesInflight(t *testing.T) {
	if got := encodeAnswerState(answerInflight); got != stateNameRetryable {
		t.Fatalf("in-flight must project to retryable on disk, got %q", got)
	}
	if got := encodeAnswerState(answerFinal); got != stateNameFinal {
		t.Fatalf("final encoding: %q", got)
	}
	if got := encodeAnswerState(answerRetryable); got != stateNameRetryable {
		t.Fatalf("retryable encoding: %q", got)
	}
	// No on-disk name decodes to the in-flight state — not even garbage.
	for _, name := range []string{stateNameFinal, stateNameRetryable, "", "inflight", "bogus"} {
		if got := decodeAnswerState(name); got == answerInflight {
			t.Fatalf("state %q decoded to answerInflight", name)
		}
	}
}

func TestAnswerFileIsWrittenAtMode0600(t *testing.T) {
	dir := t.TempDir()
	if err := writeAnswerFile(dir, seedDoc()); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := os.Stat(answerFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != answerFileMode {
		t.Fatalf("mode: got %v want %v", got, answerFileMode)
	}
}

func TestAnswerFileInterruptedWriteLeavesPreviousFileIntact(t *testing.T) {
	dir := t.TempDir()
	first := seedDoc()
	if err := writeAnswerFile(dir, first); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, err := os.ReadFile(answerFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash AFTER the tmp file was created but BEFORE the
	// rename: a leftover, half-written answers.json.tmp.
	if err := os.WriteFile(answerFilePath(dir)+".tmp", []byte(`{"version":1,"awai`), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(answerFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the visible file must be untouched by an interrupted write")
	}
	rec, events := collectRecorder()
	doc, _ := loadAnswerFile(dir, rec, time.Now())
	if len(*events) != 0 {
		t.Fatalf("a leftover tmp must not quarantine the valid file: %+v", *events)
	}
	if _, ok := doc.Awaiting["wi_a"]["a1"]; !ok {
		t.Fatalf("the previous valid content must still load: %+v", doc)
	}
	// And the next successful write still lands cleanly over the leftover.
	if err := writeAnswerFile(dir, doc); err != nil {
		t.Fatalf("rewrite over a leftover tmp: %v", err)
	}
	if _, err := os.Stat(answerFilePath(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("the tmp file must be consumed by the rename, stat err=%v", err)
	}
}

func TestAnswerFileMissingLoadsEmpty(t *testing.T) {
	dir := t.TempDir()
	rec, events := collectRecorder()
	doc, pruned := loadAnswerFile(dir, rec, time.Now())
	if pruned {
		t.Fatalf("nothing to prune on first boot")
	}
	if len(doc.Awaiting) != 0 || len(doc.Applied) != 0 {
		t.Fatalf("first boot must load empty: %+v", doc)
	}
	if len(*events) != 0 {
		t.Fatalf("a missing file is not an incident: %+v", *events)
	}
	if _, err := os.Stat(answerFilePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("the loader must not create the file: %v", err)
	}
}

func assertQuarantined(t *testing.T, dir string, events []map[string]any) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("exactly one event must describe the quarantine: %+v", events)
	}
	if events[0]["__kind"] != EvAnswerLedger || events[0]["detail"] != "quarantine" {
		t.Fatalf("quarantine event shape: %+v", events[0])
	}
	if _, err := os.Stat(answerFilePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("the bad file must be moved aside, stat err=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), answerFileName+".corrupt-") {
			found = e.Name()
		}
	}
	if found == "" {
		t.Fatalf("no answers.json.corrupt-<ts> file in %v", entries)
	}
	stamp := strings.TrimPrefix(found, answerFileName+".corrupt-")
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Fatalf("quarantine suffix %q is not RFC3339: %v", stamp, err)
	}
}

func TestAnswerFileUnparsableIsQuarantinedNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(answerFilePath(dir), []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, events := collectRecorder()
	doc, _ := loadAnswerFile(dir, rec, time.Now())
	if len(doc.Awaiting) != 0 || len(doc.Applied) != 0 {
		t.Fatalf("a corrupt file must yield an EMPTY ledger: %+v", doc)
	}
	assertQuarantined(t, dir, *events)
}

func TestAnswerFileUnknownVersionIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"version": 99, "applied": map[string]any{}})
	if err := os.WriteFile(answerFilePath(dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rec, events := collectRecorder()
	doc, _ := loadAnswerFile(dir, rec, time.Now())
	if len(doc.Applied) != 0 {
		t.Fatalf("unknown version must yield an EMPTY ledger: %+v", doc)
	}
	assertQuarantined(t, dir, *events)
}

func TestAnswerFileUnreadableIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "d")
	if err := os.MkdirAll(answerFilePath(sub), 0o755); err != nil {
		t.Fatal(err)
	}
	// answers.json is a DIRECTORY here: reading it fails with a
	// non-NotExist error, the unreadable branch.
	rec, events := collectRecorder()
	doc, _ := loadAnswerFile(sub, rec, time.Now())
	if len(doc.Applied) != 0 {
		t.Fatalf("unreadable must yield an EMPTY ledger: %+v", doc)
	}
	if len(*events) != 1 || (*events)[0]["detail"] != "quarantine" {
		t.Fatalf("unreadable must record one quarantine event: %+v", *events)
	}
}

func TestAnswerFileAgesOutStaleAppliedEntriesAtLoad(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	doc := newAnswerFileDoc()
	doc.Applied["fresh"] = answerFileEntry{
		State: stateNameFinal, WorkItem: "wi_a",
		SettledAt: now.Add(-13 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	doc.Applied["stale"] = answerFileEntry{
		State: stateNameFinal, WorkItem: "wi_a",
		SettledAt: now.Add(-15 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	doc.Applied["undated"] = answerFileEntry{State: stateNameFinal, WorkItem: "wi_a"}
	if err := writeAnswerFile(dir, doc); err != nil {
		t.Fatal(err)
	}
	got, pruned := loadAnswerFile(dir, nil, now)
	if !pruned {
		t.Fatalf("the age-out must report a change so the caller rewrites the file")
	}
	if _, ok := got.Applied["stale"]; ok {
		t.Fatalf("an applied entry older than 14 days must age out: %+v", got.Applied)
	}
	if _, ok := got.Applied["fresh"]; !ok {
		t.Fatalf("a 13-day-old entry must survive: %+v", got.Applied)
	}
	if _, ok := got.Applied["undated"]; !ok {
		t.Fatalf("an unknown age is not an old age: %+v", got.Applied)
	}
	// And the rewritten file no longer carries it.
	if err := writeAnswerFile(dir, got); err != nil {
		t.Fatal(err)
	}
	again, _ := loadAnswerFile(dir, nil, now)
	if _, ok := again.Applied["stale"]; ok {
		t.Fatalf("the rewritten file must not carry the aged-out entry")
	}
}

func TestAnswerFilePruningNeverDropsALiveAwaitingEntry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	ancient := now.Add(-365 * 24 * time.Hour).Format(time.RFC3339Nano)
	doc := newAnswerFileDoc()
	doc.Awaiting["wi_a"] = map[string]AnswerRelay{
		"old_await": {
			QuestionID: "q1", AnswerID: "old_await", AnswerText: "still unconsumed",
			AnsweredAt: ancient, AnsweredBy: "alice",
		},
	}
	doc.Applied["old_await"] = answerFileEntry{
		State: stateNameRetryable, ErrorCode: errCodeAckPending,
		WorkItem: "wi_a", SettledAt: ancient,
	}
	if err := writeAnswerFile(dir, doc); err != nil {
		t.Fatal(err)
	}
	got, pruned := loadAnswerFile(dir, nil, now)
	if pruned {
		t.Fatalf("nothing may be pruned when the only old entry is awaiting")
	}
	if _, ok := got.Awaiting["wi_a"]["old_await"]; !ok {
		t.Fatalf("a live awaiting entry must be retained at ANY age: %+v", got.Awaiting)
	}
	if _, ok := got.Applied["old_await"]; !ok {
		t.Fatalf("the applied half of a live awaiting entry must be retained: %+v", got.Applied)
	}
}
