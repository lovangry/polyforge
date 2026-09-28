package daemon

// watchdog_test.go: the dual runtime watchdog's regressions (04 §6.2
// runtime note, polyforge#44 refined option A). Fake-harness style against
// the real Core (newDaemonEnv), with the fake WI Agent as a real child
// process so the procfs activity probe observes a REAL /proc entry:
//
//   - (a) a never-returns harness: flat activity over the (shrunk) window
//     ⇒ killed ⇒ crash continuation recovers the step (same session id,
//     same generation, pid written back, Hangs=1 persisted);
//   - (b) a runtime kill of the agent: the dispatcher tick observes the
//     death and re-derives WITHOUT a daemon restart, within a bounded
//     small time;
//   - (c) the SAME step hung twice consecutively ⇒ bounded termination:
//     typed verdict, end-generation reason=block + waiting_on=
//     runtime:<machine>, no third relaunch;
//   - (d) a slow-but-alive harness (a trickle of disk writes): NOT killed;
//   - (e) the wall backstop fires when the activity probe is unavailable
//     (procfs unreadable ⇒ wall-clock-only degradation);
//   - plus: needs_input waits are exempt from both probes, the step wall
//     budget's single source (materialized workflow.md frontmatter), the
//     restart-recovery deny gate, and the persisted verdict's validation.

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// wdTickFor drives the watchdog tick until cond holds or the bound
// elapses; it returns whether cond held (never a silent timeout).
func wdTickFor(t *testing.T, d *Daemon, bound time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(bound)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		d.watchdogTick(context.Background())
		time.Sleep(60 * time.Millisecond)
	}
	return true
}

// wdTicksFor just drives the tick for a fixed duration (the
// "nothing must happen" tests).
func wdTicksFor(t *testing.T, d *Daemon, dur time.Duration) {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		d.watchdogTick(context.Background())
		time.Sleep(60 * time.Millisecond)
	}
}

func wdJobPresent(d *Daemon, wi string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.jobs[wi]
	return ok
}

func wdRingOf(d *Daemon, wi string) *Ring {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.jobs[wi].ring
}

func wdRingHas(ring *Ring, typ, cause string) bool {
	obs := ring.Observe(0, nil)
	for _, ev := range obs.Events {
		if ev.Type != typ {
			continue
		}
		if cause == "" {
			return true
		}
		if c, _ := ev.Payload["cause"].(string); c == cause {
			return true
		}
	}
	return false
}

// wdWriteExecState lands a minimal execution.json for the demo bundle's
// single "code" step (the WI Agent's single-writer domain is deliberately
// bypassed by the TEST only, through the same capsule write face).
func wdWriteExecState(t *testing.T, d *Daemon, wi string, mut func(*capsule.Execution)) {
	t.Helper()
	capc, _, err := capsule.Open(d.cfg.WorkspaceRoot, wi)
	if err != nil {
		t.Fatal(err)
	}
	e := &capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: wi, Generation: 1,
		BundleDigest: "sha256:demo", CurrentStep: "code",
		WorkflowStatus: capsule.StatusRunning,
		Steps:          map[string]*capsule.Step{"code": {Status: capsule.StepInProgress}},
		Version:        1,
	}
	if mut != nil {
		mut(e)
	}
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	if err := capc.SaveExecution(e); err != nil {
		t.Fatal(err)
	}
}

// wdSpawn submits + spawns one job and returns the env.
func wdSpawn(t *testing.T, cfgTweak func(*Config)) (*denv, context.Context) {
	t.Helper()
	e := newDaemonEnv(t, cfgTweak)
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim", Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.spawner.spawns) != 1 {
		t.Fatalf("precondition: exactly one spawn, got %d", len(e.spawner.spawns))
	}
	return e, ctx
}

