package daemon

// watchdog.go: the dual runtime watchdog (04 §6.2 runtime note, polyforge#44
// refined option A). 04 §7.1 defines liveness judgment only for the daemon
// RESTART moment; the pilot (2026-09-28) measured two runtime gaps the
// restart-time check can never close:
//
//   - runtime death is UNOBSERVED: a WI Agent killed mid-step kept its
//     jobs[] row "running" for 45+ seconds (and forever, had the daemon not
//     been restarted) because the agent-exit face (agentexit.go) has no
//     runtime consumer — the crash continuation waits for a daemon restart
//     that nobody plans to do;
//   - turn-level hangs are UNBOUNDED: an upstream model call whose
//     connection died without RST left the agent in epoll with zero I/O for
//     15+ minutes; the step wall budget is checked BETWEEN turns (the
//     executor's own discipline), so an in-flight hang escapes every bound.
//
// The ruling separates the two questions (budget wall-clock is a COST
// intent, never a liveness probe):
//
//   - Watchdog 1 (death): every dispatcher tick (200ms) polls each running
//     job's agent exit face NON-BLOCKING; an observed exit triggers the
//     04 §7.1 step-4 crash-continuation sequence immediately — collect the
//     residual process group, re-derive with the SAME session id, write the
//     new pid back into jobs[] — no waiting for shutdown or restart.
//   - Watchdog 2 (hang): ACTIVITY-based judgment. The main agent process's
//     /proc counters (I/O + CPU jiffies) flat over a configurable window
//     (default 10 minutes) ⇒ judged hung ⇒ kill ⇒ the same crash
//     continuation. A slow-but-streaming agent shows continuous activity
//     and is never touched.
//   - Wall clock is ONLY an absolute backstop: max(3× the current step's
//     declared wall budget, 15min) remains as a second bound in case the
//     activity probe itself fails or is unavailable.
//   - Storm guard: the SAME step hung/dying twice consecutively ⇒ bounded
//     termination (end-generation reason=block, waiting_on=runtime:<machine>
//     — the infra-failure/blocked shape, human attention), never a third
//     relaunch. The counter and the verdict are persisted in the jobs[] row
//     so a daemon restart cannot silently retry a bounded-terminated step.
//
// KNOWN LIMIT (recorded honestly, same discipline as freezeConditions): the
// activity probe watches the MAIN agent pid only. A step command CHILD
// running longer than the window while the main agent quietly drains its
// pipes is indistinguishable from a hang by these counters alone; the
// ruling accepts that trade (the manual diagnosis that produced it used
// exactly this signal), and the storm guard bounds the damage of one false
// verdict to a single relaunch before the bounded termination surfaces it
// for human triage. The char counters (rchar/wchar) are included precisely
// because they move on pipe and socket traffic — the block-backed
// read_bytes/write_bytes alone would stay flat even for a main agent
// actively draining a child's output.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/workflow/schema"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Config surface

const (
	// DefaultWatchdogActivityWindow is the flat-activity window of the hang
	// judgment (the ruling's daemon-configurable knob; production default
	// 10 minutes, tests tighten it through Config.WatchdogActivityWindow).
	DefaultWatchdogActivityWindow = 10 * time.Minute
	// watchdogTerminateBound bounds one agent-group collection (kill of a
	// judged-hung agent or residual collection of a dead one) before the
	// crash continuation proceeds.
	watchdogTerminateBound = 10 * time.Second
)

// Test seams: production values are FIXED by the ruling; these are
// variables only so tests can shrink the bounds without waiting
// wall-clock minutes (the same convention as procRoot / killProbe).
var (
	// watchdogStepWallDefault is the assumed per-step wall budget when the
	// current step declares none in the capsule's workflow.md frontmatter
	// (the pilot's measured shape: steps without an explicit wall budget
	// still get disciplined by the executor's 5-minute default).
	watchdogStepWallDefault = 5 * time.Minute
	// watchdogBackstopFloor is the fixed minimum of the absolute backstop
	// formula max(3× step wall, floor).
	watchdogBackstopFloor = 15 * time.Minute
	// watchProcRoot is the procfs mount the activity probe reads; separate
	// from proc.go's procRoot so a test pointing THIS at a nonexistent path
	// exercises the probe-unavailable degradation without breaking the
	// pid-reuse guards.
	watchProcRoot = "/proc"
)

