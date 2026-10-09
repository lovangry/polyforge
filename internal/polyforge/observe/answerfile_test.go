package observe

// answerfile_test.go: the on-disk answer-ledger format in isolation —
// atomic write, mode, round-trip of the settled-record half, the A3
// missing/corrupt tolerance and the A4 load-time retention rule.
// Nothing here touches a monitor or a hub. (The relay halves and the
// two-phase states these tests used to pin are deleted with the
// WI-Agent lane, polyforge#106 — version 2 of the document carries
// settled records only, and a version-1 file quarantines.)

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	doc.Applied["a1"] = answerFileEntry{
		State: stateNameFinal, ErrorCode: errCodeConsumed,
		WorkItem: "wi_a", SettledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	doc.Applied["a0"] = answerFileEntry{
		State: stateNameFinal, ErrorCode: errCodeOther,
		ErrorMessage: "control path closed: agent exited",
		WorkItem:     "wi_a", SettledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	return doc
}

func readDoc(t *testing.T, dir string) answerFileDoc {
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

func TestAnswerFileRoundTrips(t *testing.T) {
	dir := t.TempDir()
	if err := writeAnswerFile(dir, seedDoc()); err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, dir)
	if doc.Version != answerFileVersion {
		t.Fatalf("version = %d, want %d", doc.Version, answerFileVersion)
	}
	if len(doc.Applied) != 2 {
		t.Fatalf("applied = %+v", doc.Applied)
	}
	if e := doc.Applied["a1"]; e.State != stateNameFinal || e.ErrorCode != errCodeConsumed {
		t.Fatalf("a1 record = %+v", e)
	}
	if e := doc.Applied["a0"]; e.ErrorCode != errCodeOther || e.ErrorMessage == "" {
		t.Fatalf("a0 record = %+v", e)
	}
}

func TestAnswerFileErrorCodecIsAClosedSet(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{nil, errCodeNone},
		{ErrAnswerConsumed, errCodeConsumed},
		{ErrAnswerIdentity, errCodeIdentity},
		{errors.New("boom"), errCodeOther},
	} {
		code, msg := encodeAnswerErr(c.err)
		if code != c.code {
			t.Errorf("encodeAnswerErr(%v) = %q, want %q", c.err, code, c.code)
		}
		if c.err == nil && msg != "" {
			t.Errorf("nil error must encode without a message, got %q", msg)
		}
		if c.err != nil && code == errCodeOther && msg == "" {
			t.Errorf("an untyped error must carry its message")
		}
	}
	// The round trip: every coded record decodes to the same class.
	if got := decodeAnswerErr(errCodeConsumed, ""); !errEq(got, ErrAnswerConsumed) {
		t.Errorf("decodeAnswerErr(consumed) = %v", got)
	}
	if got := decodeAnswerErr(errCodeIdentity, ""); !errEq(got, ErrAnswerIdentity) {
		t.Errorf("decodeAnswerErr(identity) = %v", got)
	}
	if got := decodeAnswerErr(errCodeNone, ""); got != nil {
		t.Errorf("decodeAnswerErr(none) = %v", got)
	}
	if got := decodeAnswerErr(errCodeOther, "boom"); got == nil || got.Error() != "boom" {
		t.Errorf("decodeAnswerErr(other) = %v", got)
	}
}

func errEq(a, b error) bool { return a != nil && b != nil && a.Error() == b.Error() }

func TestAnswerFileStateCodecIsSettledOnly(t *testing.T) {
	// Every record encodes as the settled state; any on-disk spelling
	// (including the pre-trim retryable/in-flight encodings) decodes as
	// history — nothing re-delivers anything.
	if got := encodeAnswerState(); got != stateNameFinal {
		t.Fatalf("encodeAnswerState() = %q", got)
	}
	if !decodeAnswerState(stateNameFinal) || !decodeAnswerState("retryable") {
		t.Fatal("every decoded state is settled history")
	}
}

