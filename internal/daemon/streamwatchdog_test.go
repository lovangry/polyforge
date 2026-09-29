package daemon

// streamwatchdog_test.go: the polyforge#48 daemon-side regressions, at
// the realism level of watchdog_test.go / the S15-S16 usage scenarios —
// the real Core (newDaemonEnv), real child processes for the agents, and
// the real dispatcher tick. What is faked is only what a test cannot
// produce honestly: the harness's record stream (scripted through the
// OPTIONAL stream face, the same way a real supervisor would answer it)
// and the measured procfs trace of the production incident.
//
//   - (1) a silent-IN-FLIGHT stream ⇒ window ⇒ confirmation probe ⇒ no
//     ack ⇒ kill ⇒ crash continuation;
//   - (1b) the same, but the probe IS acked ⇒ the kill is cancelled;
//   - (2) the ORIGINAL #48 case: an epoll-idle runtime emitting NO
//     records and only floor-noise CPU ticks ⇒ must STILL fire. This
//     REVERSES the old trickle assertion for the procfs path — see
//     TestWatchdogTrickleHarnessSurvives, which keeps the
//     true-trickle-not-killed case, and (3b) below, which keeps it for
//     the stream shape;
//   - (3) legal thinking: records inside the window with long pauses
//     between ⇒ never killed;
//   - (4) an error record ⇒ an instant infra event on the job's ring;
//   - (6) the agent dies mid-consumption ⇒ re-derivation ⇒ idempotent
//     re-delivery ⇒ exactly one consumption.

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// ---------------------------------------------------------------------------
// A stream-capable fake handle.

// streamAgent decorates an integAgent with the OPTIONAL polyforge#48
// faces. It is the test's stand-in for mainagent.Proc: the daemon can
// only reach a harness stream through these two methods, so scripting
// them is scripting exactly the real contract surface.
type streamAgent struct {
	*integAgent
	mu        sync.Mutex
	hb        StreamHeartbeat
	incidents []StreamIncident
	// probeAck decides the confirmation probe's answer. nil means the
	// probe never answers (the wedged agent); a func returning nil is an
	// agent that acked.
	probeAck func() error
	probes   int
	// noProbeFace, when set, hides the AgentLivenessProbe face (a handle
	// that cannot be probed is killed on window expiry alone).
	noProbeFace bool
}

func (s *streamAgent) StreamHeartbeat() StreamHeartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hb
}

func (s *streamAgent) DrainStreamIncidents() []StreamIncident {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.incidents
	s.incidents = nil
	return out
}

func (s *streamAgent) ProbeLiveness(ctx context.Context) error {
	s.mu.Lock()
	s.probes++
	ack := s.probeAck
	s.mu.Unlock()
	if ack != nil {
		return ack()
	}
	<-ctx.Done() // the wedged agent: silence until the bound expires
	return ctx.Err()
}

// record simulates one harness record arriving on the stream.
func (s *streamAgent) record(inFlight bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hb.Observable = true
	s.hb.InFlight = inFlight
	s.hb.LastRecordAt = time.Now()
	s.hb.LastAnyRecordAt = time.Now()
	s.hb.Records++
}

func (s *streamAgent) queueIncident(in StreamIncident) {
	s.mu.Lock()
	s.incidents = append(s.incidents, in)
	s.mu.Unlock()
}

func (s *streamAgent) probeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes
}

// noProbeStreamAgent hides ProbeLiveness by wrapping without promoting
// it: a distinct type is the only honest way to model a handle that does
// not implement an optional interface.
type noProbeStreamAgent struct{ inner *streamAgent }

