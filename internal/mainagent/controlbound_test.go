package mainagent

// controlbound_test.go: R2N-9 regressions — the control path is bounded
// but never silently lossy:
//
//   - a harness flooding control-shaped lines past the queue bound gets
//     its control path CLOSED typed (ErrControlChannelOverflow) and the
//     agent marked unhealthy — the awaited ack never silently vanishes;
//   - a single control line beyond the bounded reader's limit closes the
//     control path typed (ErrControlLineTooLong) and marks the agent
//     unhealthy — the old bufio.Scanner died here SILENTLY (sc.Err was
//     ignored and every later freeze/boundary wait just timed out).

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

func writeHarness(t *testing.T, name, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitForUnhealthy(t *testing.T, proc *Proc) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if proc.Unhealthy() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the agent must be marked unhealthy after the control-path failure")
}

// TestControlQueueOverflowClosesLoudly: 5000 control-shaped lines from a
// flooding harness cross maxControlQueueLines; the control path must close
// typed and the agent be marked unhealthy, so the daemon's freeze/close
// fails fast and honest instead of hanging on an ack that can never come.
func TestControlQueueOverflowClosesLoudly(t *testing.T) {
	bin := writeHarness(t, "floodharness", `#!/bin/sh
i=0
while [ $i -lt 5000 ]; do
  echo '{"method":"noise"}'
  i=$((i+1))
done
sleep 300
`)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_flood")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)

	waitForUnhealthy(t, proc)
	if r, ok := proc.unhealthyReason(); !ok || !errors.Is(r, ErrControlChannelOverflow) {
		t.Fatalf("the unhealthy reason must be the typed overflow, got %v / %v", r, ok)
	}
	// Every later control call fails fast with the typed reason.
	qerr := h.Quiesce(context.Background())
	if !errors.Is(qerr, ErrControlChannelOverflow) {
		t.Fatalf("Quiesce against a closed control path must fail typed ErrControlChannelOverflow, got %v", qerr)
	}
	if !proc.Unhealthy() {
		t.Fatal("the agent must be observable as unhealthy")
	}
}

// TestOverlongControlLineClosesControlPathLoudly: one control-shaped line
// beyond maxControlLineBytes must close the control path typed and mark
// the agent unhealthy — never the old silent Scanner death — while the
// reader keeps draining so the harness cannot block on its own stdout.
func TestOverlongControlLineClosesControlPathLoudly(t *testing.T) {
	bin := writeHarness(t, "giantlineharness", `#!/bin/sh
printf '{"method":"%s"}\n' "$(head -c 5000000 /dev/zero | tr '\0' 'a')"
echo '{"method":"still-alive"}'
sleep 300
`)
	s := NewSupervisor(bin, bin)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_giant")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	proc := h.(*Proc)

	waitForUnhealthy(t, proc)
	if r, ok := proc.unhealthyReason(); !ok || !errors.Is(r, ErrControlLineTooLong) {
		t.Fatalf("the unhealthy reason must be the typed overlong-line error, got %v / %v", r, ok)
	}
	// The reader survived the overlong line and kept the child un-blocked:
	// the harness is still alive.
	if !h.Alive() {
		t.Fatal("the harness must survive an overlong line (reader keeps draining)")
	}
	qerr := h.Quiesce(context.Background())
	if !errors.Is(qerr, ErrControlLineTooLong) {
		t.Fatalf("Quiesce after an overlong line must fail typed ErrControlLineTooLong, got %v", qerr)
	}
	if !proc.Unhealthy() {
		t.Fatal("the agent must be observable as unhealthy")
	}
}

// TestReadBoundedLineFrames: unit drill over the bounded reader — normal
// lines, CR handling, an unterminated final line, an exactly-at-limit
// line and an overlong line all frame correctly, and the overlong line
// does not swallow the line AFTER it.
func TestReadBoundedLineFrames(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("first\r\nsecond\n"), 64)
	if l, tl, err := readBoundedLine(r, 1024); err != nil || tl || l != "first" {
		t.Fatalf("CRLF line: %q %v %v", l, tl, err)
	}
	if l, tl, err := readBoundedLine(r, 1024); err != nil || tl || l != "second" {
		t.Fatalf("plain line: %q %v %v", l, tl, err)
	}
	// Unterminated final line (Scanner parity).
	r = bufio.NewReaderSize(strings.NewReader("tail-no-newline"), 64)
	if l, tl, err := readBoundedLine(r, 1024); err != nil || tl || l != "tail-no-newline" {
		t.Fatalf("unterminated tail: %q %v %v", l, tl, err)
	}
	// Exactly at the limit: legal, not tooLong.
	atLimit := strings.Repeat("a", 100) + "\n"
	r = bufio.NewReaderSize(strings.NewReader(atLimit), 8)
	if l, tl, err := readBoundedLine(r, 100); err != nil || tl || l != strings.Repeat("a", 100) {
		t.Fatalf("at-limit line: len=%d %v %v", len(l), tl, err)
	}
	// Overlong: reported, and the NEXT line still frames.
	overlong := strings.Repeat("a", 300) + "\nafter\n"
	r = bufio.NewReaderSize(strings.NewReader(overlong), 8)
	if _, tl, err := readBoundedLine(r, 100); err != nil || !tl {
		t.Fatalf("overlong line must be reported: %v %v", tl, err)
	}
	if l, tl, err := readBoundedLine(r, 100); err != nil || tl || l != "after" {
		t.Fatalf("the line after the overlong one must frame: %q %v %v", l, tl, err)
	}
}

