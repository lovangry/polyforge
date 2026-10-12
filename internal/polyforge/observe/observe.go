package observe

// observe.go: the Observe hub — the lane's root object. It owns the
// events log, spawns one per-WI monitor goroutine per RTStarted, routes
// RTRecovered/WIFinished to the right monitor, and exposes the HTTP face
// (observe/answer) over the monitors. Runtime talks to Observe
// through exactly two channels carrying the five protocol messages (the
// agent Handle rides WITH the lifecycle message as the attachment this
// lane needs to observe and control — it is not a third message kind).

import (
	"context"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// Attach is one Runtime→Observe lifecycle event plus its attachment:
// the agent handle for RTStarted/RTRecovered, the per-WI facts for
// RTStarted.
type Attach struct {
	Msg    protocol.RTMsg
	Handle Handle // nil for RTFinished
	// WI is required on RTStarted and re-read on RTRecovered: the
	// successor derivation carries its own per-derivation facts (F3
	// transcript info), which the monitor adopts alongside the handle.
	WI WIInfo
}

// Config wires the hub. Zero fields take the defaults in fill().
type Config struct {
	// RT is the Runtime→Observe lifecycle channel (RTStarted/
	// RTRecovered/RTFinished). Runtime writes; Observe reads.
	RT <-chan Attach
	// OB is the Observe→Runtime channel (WINeedsRecovery, WICompleted).
	// It must be buffered (Runtime drains it; the monitor never blocks
	// on a send and drops only if the buffer is full).
	OB chan protocol.OBMsg
	// DataDir hosts the daemon events file (events.log).
	DataDir string
	// PushFunc overrides the capsule commit+push operation (tests).
	PushFunc PushFunc
	// LocateLive, when non-nil, resolves the live harness transcript of a
	// derivation (capsuleDir + spawn moment → absolute .jsonl path; "" =
	// not found). The monitor calls it at its commit-boundary sync points
	// when the runtime-attached WIInfo carries no recorded live path —
	// the F3 daemon-side transcript sync. nil disables the sync entirely
	// (unit tests that never exercise it).
	LocateLive func(capsuleDir string, spawnAt time.Time) string
	// Resolve maps a non-canonical work-item id form (a "<project>#<seq>"
	// slug) to the canonical id the monitors are keyed by; "" = unknown.
	// The daemon binds it to its own canonicalizer so CLI-side route
	// calls by the operator's submitted spelling find the monitor.
	// nil = no resolution (the route id must already be canonical).
	Resolve func(wi string) string
	// SessionVerify is the v1.5 M4 revalidation seam: the periodic
	// per-RUNNING-job session-verify the monitor tick piggybacks
	// (session.go). The daemon binds the shared aihub client; nil — the
	// unit-test default — disables revalidation entirely.
	SessionVerify SessionVerifier
	// SessionRevalidateEvery is the M4 cadence (0 = the default ~5min;
	// negative = disabled).
	SessionRevalidateEvery time.Duration

	PollInterval time.Duration // execution.json poll tick (500ms)
	HangWindow   time.Duration // stream-silence window while a turn is in flight (10min)
	// IdleWindow is the F5 idle-watchdog window: no progress record
	// AND no turn in flight for this long ⇒ liveness probe ⇒ (no ack)
	// WINeedsRecovery(idle). It covers the shape the hang window is
	// blind to by definition — a turn that already ENDED (e.g. on a
	// fatal provider error) with the session left driving nothing.
	IdleWindow   time.Duration // no-record-no-turn quiet window (10min)
	ProbeTimeout time.Duration // liveness-probe ack bound (2min)
	// WallBackstop is the §6.2 absolute bound (polyforge#114): the cap
	// on a derivation's PROGRESS silence that no probe ack can restart —
	// the monitor's checkWallBackstop. 0 = the 15m default (the §6.2
	// floor; the per-step max(3× wall budget) refinement is future
	// work), a negative value disables the backstop entirely.
	WallBackstop      time.Duration
	PushRetryBackoff  time.Duration // async push retry spacing after a failure (10s)
	BackpressureSteps int           // N consecutive failed pushes before the degradation event (5)
	BackpressureAfter time.Duration // T without a successful push before the degradation event (30min)
	PushBound         time.Duration // per-push bound (protocol.PushTimeout, 60s)
	OBCapacity        int           // OB channel buffer check (64)
	// VersionStagnantTicks is the C3' belt-and-braces bound: consecutive
	// same-Version execution.json rewrites before a fallback commit + one
	// version_stagnant event. 0 = the default (3). Injectable for tests.
	VersionStagnantTicks int
}

func (c *Config) fill() {
	if c.PollInterval == 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.HangWindow == 0 {
		c.HangWindow = 10 * time.Minute
	}
	if c.IdleWindow == 0 {
		c.IdleWindow = 10 * time.Minute
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = 2 * time.Minute
	}
	if c.WallBackstop == 0 {
		// §6.2: 墙钟仅作绝对兜底，一切判据失效时的绝对界 — the 15-minute
		// floor of max(3× step wall budget, 15min) (polyforge#114; the
		// per-step refinement is future work).
		c.WallBackstop = defaultWallBackstop
	}
	if c.PushRetryBackoff == 0 {
		c.PushRetryBackoff = 10 * time.Second
	}
	if c.BackpressureSteps == 0 {
		c.BackpressureSteps = 5
	}
	if c.BackpressureAfter == 0 {
		c.BackpressureAfter = 30 * time.Minute
	}
	if c.PushBound == 0 {
		c.PushBound = protocol.PushTimeout
	}
	if c.OBCapacity == 0 {
		c.OBCapacity = 64
	}
	if c.VersionStagnantTicks == 0 {
		c.VersionStagnantTicks = defaultVersionStagnantTicks
	}
	if c.SessionRevalidateEvery == 0 {
		c.SessionRevalidateEvery = defaultRevalidateEvery
	}
	if c.PushFunc == nil {
		c.PushFunc = defaultPush
	}
}

// defaultPush binds the shared gitops choke point with a ctx race: on
// deadline the caller learns failure while the underlying git keeps
// running — the per-WI git lock serializes any successor push. The seq
// trailer is derived INSIDE gitops (single authority); nothing seq-wise
// flows through here, and the commit/push timings ride the result path
// out untouched (observed-only latency samples).
func defaultPush(ctx context.Context, o gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
	type res struct {
		sha    string
		pushed bool
		t      gitops.PushTimings
		err    error
	}
	done := make(chan res, 1)
	go func() {
		sha, pushed, t, err := gitops.CommitPush(o, message)
		done <- res{sha, pushed, t, err}
	}()
	select {
	case r := <-done:
		return r.sha, r.pushed, r.t, r.err
	case <-ctx.Done():
		return "", false, gitops.PushTimings{}, ctx.Err()
	}
}

// Observe is the hub.
type Observe struct {
	cfg      Config
	log      *EventLog
	mu       sync.Mutex
	monitors map[string]*Monitor

	wg sync.WaitGroup
}

// New validates the config and opens the events log.
func New(cfg Config) (*Observe, error) {
	cfg.fill()
	log, err := NewEventLog(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if cfg.RT == nil || cfg.OB == nil {
		log.Close()
		return nil, errConfig("RT and OB channels are both required")
	}
	if cap(cfg.OB) < 1 {
		log.Close()
		return nil, errConfig("OB channel must be buffered")
	}
	return &Observe{cfg: cfg, log: log,
		monitors: map[string]*Monitor{}}, nil
}

type errConfig string

func (e errConfig) Error() string { return "observe: " + string(e) }

// Run drives the hub until ctx is cancelled (or the RT channel closes).
// On exit it stops every monitor, waits for them and closes the events
// log.
func (o *Observe) Run(ctx context.Context) {
	defer func() {
		o.stopAll()
		o.wg.Wait()
		_ = o.log.Close()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case at, ok := <-o.cfg.RT:
			if !ok {
				return
			}
			o.onAttach(at)
		}
	}
}

func (o *Observe) onAttach(at Attach) {
	wi := at.Msg.WorkItem
	switch at.Msg.Kind {
	case protocol.RTStarted:
		if old := o.takeMonitor(wi); old != nil {
			// A stale monitor for the same WI (Runtime restarted the WI
			// without a WIFinished): stop it, the new derivation wins.
			old.requestStop()
		}
		if at.Handle == nil || at.Msg.SpawnN <= 0 || at.WI.WorkItem == "" {
			_ = o.log.Append(wi, EvJobStopped, map[string]any{
				"error": "RTStarted without handle/spawn_n/wi-info; not monitored",
			})
			return
		}
		m := o.newMonitor(at)
		o.setMonitor(wi, m)
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			m.run()
		}()
	case protocol.RTRecovered:
		m := o.lookup(wi)
		if m == nil {
			_ = o.log.Append(wi, EvJobStopped, map[string]any{
				"error": "RTRecovered for an unmonitored work item; dropped",
			})
			return
		}
		select {
		case m.rtCh <- at:
		default:
			_ = o.log.Append(wi, EvJobStopped, map[string]any{
				"error": "RTRecovered dropped: monitor rt queue full",
			})
		}
	case protocol.RTFinished:
		if m := o.takeMonitor(wi); m != nil {
			m.requestStop() // monitor emits its own job_stopped event
		}
	default:
		_ = o.log.Append(wi, EvJobStopped, map[string]any{
			"error": "unknown RT message kind: " + string(at.Msg.Kind),
		})
	}
}