func (n *noProbeStreamAgent) PID() int                           { return n.inner.PID() }
func (n *noProbeStreamAgent) StartedAt() string                  { return n.inner.StartedAt() }
func (n *noProbeStreamAgent) Alive() bool                        { return n.inner.Alive() }
func (n *noProbeStreamAgent) GroupEmpty() bool                   { return n.inner.GroupEmpty() }
func (n *noProbeStreamAgent) NotifyStop(c context.Context) error { return n.inner.NotifyStop(c) }
func (n *noProbeStreamAgent) Quiesce(c context.Context) error    { return n.inner.Quiesce(c) }
func (n *noProbeStreamAgent) Resume(c context.Context) error     { return n.inner.Resume(c) }
func (n *noProbeStreamAgent) DeliverAnswer(r AnswerRelay) error  { return n.inner.DeliverAnswer(r) }
func (n *noProbeStreamAgent) Terminate(d time.Duration) error    { return n.inner.Terminate(d) }
func (n *noProbeStreamAgent) StreamHeartbeat() StreamHeartbeat   { return n.inner.StreamHeartbeat() }
func (n *noProbeStreamAgent) DrainStreamIncidents() []StreamIncident {
	return n.inner.DrainStreamIncidents()
}

// streamSpawn submits + spawns one job whose agents carry the stream
// faces, and returns the env plus the per-spawn streamAgent list.
func streamSpawn(t *testing.T, cfgTweak func(*Config), setup func(*streamAgent)) (*denv, *[]*streamAgent) {
	t.Helper()
	e := newDaemonEnv(t, cfgTweak)
	var made []*streamAgent
	e.spawner.wrap = func(a *integAgent) AgentHandle {
		sa := &streamAgent{integAgent: a}
		sa.hb = StreamHeartbeat{Observable: true, LastRecordAt: time.Now(), LastAnyRecordAt: time.Now()}
		if setup != nil {
			setup(sa)
		}
		made = append(made, sa)
		if sa.noProbeFace {
			return &noProbeStreamAgent{inner: sa}
		}
		return sa
	}
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim", Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	return e, &made
}

// ---------------------------------------------------------------------------
// (1) Silent IN-FLIGHT stream ⇒ probe ⇒ no ack ⇒ kill ⇒ continuation.

func TestStreamHangSilentInFlightProbedThenKilled(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the auxiliary evidence read is Linux-only")
	}
	savedProbe := setProbeBound(t, 300*time.Millisecond)
	defer savedProbe()

	e, agents := streamSpawn(t, func(c *Config) {
		c.WatchdogActivityWindow = 200 * time.Millisecond
	}, func(sa *streamAgent) {
		// A turn is OPEN and the last record is already old: the exact
		// shape of a model call whose connection died without RST.
		sa.hb = StreamHeartbeat{
			Observable: true, InFlight: true,
			LastRecordAt:    time.Now().Add(-time.Hour),
			LastAnyRecordAt: time.Now(),
			Records:         42,
		}
		sa.probeAck = nil // never answers
	})
	ring := wdRingOf(e.d, e.wi)

	if !wdTickFor(t, e.d, 15*time.Second, func() bool { return len(e.spawner.spawns) >= 2 }) {
		t.Fatalf("a silent in-flight stream past the window must be judged hung and re-derived: spawns=%d", len(e.spawner.spawns))
	}
	first := (*agents)[0]
	if first.probeCount() == 0 {
		t.Fatal("the kill must be preceded by the confirmation probe, never taken on window expiry alone")
	}
	if processAlive(e.spawner.agents[0].PID(), e.spawner.agents[0].StartedAt()) {
		t.Fatal("the confirmed-hung agent's group must be collected before the re-derivation")
	}
	// The §7.1 step-4 continuation shape, identical to the pre-#48 path.
	if e.spawner.spawns[0].SessionID != e.spawner.spawns[1].SessionID ||
		e.spawner.spawns[0].Generation != e.spawner.spawns[1].Generation {
		t.Fatalf("the continuation must reuse session id + generation: %+v vs %+v",
			e.spawner.spawns[0], e.spawner.spawns[1])
	}
	if !wdRingHas(ring, EvWIAgentRederived, "hang") {
		t.Fatal("the ring must carry the rederive event with the hang cause")
	}
	if !wdRingHasKind(ring, "hang_suspected") {
		t.Fatal("the confirmation probe being sent must itself be observable (infra_incident hang_suspected)")
	}
	st, err := NewStateStore(e.d.cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if j := st.FindJob(e.wi); j == nil || j.Watchdog == nil || j.Watchdog.Hangs != 1 {
		t.Fatalf("the hang must persist Hangs=1 before the relaunch (crash-honest): %+v", j)
	}
}

