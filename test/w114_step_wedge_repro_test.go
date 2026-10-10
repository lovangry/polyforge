package test

// w114 repro (polyforge#114, root-cause lane step "repro"): the silent
// step-process wedge is UNDETECTABLE — the hang probe acks driver-alive
// while the step process sits dead (the #113 hand-found shape: a step
// process whose next provider call never returns parks alive with an
// open turn, no envelope, no exit, no transcript growth).
//
// Two scenarios against the REAL `pf daemon`, both scripted through the
// fakepi envelope vocabulary's new "park" action (test scaffolding only —
// it manufactures the #113 wedge shape deterministically; nothing in
// production imports fakepi):
//
//   - TestW114StepWedgeDetectedWithoutOperator is the PINNED FAILURE: the
//     first step parks silently past many hang windows, and the system
//     must detect it and recover WITHOUT operator intervention (the WI's
//     acceptance). Today it fails for the reported reason: the hang
//     probe fires, the bridge acks it from the DRIVER goroutine's
//     liveness (driver-alive ≠ step-alive), hang_cleared repeats every
//     window, and no needs_recovery ever lands.
//   - TestW114StepWedgeOperatorKillRecovers pins the OTHER half of the
//     report as a passing control: the identical wedge, then the
//     operator's kill (the #113 intervention). The kill path works —
//     envelope-missing → driver error → recovery → in-place resume →
//     wrap — proving DETECTION is the only missing piece.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// w114Plan parks the first dispatch of grill and scripts the healthy
// continuation for every later ordinal, so a daemon that DOES detect the
// wedge kills it, re-dispatches in place under the next ordinal, and the
// workflow completes and wraps.
func w114Plan() []map[string]any {
	return []map[string]any{
		// Ordinal 1: grill parks silently — open turn, then nothing:
		// no envelope, no exit, no further record, indefinitely.
		{"do": "envelope", "id": "grill", "park": true},
		// Ordinals 2..5: the recovery continuation (grill resume) and
		// the remaining steps, all healthy.
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
		{"do": "envelope", "id": "release", "outcome": "completed"},
	}
}

// w114WaitParked waits until the first step process has parked (the
// wedge is in place: park journaled, process alive, no envelope).
func w114WaitParked(t *testing.T, d *v1Daemon, wi string) {
	t.Helper()
	capDir := d.capsuleDir(wi)
	waitUntil(t, 30*time.Second, "the wedge never engaged (no park journal record for grill)", func() bool {
		for _, rec := range fakeJournal(t, capDir) {
			if rec["type"] == "park" && rec["step"] == "grill" {
				return true
			}
		}
		return false
	})
	waitUntil(t, 15*time.Second, "the parked step process is not alive", func() bool {
		return len(orphanPIDs(wi)) > 0
	})
}

// w114IncidentKinds lists the infra_incident payload kinds (order kept)
// — the watchdog surface the repro's failure message reports verbatim.
func w114IncidentKinds(t *testing.T, d *v1Daemon, wi string) []string {
	t.Helper()
	var out []string
	for _, e := range d.eventsOf(t, wi) {
		if e.Kind != "infra_incident" {
			continue
		}
		var b struct {
			Kind string `json:"kind"`
		}
		if len(e.Body) > 0 && json.Unmarshal(e.Body, &b) == nil && b.Kind != "" {
			out = append(out, b.Kind)
		}
	}
	return out
}

// w114WedgeDiagnostic is the repro's evidence snapshot: everything the
// supervision surface shows while the step process sits wedged.
func w114WedgeDiagnostic(t *testing.T, d *v1Daemon, inst *v1Install, wi string) string {
	t.Helper()
	capDir := d.capsuleDir(wi)
	var journalKinds []string
	for _, rec := range fakeJournal(t, capDir) {
		if k, _ := rec["type"].(string); k != "" {
			journalKinds = append(journalKinds, k)
		}
	}
	envelope := ""
	if raw, err := os.ReadFile(filepath.Join(capDir, "runtime-state", "pending-results", "1.json")); err == nil {
		envelope = fmt.Sprintf("%d bytes", len(raw))
	} else if os.IsNotExist(err) {
		envelope = "absent (the wedge never wrote it)"
	} else {
		envelope = "unreadable: " + err.Error()
	}
	return fmt.Sprintf(
		"wi status=%q (want eventual wrapped)\n"+
			"event kinds: %v\n"+
			"infra incident kinds: %v\n"+
			"fakepi journal: %v\n"+
			"step envelope drop (ordinal 1): %s\n"+
			"parked step processes alive: %v\n",
		inst.wiStatus(t, wi), d.eventKinds(t, wi), w114IncidentKinds(t, d, wi),
		journalKinds, envelope, orphanPIDs(wi))
}