func (o *Observe) stopAll() {
	o.mu.Lock()
	ms := make([]*Monitor, 0, len(o.monitors))
	for _, m := range o.monitors {
		ms = append(ms, m)
	}
	o.monitors = map[string]*Monitor{}
	o.mu.Unlock()
	for _, m := range ms {
		m.requestStop()
	}
}

func (o *Observe) lookup(wi string) *Monitor {
	o.mu.Lock()
	m := o.monitors[wi]
	o.mu.Unlock()
	if m != nil {
		return m
	}
	// a non-canonical spelling (the operator's slug): resolve and retry.
	// The monitors are keyed by the canonical id the runtime announced.
	if o.cfg.Resolve == nil {
		return nil
	}
	canonical := o.cfg.Resolve(wi)
	if canonical == "" || canonical == wi {
		return nil
	}
	o.mu.Lock()
	m = o.monitors[canonical]
	o.mu.Unlock()
	return m
}

func (o *Observe) setMonitor(wi string, m *Monitor) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.monitors[wi] = m
}

// MarkClosing arms the expected-death suppression for a daemon-initiated
// close: the agent's imminent death raises no recovery (the monitor's
// sealing flag). No monitor (never monitored) is a no-op.
func (o *Observe) MarkClosing(wi string) {
	if m := o.lookup(wi); m != nil {
		m.markClosing()
	}
}