// watchdogBackstopBound is the fixed absolute backstop over one agent
// instance: max(3× the step's wall budget, the 15-minute floor).
func watchdogBackstopBound(stepWall time.Duration) time.Duration {
	b := 3 * stepWall
	if b < watchdogBackstopFloor {
		b = watchdogBackstopFloor
	}
	return b
}

// activityWindow resolves the configured hang window.
func (d *Daemon) activityWindow() time.Duration {
	if d.cfg.WatchdogActivityWindow > 0 {
		return d.cfg.WatchdogActivityWindow
	}
	return DefaultWatchdogActivityWindow
}

// ---------------------------------------------------------------------------
// Activity probe (procfs, portable fallback)

// activitySample is one point-in-time activity reading of the MAIN agent
// process. ANY field moving between two samples is activity; ALL flat over
// the window is the hang verdict's material.
type activitySample struct {
	rchar      uint64 // /proc/<pid>/io: bytes through read(2) — pipes/sockets included
	wchar      uint64 // /proc/<pid>/io: bytes through write(2)
	readBytes  uint64 // /proc/<pid>/io: block-backed reads
	writeBytes uint64 // /proc/<pid>/io: block-backed writes
	cpuTicks   uint64 // /proc/<pid>/stat: utime+stime in ticks
}

// flat reports whether NOTHING moved between two samples.
func (s activitySample) flat(prev activitySample) bool {
	return s == prev
}

// procfsActivitySample reads one activity sample for pid. Portability
// contract: when procfs is unavailable (non-Linux, unreadable mount, a
// pid we may not read) the probe answers (zero, false) and the watchdog
// DEGRADES to wall-clock-only for that job — it never crashes on a missing
// procfs and never guesses activity it could not observe.
func procfsActivitySample(pid int) (activitySample, bool) {
	if pid <= 0 {
		return activitySample{}, false
	}
	dir := filepath.Join(watchProcRoot, strconv.Itoa(pid))
	raw, err := os.ReadFile(filepath.Join(dir, "io"))
	if err != nil {
		return activitySample{}, false
	}
	var s activitySample
	for _, line := range strings.Split(string(raw), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return activitySample{}, false
		}
		switch strings.TrimSpace(name) {
		case "rchar":
			s.rchar = v
		case "wchar":
			s.wchar = v
		case "read_bytes":
			s.readBytes = v
		case "write_bytes":
			s.writeBytes = v
		}
	}
	statRaw, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return activitySample{}, false
	}
	// comm may contain spaces inside parentheses: split after the LAST ')'.
	// fields[0]=state(3) … fields[11]=utime(14), fields[12]=stime(15).
	idx := strings.LastIndex(string(statRaw), ")")
	if idx < 0 || idx+2 > len(statRaw) {
		return activitySample{}, false
	}
	fields := strings.Fields(string(statRaw)[idx+2:])
	if len(fields) < 13 {
		return activitySample{}, false
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return activitySample{}, false
	}
	s.cpuTicks = utime + stime
	return s, true
}

// sampleActivity reads one sample through the daemon's probe face (the
// default is procfs; tests replace the field to script availability).
func (d *Daemon) sampleActivity(pid int) (activitySample, bool) {
	if d.wdSample == nil {
		return activitySample{}, false
	}
	return d.wdSample(pid)
}

// watchdogStartupCheck probes procfs ONCE on the daemon's own pid and says
// so loudly when unavailable: the hang watchdog then runs wall-clock-only
// (the backstop stays), and the operator hears it at startup rather than
// discovering it when a hung agent sails past the window untouched.
func (d *Daemon) watchdogStartupCheck() {
	if _, ok := d.sampleActivity(os.Getpid()); !ok {
		d.wdProbeAvailable = false
		fmt.Fprintf(os.Stderr, "pf-daemon: /proc activity counters unavailable; the hang watchdog degrades to wall-clock-only (backstop max(3x step wall, %s)) for every job\n",
			watchdogBackstopFloor)
		return
	}
	d.wdProbeAvailable = true
}

// ---------------------------------------------------------------------------
// Death probe (Watchdog 1)

