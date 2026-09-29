package observe

// observe.go: the Observe hub — the lane's root object. It owns the
// events log, spawns one per-WI monitor goroutine per RTStarted, routes
// RTRecovered/WIFinished to the right monitor, and exposes the HTTP face
// (observe/answer/flush) over the monitors. Runtime talks to Observe
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
	WI     WIInfo // required on RTStarted; ignored afterwards
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

	PollInterval      time.Duration // execution.json poll tick (500ms)
	HangWindow        time.Duration // stream-silence window while a turn is in flight (10min)
	ProbeTimeout      time.Duration // liveness-probe ack bound (2min)
	AnswerAckWindow   time.Duration // pf/answer consumption-ack window (2min)
	PushRetryBackoff  time.Duration // async push retry spacing after a failure (10s)
	BackpressureSteps int           // N consecutive failed pushes before the degradation event (5)
	BackpressureAfter time.Duration // T without a successful push before the degradation event (30min)
	FlushBound        time.Duration // per-flush bound (protocol.FlushTimeout, 60s)
	CallTimeout       time.Duration // HTTP-face wait for a monitor answer (65s)
	OBCapacity        int           // OB channel buffer check (64)
}

func (c *Config) fill() {
	if c.PollInterval == 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.HangWindow == 0 {
		c.HangWindow = 10 * time.Minute
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = 2 * time.Minute
	}
	if c.AnswerAckWindow == 0 {
		c.AnswerAckWindow = 2 * time.Minute
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
	if c.FlushBound == 0 {
		c.FlushBound = protocol.FlushTimeout
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = protocol.FlushTimeout + 5*time.Second
	}
	if c.OBCapacity == 0 {
		c.OBCapacity = 64
	}
	if c.PushFunc == nil {
		c.PushFunc = defaultPush
	}
}

// defaultPush binds the shared gitops choke point with a ctx race: on
// deadline the caller learns failure while the underlying git keeps
// running — the per-WI git lock serializes any successor push. The seq
// trailer is derived INSIDE gitops (single authority); nothing seq-wise
// flows through here.
func defaultPush(ctx context.Context, o gitops.Opts, message string) (string, bool, error) {
	type res struct {
		sha    string
		pushed bool
		err    error
	}
	done := make(chan res, 1)
	go func() {
		sha, pushed, err := gitops.CommitPush(o, message)
		done <- res{sha, pushed, err}
	}()
	select {
	case r := <-done:
		return r.sha, r.pushed, r.err
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

// Observe is the hub.
type Observe struct {
	cfg Config
	log *EventLog

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
	return &Observe{cfg: cfg, log: log, monitors: map[string]*Monitor{}}, nil
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
			// without a WIFinished): stop it, the new attempt wins.
			old.requestStop()
		}
		if at.Handle == nil || at.Msg.AttemptID == "" || at.WI.WorkItem == "" {
			_ = o.log.Append(wi, EvJobStopped, map[string]any{
				"error": "RTStarted without handle/attempt-id/wi-info; not monitored",
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
	defer o.mu.Unlock()
	return o.monitors[wi]
}

func (o *Observe) setMonitor(wi string, m *Monitor) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.monitors[wi] = m
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
			PollInterval:      o.cfg.PollInterval,
			HangWindow:        o.cfg.HangWindow,
			ProbeTimeout:      o.cfg.ProbeTimeout,
			AnswerAckWindow:   o.cfg.AnswerAckWindow,
			PushRetryBackoff:  o.cfg.PushRetryBackoff,
			BackpressureSteps: o.cfg.BackpressureSteps,
			BackpressureAfter: o.cfg.BackpressureAfter,
			FlushBound:        o.cfg.FlushBound,
			push:              o.cfg.PushFunc,
		},
		log:    o.log,
		ob:     o.cfg.OB,
		ledger: NewAnswerLedger(),

		rtCh:     make(chan Attach, 4),
		stopCh:   make(chan struct{}),
		answerCh: make(chan AnswerCall, 8),
		flushCh:  make(chan FlushCall, 8),

		gate:      make(chan struct{}, 1),
		pushDone:  make(chan pushResult, 1),
		wrapDone:  make(chan wrapResult, 1),
		probeDone: make(chan probeResult, 1),

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
	m.stateMu.Lock()
	m.attemptID = at.Msg.AttemptID
	m.stateMu.Unlock()
}

// Events exposes the log (diagnostic/tests).
func (o *Observe) Events() *EventLog { return o.log }
