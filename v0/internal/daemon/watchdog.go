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
//   - Watchdog 2 (hang): STREAM-based judgment since polyforge#48 (see
//     the stream-heartbeat block below). The supervised agent's protocol
//     record stream silent while a TURN IS IN FLIGHT over a configurable
//     window (default 10 minutes) ⇒ confirmation probe ⇒ no ack ⇒ kill ⇒
//     the same crash continuation. A slow-but-streaming agent keeps
//     emitting records and is never touched. The /proc activity counters
//     are DEMOTED to auxiliary evidence (and remain the judgment only for
//     a handle with no observable stream, e.g. an adopted agent).
//   - Wall clock is ONLY an absolute backstop: max(3× the current step's
//     declared wall budget, 15min) remains as a second bound in case the
//     activity probe itself fails or is unavailable.
//   - Storm guard: the SAME step hung/dying twice consecutively ⇒ bounded
//     termination (end-generation reason=block, waiting_on=runtime:<machine>
//     — the infra-failure/blocked shape, human attention), never a third
//     relaunch. The counter and the verdict are persisted in the jobs[] row
//     so a daemon restart cannot silently retry a bounded-terminated step.
//
// ---------------------------------------------------------------------------
// polyforge#48 — THE STREAM-HEARTBEAT REVISION of Watchdog 2.
//
// WHAT BROKE. The activity criterion above was measured failing in
// production (pilot 2026-09-29 00:45, wi polyforge#7). Every human
// criterion said hung — no TCP connection in flight, no session write
// for 12+ minutes, zero io increment — and the watchdog never fired,
// because /proc/<pid>/stat kept ticking +1 jiffy every ~10 seconds: the
// node runtime's epoll timer accounting. "ANY counter moved ⇒ activity"
// is over-sensitive to IDLE NOISE. The regression that was supposed to
// cover this (test (d)) simulated a steady TRICKLE of real work, never
// the floor noise of an idle runtime, so the false negative shipped.
//
// THE RULED CRITERION (primary). The supervisor's stdout reader already
// classifies every protocol record — it must, it is the ack carrier —
// so the stream costs nothing extra to observe and it measures the
// right thing: AGENT PROGRESS, not process scheduling.
//
//	inFlight && now-lastRecordAt > window ⇒ hung
//
// inFlight is flipped by the harness's own run/turn lifecycle records
// (pinned against a real capture, see mainagent/streamheartbeat.go).
// Both halves matter: silence with NO turn in flight is an idle agent,
// never a hang.
//
// WINDOW CALIBRATION (human-approved, default 10 minutes). Legal silence
// on this stream is THINKING ONLY — a model reasoning before its first
// text delta. Everything else streams: text deltas arrive continuously,
// and a step command or subagent emits records throughout its run. The
// pilot measured legal thinking gaps at ≤4 minutes and real hangs at
// 7-15+ minutes that never returned. 10 minutes sits above the measured
// legal maximum with >2x headroom and below the observed hang floor.
// Configurable by the daemon operator (Config.WatchdogActivityWindow /
// POLYFORGE_WATCHDOG_WINDOW) AND per-step, through the same workflow.md
// frontmatter mechanism the step wall budget already uses
// (budget.hang_window_minutes) — a step that legitimately thinks longer
// declares it rather than forcing a global loosening.
//
// CONFIRMATION PROBE (before any kill). Window expiry is a SUSPICION,
// not a verdict. The daemon sends one control-shaped pf/probe and waits
// turn-scale for the agent's ack; only silence there confirms the hang.
// The ack must be the AGENT's, not the harness's: a harness whose
// upstream call is wedged still answers its RPC layer instantly (the
// measured `response` record precedes any model work), so a
// transport-level echo would rebuild the very false negative this fixes.
//
// THE PROCFS PROBE DEMOTES to auxiliary evidence: sampled and attached
// to the incident for the operator, never overriding the stream. It
// stays the CRITERION only where no stream exists (an adopted agent
// re-attached to a bare pid after a daemon restart), and there it now
// carries the threshold band #48 asked for — see activityDelta.
//
// KNOWN LIMIT (recorded honestly, same discipline as freezeConditions):
// the procfs fallback watches the MAIN agent pid only. A step command
// CHILD running longer than the window while the main agent quietly
// drains its pipes is indistinguishable from a hang by those counters
// alone; the ruling accepts that trade, and the storm guard bounds the
// damage of one false verdict to a single relaunch before the bounded
// termination surfaces it for human triage. The char counters
// (rchar/wchar) are included precisely because they move on pipe and
// socket traffic. The STREAM criterion does not share this limit: a
// step agent's output flows through the same record stream.

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
	// watchdogProbeBoundVar is the turn-scale bound of ONE confirmation
	// probe (polyforge#48). It is spent only AFTER the hang window has
	// already expired, so it is never on a healthy path; it is the last
	// benefit of the doubt a legitimately-slow turn gets. A handle whose
	// probe face is missing skips straight to the kill, exactly as
	// before this existed.
	watchdogProbeBoundVar = 2 * time.Minute
)