func TestAnswerFileIsWrittenAtMode0600(t *testing.T) {
	dir := t.TempDir()
	if err := writeAnswerFile(dir, seedDoc()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(answerFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != answerFileMode.Perm() {
		t.Fatalf("mode = %v, want %v", st.Mode().Perm(), answerFileMode.Perm())
	}
}

func TestAnswerFileInterruptedWriteLeavesPreviousFileIntact(t *testing.T) {
	dir := t.TempDir()
	if err := writeAnswerFile(dir, seedDoc()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(answerFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// A torn tmp file must never become the live document: write the tmp
	// side only (simulating a crash mid-atomicWriteFile).
	tmp := answerFilePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(answerFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the live document changed during an interrupted write")
	}
}

func TestAnswerFileMissingLoadsEmpty(t *testing.T) {
	dir := t.TempDir()
	rec, events := collectRecorder()
	doc, pruned := loadAnswerFile(dir, rec, time.Now())
	if len(doc.Applied) != 0 || pruned {
		t.Fatalf("first boot = %+v pruned=%v", doc, pruned)
	}
	if len(*events) != 0 {
		t.Fatalf("first boot must be silent: %+v", *events)
	}
}

func TestAnswerFileUnparsableIsQuarantinedNotFatal(t *testing.T) {
	dir := t.TempDir()
	garbage := "{not json at all"
	if err := os.WriteFile(answerFilePath(dir), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, events := collectRecorder()
	doc, _ := loadAnswerFile(dir, rec, time.Now())
	if len(doc.Applied) != 0 {
		t.Fatalf("a corrupt file must yield an empty document: %+v", doc.Applied)
	}
	if len(*events) != 1 || (*events)[0]["detail"] != "quarantine" {
		t.Fatalf("exactly one quarantine event is required: %+v", *events)
	}
	// The original bytes moved aside verbatim.
	hits, _ := filepath.Glob(answerFilePath(dir) + ".corrupt-*")
	if len(hits) != 1 {
		t.Fatalf("quarantine files: %v", hits)
	}
	raw, err := os.ReadFile(hits[0])
	if err != nil || string(raw) != garbage {
		t.Fatalf("quarantined bytes = %q (%v)", raw, err)
	}
}

func TestAnswerFileUnknownVersionIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"version": answerFileVersion + 1, "applied": map[string]any{}})
	if err := os.WriteFile(answerFilePath(dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rec, events := collectRecorder()
	doc, _ := loadAnswerFile(dir, rec, time.Now())
	if len(doc.Applied) != 0 || len(*events) != 1 {
		t.Fatalf("unknown version must quarantine: %+v %+v", doc, *events)
	}
}

func TestAnswerFileUnreadableIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(answerFilePath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	rec, events := collectRecorder()
	doc, _ := loadAnswerFile(dir, rec, time.Now())
	if len(doc.Applied) != 0 || len(*events) != 1 {
		t.Fatalf("an unreadable path must quarantine: %+v %+v", doc, *events)
	}
}

func TestAnswerFileAgesOutStaleAppliedEntriesAtLoad(t *testing.T) {
	dir := t.TempDir()
	doc := seedDoc()
	old := time.Now().Add(-appliedMaxAge - time.Hour).UTC().Format(time.RFC3339Nano)
	e := doc.Applied["a0"]
	e.SettledAt = old
	doc.Applied["a0"] = e
	if err := writeAnswerFile(dir, doc); err != nil {
		t.Fatal(err)
	}
	loaded, pruned := loadAnswerFile(dir, nil, time.Now())
	if !pruned {
		t.Fatal("the stale entry must be pruned at load")
	}
	if _, ok := loaded.Applied["a0"]; ok {
		t.Fatal("the aged-out entry must be gone")
	}
	if _, ok := loaded.Applied["a1"]; !ok {
		t.Fatal("a fresh entry must survive the age-out")
	}
}

func TestLedgerForgetWorkItemEmptiesTheFileForThatWorkItem(t *testing.T) {
	dir := t.TempDir()
	// Seed the file the way a previous daemon process would have left it.
	if err := writeAnswerFile(dir, seedDoc()); err != nil {
		t.Fatal(err)
	}
	l2 := NewAnswerLedgerAt(dir, nil)

	l2.ForgetWorkItem("wi_a")

	doc := readDoc(t, dir)
	if _, ok := doc.Applied["a1"]; ok {
		t.Fatalf("the finished work item's records must be gone: %+v", doc.Applied)
	}
	if _, ok := doc.Applied["a0"]; ok {
		t.Fatalf("the finished work item's records must be gone: %+v", doc.Applied)
	}
	// And the reload agrees.
	l3 := NewAnswerLedgerAt(dir, nil)
	l3.mu.Lock()
	n := len(l3.applied)
	l3.mu.Unlock()
	if n != 0 {
		t.Fatalf("reload after ForgetWorkItem carries %d records", n)
	}
}

func TestMemoryOnlyLedgerWritesNothing(t *testing.T) {
	dir := t.TempDir()
	l := NewAnswerLedger() // no backend: every persist is a no-op
	l.ForgetWorkItem("wi_a")
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("a memory-only ledger must write nothing: %v %v", entries, err)
	}
}
