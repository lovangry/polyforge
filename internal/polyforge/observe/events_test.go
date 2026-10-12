package observe

// events_test.go: #21 — the ring's seq is a per-WI MONOTONIC append
// counter (truncation only bounds retention, never the numbering), and
// the GET /observe ?after cursor filters on it.

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
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
		if err := l.Append("wi_seq", EvInfraIncident, nil); err != nil {
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
	if err := l.Append("wi_seq", EvInfraIncident, nil); err != nil {
		t.Fatal(err)
	}
	ring = l.Since("wi_seq")
	if got := ring[len(ring)-1].Seq; got != int64(total+1) {
		t.Fatalf("post-truncation seq: got %d, want %d", got, total+1)
	}
	// The durable file keeps every entry (truncation is a ring-only concept);
	// its wire shape stays protocol.EventEntry — seq is a ring/serve-layer
	// concept and is deliberately NOT written to the wire-stable file.
	fileEntries, err := ReadEventsFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(fileEntries) != total+1 {
		t.Fatalf("file entry count: got %d, want %d", len(fileEntries), total+1)
	}
	for i, e := range fileEntries {
		if e.Seq != 0 {
			t.Fatalf("file entry %d must not carry a ring seq: got %d", i, e.Seq)
		}
	}
	// Another WI's counter is independent.
	if err := l.Append("wi_other", EvInfraIncident, nil); err != nil {
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
		if err := l.Append("wi_cur", EvInfraIncident, nil); err != nil {
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
	// A cursor inside the truncated span yields the whole retained ring.
	for i := 0; i < RingCap+5; i++ {
		if err := l.Append("wi_cur", EvInfraIncident, nil); err != nil {
			t.Fatal(err)
		}
	}
	got = l.SinceAfter("wi_cur", 3)
	if len(got) != RingCap {
		t.Fatalf("truncated-span cursor: got %d entries, want %d", len(got), RingCap)
	}
	if got[0].Seq != int64(total+RingCap+5-RingCap+1) {
		t.Fatalf("truncated-span oldest seq: got %d", got[0].Seq)
	}
}

// The HTTP face honors ?after: same filtering over the wire, plus the
// bad-cursor 400.
func TestHTTPObserveAfterCursor(t *testing.T) {
	h := newFakeHandle("att_cur")
	hs := newHarness(t)
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == 200
	})
	// Generate a handful of events through execution.json (each
	// distinct question_id is one needs_input event).
	m := hs.o.lookup("wi_http")
	if m == nil {
		t.Fatal("monitor not registered")
	}
	for i := 0; i < 3; i++ {
		writePendingQuestion(t, m, "q_http_"+strconv.Itoa(i), "again?", "code")
	}
	soon(t, time.Second, "events appended", func() bool {
		return len(hs.o.Events().Since("wi_http")) >= 4 // job_started + 3 needs_input
	})
	ring := hs.o.Events().Since("wi_http")
	maxSeq := ring[len(ring)-1].Seq

	// after=0 (the default shape): the whole ring.
	_, body := hs.get(t, "/jobs/wi_http/observe?after=0")
	var view protocol.JobView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("view decode: %v (%s)", err, body)
	}
	if len(view.Events) != len(ring) {
		t.Fatalf("after=0 events: got %d, want %d", len(view.Events), len(ring))
	}
	// Served entries carry their seq (#21 gap: the client's next cursor is
	// the last event's seq, so the EventEntry wire must expose it).
	if got := view.Events[len(view.Events)-1].Seq; got != maxSeq {
		t.Fatalf("last served seq: got %d, want %d (the ring's newest)", got, maxSeq)
	}
	// The file layer's wire shape is unchanged: bare entries, no seq.
	// after=maxSeq: nothing new.
	_, body = hs.get(t, "/jobs/wi_http/observe?after="+strconv.FormatInt(maxSeq, 10))
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("view decode: %v (%s)", err, body)
	}
	if len(view.Events) != 0 {
		t.Fatalf("after=max events: got %d, want 0", len(view.Events))
	}
	// after=maxSeq-2: exactly the last two.
	_, body = hs.get(t, "/jobs/wi_http/observe?after="+strconv.FormatInt(maxSeq-2, 10))
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("view decode: %v (%s)", err, body)
	}
	if len(view.Events) != 2 {
		t.Fatalf("after=max-2 events: got %d, want 2", len(view.Events))
	}
	// A malformed cursor is DAEMON-400.
	if code, _ := hs.get(t, "/jobs/wi_http/observe?after=abc"); code != 400 {
		t.Fatalf("bad cursor: got %d, want 400", code)
	}
	if code, _ := hs.get(t, "/jobs/wi_http/observe?after=-1"); code != 400 {
		t.Fatalf("negative cursor: got %d, want 400", code)
	}
	// No cursor at all: the full projection (backward-compatible).
	if code, _ := hs.get(t, "/jobs/wi_http/observe"); code != 200 {
		t.Fatal("cursor-less observe must keep working")
	}
}