// watchdogCPUNoiseRate is the threshold band of the DEMOTED procfs
// criterion (polyforge#48 item 1 / the original #44 activity probe).
// Measured floor noise of an idle node runtime in epoll: ~1 jiffy per
// 10 seconds, i.e. ~6 jiffies/minute, produced by timer accounting with
// zero io increment. A real streaming turn burns orders of magnitude
// more (JSON parsing of a token stream is not free). 30 jiffies/minute
// — 0.5% of one core at the usual USER_HZ=100 — sits ~5x above the
// measured noise and far below any real work, so
//
//	io chars flat  &&  cpu delta < rate×window  ⇒  FLAT
//
// A RATE (not a fixed per-window count) is required: the production
// noise accumulated 60 jiffies over the 10-minute window, which any
// fixed small constant would have read as work — the exact false
// negative being fixed.
const watchdogCPUNoiseRate = 30.0 // jiffies per minute

// noiseFloorTicks is the jiffy budget the band allows over window.
// Floored at 2 so a very short (test-shrunk) window still tolerates a
// scheduling tick without calling a busy process hung.
func noiseFloorTicks(window time.Duration) uint64 {
	n := uint64(watchdogCPUNoiseRate * window.Minutes())
	if n < 2 {
		return 2
	}
	return n
}

// watchdogBackstopBound is the fixed absolute backstop over one agent
// instance: max(3× the step's wall budget, the 15-minute floor).
func watchdogBackstopBound(stepWall time.Duration) time.Duration {
	b := 3 * stepWall
	if b < watchdogBackstopFloor {
		b = watchdogBackstopFloor
	}
	return b
}

// activityWindow resolves the DAEMON-WIDE hang window: the operator's
// Config value (the daemon flag / POLYFORGE_WATCHDOG_WINDOW, resolved in
// New) or the design default.
func (d *Daemon) activityWindow() time.Duration {
	if d.cfg.WatchdogActivityWindow > 0 {
		return d.cfg.WatchdogActivityWindow
	}
	return DefaultWatchdogActivityWindow
}