// (1b) The probe is ACKED: the agent was slow, not hung. No kill.
func TestStreamHangProbeAckCancelsTheKill(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available")
	}
	restore := setProbeBound(t, 2*time.Second)
	defer restore()

	e, agents := streamSpawn(t, func(c *Config) {
		c.WatchdogActivityWindow = 200 * time.Millisecond
	}, func(sa *streamAgent) {
		sa.hb = StreamHeartbeat{
			Observable: true, InFlight: true,
			LastRecordAt: time.Now().Add(-time.Hour), Records: 7,
		}
		// Acks after a turn-scale pause, then the stream resumes: the
		// legitimately-slow agent the probe exists to protect.
		sa.probeAck = func() error {
			time.Sleep(120 * time.Millisecond)
			sa.record(true)
			return nil
		}
	})
	a0 := e.spawner.agents[0]
	ring := wdRingOf(e.d, e.wi)

	if !wdTickFor(t, e.d, 5*time.Second, func() bool { return (*agents)[0].probeCount() > 0 }) {
		t.Fatal("the window must expire and arm the probe")
	}
	// Keep the stream alive the way a recovered agent would.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		(*agents)[0].record(true)
		e.d.watchdogTick(context.Background())
		time.Sleep(40 * time.Millisecond)
	}
	if n := len(e.spawner.spawns); n != 1 {
		t.Fatalf("an acked confirmation probe must CANCEL the kill, got %d spawns", n)
	}
	if !processAlive(a0.PID(), a0.StartedAt()) {
		t.Fatal("an agent that answered the probe must still be alive")
	}
	if !wdRingHasKind(ring, "hang_cleared") {
		t.Fatal("the cleared suspicion must be observable too")
	}
}

// ---------------------------------------------------------------------------
// (2) The ORIGINAL polyforge#48 case: idle-runtime floor noise, NO records.

// noiseSampler wraps the real procfs probe and adds the MEASURED
// production noise on top of a genuinely idle child: +1 CPU jiffy per
// noiseEvery, io counters untouched. That is the trace the pilot
// recorded (io zero-increment for 12+ minutes, /proc/<pid>/stat +1 every
// ~10 seconds from node's epoll timer accounting) — reproduced exactly
// rather than approximated by a shell loop whose CPU burn is not
// controllable.
func noiseSampler(real func(int) (activitySample, bool), noiseEvery time.Duration, start time.Time) func(int) (activitySample, bool) {
	return func(pid int) (activitySample, bool) {
		s, ok := real(pid)
		if !ok {
			return s, false
		}
		s.cpuTicks += uint64(time.Since(start) / noiseEvery)
		return s, true
	}
}

