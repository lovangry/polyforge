package supervise

// carries: internal/polyforge/supervise/contract.go, internal/polyforge/supervise/rules.go, internal/polyforge/supervise/tracker.go, internal/polyforge/supervise/defaults.go, internal/polyforge/supervise/session_stall_test.go

import (
	"strings"
	"testing"
	"time"
)

// session_stall_test.go: rule 0 session_stalled — the zero-output wedge of
// polyforge#42 (#38/#40: a dispatch whose live session file froze at the
// dispatch prompt, 58875 bytes / 5 lines, for 25-30 minutes while every
// other rule waited for an exit that never came). The live session file is
// the ground truth the record stream can only echo: zero bytes appended
// inside the session-stall window means the endpoint never delivered even
// its first token, and the candidate is judged unusable so the chain
// moves on. The misjudge guards are the ticket's own: the clock runs from
// the last write the watch observed (a write INSIDE the window restarts
// it — a slow first token that lands is a write, never a stall), never
// from the file's mere existence or a write that predates the window; any
// work-class signal defers to the record-based rules; and a watch that
// never saw the file leaves the dispatch to harness_silent.

func sessionStallThresholds() Thresholds {
	return Thresholds{SessionStall: 5 * time.Minute, Silence: 10 * time.Minute, StepTimeout: 90 * time.Minute}
}

func TestSessionStalledZeroOutputJudgedDead(t *testing.T) {
	c := &fixClock{now: time.Unix(1000, 0)}
	tr := newTrackerAt(c, sessionStallThresholds())
	// the watch's first observation: the dispatch prompt landed (the
	// incident's 58875 bytes / 5 lines) and then the file froze
	tr.SessionWrite(58875, c.now)

	c.advance(4 * time.Minute)
	if v := judge(c, tr); v != nil {
		t.Fatalf("inside the window: no verdict, got %+v", v)
	}
	c.advance(time.Minute + time.Second)
	v := judge(c, tr)
	if v == nil || v.Cause != CauseSessionStalled {
		t.Fatalf("verdict %+v, want session_stalled after the window with zero writes", v)
	}
	if !strings.HasPrefix(v.Evidence, "stalled: no session output within 5m0s") {
		t.Fatalf("evidence %q, want the required `stalled: no session output within <window>` log line", v.Evidence)
	}
	if !strings.Contains(v.Evidence, "58875 bytes") {
		t.Fatalf("evidence %q, want the frozen byte count named", v.Evidence)
	}
}

func TestSessionStalledNotMisjudged(t *testing.T) {
	t.Run("a write inside the window restarts the clock", func(t *testing.T) {
		c := &fixClock{now: time.Unix(1000, 0)}
		tr := newTrackerAt(c, sessionStallThresholds())
		tr.SessionWrite(58875, c.now)
		c.advance(4 * time.Minute)
		tr.SessionWrite(58876, c.now) // the slow first token finally lands
		c.advance(4 * time.Minute)
		if v := judge(c, tr); v != nil {
			t.Fatalf("a normal long task whose write lands late: no verdict, got %+v", v)
		}
		c.advance(time.Minute + time.Second)
		if v := judge(c, tr); v == nil || v.Cause != CauseSessionStalled {
			t.Fatalf("verdict %+v, want session_stalled once the window truly elapses after the last write", v)
		}
	})

	t.Run("a write that predates the window does not keep the dispatch alive", func(t *testing.T) {
		// the owner's ruling guard: the criterion sees writes inside the
		// window, never a write that happened before it
		c := &fixClock{now: time.Unix(1000, 0)}
		tr := newTrackerAt(c, sessionStallThresholds())
		tr.SessionWrite(100, c.now)
		c.advance(6 * time.Minute)
		if v := judge(c, tr); v == nil || v.Cause != CauseSessionStalled {
			t.Fatalf("verdict %+v, want session_stalled: the only write is outside the window", v)
		}
	})

	t.Run("a session last written before the dispatch counts from the dispatch", func(t *testing.T) {
		// a resumed dispatch appending to an old transcript: the file's
		// mtime predates StartedAt, so the clock starts at the dispatch
		c := &fixClock{now: time.Unix(1000, 0)}
		tr := newTrackerAt(c, sessionStallThresholds())
		tr.SessionWrite(100, c.now.Add(-time.Hour))
		c.advance(4 * time.Minute)
		if v := judge(c, tr); v != nil {
			t.Fatalf("inside the window from the dispatch: no verdict, got %+v", v)
		}
		c.advance(time.Minute + time.Second)
		if v := judge(c, tr); v == nil || v.Cause != CauseSessionStalled {
			t.Fatalf("verdict %+v, want session_stalled measured from the dispatch", v)
		}
	})

	t.Run("any work-class signal defers to the record-based rules", func(t *testing.T) {
		c := &fixClock{now: time.Unix(1000, 0)}
		tr := newTrackerAt(c, sessionStallThresholds())
		tr.SessionWrite(100, c.now)
		tr.ToolStart("c1", "bash", "{}") // live work: the tool window judges it, not rule 0
		c.advance(6 * time.Minute)
		if v := judge(c, tr); v != nil && v.Cause == CauseSessionStalled {
			t.Fatalf("a dispatch with live work signals must not be session_stalled, got %+v", v)
		}
	})

	t.Run("a watch that never saw the file leaves the dispatch to harness_silent", func(t *testing.T) {
		// no SessionWrite at all: no locatable transcript (or a monitor
		// without a watch) — session_stalled stays inert and the
		// record-based rule keeps its backstop
		c := &fixClock{now: time.Unix(1000, 0)}
		tr := newTrackerAt(c, sessionStallThresholds())
		c.advance(11 * time.Minute)
		if v := judge(c, tr); v == nil || v.Cause != CauseHarnessSilent {
			t.Fatalf("verdict %+v, want harness_silent: the backstop must survive the new rule", v)
		}
	})

	t.Run("the ask's observation gap does not count toward the window", func(t *testing.T) {
		c := &fixClock{now: time.Unix(1000, 0)}
		tr := newTrackerAt(c, sessionStallThresholds())
		tr.SessionWrite(100, c.now)
		c.advance(2 * time.Minute)   // 2 minutes watched
		c.advance(10 * time.Minute)  // the owner's ask: the supervisor does not watch
		tr.ObserveTick(c.now, 10*time.Minute)
		c.advance(2 * time.Minute) // 4 minutes watched in total
		if v := judge(c, tr); v != nil {
			t.Fatalf("the unwatched gap must not count toward the window, got %+v", v)
		}
	})
}

func TestSessionStallDefaults(t *testing.T) {
	// AC1's default: 5 minutes; a negative value passes Fill untouched
	// (disabled at the rule's own guard), zero is filled to the default.
	if got := (Defaults{}).Fill().SessionStall; got != 5*time.Minute {
		t.Fatalf("default SessionStall = %v, want 5m", got)
	}
	if got := (Defaults{SessionStall: -time.Minute}).Fill().SessionStall; got != -time.Minute {
		t.Fatalf("a negative SessionStall must pass Fill untouched (disabled), got %v", got)
	}
	if got := (Defaults{SessionStall: 42 * time.Second}).Fill().SessionStall; got != 42*time.Second {
		t.Fatalf("an explicit SessionStall must be kept, got %v", got)
	}
}