// hangWindow resolves the window ACTUALLY applied to one job: the
// per-step frontmatter override when the current step declares one,
// else the daemon-wide window (polyforge#48: "configurable by daemon
// flag AND per-step frontmatter override, same mechanism as the step
// budget"). A step that legitimately thinks longer than the global
// window declares budget.hang_window_minutes instead of forcing every
// other step to be judged loosely.
//
// The override is scaled, never absolute, when the daemon window has
// been tightened below the default: a test (or an operator) running a
// 250ms window must not have a step's "3 minutes" silently reinstate a
// production-scale window. The ratio the step declares against the
// DESIGN default is what carries over.
func (d *Daemon) hangWindow(stepHang time.Duration) time.Duration {
	base := d.activityWindow()
	if stepHang <= 0 {
		return base
	}
	if base == DefaultWatchdogActivityWindow {
		return stepHang
	}
	scaled := time.Duration(float64(base) * (float64(stepHang) / float64(DefaultWatchdogActivityWindow)))
	if scaled <= 0 {
		return base
	}
	return scaled
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

// flat reports whether NOTHING moved between two samples. Kept for the
// exact-equality diagnostics; the JUDGMENT uses flatWithin below.
func (s activitySample) flat(prev activitySample) bool {
	return s == prev
}

// flatWithin is the polyforge#48 threshold band: s is flat against the
// window BASE sample when no io byte moved and the CPU delta stayed
// inside the idle-noise budget for window. The io side keeps its strict
// zero-increment rule deliberately — the pilot measured io at exactly
// zero through the whole 12-minute hang, and unlike the CPU counters
// the char counters have no equivalent idle floor (an idle runtime that
// is not reading or writing anything moves neither).
//
// A counter that went BACKWARDS (impossible within one process life, so:
// a pid recycled under us) is treated as movement — fail safe, do not
// kill on a reading we cannot trust.
func (s activitySample) flatWithin(base activitySample, window time.Duration) bool {
	if s.rchar != base.rchar || s.wchar != base.wchar ||
		s.readBytes != base.readBytes || s.writeBytes != base.writeBytes {
		return false
	}
	if s.cpuTicks < base.cpuTicks {
		return false
	}
	return s.cpuTicks-base.cpuTicks < noiseFloorTicks(window)
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
	stepHang    time.Duration // per-step hang-window override (0 = none)
	profileRead bool
	// probeFailLogged suppresses the per-job "sample unreadable" line to
	// one occurrence instead of one per 200ms tick.
	probeFailLogged bool
	// --- polyforge#48 stream-heartbeat bookkeeping ---
	// streamSeen records that this instance's handle answered the stream
	// face at least once; a handle that never does keeps the DEGRADED
	// procfs criterion (and says so once).
	streamSeen   bool
	streamNoteed bool // the one-shot "no observable stream" log
	// confirm is the in-flight confirmation probe, non-nil from the
	// moment the window expired until the probe settles. The tick stays
	// NON-BLOCKING: the probe runs on its own goroutine and the tick
	// polls its channel, the same discipline as the death probe.
	confirm *confirmProbe
}

// confirmProbe is one in-flight watchdog confirmation probe.
type confirmProbe struct {
	sentAt time.Time
	done   chan error
	cancel context.CancelFunc
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
//
//   - needsInput: the current step holds an open pending_question WITH NO
//     pending_answer yet — a PROTOCOL wait for a human. Both probes are
//     exempt while it holds: an agent legitimately parked on needs_input
//     can outlive every window, and killing it would only re-enter the
//     same wait after a pointless relaunch.
//
//     NARROWED BY polyforge#48 (folded from #50). The old test was
//     "pending_question != nil", which the pilot of 2026-09-29 01:20
//     measured as far too wide: an answer had been DELIVERED and the
//     agent hung mid-consumption (zero TCP / zero io / zero writes for
//     8+ minutes), yet the question was still open, so the exemption
//     held, the wall backstop was exempt with it, and all three
//     self-healing paths were dead — only a human could recover it.
//     pending_answer is the switch: pf-execute persists it BEFORE it
//     processes the answer (SKILL.md §12, and the daemon relies on that
//     ordering elsewhere already), so its presence means "the agent is
//     WORKING on an answer", which is ordinary in-flight work and earns
//     no exemption at all.
//
//     Under the STREAM criterion this stops being a special case: a
//     needs_input wait has no turn in flight, so the criterion
//     (inFlight && silence) cannot fire on it, while a consuming agent
//     DOES have a turn in flight and is judged like any other turn. The
//     explicit flag is kept for the degraded procfs path, which has no
//     way to see a turn.
//
//   - stepWall: the current step's declared wall budget — the ONE source
//     the backstop formula consumes: the per-step budget.wall_minutes in
//     the capsule's MATERIALIZED workflow.md frontmatter (03 §4.5), the
//     same declaration the executor's between-turns budget discipline
//     reads, materialized by this daemon itself from its own checkout —
//     no data-plane read, no second authority. A step without an explicit
//     wall budget assumes the default (watchdogStepWallDefault).
func (d *Daemon) stepProfile(wi string, jr *jobRuntime) (stepID string, needsInput bool, stepWall, stepHang time.Duration) {
	exec, err := jr.cap.LoadExecution()
	if err != nil || exec == nil {
		// No execution state yet (first step not started): no step id, no
		// protocol wait, the default wall for the backstop.
		return "", false, watchdogStepWallDefault, 0
	}
	stepID = exec.CurrentStep
	if q, _ := exec.PendingQuestion(); q != nil && q.PendingAnswer == nil {
		// Open question, answer not yet PERSISTED. Two sub-cases, and the
		// capsule alone cannot tell them apart:
		//
		//   - nothing was ever delivered: a genuine wait for a human ⇒
		//     exempt;
		//   - the daemon DID deliver a relay for this question and never
		//     observed its consumption: the answer is in the agent's hands
		//     and the agent is working. Not a wait ⇒ NOT exempt. This is
		//     the window the pilot's hang landed in (delivered, agent hung
		//     before its atomic pending_answer persist), and the daemon is
		//     the only party that can see it — it did the delivering.
		needsInput = !d.ledger.AwaitingQuestion(wi, q.QuestionID)
	}
	// The workflow.md read is cached per CURRENT STEP (the declaration
	// cannot change mid-step); the needs_input flag is NOT cached — a
	// question can be asked, and answered, at any point inside the step.
	if jr.wd != nil && jr.wd.profileRead && jr.wd.stepID == stepID {
		return stepID, needsInput, jr.wd.stepWall, jr.wd.stepHang
	}
	wall, hang := d.declaredStepBudget(jr, stepID)
	if jr.wd != nil {
		jr.wd.stepID, jr.wd.stepWall, jr.wd.stepHang, jr.wd.profileRead = stepID, wall, hang, true
	}
	return stepID, needsInput, wall, hang
}

// declaredStepWall resolves one step's declared wall budget from the
// capsule's materialized workflow.md frontmatter (see stepProfile for the
// source ruling). Unparsable/absent frontmatter degrades to the default —
// the backstop is a safety net, and a missing declaration must never
// disable it.
func (d *Daemon) declaredStepWall(jr *jobRuntime, stepID string) time.Duration {
	wall, _ := d.declaredStepBudget(jr, stepID)
	return wall
}

// declaredStepBudget reads BOTH watchdog-relevant declarations of one
// step from the SAME single source: budget.wall_minutes (the backstop's
// input, unchanged) and budget.hang_window_minutes (the polyforge#48
// per-step hang-window override; 0 when undeclared, meaning the
// daemon-wide window applies). One read, one cache, one authority.
func (d *Daemon) declaredStepBudget(jr *jobRuntime, stepID string) (wall, hang time.Duration) {
	raw, err := os.ReadFile(filepath.Join(jr.cap.Dir, "workflow.md"))
	if err != nil {
		return watchdogStepWallDefault, 0
	}
	front, _, ok := schema.SplitFrontmatter(raw)
	if !ok {
		return watchdogStepWallDefault, 0
	}
	var fm schema.Frontmatter
	if err := yaml.Unmarshal(front, &fm); err != nil {
		return watchdogStepWallDefault, 0
	}
	for _, st := range fm.Steps {
		if st.ID != stepID {
			continue
		}
		wall = watchdogStepWallDefault
		if st.Budget == nil {
			return wall, 0
		}
		if st.Budget.WallMinutes != nil && *st.Budget.WallMinutes > 0 {
			wall = time.Duration(*st.Budget.WallMinutes) * time.Minute
		}
		hang = declaredStepHangWindow(front, stepID)
		return wall, hang
	}
	return watchdogStepWallDefault, 0
}

// wdFrontmatter is the DAEMON-LOCAL view of the one frontmatter key this
// watchdog owns. budget.hang_window_minutes is a RUNTIME-supervision
// knob, not a workflow-semantics one: the executor never reads it, the
// bundle validator has no opinion on it, and the workflow schema
// (internal/workflow/schema) stays unextended on purpose — a supervision
// detail must not become part of the authored-bundle contract. YAML
// unmarshalling is non-strict, so a bundle carrying the key parses
// identically for every other consumer, and a bundle without it simply
// yields 0 (= use the daemon-wide window).
type wdFrontmatter struct {
	Steps []struct {
		ID     string `yaml:"id"`
		Budget *struct {
			HangWindowMinutes *float64 `yaml:"hang_window_minutes"`
		} `yaml:"budget"`
	} `yaml:"steps"`
}

// declaredStepHangWindow extracts one step's hang-window override from
// already-split frontmatter bytes. 0 means undeclared.
func declaredStepHangWindow(front []byte, stepID string) time.Duration {
	var fm wdFrontmatter
	if err := yaml.Unmarshal(front, &fm); err != nil {
		return 0
	}
	for _, st := range fm.Steps {
		if st.ID != stepID || st.Budget == nil || st.Budget.HangWindowMinutes == nil {
			continue
		}
		if m := *st.Budget.HangWindowMinutes; m > 0 {
			return time.Duration(m * float64(time.Minute))
		}
	}
	return 0
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
	stepID, needsInput, stepWall, stepHang := d.stepProfile(wi, jr)
	d.stormResetOnProgress(wi, jr, stepID)
	// Infra incidents first, ALWAYS: a provider error is news even for an
	// exempt or healthy job, and it is the earliest warning the operator
	// can get (polyforge#48 item 3).
	d.drainStreamIncidents(wi, jr, agent)

	// Watchdog 1 — death: the moment the exit face says the process is
	// gone, the crash continuation owns the job (collect + re-derive).
	if agentDone(agent) {
		d.cancelConfirmProbe(jr)
		d.watchdogIncident(ctx, wi, jr, stepID, "agent_exit")
		return
	}
	// needs_input: a protocol wait for a HUMAN (open question, answer not
	// yet delivered) — exempt from both probes (see stepProfile for the
	// polyforge#48 narrowing: a delivered-but-unconsumed answer is NOT
	// exempt). The process is alive (checked above); the step cannot
	// advance until the answer relay arrives.
	if needsInput {
		d.cancelConfirmProbe(jr)
		return
	}
	now := time.Now()
	// Absolute backstop: per agent instance, regardless of every probe.
	// This is the bound that holds when the criteria are disabled,
	// unavailable, or themselves lied to.
	if bound := watchdogBackstopBound(stepWall); now.Sub(jr.wd.instStart) > bound {
		d.cancelConfirmProbe(jr)
		d.killAgentGroup(jr)
		d.watchdogIncident(ctx, wi, jr, stepID, "wall_backstop")
		return
	}
	window := d.hangWindow(stepHang)
	// A confirmation probe already in flight owns the verdict until it
	// settles: no second probe, no parallel judgment.
	if jr.wd.confirm != nil {
		d.settleConfirmProbe(ctx, wi, jr, agent, stepID, now)
		return
	}
	// Watchdog 2 — hang. PRIMARY criterion: the protocol record stream.
	if hb, ok := streamHeartbeat(agent); ok {
		jr.wd.streamSeen = true
		// The procfs sample is still taken, as AUXILIARY EVIDENCE only:
		// it is attached to the incident so the operator sees both
		// stories, and it never overrides the stream.
		if hb.Silent(now, window) {
			d.beginConfirmProbe(wi, jr, agent, stepID, hb, window, now)
		}
		return
	}
	if !jr.wd.streamNoteed {
		jr.wd.streamNoteed = true
		fmt.Fprintf(os.Stderr, "pf-daemon: %s agent pid %d exposes no record stream (adopted handle?); the hang watchdog falls back to the DEMOTED /proc activity criterion plus the wall backstop\n", wi, agent.PID())
	}
	// DEGRADED criterion: /proc counters, now with the polyforge#48
	// threshold band (idle runtime noise is not activity). Only reached
	// when no stream is observable.
	sample, ok := d.sampleActivity(agent.PID())
	if !ok {
		if d.wdProbeAvailable && !jr.wd.probeFailLogged {
			jr.wd.probeFailLogged = true
			fmt.Fprintf(os.Stderr, "pf-daemon: %s agent pid %d activity counters unreadable; hang watchdog degrades to wall-clock-only for this job\n", wi, agent.PID())
		}
		return
	}
	if !jr.wd.hasSample {
		jr.wd.hasSample = true
		jr.wd.last = sample
		jr.wd.flatSince = now
		return
	}
	// The band is measured against the WINDOW BASE, never against the
	// previous tick: 1 jiffy per tick would otherwise look like movement
	// forever, which is exactly the production false negative.
	if sample.flatWithin(jr.wd.last, window) {
		if now.Sub(jr.wd.flatSince) >= window {
			d.beginConfirmProbe(wi, jr, agent, stepID, StreamHeartbeat{}, window, now)
		}
		return
	}
	// Real activity observed: the window restarts, the agent is left alone.
	jr.wd.last = sample
	jr.wd.flatSince = now
}

// streamHeartbeat reads one handle's stream heartbeat through the
// OPTIONAL face; ok=false means this handle has no observable stream.
func streamHeartbeat(h AgentHandle) (StreamHeartbeat, bool) {
	sp, is := h.(AgentStreamProbe)
	if !is {
		return StreamHeartbeat{}, false
	}
	hb := sp.StreamHeartbeat()
	if !hb.Observable {
		return StreamHeartbeat{}, false
	}
	return hb, true
}

// ---------------------------------------------------------------------------
// Infra incidents from the record stream (polyforge#48 item 3)

// drainStreamIncidents moves whatever the supervisor's classifier queued
// into the job's observe ring and the daemon log. Before this, a
// provider failure was bulk output: the operator's first sign of a dead
// upstream was the hang that followed minutes later. The incident is an
// IMMEDIATE signal — it appears within one 200ms dispatcher tick of the
// harness reporting it — and it is deliberately NOT a verdict: nothing
// here kills, relaunches or counts toward the storm guard. A provider
// error is information; the hang criterion remains the judgment.
func (d *Daemon) drainStreamIncidents(wi string, jr *jobRuntime, agent AgentHandle) {
	sp, ok := agent.(AgentStreamProbe)
	if !ok {
		return
	}
	for _, in := range sp.DrainStreamIncidents() {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s harness infra incident [%s] fatal=%v: %s\n", wi, in.Kind, in.Fatal, in.Detail)
		jr.ring.Append(EvInfraIncident, map[string]any{
			"kind": in.Kind, "detail": in.Detail, "fatal": in.Fatal,
			"at": in.At.UTC().Format(time.RFC3339Nano),
		})
	}
}

// ---------------------------------------------------------------------------
// Confirmation probe (polyforge#48 item 2)

// beginConfirmProbe arms ONE confirmation probe for a job whose hang
// window just expired. Window expiry is a suspicion; the probe is the
// ruled last step before a kill. A handle with no probe face has no way
// to answer, so the verdict stands immediately — the pre-#48 behavior,
// unchanged for those handles.
func (d *Daemon) beginConfirmProbe(wi string, jr *jobRuntime, agent AgentHandle, stepID string, hb StreamHeartbeat, window time.Duration, now time.Time) {
	aux := d.auxiliaryEvidence(agent)
	lp, ok := agent.(AgentLivenessProbe)
	if !ok {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s judged hung (%s silent %s, in_flight=%v; aux: %s) and exposes no confirmation-probe face; killing\n",
			wi, hangCriterion(hb), window, hb.InFlight, aux)
		d.killAgentGroup(jr)
		d.watchdogIncident(context.Background(), wi, jr, stepID, "hang")
		return
	}
	fmt.Fprintf(os.Stderr, "pf-daemon: %s %s silent for %s (in_flight=%v; aux: %s); sending the watchdog confirmation probe (bound %s) before any kill\n",
		wi, hangCriterion(hb), window, hb.InFlight, aux, watchdogProbeBoundVar)
	pctx, cancel := context.WithTimeout(context.Background(), watchdogProbeBoundVar)
	cp := &confirmProbe{sentAt: now, done: make(chan error, 1), cancel: cancel}
	jr.wd.confirm = cp
	jr.ring.Append(EvInfraIncident, map[string]any{
		"kind": "hang_suspected", "detail": fmt.Sprintf("%s silent for %s; confirmation probe sent", hangCriterion(hb), window),
		"fatal": false, "at": now.UTC().Format(time.RFC3339Nano),
	})
	go func() {
		cp.done <- lp.ProbeLiveness(pctx)
		cancel()
	}()
}

// settleConfirmProbe polls the in-flight probe WITHOUT blocking the
// tick. An ack cancels the kill and restarts the window (the agent was
// slow, not hung); silence past the bound confirms the verdict.
func (d *Daemon) settleConfirmProbe(ctx context.Context, wi string, jr *jobRuntime, agent AgentHandle, stepID string, now time.Time) {
	cp := jr.wd.confirm
	select {
	case err := <-cp.done:
		jr.wd.confirm = nil
		cp.cancel()
		if err == nil {
			fmt.Fprintf(os.Stderr, "pf-daemon: %s answered the watchdog confirmation probe after %s; NOT hung, window restarted\n", wi, now.Sub(cp.sentAt).Round(time.Second))
			jr.wd.flatSince = now
			if s, ok := d.sampleActivity(agent.PID()); ok {
				jr.wd.last, jr.wd.hasSample = s, true
			}
			jr.ring.Append(EvInfraIncident, map[string]any{
				"kind": "hang_cleared", "detail": "confirmation probe acked; the agent is alive and advancing",
				"fatal": false, "at": now.UTC().Format(time.RFC3339Nano),
			})
			return
		}
		fmt.Fprintf(os.Stderr, "pf-daemon: %s did not answer the watchdog confirmation probe (%v); hang CONFIRMED, killing (aux: %s)\n", wi, err, d.auxiliaryEvidence(agent))
		d.killAgentGroup(jr)
		d.watchdogIncident(ctx, wi, jr, stepID, "hang")
	default:
		// Still waiting. The probe goroutine's own ctx bounds it, so this
		// cannot wait forever; the backstop above bounds it again.
	}
}

// cancelConfirmProbe drops an in-flight probe whose job left the judged
// state (died, or entered a needs_input wait).
func (d *Daemon) cancelConfirmProbe(jr *jobRuntime) {
	if jr.wd == nil || jr.wd.confirm == nil {
		return
	}
	jr.wd.confirm.cancel()
	jr.wd.confirm = nil
}

// hangCriterion names which criterion is speaking, for the operator.
func hangCriterion(hb StreamHeartbeat) string {
	if hb.Observable {
		return "record stream"
	}
	return "/proc activity (degraded criterion)"
}

// auxiliaryEvidence renders the DEMOTED procfs reading for the incident
// log: it no longer decides anything, but "cpu +N jiffies, io flat" is
// exactly what a human needs to tell an epoll-idle runtime (the
// polyforge#48 production case) from a genuinely busy one.
func (d *Daemon) auxiliaryEvidence(agent AgentHandle) string {
	s, ok := d.sampleActivity(agent.PID())
	if !ok {
		return "/proc counters unreadable"
	}
	return fmt.Sprintf("cpu=%d ticks, rchar=%d, wchar=%d", s.cpuTicks, s.rchar, s.wchar)
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
	// polyforge#48 item 5: a NEW DERIVATION is a new consumer. Any answer
	// whose consumption was never observed is re-armed for it here — the
	// missing half of the self-healing chain the pilot measured.
	d.redeliverPendingAnswers(wi, jr)
	// Write the new process identity back into the jobs[] row (04 §7.1
	// step 4's final write), preserving the persisted watchdog state.
	j.MainAgentPID = handle.PID()
	j.MainAgentStartedAt = handle.StartedAt()
	j.Status = JobRunning
	if err := d.state.UpsertJob(d.st, *j); err != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: %s re-derivation pid write-back persist failed: %v\n", wi, err)
	}
}