// TestWatchdogNoiseOnlyRuntimeStillFires is the polyforge#48 REGRESSION
// PROPER, on the DEMOTED procfs path (no stream face at all — the
// adopted-agent shape). An idle process whose only activity is floor
// noise must be judged hung. This deliberately REVERSES the pre-#48
// assertion that "any counter movement = alive": test (d)
// TestWatchdogTrickleHarnessSurvives keeps the true-trickle case (real
// io, orders of magnitude above the band) and must stay green alongside
// this one — together they are the whole point of the threshold band.
func TestWatchdogNoiseOnlyRuntimeStillFires(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: the activity criterion is Linux-only")
	}
	window := 600 * time.Millisecond
	e := newDaemonEnv(t, func(c *Config) { c.WatchdogActivityWindow = window })
	// A genuinely idle child (the epoll-parked node runtime) — no
	// records, no io, and NO stream face on the handle.
	e.spawner.cmd = func() *exec.Cmd { return exec.Command("sleep", "120") }
	t.Cleanup(func() {
		for _, a := range e.spawner.agents {
			if a.child != nil && a.child.Process != nil {
				_ = a.child.Process.Kill()
			}
		}
	})
	// The measured noise, scaled to the shrunk window. What must be
	// preserved is the RATE RELATIONSHIP, not the tick count: production
	// noise was ~6 jiffies/min against a 30 jiffies/min band, i.e. the
	// noise sits at a fifth of the budget. At test scale the budget is
	// noiseFloorTicks(window) (its floor of 2), so the noise is one tick
	// per window — strictly inside the band and, crucially, NON-ZERO:
	// the pre-#48 exact-equality test (`sample.flat(prev)`) resets its
	// window on any movement at all, so it would never fire here. That
	// difference IS the bug.
	noiseStart := time.Now()
	e.d.wdSample = noiseSampler(procfsActivitySample, window, noiseStart)

	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim", Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	if !wdTickFor(t, e.d, 20*time.Second, func() bool { return len(e.spawner.spawns) >= 2 }) {
		t.Fatalf("an epoll-idle runtime emitting only floor-noise CPU ticks MUST be judged hung (this is the production false negative of polyforge#48): spawns=%d", len(e.spawner.spawns))
	}
	if processAlive(e.spawner.agents[0].PID(), e.spawner.agents[0].StartedAt()) {
		t.Fatal("the noise-only agent must be killed before the re-derivation")
	}
	// Prove the noise was REAL, i.e. that the pre-#48 criterion could not
	// have produced this verdict: the counter genuinely moved during the
	// run, so exact-equality flatness was false the whole time.
	if time.Since(noiseStart) < window {
		t.Fatal("precondition: the run must have spanned at least one window of injected noise")
	}
	a, okA := e.d.wdSample(os.Getpid())
	time.Sleep(window + 50*time.Millisecond)
	b, okB := e.d.wdSample(os.Getpid())
	if !okA || !okB || b.cpuTicks <= a.cpuTicks {
		t.Fatalf("precondition: the injected noise must really move the CPU counter (the old exact-equality test would have seen ACTIVITY): %d -> %d", a.cpuTicks, b.cpuTicks)
	}
	if b.flat(a) {
		t.Fatal("precondition: under the PRE-#48 exact-equality criterion this process reads as ACTIVE — that is the false negative being fixed")
	}
}

// TestNoiseBandSeparatesIdleFromWork pins the band's arithmetic against
// the MEASURED numbers directly, at production scale, where an
// end-to-end test cannot run: the pilot's 10-minute window with 1 jiffy
// per 10 seconds must read FLAT, and any real work must not.
func TestNoiseBandSeparatesIdleFromWork(t *testing.T) {
	const window = 10 * time.Minute
	base := activitySample{rchar: 1000, wchar: 2000, cpuTicks: 500}

	// The production trace: io EXACTLY flat, cpu +60 over the window.
	idle := base
	idle.cpuTicks += 60
	if !idle.flatWithin(base, window) {
		t.Fatalf("the MEASURED production noise (60 jiffies over 10min, io zero-increment) must read FLAT; noise budget is %d ticks", noiseFloorTicks(window))
	}
	// One byte of io movement is real work: the pilot measured io at
	// exactly zero through the whole hang, so the io side stays strict.
	io := base
	io.rchar++
	if io.flatWithin(base, window) {
		t.Fatal("any io byte movement must count as activity")
	}
	// A streaming turn burns far more CPU than the band.
	work := base
	work.cpuTicks += noiseFloorTicks(window)
	if work.flatWithin(base, window) {
		t.Fatal("CPU at or above the band must count as activity")
	}
	// A counter going backwards means the pid was recycled: fail safe.
	back := base
	back.cpuTicks--
	if back.flatWithin(base, window) {
		t.Fatal("a backwards counter is an untrustworthy reading, never a hang verdict")
	}
	// The band must be a RATE: the same 60 ticks over a ONE-minute
	// window is 20x the rate and is real work.
	if idle.flatWithin(base, time.Minute) {
		t.Fatal("the band is a rate, not a fixed per-window count")
	}
}

// ---------------------------------------------------------------------------
// (3) Legal thinking, and (3b) the true-trickle case for the STREAM shape.