// (a) TestWatchdogHangKillsAndCrashContinues: the never-returns harness.
func TestWatchdogHangKillsAndCrashContinues(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the activity probe is Linux-only")
	}
	e, _ := wdSpawn(t, func(c *Config) { c.WatchdogActivityWindow = 250 * time.Millisecond })
	ring := wdRingOf(e.d, e.wi)

	if !wdTickFor(t, e.d, 8*time.Second, func() bool { return len(e.spawner.spawns) >= 2 }) {
		t.Fatalf("the flat-activity window must fire and re-derive: spawns=%d", len(e.spawner.spawns))
	}
	// The crash continuation is the §7.1 step-4 shape: SAME generation,
	// SAME session id (the only session-reuse case), a NEW process.
	if e.spawner.spawns[0].SessionID != e.spawner.spawns[1].SessionID ||
		e.spawner.spawns[0].Generation != e.spawner.spawns[1].Generation {
		t.Fatalf("continuation must reuse session id + generation: %+v vs %+v",
			e.spawner.spawns[0], e.spawner.spawns[1])
	}
	// The judged-hung agent is really dead (its whole group collected).
	if processAlive(e.spawner.agents[0].PID(), e.spawner.agents[0].StartedAt()) {
		t.Fatal("the hung agent must be killed before the re-derivation")
	}
	// The jobs[] row carries the NEW pid and the persisted storm counter.
	st, err := NewStateStore(e.d.cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	j := st.FindJob(e.wi)
	if j == nil || j.MainAgentPID != e.spawner.agents[1].PID() {
		t.Fatalf("the jobs[] row must carry the re-derived pid: %+v", j)
	}
	if j.Watchdog == nil || j.Watchdog.Hangs != 1 {
		t.Fatalf("the first hang must persist Hangs=1 (before the relaunch, crash-honest): %+v", j.Watchdog)
	}
	if !wdRingHas(ring, EvWIAgentRederived, "hang") {
		t.Fatal("the ring must carry the rederive event with the hang cause")
	}
}