// AgentExitProbe is the OPTIONAL non-blocking extension of the AgentHandle
// face the death watchdog polls per tick: Done reports, without ever
// blocking, whether the agent process has exited. Additive by design —
// agentexit.go's blocking AgentExitWaiter.Wait and every existing consumer
// keep their semantics untouched; a handle without Done falls back to the
// pid-reuse-guarded procfs liveness check (processAlive), which the
// adopted-agent face already uses.
type AgentExitProbe interface {
	Done() bool
}

// agentDone is the per-tick death judgment: the reaper's own signal when the
// handle exposes it (mainagent.Proc), otherwise the recorded pid's
// pid-reuse-guarded liveness (a recycled pid is NOT our agent's death).
func agentDone(h AgentHandle) bool {
	if h == nil {
		return true
	}
	if dp, ok := h.(AgentExitProbe); ok {
		return dp.Done()
	}
	return !processAlive(h.PID(), h.StartedAt())
}

// ---------------------------------------------------------------------------
// Per-job runtime bookkeeping (in-memory only; the persisted bits live in
// the jobs[] row — see JobWatchdog in state.go)

// watchState is one job's watchdog bookkeeping. Touched only from the
// dispatcher goroutine (Run's tick / the watchdog tests), the same
// single-driver discipline the rest of jobRuntime follows.
type watchState struct {
	instStart time.Time // current agent instance start: the backstop base
	last      activitySample
	hasSample bool
	flatSince time.Time // when the current all-flat window opened
	// step profile cache: re-read only when the current step changes.
	stepID      string
	stepWall    time.Duration
	profileRead bool
	// probeFailLogged suppresses the per-job "sample unreadable" line to
	// one occurrence instead of one per 200ms tick.
	probeFailLogged bool
}

func newWatchState(h AgentHandle) *watchState {
	wd := &watchState{instStart: time.Now()}
	if h != nil {
		if t, err := time.Parse(time.RFC3339Nano, h.StartedAt()); err == nil {
			wd.instStart = t
		}
	}
	return wd
}

// stepProfile reads the job's CURRENT execution profile read-only from the
// capsule (the same read the observe face and the answer relay already
// make; execution.json stays the WI Agent's single-writer domain, INV-1 —
// the daemon never writes it). It yields:
//
//   - stepID: the current step ("" before the first step starts), the
//     storm guard's identity key;
//   - needsInput: the current step holds an open pending_question — a
//     PROTOCOL wait for a human answer. Both probes are exempt while it
//     holds: an agent legitimately parked on needs_input can outlive every
//     window, and killing it would only re-enter the same wait after a
//     pointless relaunch.
//   - stepWall: the current step's declared wall budget — the ONE source
//     the backstop formula consumes: the per-step budget.wall_minutes in
//     the capsule's MATERIALIZED workflow.md frontmatter (03 §4.5), the
//     same declaration the executor's between-turns budget discipline
//     reads, materialized by this daemon itself from its own checkout —
//     no data-plane read, no second authority. A step without an explicit
//     wall budget assumes the default (watchdogStepWallDefault).
func (d *Daemon) stepProfile(jr *jobRuntime) (stepID string, needsInput bool, stepWall time.Duration) {
	exec, err := jr.cap.LoadExecution()
	if err != nil || exec == nil {
		// No execution state yet (first step not started): no step id, no
		// protocol wait, the default wall for the backstop.
		return "", false, watchdogStepWallDefault
	}
	stepID = exec.CurrentStep
	if q, _ := exec.PendingQuestion(); q != nil {
		needsInput = true
	}
	// The workflow.md read is cached per CURRENT STEP (the declaration
	// cannot change mid-step); the needs_input flag is NOT cached — a
	// question can be asked at any point inside the step.
	if jr.wd != nil && jr.wd.profileRead && jr.wd.stepID == stepID {
		return stepID, needsInput, jr.wd.stepWall
	}
	wall := d.declaredStepWall(jr, stepID)
	if jr.wd != nil {
		jr.wd.stepID, jr.wd.stepWall, jr.wd.profileRead = stepID, wall, true
	}
	return stepID, needsInput, wall
}