// TestStreamLegalThinkingNotKilled: an agent that emits records inside
// the window but pauses between them (a reasoning model before its
// first text delta) must never be touched, however long it runs.
func TestStreamLegalThinkingNotKilled(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available")
	}
	window := 400 * time.Millisecond
	e, agents := streamSpawn(t, func(c *Config) { c.WatchdogActivityWindow = window }, func(sa *streamAgent) {
		sa.hb = StreamHeartbeat{Observable: true, InFlight: true, LastRecordAt: time.Now()}
		sa.probeAck = func() error { return context.DeadlineExceeded } // would fail if ever probed
	})
	a0 := e.spawner.agents[0]

	// Long pauses (70% of the window) with a record in between: legal
	// thinking, sustained for many windows.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(window * 7 / 10)
		(*agents)[0].record(true)
		e.d.watchdogTick(context.Background())
	}
	if n := len(e.spawner.spawns); n != 1 {
		t.Fatalf("records inside the window (however sparse) must never be judged hung, got %d spawns", n)
	}
	if (*agents)[0].probeCount() != 0 {
		t.Fatal("a thinking agent must never even be probed")
	}
	if !processAlive(a0.PID(), a0.StartedAt()) {
		t.Fatal("a thinking agent must stay alive")
	}
}

// TestStreamIdleNoTurnInFlightNotKilled: silence with NO turn in flight
// is an idle agent, not a hang — the half of the criterion that makes
// the needs_input exemption fall out instead of being special-cased.
func TestStreamIdleNoTurnInFlightNotKilled(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available")
	}
	e, agents := streamSpawn(t, func(c *Config) {
		c.WatchdogActivityWindow = 150 * time.Millisecond
	}, func(sa *streamAgent) {
		sa.hb = StreamHeartbeat{
			Observable: true, InFlight: false, // no turn open
			LastRecordAt: time.Now().Add(-time.Hour), Records: 3,
		}
	})
	wdTicksFor(t, e.d, 1500*time.Millisecond)
	if n := len(e.spawner.spawns); n != 1 {
		t.Fatalf("silence with no turn in flight is idleness, not a hang, got %d spawns", n)
	}
	if (*agents)[0].probeCount() != 0 {
		t.Fatal("an idle agent must never be probed")
	}
}

// TestStreamHangWithoutProbeFaceKillsDirectly: a handle that cannot be
// probed keeps the pre-#48 behavior (window expiry is the verdict) —
// the probe is an ADDITION, never a new way to escape the bound.
func TestStreamHangWithoutProbeFaceKillsDirectly(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available")
	}
	e, _ := streamSpawn(t, func(c *Config) {
		c.WatchdogActivityWindow = 150 * time.Millisecond
	}, func(sa *streamAgent) {
		sa.noProbeFace = true
		sa.hb = StreamHeartbeat{
			Observable: true, InFlight: true,
			LastRecordAt: time.Now().Add(-time.Hour), Records: 5,
		}
	})
	if !wdTickFor(t, e.d, 10*time.Second, func() bool { return len(e.spawner.spawns) >= 2 }) {
		t.Fatalf("a hung handle with no probe face must still be killed on window expiry: spawns=%d", len(e.spawner.spawns))
	}
}

// ---------------------------------------------------------------------------
// (4) Error record ⇒ instant infra event on the ring.

// TestStreamInfraIncidentSurfacedImmediately: a provider error reaches
// the job's observe ring within one dispatcher tick, and does NOT act as
// a verdict (no kill, no relaunch, no storm-guard increment) — it is
// news, not judgment.
func TestStreamInfraIncidentSurfacedImmediately(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available")
	}
	e, agents := streamSpawn(t, nil, func(sa *streamAgent) {
		sa.hb = StreamHeartbeat{Observable: true, InFlight: true, LastRecordAt: time.Now()}
	})
	ring := wdRingOf(e.d, e.wi)
	(*agents)[0].queueIncident(StreamIncident{
		At: time.Now(), Kind: IncidentProviderError, Fatal: true,
		Detail: "sub2api-openai API error (401): 401 status code (no body)",
	})
	e.d.watchdogTick(context.Background())

	obs := ring.Observe(0, nil)
	var found *Event
	for i := range obs.Events {
		if obs.Events[i].Type == EvInfraIncident {
			found = &obs.Events[i]
		}
	}
	if found == nil {
		t.Fatal("a provider error must surface on the job's observe ring within one tick, not be counted as bulk")
	}
	if k, _ := found.Payload["kind"].(string); k != IncidentProviderError {
		t.Errorf("the incident kind must survive to the observer, got %v", found.Payload["kind"])
	}
	if d, _ := found.Payload["detail"].(string); d == "" || !containsAll(d, "401") {
		t.Errorf("the provider's own text must reach the observer, got %v", found.Payload["detail"])
	}
	if fatal, _ := found.Payload["fatal"].(bool); !fatal {
		t.Error("the fatal flag must survive to the observer")
	}
	// Information, not verdict.
	if n := len(e.spawner.spawns); n != 1 {
		t.Fatalf("an infra incident must NOT relaunch anything, got %d spawns", n)
	}
	st, err := NewStateStore(e.d.cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if j := st.FindJob(e.wi); j != nil && j.Watchdog != nil && j.Watchdog.Hangs != 0 {
		t.Fatalf("an infra incident must not count toward the storm guard: %+v", j.Watchdog)
	}
}