func (o *Observe) takeMonitor(wi string) *Monitor {
	o.mu.Lock()
	defer o.mu.Unlock()
	m := o.monitors[wi]
	delete(o.monitors, wi)
	return m
}

// newMonitor builds the per-WI supervisor for one RTStarted attachment.
func (o *Observe) newMonitor(at Attach) *Monitor {
	m := &Monitor{
		wi: at.WI,
		cfg: monitorCfg{
			PollInterval:         o.cfg.PollInterval,
			HangWindow:           o.cfg.HangWindow,
			IdleWindow:           o.cfg.IdleWindow,
			ProbeTimeout:         o.cfg.ProbeTimeout,
			WallBackstop:         o.cfg.WallBackstop,
			PushRetryBackoff:     o.cfg.PushRetryBackoff,
			BackpressureSteps:    o.cfg.BackpressureSteps,
			BackpressureAfter:    o.cfg.BackpressureAfter,
			PushBound:            o.cfg.PushBound,
			VersionStagnantTicks: o.cfg.VersionStagnantTicks,
			revalidate:           o.cfg.SessionRevalidateEvery,
			verify:               o.cfg.SessionVerify,
			push:                 o.cfg.PushFunc,
			locateLive:           o.cfg.LocateLive,
		},
		log:    o.log,
		ob:     o.cfg.OB,
		rtCh:   make(chan Attach, 4),
		stopCh: make(chan struct{}),

		gate:       make(chan struct{}, 1),
		pushDone:   make(chan pushResult, 1),
		probeDone:  make(chan probeResult, 1),
		verifyDone: make(chan verifyResult, 1),

		hangAnchor: time.Now(),
		stream:     newStreamState(),
	}
	m.execPath = m.execJSONPath()
	m.attach(at)
	return m
}

// attach binds the first handle of a monitor's life (onRecovered does
// the same for successors).
func (m *Monitor) attach(at Attach) {
	m.handle = at.Handle
	m.records = at.Handle.Records()
	m.exitCh = at.Handle.Exit()
	m.idleAnchor = time.Now() // the idle window's pre-first-record baseline
	m.attachAt = time.Now()   // the wall backstop's non-resettable silence floor
	m.stateMu.Lock()
	m.spawnN = at.Msg.SpawnN
	m.sessionID = at.Msg.SessionID
	m.stateMu.Unlock()
	m.setTranscriptInfo(at.WI)
}

// Events exposes the log (diagnostic/tests).
func (o *Observe) Events() *EventLog { return o.log }