// declaredStepWall resolves one step's declared wall budget from the
// capsule's materialized workflow.md frontmatter (see stepProfile for the
// source ruling). Unparsable/absent frontmatter degrades to the default —
// the backstop is a safety net, and a missing declaration must never
// disable it.
func (d *Daemon) declaredStepWall(jr *jobRuntime, stepID string) time.Duration {
	raw, err := os.ReadFile(filepath.Join(jr.cap.Dir, "workflow.md"))
	if err != nil {
		return watchdogStepWallDefault
	}
	front, _, ok := schema.SplitFrontmatter(raw)
	if !ok {
		return watchdogStepWallDefault
	}
	var fm schema.Frontmatter
	if err := yaml.Unmarshal(front, &fm); err != nil {
		return watchdogStepWallDefault
	}
	for _, st := range fm.Steps {
		if st.ID != stepID {
			continue
		}
		if st.Budget != nil && st.Budget.WallMinutes != nil && *st.Budget.WallMinutes > 0 {
			return time.Duration(*st.Budget.WallMinutes) * time.Minute
		}
		return watchdogStepWallDefault
	}
	return watchdogStepWallDefault
}

// ---------------------------------------------------------------------------
// The tick

// watchdogTick is the per-dispatcher-tick pass over every running job. It
// is the RUNTIME landing of the §7.1 liveness duty: non-blocking, one pass
// per 200ms, no goroutine per job. Skipped wholesale while shutting down
// (the close paths own dying agents there) and per-job while a close is
// mid-flight (the one-close guard).
func (d *Daemon) watchdogTick(ctx context.Context) {
	if d.modes.ShuttingDown() {
		return
	}
	type wdItem struct {
		wi string
		jr *jobRuntime
	}
	d.mu.Lock()
	items := make([]wdItem, 0, len(d.jobs))
	for wi, jr := range d.jobs {
		if d.closing[wi] {
			continue // a pause/wrap/shutdown close owns this job right now
		}
		items = append(items, wdItem{wi: wi, jr: jr})
	}
	d.mu.Unlock()
	for _, it := range items {
		d.watchdogJob(ctx, it.wi, it.jr)
	}
}

// watchdogJob runs both probes plus the backstop for one job.
func (d *Daemon) watchdogJob(ctx context.Context, wi string, jr *jobRuntime) {
	d.mu.Lock()
	agent := jr.agent
	d.mu.Unlock()
	if agent == nil {
		return // no live agent under supervision (spawn/adopt denial): human attention
	}
	if jr.wd == nil {
		jr.wd = newWatchState(agent)
	}
	stepID, needsInput, stepWall := d.stepProfile(jr)
	d.stormResetOnProgress(wi, jr, stepID)

	// Watchdog 1 — death: the moment the exit face says the process is
	// gone, the crash continuation owns the job (collect + re-derive).
	if agentDone(agent) {
		d.watchdogIncident(ctx, wi, jr, stepID, "agent_exit")
		return
	}
	// needs_input: a protocol wait for a human answer — exempt from both
	// probes (see stepProfile). The process is alive (checked above); the
	// step cannot advance until the answer relay arrives.
	if needsInput {
		return
	}
	now := time.Now()
	// Absolute backstop: per agent instance, regardless of the probe. This
	// is the bound that holds when the activity probe is disabled,
	// unavailable, or itself lied to.
	if bound := watchdogBackstopBound(stepWall); now.Sub(jr.wd.instStart) > bound {
		d.killAgentGroup(jr)
		d.watchdogIncident(ctx, wi, jr, stepID, "wall_backstop")
		return
	}
	// Watchdog 2 — hang: activity judgment, only when a sample is
	// observable. An unavailable probe degrades this job to backstop-only.
	sample, ok := d.sampleActivity(agent.PID())
	if !ok {
		if d.wdProbeAvailable && !jr.wd.probeFailLogged {
			jr.wd.probeFailLogged = true
			fmt.Fprintf(os.Stderr, "pf-daemon: %s agent pid %d activity counters unreadable; hang watchdog degrades to wall-clock-only for this job\n", wi, agent.PID())
		}
		return
	}
	if !jr.wd.hasSample || sample.flat(jr.wd.last) {
		if !jr.wd.hasSample {
			jr.wd.hasSample = true
			jr.wd.last = sample
			jr.wd.flatSince = now
			return
		}
		window := d.activityWindow()
		if now.Sub(jr.wd.flatSince) >= window {
			d.killAgentGroup(jr)
			d.watchdogIncident(ctx, wi, jr, stepID, "hang")
		}
		return
	}
	// Activity observed: the window restarts, the agent is left alone.
	jr.wd.last = sample
	jr.wd.flatSince = now
}