// (b) TestWatchdogDeathObservedWithinTick: a runtime kill of the agent is
// observed by the dispatcher tick and crash-continued WITHOUT any daemon
// restart, within a bounded small time (one 200ms tick + the re-derivation).
func TestWatchdogDeathObservedWithinTick(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the death probe fallback is Linux-only")
	}
	e, _ := wdSpawn(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.d.Run(ctx)

	// The agent dies for real, mid-step, with NO daemon involvement.
	a0 := e.spawner.agents[0]
	if err := a0.child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = a0.child.Wait()

	killedAt := time.Now()
	if !wdTickFor(t, e.d, 5*time.Second, func() bool { return len(e.spawner.spawns) >= 2 }) {
		t.Fatalf("the tick must observe the death and re-derive: spawns=%d", len(e.spawner.spawns))
	}
	if elapsed := time.Since(killedAt); elapsed > 3*time.Second {
		t.Fatalf("the takeover must land within a bounded small time, took %s", elapsed)
	}
	if e.spawner.spawns[0].SessionID != e.spawner.spawns[1].SessionID {
		t.Fatal("the death continuation must reuse the session id")
	}
	st, err := NewStateStore(e.d.cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if j := st.FindJob(e.wi); j == nil || j.MainAgentPID != e.spawner.agents[1].PID() {
		t.Fatalf("the jobs[] row must carry the re-derived pid: %+v", j)
	}
}

// (c) TestWatchdogStormBoundedTermination: the SAME step hung twice
// consecutively ⇒ the typed bounded-termination verdict, the job closed
// with the infra-failure/blocked shape server-side, and NEVER a third
// relaunch.
func TestWatchdogStormBoundedTermination(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the activity probe is Linux-only")
	}
	e, _ := wdSpawn(t, func(c *Config) { c.WatchdogActivityWindow = 250 * time.Millisecond })
	wdWriteExecState(t, e.d, e.wi, nil) // current_step = "code", in_progress
	ring := wdRingOf(e.d, e.wi)

	if !wdTickFor(t, e.d, 20*time.Second, func() bool { return !wdJobPresent(e.d, e.wi) }) {
		t.Fatalf("the second consecutive hang of step code must bounded-terminate the job (spawns=%d)", len(e.spawner.spawns))
	}
	// No third relaunch, ever: one crash continuation then the verdict.
	if n := len(e.spawner.spawns); n != 2 {
		t.Fatalf("exactly one relaunch before the bounded termination, got %d spawns", n)
	}
	// The ring tells the live observer both halves of the story.
	if !wdRingHas(ring, EvWIAgentRederived, "hang") {
		t.Fatal("the ring must carry the first hang's rederive event")
	}
	if !wdRingHas(ring, EvJobStopped, "watchdog_bounded_termination") {
		t.Fatal("the ring must carry the bounded-termination stop")
	}
	// The jobs[] row is gone (the close sealed the job).
	st, err := NewStateStore(e.d.cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.FindJob(e.wi) != nil {
		t.Fatal("the bounded-terminated job must be sealed out of jobs[]")
	}
	// Server-side: the infra-failure/blocked shape — status blocked, the
	// WI waiting on THIS machine's recovery (02 §2.4 closed value domain).
	var status, waitingOn string
	var won sql.NullString
	if err := e.db.QueryRow(`SELECT status, waiting_on FROM work_items WHERE id=?1`, e.wi).Scan(&status, &won); err != nil {
		t.Fatal(err)
	}
	if won.Valid {
		waitingOn = won.String
	}
	if status != "blocked" || waitingOn != "runtime:mac_a" {
		t.Fatalf("bounded termination must end blocked on runtime:<machine>, got status=%q waiting_on=%q", status, waitingOn)
	}
	// A few more ticks change nothing: the verdict holds.
	for i := 0; i < 5; i++ {
		e.d.watchdogTick(context.Background())
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(e.spawner.spawns); n != 2 {
		t.Fatalf("no third relaunch after the bounded termination, got %d spawns", n)
	}
}

// (d) TestWatchdogTrickleHarnessSurvives: a harness that keeps a trickle
// of procfs activity moving (disk writes every 50ms) is never touched, no
// matter how far beyond the hang window it runs.
func TestWatchdogTrickleHarnessSurvives(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the activity probe is Linux-only")
	}
	trickle := filepath.Join(t.TempDir(), "trickle.out")
	e := newDaemonEnv(t, func(c *Config) { c.WatchdogActivityWindow = 250 * time.Millisecond })
	e.spawner.cmd = func() *exec.Cmd {
		return exec.Command("sh", "-c", "while :; do echo x >> "+trickle+"; sleep 0.05; done")
	}
	t.Cleanup(func() {
		for _, a := range e.spawner.agents {
			if a.child != nil && a.child.Process != nil {
				_ = a.child.Process.Kill()
			}
		}
	})
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim", Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	a0 := e.spawner.agents[0]
	// Tick for ~8x the hang window: the trickle keeps every counter moving.
	wdTicksFor(t, e.d, 2*time.Second)
	if n := len(e.spawner.spawns); n != 1 {
		t.Fatalf("a slow-but-alive agent must never be killed, got %d spawns", n)
	}
	if !processAlive(a0.PID(), a0.StartedAt()) {
		t.Fatal("a slow-but-alive agent must still be alive")
	}
}

// (e) TestWatchdogBackstopWhenProbeUnavailable: with procfs unreadable the
// hang watchdog degrades to wall-clock-only (and says so once at startup),
// and the FIXED absolute backstop still bounds the instance.
func TestWatchdogBackstopWhenProbeUnavailable(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the degradation path needs a readable /proc to swap away")
	}
	savedProc, savedWall, savedFloor := watchProcRoot, watchdogStepWallDefault, watchdogBackstopFloor
	watchProcRoot = filepath.Join(t.TempDir(), "no-proc-here")
	// Shrink the FIXED formula's inputs (test seam only): step wall default
	// 50ms ⇒ 3x = 150ms; floor 100ms ⇒ backstop = max(150ms, 100ms) = 150ms.
	watchdogStepWallDefault = 50 * time.Millisecond
	watchdogBackstopFloor = 100 * time.Millisecond
	defer func() {
		watchProcRoot, watchdogStepWallDefault, watchdogBackstopFloor = savedProc, savedWall, savedFloor
	}()

	e, _ := wdSpawn(t, nil)
	if e.d.wdProbeAvailable {
		t.Fatal("with an unreadable procfs the startup probe must report wall-clock-only degradation")
	}
	if !wdTickFor(t, e.d, 8*time.Second, func() bool { return len(e.spawner.spawns) >= 2 }) {
		t.Fatalf("the absolute backstop must fire when the activity probe is unavailable: spawns=%d", len(e.spawner.spawns))
	}
	st, err := NewStateStore(e.d.cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	j := st.FindJob(e.wi)
	if j == nil || j.Watchdog == nil || j.Watchdog.Hangs != 1 {
		t.Fatalf("the backstop incident must persist Hangs=1: %+v", j)
	}
	if j.MainAgentPID != e.spawner.agents[1].PID() {
		t.Fatalf("the backstop continuation must write the new pid back: %+v", j)
	}
}

// TestWatchdogNeedsInputExempt: a step holding an open pending_question is
// a PROTOCOL wait for a human answer — both probes leave it alone.
func TestWatchdogNeedsInputExempt(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the activity probe is Linux-only")
	}
	e, _ := wdSpawn(t, func(c *Config) { c.WatchdogActivityWindow = 200 * time.Millisecond })
	wdWriteExecState(t, e.d, e.wi, func(x *capsule.Execution) {
		x.Steps["code"].PendingQuestion = &capsule.PendingQuestion{
			QuestionID: "q_wd1", AskedAt: NowStamp(), Content: "what next?",
		}
	})
	a0 := e.spawner.agents[0]
	wdTicksFor(t, e.d, 2*time.Second)
	if n := len(e.spawner.spawns); n != 1 {
		t.Fatalf("a needs_input wait must never be judged hung, got %d spawns", n)
	}
	if !processAlive(a0.PID(), a0.StartedAt()) {
		t.Fatal("a needs_input wait must leave the agent alive")
	}
}

