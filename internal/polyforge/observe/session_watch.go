package observe

// session_watch.go: rule 0's clock source (polyforge#42 — zero-output
// no longer spins). The owner's misjudge guard, restated: the stall
// clock runs from the LAST OBSERVED WRITE, so a write inside the window
// restarts it and a long, quiet-but-alive task is never misjudged; a
// file that predates the dispatch counts from the dispatch (the rule's
// clock is max(StartedAt, last write)). The watch only reports what the
// file shows — size and mtime — and defers every judgement to the
// record rules in package supervise; a file it cannot stat (a claude
// session not yet visible, a vanished transcript) reports nothing, and
// the harness_silent backstop stays what judges then.

import (
	"os"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// sessionWatch feeds rule 0 from one dispatch's live session file: the
// resolved path (set once at step_started — the dispatch's live
// transcript does not move mid-flight) and the last write it observed.
// Owned by the monitor goroutine; every method runs there and only
// there (never under a lock — the stat is I/O).
type sessionWatch struct {
	path string
}

// newSessionWatch resolves the dispatch's live session file: a resumed
// dispatch appends to its own transcript (the capsule file itself; a
// claude dispatch's native session lives outside the capsule even when
// resumed, so it always resolves through the locator), anything else
// through the F3 locateLive locator. A nil locator or one that answers
// "" leaves the watch inert — rule 0 then never fires and
// harness_silent stays the backstop.
func newSessionWatch(capsuleDir string, f supervise.StepStarted, locate func(capsuleDir string, spawnAt time.Time, harness, session string) string) *sessionWatch {
	w := &sessionWatch{}
	if f.Resumed && f.Harness != "claude" {
		w.path = f.TranscriptPath
		return w
	}
	if locate != nil {
		w.path = locate(capsuleDir, f.StartedAt, f.Harness, f.HarnessSession)
	}
	return w
}

// feed stats the live file and reports the write it saw to the
// dispatch's tracker. One stat per monitor tick — the same cadence that
// judges. A vanished or unreadable file keeps the last known state: the
// rule stays on the record rules' side and the harness_silent backstop
// is unchanged. The tracker itself keeps the clock honest (a write that
// does not advance size or mtime does not restart it).
func (w *sessionWatch) feed(tr *supervise.Tracker) {
	if w == nil || w.path == "" {
		return
	}
	st, err := os.Stat(w.path)
	if err != nil {
		return
	}
	tr.SessionWrite(st.Size(), st.ModTime())
}