// killAgentGroup terminates a judged-hung agent's WHOLE process group with
// the shared bound (04 §6.2 B / §7.1 residue discipline: the group, not
// just the leader).
func (d *Daemon) killAgentGroup(jr *jobRuntime) {
	d.mu.Lock()
	agent := jr.agent
	d.mu.Unlock()
	if agent == nil {
		return
	}
	if err := agent.Terminate(watchdogTerminateBound); err != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: watchdog kill of the agent group failed (continuing to the crash judgment): %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// Storm guard (persisted counter + typed verdict)

// stormResetOnProgress clears the per-step incident counter whenever the
// current step CHANGED since the last recorded incident: a relaunch that
// completed its step and moved on has proven the recovery worked, and
// "twice CONSECUTIVELY" starts over. The reset is persisted (the counter
// survives a daemon crash exactly like the increments do).
func (d *Daemon) stormResetOnProgress(wi string, jr *jobRuntime, stepID string) {
	j := d.st.FindJob(wi)
	if j == nil || j.Watchdog == nil {
		return
	}
	if j.Watchdog.Bounded || j.Watchdog.StepID == stepID {
		return
	}
	j.Watchdog.StepID = stepID
	j.Watchdog.Hangs = 0
	if err := d.state.UpsertJob(d.st, *j); err != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: watchdog storm-counter reset persist failed for %s: %v\n", wi, err)
	}
}

// watchdogIncident is the ONE convergence point of both probes: one
// observed incident (hang verdict, backstop expiry or agent death) on the
// current step. Order of record is deliberate and crash-honest:
//
//  1. increment + persist the per-step counter BEFORE any relaunch, so a
//     daemon crash between the kill and the re-derivation leaves the
//     honest count for the restart's recovery to consume;
//  2. two consecutive incidents on the SAME step ⇒ bounded termination:
//     the typed verdict is persisted first, then the job is closed through
//     the shared 04 §6.2 A-path close with reason=block and
//     waiting_on=runtime:<machine> (02 §2.4: "等机器恢复" — the
//     infra-failure/blocked shape; human attention, no third relaunch);
//  3. otherwise: the 04 §7.1 step-4 crash continuation, in place (collect
//     the residual group, re-derive with the same session id, write the
//     new pid back).
func (d *Daemon) watchdogIncident(ctx context.Context, wi string, jr *jobRuntime, stepID, cause string) {
	j := d.st.FindJob(wi)
	if j == nil {
		return
	}
	wd := j.Watchdog
	if wd == nil {
		wd = &JobWatchdog{}
	}
	if wd.Bounded {
		// The verdict is already on record (a bounded close that failed or
		// a crash mid-close): never relaunch. Retry the close once here —
		// the generation may still be open server-side.
		d.boundedTerminate(ctx, wi, jr)
		return
	}
	if wd.StepID != stepID {
		wd.StepID = stepID
		wd.Hangs = 0
	}
	wd.Hangs++
	j.Watchdog = wd
	if err := d.state.UpsertJob(d.st, *j); err != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: watchdog incident persist failed for %s (%s): %v\n", wi, cause, err)
	}
	if wd.Hangs >= 2 {
		wd.Bounded = true
		if stepID == "" {
			wd.BoundedStep = "(no-step)"
		} else {
			wd.BoundedStep = stepID
		}
		wd.BoundedAt = NowStamp()
		if err := d.state.UpsertJob(d.st, *j); err != nil {
			fmt.Fprintf(os.Stderr, "pf-daemon: watchdog bounded-verdict persist failed for %s: %v\n", wi, err)
		}
		fmt.Fprintf(os.Stderr, "pf-daemon: %s step %q hung/died twice consecutively (%s); bounded termination — end-generation reason=block, no further relaunch (human attention)\n",
			wi, wd.BoundedStep, cause)
		d.boundedTerminate(ctx, wi, jr)
		return
	}
	d.crashContinueJob(ctx, wi, jr, cause)
}

