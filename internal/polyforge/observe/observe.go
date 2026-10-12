package observe

// observe.go: the Observe hub — the supervisor's root object. It owns
// the events log (driver events land in it too, durably), spawns one
// per-WI supervisor goroutine per RTStarted, routes RTRecovered/WIFinished
// to the right monitor, and exposes the HTTP face (observe/answer) over
// the monitors. Runtime talks to Observe through exactly two channels
// carrying the five protocol messages (the driver Handle rides WITH the
// lifecycle message as the attachment — it is not a third message kind).

import (
	"context"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
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
	RT *supervise.Queue[Attach]
	// OB is the Observe→Runtime queue (WINeedsRecovery, WICompleted).
	// Unbounded: a send never blocks and never drops.
	OB *supervise.Queue[protocol.OBMsg]
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

	// Registry hands out the per-WI supervision links shared with the
	// driver (facts in, verdicts out, harness trackers). nil = every
	// monitor builds a private link (unit tests).
	Registry *supervise.Registry
	// PollInterval is the supervisor tick (judging cadence; 500ms).
	PollInterval time.Duration
	// ModelErrorGrace is rule 1's grace after an error-ended turn (1m).
	ModelErrorGrace time.Duration
	// DriverStall is N: the driver between steps for longer than this,
	// not waiting on a human, is stalled (10m; negative disables).
	DriverStall       time.Duration
	PushRetryBackoff  time.Duration // async push retry spacing after a failure (10s)
	BackpressureSteps int           // N consecutive failed pushes before the degradation event (5)
	BackpressureAfter time.Duration // T without a successful push before the degradation event (30min)
	PushBound         time.Duration // per-push bound (protocol.PushTimeout, 60s)
}

func (c *Config) fill() {
	if c.PollInterval == 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.ModelErrorGrace == 0 {
		c.ModelErrorGrace = supervise.DefaultModelErrorGrace
	}
	if c.DriverStall == 0 {
		c.DriverStall = 10 * time.Minute
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
		return nil, errConfig("RT and OB queues are both required")
	}
	return &Observe{cfg: cfg, log: log,
		monitors: map[string]*Monitor{}}, nil
}

type errConfig string

func (e errConfig) Error() string { return "observe: " + string(e) }

// Run drives the hub until ctx is cancelled .
// On exit it stops every monitor, waits for them and closes the events
// log.
func (o *Observe) Run(ctx context.Context) {
	defer func() {
		o.stopAll()
		o.wg.Wait()
		_ = o.log.Close()
	}()
	for {
		for {
			at, ok := o.cfg.RT.Pop()
			if !ok {
				break
			}
			o.onAttach(at)
		}
		select {
		case <-ctx.Done():
			return
		case <-o.cfg.RT.Notify():
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
		m.announceStart()
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
		m.rtQ.Push(at)
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
	var link *supervise.Link
	if o.cfg.Registry != nil {
		link = o.cfg.Registry.Link(at.WI.WorkItem)
	} else {
		link = supervise.NewLink(at.WI.WorkItem)
	}
	m := &Monitor{
		wi: at.WI,
		cfg: monitorCfg{
			PollInterval:      o.cfg.PollInterval,
			ModelErrorGrace:   o.cfg.ModelErrorGrace,
			DriverStall:       o.cfg.DriverStall,
			PushRetryBackoff:  o.cfg.PushRetryBackoff,
			BackpressureSteps: o.cfg.BackpressureSteps,
			BackpressureAfter: o.cfg.BackpressureAfter,
			PushBound:         o.cfg.PushBound,
			revalidate:        o.cfg.SessionRevalidateEvery,
			verify:            o.cfg.SessionVerify,
			push:              o.cfg.PushFunc,
			locateLive:        o.cfg.LocateLive,
		},
		log:    o.log,
		ob:     o.cfg.OB,
		link:   link,
		rtQ:    supervise.NewQueue[Attach](),
		stopCh: make(chan struct{}),

		gate:       make(chan struct{}, 1),
		pushDone:   make(chan pushResult, 1),
		verifyDone: make(chan verifyResult, 1),
	}
	m.attach(at)
	m.phase = phaseBetween
	m.phaseSince = m.now()
	return m
}

// attach binds the first handle of a monitor's life (onRecovered does
// the same for successors).
func (m *Monitor) attach(at Attach) {
	m.handle = at.Handle
	m.exitCh = at.Handle.Exit()
	m.stateMu.Lock()
	m.spawnN = at.Msg.SpawnN
	m.sessionID = at.Msg.SessionID
	m.stateMu.Unlock()
	m.setTranscriptInfo(at.WI)
}

// Events exposes the log (diagnostic/tests).
func (o *Observe) Events() *EventLog { return o.log }