// ---------------------------------------------------------------------------
// The narrowed needs_input exemption (polyforge#48 / #50).

// TestNeedsInputExemptionNarrowsOnPendingAnswer is the #50 regression:
// with the answer DELIVERED (pending_answer persisted) the step is no
// longer "waiting for a human" — it is doing ordinary work — so a hang
// during consumption must be judged like any other hang. The
// still-unanswered question keeps its exemption in the control half.
func TestNeedsInputExemptionNarrowsOnPendingAnswer(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available")
	}
	restore := setProbeBound(t, 200*time.Millisecond)
	defer restore()

	// CONTROL: an open question with NO answer ⇒ exempt, never killed.
	eCtl, agentsCtl := streamSpawn(t, func(c *Config) {
		c.WatchdogActivityWindow = 150 * time.Millisecond
	}, func(sa *streamAgent) {
		sa.hb = StreamHeartbeat{Observable: true, InFlight: true, LastRecordAt: time.Now().Add(-time.Hour)}
	})
	wdWriteExecState(t, eCtl.d, eCtl.wi, func(x *capsule.Execution) {
		x.Steps["code"].PendingQuestion = &capsule.PendingQuestion{
			QuestionID: "q_wait", AskedAt: NowStamp(), Content: "which approach?",
		}
	})
	wdTicksFor(t, eCtl.d, 1500*time.Millisecond)
	if n := len(eCtl.spawner.spawns); n != 1 {
		t.Fatalf("an UNANSWERED needs_input wait must stay exempt, got %d spawns", n)
	}
	if (*agentsCtl)[0].probeCount() != 0 {
		t.Fatal("an exempt step must not even be probed")
	}

	// THE FIX: the same open question, but the answer has been delivered
	// and persisted ⇒ consuming ⇒ NOT exempt ⇒ hang judged and healed.
	e, _ := streamSpawn(t, func(c *Config) {
		c.WatchdogActivityWindow = 150 * time.Millisecond
	}, func(sa *streamAgent) {
		sa.hb = StreamHeartbeat{Observable: true, InFlight: true, LastRecordAt: time.Now().Add(-time.Hour)}
	})
	wdWriteExecState(t, e.d, e.wi, func(x *capsule.Execution) {
		x.Steps["code"].PendingQuestion = &capsule.PendingQuestion{
			QuestionID: "q_wait", AskedAt: NowStamp(), Content: "which approach?",
			PendingAnswer: &capsule.AnswerRecord{
				AnswerText: "use approach B", AnsweredAt: NowStamp(),
				AnsweredBy: "u_ops", ReceivedAt: NowStamp(),
			},
		}
	})
	if !wdTickFor(t, e.d, 10*time.Second, func() bool { return len(e.spawner.spawns) >= 2 }) {
		t.Fatalf("a hang DURING ANSWER CONSUMPTION must be judged (polyforge#50: the exemption used to swallow it and all three self-healing paths died): spawns=%d", len(e.spawner.spawns))
	}
}

// ---------------------------------------------------------------------------
// (6) Death mid-consumption ⇒ re-derivation ⇒ idempotent re-delivery.