// boundedTerminate closes a storm-guard-terminated job with the
// infra-failure/blocked shape through the ONE shared 04 §6.2 A-path close
// (final flush + end-generation reason=block carrying
// waiting_on=runtime:<machine_id>, 02 §2.4's closed value domain — the WI
// becomes blocked server-side and the ready queue stops offering it). On a
// failed close the job leaves the runtime but KEEPS its jobs[] row with the
// verdict: the next daemon restart's recovery denies re-derivation
// (watchdogRecoveryDeny) instead of silently retrying the step forever.
func (d *Daemon) boundedTerminate(ctx context.Context, wi string, jr *jobRuntime) {
	jr2, err := d.beginClose(wi)
	if err != nil {
		// Another close already holds the one-close guard (a concurrent
		// pause/wrap/shutdown): it ends the generation its own way and the
		// persisted verdict stays on the row. Interfering here (dropping the
		// job from the runtime underneath the running close) could make the
		// close's seal remove the very row that carries the verdict.
		fmt.Fprintf(os.Stderr, "pf-daemon: bounded termination of %s defers to a close already in flight (%v); the verdict stays persisted for recovery\n", wi, err)
		return
	}
	_, cerr := d.closeJob(ctx, wi, jr2, "block", "watchdog_bounded_termination", "runtime:"+d.cfg.MachineID)
	d.endClose(wi)
	if cerr != nil {
		// A rolled-back credential / taken-over generation has already
		// been isolated by the shared classifier inside the close; every
		// other failure leaves the row in place for the restart gate.
		if !errors.Is(cerr, ErrSecretRolledBack) && !errors.Is(cerr, ErrStaleGeneration) {
			fmt.Fprintf(os.Stderr, "pf-daemon: bounded termination close of %s failed: %v (jobs[] row kept with the verdict; recovery will deny re-derivation — human attention)\n", wi, cerr)
			d.abandonWatchdogJob(wi, jr)
		}
		return
	}
}

// abandonWatchdogJob drops a bounded-terminated job from the live runtime
// WITHOUT sealing or removing its jobs[] row: the watcher stops (nothing
// left to observe), the in-memory bookkeeping goes, the persisted verdict
// stays for RecoverFromState's deny gate.
func (d *Daemon) abandonWatchdogJob(wi string, jr *jobRuntime) {
	if jr.watcher != nil {
		jr.watcher.Stop()
	}
	d.mu.Lock()
	delete(d.jobs, wi)
	d.mu.Unlock()
	d.jobq.Complete(wi)
}

// ---------------------------------------------------------------------------
// Runtime crash continuation (04 §7.1 step 4, in place)

