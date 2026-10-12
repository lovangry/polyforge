package observe

// events_test.go: #21 — the seq is a per-WI MONOTONIC append counter
// (truncation only bounds retention, never the numbering), and the
// SinceAfter cursor filters on it. v2: the id is persisted on
// every line and survives daemon restarts, the file rotates by size, and
// history is served from the durable files, not only the in-memory ring.

import (
	"os"
	"testing"
)

// Seqs keep counting up after the ring has truncated: the last retained
// entry of an overfull ring carries the TOTAL append count, and the file
// keeps every entry.
func TestEventSeqMonotonicAcrossTruncation(t *testing.T) {
	dir := t.TempDir()
	l, err := NewEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const total = RingCap + 50
	for i := 0; i < total; i++ {
		if err := l.Append("wi_seq", "t_event", nil); err != nil {
			t.Fatal(err)
		}
	}
	ring := l.Since("wi_seq")
	if len(ring) != RingCap {
		t.Fatalf("retained ring size: got %d, want %d", len(ring), RingCap)
	}
	// Oldest retained = total-RingCap+1, newest = total.
	if got := ring[0].Seq; got != int64(total-RingCap+1) {
		t.Fatalf("oldest retained seq: got %d, want %d", got, total-RingCap+1)
	}
	if got := ring[RingCap-1].Seq; got != int64(total) {
		t.Fatalf("newest retained seq: got %d, want %d", got, total)
	}
	// A NEW entry after truncation still advances (the #21 bug pinned it
	// at RingCap+1 forever).
	if err := l.Append("wi_seq", "t_event", nil); err != nil {
		t.Fatal(err)
	}
	ring = l.Since("wi_seq")
	if got := ring[len(ring)-1].Seq; got != int64(total+1) {
		t.Fatalf("post-truncation seq: got %d, want %d", got, total+1)
	}
	// The durable file keeps every entry (truncation is a ring-only
	// concept) and EVERY line carries its id: the numbering must survive a
	// restart.
	fileEntries, err := ReadEventsFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(fileEntries) != total+1 {
		t.Fatalf("file entry count: got %d, want %d", len(fileEntries), total+1)
	}
	for i, e := range fileEntries {
		if e.Seq != int64(i+1) {
			t.Fatalf("file entry %d carries seq %d, want %d", i, e.Seq, i+1)
		}
	}
	// Another WI's counter is independent.
	if err := l.Append("wi_other", "t_event", nil); err != nil {
		t.Fatal(err)
	}
	if got := l.Since("wi_other")[0].Seq; got != 1 {
		t.Fatalf("second WI's first seq: got %d, want 1", got)
	}
}

// SinceAfter filters strictly by seq: the ?after cursor.
func TestSinceAfterCursor(t *testing.T) {
	dir := t.TempDir()
	l, err := NewEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const total = 10
	for i := 0; i < total; i++ {
		if err := l.Append("wi_cur", "t_event", nil); err != nil {
			t.Fatal(err)
		}
	}
	got := l.SinceAfter("wi_cur", 4)
	if len(got) != total-4 {
		t.Fatalf("after=4: got %d entries, want %d", len(got), total-4)
	}
	if got[0].Seq != 5 || got[len(got)-1].Seq != int64(total) {
		t.Fatalf("after=4 span: %d..%d", got[0].Seq, got[len(got)-1].Seq)
	}
	if got := l.SinceAfter("wi_cur", int64(total)); len(got) != 0 {
		t.Fatalf("after=total: got %d entries, want 0", len(got))
	}
	if got := l.SinceAfter("wi_cur", 0); len(got) != total {
		t.Fatalf("after=0: got %d entries, want %d", len(got), total)
	}
	if got := l.SinceAfter("wi_nobody", 0); len(got) != 0 {
		t.Fatalf("unknown WI: got %d entries, want 0", len(got))
	}
	// A cursor inside the truncated span is served from the DURABLE file
	// (v2: history is not limited to the ring): every entry after it.
	for i := 0; i < RingCap+5; i++ {
		if err := l.Append("wi_cur", "t_event", nil); err != nil {
			t.Fatal(err)
		}
	}
	got = l.SinceAfter("wi_cur", 3)
	if want := total + RingCap + 5 - 3; len(got) != want {
		t.Fatalf("truncated-span cursor: got %d entries, want %d (the whole durable history after it)", len(got), want)
	}
	if got[0].Seq != 4 {
		t.Fatalf("truncated-span oldest seq: got %d", got[0].Seq)
	}
}