// TestAnswerRedeliveredOnceAfterDeathMidConsumption is the other half of
// the #50 chain: the relay whose consumption was never observed is
// re-armed for the re-derived agent, exactly once, and is NOT re-armed
// when the dead agent had already consumed it.
func TestAnswerRedeliveredOnceAfterDeathMidConsumption(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available")
	}
	restore := setProbeBound(t, 200*time.Millisecond)
	defer restore()
	var mu sync.Mutex
	delivered := map[string][]AnswerRelay{} // per agent instance

	e := newDaemonEnv(t, func(c *Config) { c.WatchdogActivityWindow = 150 * time.Millisecond })
	idx := 0
	e.spawner.wrap = func(a *integAgent) AgentHandle {
		myIdx := idx
		idx++
		sa := &streamAgent{integAgent: a}
		// The FIRST agent hangs mid-consumption; the re-derived one is
		// healthy (a record just arrived, no turn stuck).
		if myIdx == 0 {
			sa.hb = StreamHeartbeat{Observable: true, InFlight: true, LastRecordAt: time.Now().Add(-time.Hour)}
		} else {
			sa.hb = StreamHeartbeat{Observable: true, InFlight: false, LastRecordAt: time.Now()}
		}
		a.onAnswer = func(r AnswerRelay) {
			mu.Lock()
			delivered[sa.startedAt+"#"+itoaSmall(myIdx)] = append(delivered[sa.startedAt+"#"+itoaSmall(myIdx)], r)
			mu.Unlock()
		}
		return sa
	}
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim", Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	// The step is waiting on a question, and the answer has been
	// DELIVERED but not yet consumed (no pending_answer on the capsule:
	// the agent died before its atomic persist).
	wdWriteExecState(t, e.d, e.wi, func(x *capsule.Execution) {
		x.Steps["code"].PendingQuestion = &capsule.PendingQuestion{
			QuestionID: "q_mid01", AskedAt: NowStamp(), Content: "go on?",
		}
	})
	relay := AnswerRelay{
		QuestionID: "q_mid01", AnswerID: "a_mid01", AnswerText: "yes, proceed",
		AnsweredAt: NowStamp(), AnsweredBy: "u_ops",
	}
	// The first delivery reached the agent but its consumption was never
	// observed (the two-phase pending outcome).
	err := e.d.ledger.SubmitFor(e.wi, relay, "u_ops",
		&execQuestionSource{load: func() (string, string, bool) { return "q_mid01", "code", true }},
		func(AnswerRelay) error { return ErrAnswerAckPending })
	if err != ErrAnswerAckPending {
		t.Fatalf("precondition: the first delivery must settle as consumption-pending, got %v", err)
	}
	if n := len(e.d.ledger.Awaiting(e.wi)); n != 1 {
		t.Fatalf("an unobserved consumption must be held for re-delivery, got %d", n)
	}

	// The agent hangs and is healed. The continuation must re-deliver.
	if !wdTickFor(t, e.d, 10*time.Second, func() bool { return len(e.spawner.spawns) >= 2 }) {
		t.Fatalf("the consuming agent's hang must be healed: spawns=%d", len(e.spawner.spawns))
	}
	mu.Lock()
	second := len(delivered[e.spawner.agents[1].startedAt+"#1"])
	mu.Unlock()
	if second != 1 {
		t.Fatalf("the re-derived agent must receive the SAME answer exactly once, got %d deliveries", second)
	}
	if n := len(e.d.ledger.Awaiting(e.wi)); n != 0 {
		t.Fatalf("an observed consumption must clear the awaiting entry, got %d", n)
	}

	// Idempotency: a further continuation must NOT deliver it again.
	d2 := e.d
	d2.redeliverPendingAnswers(e.wi, jobRuntimeOf(t, d2, e.wi))
	mu.Lock()
	again := len(delivered[e.spawner.agents[1].startedAt+"#1"])
	mu.Unlock()
	if again != 1 {
		t.Fatalf("re-delivery must be idempotent: a consumed answer is never delivered twice, got %d", again)
	}
}