// TestReadBoundedLineCompletedPayloadBound (R3N-F9): the size bound is
// enforced on COMPLETED lines too, not only on the ErrBufferFull
// accumulation path. A line that fits the reader's whole buffer in ONE
// ReadSlice (or ends at EOF) used to be appended without any length check,
// so over-limit completed lines evaded ErrControlLineTooLong. The matrix:
// payloads of limit-1 / limit / limit+1 for LF, CRLF and EOF endings.
//
// The bound applies to line CONTENT — the WHOLE terminator is framing
// (R4N-4: the CRLF carriage return is part of the terminator, not
// content; the old expectation here counted it as content and wrongly
// rejected a max-content CRLF line) — so:
//   - LF:   payloads limit-1 and limit are legal, limit+1 is tooLong;
//   - CRLF: payloads limit-1 and limit are legal, limit+1 is tooLong;
//   - EOF:  unterminated payloads follow the LF rule (no terminator).
func TestReadBoundedLineCompletedPayloadBound(t *testing.T) {
	const limit = 64
	// One ReadSlice per line: the reader's buffer exceeds every line, so
	// ONLY the completed-line check (the R3N-F9 fix) can flag the
	// over-limit cases.
	big := func(s string) *bufio.Reader { return bufio.NewReaderSize(strings.NewReader(s), 4096) }
	payload := func(n int) string { return strings.Repeat("a", n) }

	cases := []struct {
		ending string // "lf" | "crlf" | "eof"
		n      int    // payload length
		tooLng bool
	}{
		{"lf", limit - 1, false},
		{"lf", limit, false},
		{"lf", limit + 1, true},
		{"crlf", limit - 1, false},
		{"crlf", limit, false},
		{"crlf", limit + 1, true},
		{"eof", limit - 1, false},
		{"eof", limit, false},
		{"eof", limit + 1, true},
	}
	for _, c := range cases {
		var in string
		switch c.ending {
		case "lf":
			in = payload(c.n) + "\n"
		case "crlf":
			in = payload(c.n) + "\r\n"
		case "eof":
			in = payload(c.n)
		}
		l, tl, err := readBoundedLine(big(in), limit)
		if err != nil {
			t.Fatalf("%s payload %d: %v", c.ending, c.n, err)
		}
		if tl != c.tooLng {
			t.Fatalf("%s payload %d: tooLong=%v, want %v (line len %d)", c.ending, c.n, tl, c.tooLng, len(l))
		}
		if !c.tooLng {
			want := payload(c.n)
			if c.ending == "crlf" {
				want = payload(c.n) // the trailing \r is trimmed
			}
			if l != want {
				t.Fatalf("%s payload %d: wrong line content (len %d)", c.ending, c.n, len(l))
			}
		}
	}

	// Small-buffer variant: the same over-limit line, fed through the
	// ErrBufferFull accumulation path, must be flagged AND leave the
	// stream framed for the next line.
	small := bufio.NewReaderSize(strings.NewReader(payload(limit+1)+"\nafter\n"), 8)
	if _, tl, err := readBoundedLine(small, limit); err != nil || !tl {
		t.Fatalf("small-buffer over-limit completed line must be flagged: %v %v", tl, err)
	}
	if l, tl, err := readBoundedLine(small, limit); err != nil || tl || l != "after" {
		t.Fatalf("the line after the flagged one must frame: %q %v %v", l, tl, err)
	}
	// R4N-4, accumulation path: a max-content CRLF line whose terminator
	// may split across ErrBufferFull chunks must PASS — the full "\r\n"
	// is framing, and the CR is measured as part of the terminator even
	// when it lands at a chunk boundary.
	small2 := bufio.NewReaderSize(strings.NewReader(payload(limit)+"\r\nafter\n"), 8)
	if l, tl, err := readBoundedLine(small2, limit); err != nil || tl || l != payload(limit) {
		t.Fatalf("small-buffer at-limit CRLF line must pass unflagged: %q %v %v", l, tl, err)
	}
	if l, tl, err := readBoundedLine(small2, limit); err != nil || tl || l != "after" {
		t.Fatalf("the line after the at-limit CRLF one must frame: %q %v %v", l, tl, err)
	}
}