// The ids are monotonic ACROSS daemon restarts: a reopened log continues
// every WI's numbering (the old per-process counter restarted at 1 and
// collided with earlier runs' lines).
func TestEventSeqMonotonicAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	l, err := NewEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		_ = l.Append("wi_a", "t_event", map[string]int{"i": i})
	}
	_ = l.Append("wi_b", "t_event", nil)
	_ = l.Close()

	l2, err := NewEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	_ = l2.Append("wi_a", "t_event", nil)
	_ = l2.Append("wi_b", "t_event", nil)
	ring := l2.Since("wi_a")
	if got := ring[len(ring)-1].Seq; got != 8 {
		t.Fatalf("wi_a first seq after restart: got %d, want 8", got)
	}
	if len(ring) != 8 {
		t.Fatalf("the ring must be pre-filled from the durable file: %d entries, want 8", len(ring))
	}
	if got := l2.Since("wi_b")[1].Seq; got != 2 {
		t.Fatalf("wi_b second seq: got %d, want 2", got)
	}
}

// The file rotates by size and the numbering survives the rotation AND a
// restart — even for a WI whose newest lines sit in a generation that was
// later dropped (the counter snapshot written at rotation covers it).
func TestEventLogRotatesBySizeAndKeepsCounting(t *testing.T) {
	dir := t.TempDir()
	l, err := NewEventLogOpts(dir, EventLogOpts{MaxBytes: 600, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Append("wi_old", "t_event", nil)
	_ = l.Append("wi_old", "t_event", nil)
	for i := 0; i < 60; i++ { // enough to rotate several times and drop wi_old's lines
		if err := l.Append("wi_hot", "t_event", map[string]string{"pad": "xxxxxxxxxxxxxxxxxxxxxxxx"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(l.Path() + ".1"); err != nil {
		t.Fatalf("the file must have rotated: %v", err)
	}
	if _, err := os.Stat(l.Path() + ".3"); err == nil {
		t.Fatal("only Keep generations may remain")
	}
	st, _ := os.Stat(l.Path())
	if st.Size() > 600+400 {
		t.Fatalf("the live file is oversized: %d", st.Size())
	}
	_ = l.Close()

	l2, err := NewEventLogOpts(dir, EventLogOpts{MaxBytes: 600, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	_ = l2.Append("wi_hot", "t_event", nil)
	hot := l2.Since("wi_hot")
	if got := hot[len(hot)-1].Seq; got != 61 {
		t.Fatalf("wi_hot seq after rotation+restart: got %d, want 61", got)
	}
	// wi_old's lines rotated away entirely; its counter must not restart
	_ = l2.Append("wi_old", "t_event", nil)
	old := l2.Since("wi_old")
	if got := old[len(old)-1].Seq; got != 3 {
		t.Fatalf("wi_old seq after its lines rotated away: got %d, want 3 (snapshot-backed)", got)
	}
}

// A cursor older than the ring is answered from the durable files:
// history is not limited to the in-memory ring.
func TestSinceAfterServesHistoryFromDurableFile(t *testing.T) {
	dir := t.TempDir()
	l, err := NewEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	total := RingCap + 100
	for i := 0; i < total; i++ {
		_ = l.Append("wi_h", "t_event", nil)
	}
	got := l.SinceAfter("wi_h", 10)
	if len(got) != total-10 {
		t.Fatalf("history after=10: got %d entries, want %d (the ring holds only %d)", len(got), total-10, RingCap)
	}
	if got[0].Seq != 11 || got[len(got)-1].Seq != int64(total) {
		t.Fatalf("history span %d..%d", got[0].Seq, got[len(got)-1].Seq)
	}
}

// An append failure is a degradation, never a correctness dependency:
// the error is returned, the live ring and the counters keep working.
func TestEventAppendFailureIsDegradationOnly(t *testing.T) {
	dir := t.TempDir()
	l, err := NewEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close() // writes now fail
	if err := l.Append("wi_f", "t_event", nil); err == nil {
		t.Fatal("a failed file write must be reported to the caller")
	}
	if got := l.Since("wi_f"); len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("the ring must still record the event: %+v", got)
	}
}