// TestRedeliveryDroppedWhenAlreadyConsumed: the capsule is the authority.
// A question the dead agent finished consuming is gone from
// pending_question, so its answer must be DROPPED, never re-applied.
func TestRedeliveryDroppedWhenAlreadyConsumed(t *testing.T) {
	e, agents := streamSpawn(t, nil, nil)
	wdWriteExecState(t, e.d, e.wi, nil) // current step "code", no pending question
	relay := AnswerRelay{
		QuestionID: "q_gone", AnswerID: "a_gone", AnswerText: "stale",
		AnsweredAt: NowStamp(), AnsweredBy: "u_ops",
	}
	e.d.ledger.noteAwaiting(e.wi, relay)
	before := len((*agents)[0].integAgent.answers)
	e.d.redeliverPendingAnswers(e.wi, jobRuntimeOf(t, e.d, e.wi))
	if n := len((*agents)[0].integAgent.answers); n != before {
		t.Fatalf("an answer whose question is no longer open must be DROPPED, not re-applied: %d new deliveries", n-before)
	}
	if n := len(e.d.ledger.Awaiting(e.wi)); n != 0 {
		t.Fatalf("the stale relay must be forgotten, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// The per-step hang-window override.

// TestPerStepHangWindowOverride: budget.hang_window_minutes in the
// materialized workflow.md is read as the step's own hang window (the
// same single-source mechanism the wall budget uses), and a step without
// it falls back to the daemon-wide window.
func TestPerStepHangWindowOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dirJoin(dir, "workflow.md"), []byte(
		"---\nname: hangprobe\nsteps:\n  - id: s1\n    kind: code\n    budget: {wall_minutes: 2, hang_window_minutes: 25}\n  - id: s2\n    kind: code\n    budget: {wall_minutes: 3}\n  - id: s3\n    kind: code\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{MachineID: "m-hang", DataDir: dirJoin(dir, "d"), WorkspaceRoot: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jr := &jobRuntime{cap: &capsule.Capsule{Dir: dir}}
	wall, hang := d.declaredStepBudget(jr, "s1")
	if wall != 2*time.Minute || hang != 25*time.Minute {
		t.Fatalf("both declarations must come from the ONE read: wall=%s hang=%s", wall, hang)
	}
	if _, h := d.declaredStepBudget(jr, "s2"); h != 0 {
		t.Fatalf("a step declaring only a wall budget must not invent a hang window, got %s", h)
	}
	if _, h := d.declaredStepBudget(jr, "s3"); h != 0 {
		t.Fatalf("a step with no budget block must not invent a hang window, got %s", h)
	}
	// Resolution: the override wins at production scale.
	if got := d.hangWindow(25 * time.Minute); got != 25*time.Minute {
		t.Fatalf("the per-step override must win over the default, got %s", got)
	}
	if got := d.hangWindow(0); got != DefaultWatchdogActivityWindow {
		t.Fatalf("no override ⇒ the daemon-wide window, got %s", got)
	}
	// A tightened daemon window SCALES the override rather than letting a
	// step's production-scale declaration reinstate production timings
	// under a test/operator-tightened daemon.
	d2, err := New(Config{
		MachineID: "m-hang2", DataDir: dirJoin(dir, "d2"), WorkspaceRoot: dir,
		WatchdogActivityWindow: 200 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := d2.hangWindow(30 * time.Minute) // 3x the default
	if got != 600*time.Millisecond {
		t.Fatalf("a tightened daemon window must scale the override by its declared ratio, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// helpers

// setProbeBound shrinks the confirmation-probe bound for a test and
// returns the restore func (the same test-seam convention as
// watchProcRoot / watchdogStepWallDefault).
func setProbeBound(t *testing.T, d time.Duration) func() {
	t.Helper()
	saved := watchdogProbeBoundVar
	watchdogProbeBoundVar = d
	return func() { watchdogProbeBoundVar = saved }
}

func wdRingHasKind(ring *Ring, kind string) bool {
	for _, ev := range ring.Observe(0, nil).Events {
		if ev.Type != EvInfraIncident {
			continue
		}
		if k, _ := ev.Payload["kind"].(string); k == kind {
			return true
		}
	}
	return false
}

func jobRuntimeOf(t *testing.T, d *Daemon, wi string) *jobRuntime {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	jr, ok := d.jobs[wi]
	if !ok {
		t.Fatalf("no runtime job for %s", wi)
	}
	return jr
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func itoaSmall(i int) string { return string(rune('0' + i)) }

func dirJoin(parts ...string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += string(os.PathSeparator) + p
	}
	return out
}