// crashContinueJob is the runtime landing of the 04 §7.1 step-4 sequence:
// the agent is dead (or was just killed as hung); collect its residual
// process group, then re-derive a new persistent WI Agent with the SAME
// session id (the only session-reuse case, 03 §4.4), the same generation,
// the crash-recovery prompt, and write the new process identity back into
// the jobs[] row. Identical judgments to the restart-time path — same
// materialization trust gate (same-generation snapshot reuse), same
// proven-residue precondition — executed by the LIVE daemon instead of by
// the next boot.
func (d *Daemon) crashContinueJob(ctx context.Context, wi string, jr *jobRuntime, cause string) {
	if d.spawner == nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s agent lost (%s) but no spawner is wired; job left without an agent (human attention)\n", wi, cause)
		d.mu.Lock()
		jr.agent = nil
		d.mu.Unlock()
		return
	}
	j := d.st.FindJob(wi)
	if j == nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s agent lost (%s) but no jobs[] row survives; job left without an agent (human attention)\n", wi, cause)
		d.mu.Lock()
		jr.agent = nil
		d.mu.Unlock()
		return
	}
	// Residual collection first (04 §7.1 step 4): the dead agent's orphaned
	// step agents / command children must stop writing before the new
	// instance starts (single-writer domain, 03 §7). The handle's own
	// group-terminating face is the collection; an unprovable collection
	// must NOT let a second writer start, so a typed failure leaves the
	// job without an agent for human attention — exactly like the
	// restart-time path's collectResidualProcessGroup denial.
	if cerr := d.collectResidual(jr); cerr != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s residual group collection did not prove death; re-derivation DENIED (human attention): %v\n", wi, cerr)
		d.mu.Lock()
		jr.agent = nil
		d.mu.Unlock()
		return
	}
	// Same-generation snapshot trust gate (03 §4.5/§7): identical to the
	// restart-time re-derivation.
	matID := capsule.MaterializationIdentity{
		BundleName: j.BundleName, BundleDigest: j.BundleDigest,
		Adapters: []string{j.HarnessAdapter},
	}
	if merr := d.ensureMaterialized(jr.cap, matID); merr != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s materialization failed before re-derivation; job left without an agent (human attention required): %v\n", wi, merr)
		d.mu.Lock()
		jr.agent = nil
		d.mu.Unlock()
		return
	}
	prompt, perr := d.crashRecoveryPrompt(*j)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s crash-recovery prompt unreadable; job left without an agent (human attention): %v\n", wi, perr)
		d.mu.Lock()
		jr.agent = nil
		d.mu.Unlock()
		return
	}
	handle, serr := d.spawner.Spawn(ctx, SpawnParams{
		WorkItemID: wi, CapsuleDir: jr.cap.Dir, Generation: j.Generation,
		SessionID: j.HarnessSessionID, Adapter: j.HarnessAdapter, InitialPrompt: prompt,
	})
	if serr != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s crash-continuation re-derivation failed; job left without an agent (human attention): %v\n", wi, serr)
		d.mu.Lock()
		jr.agent = nil
		d.mu.Unlock()
		return
	}
	d.mu.Lock()
	jr.agent = handle
	d.mu.Unlock()
	jr.wd = newWatchState(handle) // fresh instance: fresh activity window + backstop base
	jr.ring.Append(EvWIAgentRederived, map[string]any{
		"cause": cause, "session_id": j.HarnessSessionID, "runtime_watchdog": true,
	})
	// Write the new process identity back into the jobs[] row (04 §7.1
	// step 4's final write), preserving the persisted watchdog state.
	j.MainAgentPID = handle.PID()
	j.MainAgentStartedAt = handle.StartedAt()
	j.Status = JobRunning
	if err := d.state.UpsertJob(d.st, *j); err != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s re-derivation pid write-back persist failed: %v\n", wi, err)
	}
}

// collectResidual collects the dead/hung agent's residual process group
// through the handle's own terminating face (bounded SIGTERM→SIGKILL with
// a proven-empty group, the same discipline collectResidualProcessGroup
// applies to a bare recorded pid). A nil error is the PROVEN outcome.
func (d *Daemon) collectResidual(jr *jobRuntime) error {
	d.mu.Lock()
	agent := jr.agent
	d.mu.Unlock()
	if agent == nil {
		return nil
	}
	if err := agent.Terminate(watchdogTerminateBound); err != nil {
		return fmt.Errorf("agent group collection: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Restart-recovery gate (consumed by RecoverFromState)

// watchdogRecoveryDeny decides whether the 04 §7.1 restart recovery may
// re-derive/re-attach a recorded job at all. The persisted watchdog state
// is exactly the bit recovery needs (the ruling: a bounded-terminated step
// must not be silently retried forever across daemon restarts):
//
//   - a BOUNDED verdict on record: the bounded close never completed (the
//     daemon died mid-close) — deny BOTH branches (adopting the still-alive
//     agent would silently resume what the verdict terminated), human
//     attention;
//   - two recorded consecutive incidents on the CURRENT step (the crash
//     window between the second increment and the verdict): deny the
//     re-derivation, human attention. A recorded incident count for a
//     DIFFERENT step is progress, not a storm — the ordinary path runs.
func watchdogRecoveryDeny(j *Job, capc *capsule.Capsule) (bool, string) {
	if j == nil || j.Watchdog == nil {
		return false, ""
	}
	if j.Watchdog.Bounded {
		return true, fmt.Sprintf("bounded termination verdict for step %q (at %s) never completed its close", j.Watchdog.BoundedStep, j.Watchdog.BoundedAt)
	}
	if j.Watchdog.Hangs >= 2 {
		var cur string
		if exec, err := capc.LoadExecution(); err == nil && exec != nil {
			cur = exec.CurrentStep
		}
		if j.Watchdog.StepID == cur {
			return true, fmt.Sprintf("step %q recorded %d consecutive watchdog incidents", j.Watchdog.StepID, j.Watchdog.Hangs)
		}
	}
	return false, ""
}