// optWallWindow injects the §6.2 wall backstop (polyforge#114's new
// window) through the daemon's own flag — the same real path
// optObserveWindows uses. The detection scenario needs a bound small
// enough to fire inside its 30s observation window; the control scenario
// needs one LARGER than its 8s undetected-wedge window so the operator's
// kill stays the trigger under test (the pre-#114 world it pins).
func optWallWindow(wall time.Duration) daemonOpt {
	return func(d *v1Daemon) {
		if wall > 0 {
			d.extraArgs = append(d.extraArgs, "--wall-window", wall.String())
		}
	}
}

func TestW114StepWedgeDetectedWithoutOperator(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "w114 repro: step process parks silently mid-run (the #113 wedge)",
		w114Plan(), nil)
	d := newV1Daemon(t, inst, optObserveWindows(time.Second, 0, time.Second), optWallWindow(6*time.Second))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	// The wedge engages: grill's first dispatch parks (open turn, no
	// envelope, no exit, no transcript growth).
	w114WaitParked(t, d, wi)

	// THE PINNED FAILURE. The acceptance (polyforge#114): a step process
	// that parks silently past the window is detected and recovered
	// WITHOUT operator intervention. Detection must surface as a
	// needs_recovery event within a few hang windows of the park; today
	// nothing detects it and this wait times out — the failure message
	// carries the full supervision snapshot as the repro's observed
	// output (hang probes acked driver-alive, hang_cleared repeating,
	// zero recoveries, the wedge alive).
	deadline := time.Now().Add(30 * time.Second)
	for {
		sawRecovery := false
		for _, e := range d.eventsOf(t, wi) {
			if e.Kind == "needs_recovery" {
				sawRecovery = true
				break
			}
		}
		if sawRecovery {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the silent step-process wedge went UNDETECTED: no needs_recovery fired within %v "+
				"while grill's parked dispatch sat past dozens of 1s hang windows.\nobserved state:\n%s",
				30*time.Second, w114WedgeDiagnostic(t, d, inst, wi))
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Detected (post-fix world): the recovery re-dispatches in place and
	// the workflow completes and wraps, still with no operator action.
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)
}

func TestW114StepWedgeOperatorKillRecovers(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "w114 control: the #113 operator kill against the identical wedge",
		w114Plan(), nil)
	d := newV1Daemon(t, inst, optObserveWindows(time.Second, 0, time.Second), optWallWindow(45*time.Second))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	// The identical wedge engages.
	w114WaitParked(t, d, wi)

	// Let the wedge sit through several hang windows first — the exact
	// supervision state the operator found in #113 (probes acked, no
	// detection). Bound generously so the control never races a slow CI
	// box into a shorter wedge than the failure scenario observes.
	time.Sleep(8 * time.Second)

	// The operator intervention (#113: found by hand, killed by hand).
	pids := orphanPIDs(wi)
	if len(pids) == 0 {
		t.Fatalf("no live step process to kill — the wedge never engaged")
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	// The kill path works exactly as reported: envelope-missing exit →
	// driver error → recovery → in-place resume → wrap. ONLY detection
	// was missing.
	rec := waitForEvent(t, d, wi, "needs_recovery", 60*time.Second)
	var body struct {
		Reason string `json:"reason"`
	}
	eventBody(t, rec, &body)
	if body.Reason != "agent_death" {
		t.Fatalf("recovery reason %q, want agent_death (the killed wedge)", body.Reason)
	}
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)
	spawns := waitSpawnCount(t, d, wi, 5, 60*time.Second)
	if len(spawns) != 5 || spawns[1]["resume"] != true {
		t.Fatalf("the recovery was not an in-place resume: %+v", spawns)
	}
}
