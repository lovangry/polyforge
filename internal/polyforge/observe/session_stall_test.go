package observe

// session_stall_test.go: polyforge#42's watch half — the supervisor's
// rule 0 (supervise/rules.go) judges a dispatch whose live session file
// stopped writing; these tests pin the MONITOR feeding it from the file
// the F3 locator resolves (the controllable fake stub of AC4: a temp
// .jsonl the test itself writes — no real endpoint involved).
//
// carries: monitor.go (the per-dispatch session watch and its per-tick
// feed), fake_test.go (the monitor scaffolding), and the session-stall
// rule in package supervise via the verdicts it returns.
//
// The frozen half is the incident (a transcript that never grows: the
// agent process sits there producing nothing); the growing half is the
// owner's misjudge guard — a write inside the window restarts the clock,
// so a long, quiet-but-alive task stays judged alive.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// freezeSession writes the test's frozen live transcript and answers its
// path to the monitor's locator.
func freezeSession(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 58875)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// stallMonitor is newTestMonitor over a locator that always resolves the
// frozen-or-growing file the test controls.
func stallMonitor(t *testing.T, session string) *Monitor {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.locateLive = func(capsuleDir string, spawnAt time.Time, harness, sess string) string {
			return session
		}
	})
	return m
}

// TestSessionStalledFrozenTranscriptVerdict: a dispatch whose live
// session file never grows is judged dead with cause session_stalled,
// the evidence opening with the AC1 line, naming the byte count — and
// only ONE verdict leaves the dispatch.
func TestSessionStalledFrozenTranscriptVerdict(t *testing.T) {
	session := freezeSession(t)
	m := stallMonitor(t, session)
	// Silence an hour out: only the session stall can speak.
	startDispatch(m, 1, "grill", supervise.Thresholds{Silence: time.Hour, SessionStall: 50 * time.Millisecond})

	v := nextVerdict(t, m, "session_stalled")
	dead, ok := v.(supervise.StepDead)
	if !ok || dead.Cause != supervise.CauseSessionStalled || dead.SpawnN != 1 || dead.Step != "grill" {
		t.Fatalf("verdict = %+v, want step_dead session_stalled for grill@1", v)
	}
	if !strings.HasPrefix(dead.Evidence, "stalled: no session output within 50ms") {
		t.Fatalf("evidence %q must open with the stalled: no session output line", dead.Evidence)
	}
	if !strings.Contains(dead.Evidence, "58875 bytes") {
		t.Fatalf("evidence %q must name the frozen byte count", dead.Evidence)
	}
	// one verdict per dispatch ("已发后静默到 step_ended")
	expectNoVerdict(t, m, 150*time.Millisecond, "after the verdict")
}

// TestSessionStalledGrowingTranscriptStaysSilent: a session file that
// keeps writing (a long, quiet-but-alive task) is NOT misjudged — every
// write restarts the clock — and only after the writing really stops does
// the verdict come. The post-stop verdict keeps the control honest: it
// fails against a watch that never locates the file, not just one that
// never fires.
func TestSessionStalledGrowingTranscriptStaysSilent(t *testing.T) {
	session := freezeSession(t)
	m := stallMonitor(t, session)
	startDispatch(m, 1, "grill", supervise.Thresholds{Silence: time.Hour, SessionStall: 60 * time.Millisecond})

	// grow the file every 20ms for 300ms — always inside the window
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f, err := os.OpenFile(session, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			if _, err := f.WriteString(strings.Repeat("y", 64)); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	expectNoVerdict(t, m, 300*time.Millisecond, "a session file that keeps growing")
	wg.Wait()

	// the writing stopped: the verdict now comes
	v := nextVerdict(t, m, "session_stalled after the growth stopped")
	dead, ok := v.(supervise.StepDead)
	if !ok || dead.Cause != supervise.CauseSessionStalled {
		t.Fatalf("verdict = %+v, want step_dead session_stalled", v)
	}
}

// TestSessionStalledUnlocatableTranscriptFallsBack: no locatable live
// transcript (a claude dispatch's native file not yet visible, a pi
// session not yet created) leaves rule 0 inert — the harness_silent
// backstop is what judges, unchanged (polyforge#38/#40's shape).
func TestSessionStalledUnlocatableTranscriptFallsBack(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.locateLive = func(capsuleDir string, spawnAt time.Time, harness, sess string) string {
			return ""
		}
	})
	startDispatch(m, 1, "grill", supervise.Thresholds{Silence: 100 * time.Millisecond, SessionStall: 50 * time.Millisecond})

	v := nextVerdict(t, m, "harness_silent")
	if dead, ok := v.(supervise.StepDead); !ok || dead.Cause != supervise.CauseHarnessSilent {
		t.Fatalf("verdict = %+v, want the harness_silent backstop", v)
	}
}