// TestWatchdogStepWallSingleSource: the backstop's step wall budget comes
// from ONE source — the per-step budget.wall_minutes in the capsule's
// MATERIALIZED workflow.md frontmatter; absent/undeclared ⇒ default.
func TestWatchdogStepWallSingleSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workflow.md"), []byte(
		"---\nname: wdprobe\nsteps:\n  - id: s1\n    kind: code\n    budget: {wall_minutes: 2}\n  - id: s2\n    kind: code\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{
		MachineID: "m-wdprobe", DataDir: filepath.Join(dir, "d"), WorkspaceRoot: dir,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jr := &jobRuntime{cap: &capsule.Capsule{Dir: dir}}
	if got := d.declaredStepWall(jr, "s1"); got != 2*time.Minute {
		t.Fatalf("declared wall budget must be honored, got %s", got)
	}
	if got := d.declaredStepWall(jr, "s2"); got != watchdogStepWallDefault {
		t.Fatalf("an undeclared step wall must fall back to the default, got %s", got)
	}
	if got := d.declaredStepWall(jr, "missing"); got != watchdogStepWallDefault {
		t.Fatalf("a missing step must fall back to the default, got %s", got)
	}
	if got := watchdogBackstopBound(10 * time.Minute); got != 30*time.Minute {
		t.Fatalf("backstop = max(3x step wall, floor): 3x of 10m must be 30m, got %s", got)
	}
	if got := watchdogBackstopBound(2 * time.Minute); got != watchdogBackstopFloor {
		t.Fatalf("backstop = max(3x step wall, floor): below the floor the FLOOR wins, got %s", got)
	}
}

// TestWatchdogRecoveryDeniesBoundedVerdict: a persisted bounded-termination
// verdict denies BOTH the adopt and the re-derive branches of the 04 §7.1
// restart recovery — a bounded-terminated step is never silently retried
// across daemon restarts.
func TestWatchdogRecoveryDeniesBoundedVerdict(t *testing.T) {
	e, _ := wdSpawn(t, nil)
	store := NewStateStore(e.d.cfg.DataDir)
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	j := st.FindJob(e.wi)
	j.Watchdog = &JobWatchdog{Bounded: true, BoundedStep: "code", BoundedAt: NowStamp()}
	if err := store.UpsertJob(st, *j); err != nil {
		t.Fatal(err)
	}
	spawner2 := newIntegSpawner()
	d2, err := New(e.d.cfg, spawner2)
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(spawner2.spawns); n != 0 {
		t.Fatalf("a bounded verdict must deny re-derivation at recovery, got %d spawns", n)
	}
	d2.mu.Lock()
	jr, ok := d2.jobs[e.wi]
	agent := jr.agent
	d2.mu.Unlock()
	if !ok || agent != nil {
		t.Fatalf("the denied job must sit in the runtime WITHOUT an agent: ok=%v agent=%v", ok, agent != nil)
	}
}