// redeliverPendingAnswers re-delivers, to a freshly derived agent, every
// relay whose CONSUMPTION the daemon never observed (polyforge#48 item
// 5, folded from #50). Before this, the three-state ledger called such a
// delivery "retryable" and then nothing ever retried it: the pilot of
// 2026-09-29 01:20 watched an answer die with the agent that was
// consuming it, with the question still open and no path back short of a
// human re-running `pf answer`.
//
// IDEMPOTENCY is the relay's OWN ids, exactly as 00 §3.4a intends:
//
//   - the capsule is re-read first, and a relay is re-delivered ONLY
//     while its question_id is still the open pending_question. A
//     question the agent consumed before dying is gone from the capsule,
//     so its answer is dropped here and can never be applied twice;
//   - a pending_answer ALREADY persisted for this answer_id means the
//     dead agent got far enough to record it; the continuation prompt
//     resumes from that persisted state, so re-delivering would only
//     duplicate it. Dropped too.
//
// Best-effort by design: a re-delivery that fails leaves the relay in
// the awaiting set for the NEXT continuation, and never fails the
// continuation itself.
func (d *Daemon) redeliverPendingAnswers(wi string, jr *jobRuntime) {
	pending := d.ledger.Awaiting(wi)
	if len(pending) == 0 {
		return
	}
	exec, err := jr.cap.LoadExecution()
	if err != nil || exec == nil {
		return
	}
	q, _ := exec.PendingQuestion()
	d.mu.Lock()
	agent := jr.agent
	d.mu.Unlock()
	for _, relay := range pending {
		if q == nil || q.QuestionID != relay.QuestionID {
			// Consumed (or superseded) before the death: nothing to redo.
			d.ledger.clearAwaiting(wi, relay.AnswerID)
			continue
		}
		if q.PendingAnswer != nil && q.PendingAnswer.AnsweredAt == relay.AnsweredAt &&
			q.PendingAnswer.AnswerText == relay.AnswerText {
			// Already durably recorded by the dead agent: the continuation
			// resumes from it.
			d.ledger.clearAwaiting(wi, relay.AnswerID)
			continue
		}
		if agent == nil {
			return
		}
		// The ledger entry is re-opened so this delivery is not refused by
		// the idempotency map it filled in on the first attempt.
		d.ledger.markRedeliverable(relay.AnswerID)
		derr := agent.DeliverAnswer(relay)
		switch {
		case derr == nil:
			d.ledger.clearAwaiting(wi, relay.AnswerID)
			jr.ring.Append(EvAnswered, map[string]any{
				"question_id": relay.QuestionID, "answer_id": relay.AnswerID,
				"redelivered": true,
			})
			fmt.Fprintf(os.Stderr, "pf-daemon: %s answer %s re-delivered to the re-derived agent and consumed\n", wi, relay.AnswerID)
		case errors.Is(derr, ErrAnswerAckPending):
			jr.ring.Append(EvAnswered, map[string]any{
				"question_id": relay.QuestionID, "answer_id": relay.AnswerID,
				"redelivered": true, "consumption": "pending",
			})
			fmt.Fprintf(os.Stderr, "pf-daemon: %s answer %s re-delivered to the re-derived agent; consumption pending\n", wi, relay.AnswerID)
		default:
			fmt.Fprintf(os.Stderr, "pf-daemon: %s answer %s re-delivery failed (kept for the next continuation): %v\n", wi, relay.AnswerID, derr)
		}
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