// TestWatchdogRecoveryDeniesStormOnCurrentStep: two recorded consecutive
// incidents on the CURRENT step (the crash window between the second
// increment and the verdict) deny the restart re-derivation too; a count
// belonging to a DIFFERENT step is progress, not a storm.
func TestWatchdogRecoveryDeniesStormOnCurrentStep(t *testing.T) {
	e, _ := wdSpawn(t, nil)
	wdWriteExecState(t, e.d, e.wi, nil) // current_step = "code"
	store := NewStateStore(e.d.cfg.DataDir)
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	j := st.FindJob(e.wi)
	j.Watchdog = &JobWatchdog{StepID: "code", Hangs: 2}
	if err := store.UpsertJob(st, *j); err != nil {
		t.Fatal(err)
	}
	spawner2 := newIntegSpawner()
	d2, err := New(e.d.cfg, spawner2)
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(spawner2.spawns); n != 0 {
		t.Fatalf("a storm on the current step must deny re-derivation, got %d spawns", n)
	}

	// Control: the same count on a DIFFERENT (older) step is progress —
	// recovery takes the ordinary adopt path, no re-derivation either (the
	// recorded agent is alive).
	j.Watchdog = &JobWatchdog{StepID: "older_step", Hangs: 2}
	if err := store.UpsertJob(st, *j); err != nil {
		t.Fatal(err)
	}
	spawner3 := newIntegSpawner()
	d3, err := New(e.d.cfg, spawner3)
	if err != nil {
		t.Fatal(err)
	}
	if err := d3.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(spawner3.spawns); n != 0 {
		t.Fatalf("a stale storm count is progress, not a storm (alive agent is adopted, not re-derived): %d spawns", n)
	}
	d3.mu.Lock()
	jr, ok := d3.jobs[e.wi]
	agent := jr.agent
	d3.mu.Unlock()
	if !ok || agent == nil {
		t.Fatal("the ordinary alive-agent recovery must adopt the agent")
	}
}

// TestWatchdogStateValidation: the persisted verdict's closed shape.
func TestWatchdogStateValidation(t *testing.T) {
	cases := []struct {
		name string
		wd   *JobWatchdog
	}{
		{"bounded without step", &JobWatchdog{Bounded: true, BoundedAt: NowStamp()}},
		{"bounded without stamp", &JobWatchdog{Bounded: true, BoundedStep: "code"}},
		{"stamp without verdict", &JobWatchdog{BoundedAt: NowStamp()}},
		{"step without verdict", &JobWatchdog{BoundedStep: "code"}},
		{"negative hangs", &JobWatchdog{StepID: "code", Hangs: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := validHeaderState()
			j := validJob("wi_wdval")
			j.Watchdog = tc.wd
			st.Jobs = []Job{j}
			dir := t.TempDir()
			writeStateFile(t, dir, st)
			if _, err := NewStateStore(dir).Load(); err == nil {
				t.Fatal("an out-of-shape watchdog record must be rejected on load")
			}
		})
	}
	// The well-formed verdict loads.
	st := validHeaderState()
	j := validJob("wi_wdval2")
	j.Watchdog = &JobWatchdog{StepID: "code", Hangs: 1, Bounded: true, BoundedStep: "code", BoundedAt: NowStamp()}
	st.Jobs = []Job{j}
	dir := t.TempDir()
	writeStateFile(t, dir, st)
	loaded, err := NewStateStore(dir).Load()
	if err != nil {
		t.Fatalf("a well-formed watchdog record must load: %v", err)
	}
	if loaded.FindJob("wi_wdval2").Watchdog.Hangs != 1 {
		t.Fatal("the watchdog record must round-trip")
	}
}
