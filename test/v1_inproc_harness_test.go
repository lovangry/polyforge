package test

// v1_inproc_harness_test.go: the in-process runtime+observe rig for
// fault-injection scenarios whose detection windows must be shrunk
// (hang watchdog, probe bound). Since #12 the `pf daemon` CLI takes the
// window flags itself (--hang-window / --idle-window / --probe-timeout /
// --answer-ack-window), so s06 — the last scenario on this rig — now
// runs against the real daemon process and no scenario needs the rig
// today; it stays as the in-process injection seam, assembling the SAME
// production components — runtime.New (lifecycle + recovery engine), observe.New
// (per-WI monitor + detection faces), runtime.DefaultSpawner (the real
// pi-spawner that derives the real fakepi process) and observe.Handler
// (the real UDS flush/answer/observe routes) — with the observe config
// injected.
//
// The RecordTap binding order bug the lane report pinned here is fixed
// at the source: DefaultSpawner now takes the runtime config by pointer,
// so the tap set after the spawner is built (the rig's own order below)
// is the same binding the production daemon's assembly produces.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// inprocAdapter bridges runtime.AgentHandle to observe.Handle (the same
// shape as the daemon's agentAdapter, which is unexported).
type inprocAdapter struct {
	h        runtime.AgentHandle
	attempt  string
	recs     chan []byte
	exit     chan struct{}
	pollStop chan struct{}
	retire   sync.Once
}

func (a *inprocAdapter) AttemptID() string      { return a.attempt }
func (a *inprocAdapter) Exit() <-chan struct{}  { return a.exit }
func (a *inprocAdapter) Records() <-chan []byte { return a.recs }

func (a *inprocAdapter) retireAdapter() { a.retire.Do(func() { close(a.pollStop) }) }

func (a *inprocAdapter) pollDone(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-a.pollStop:
			return
		case <-t.C:
			if a.h.Done() {
				close(a.exit)
				return
			}
		}
	}
}

// SendControl composes the handle's write+ack pair into the observe
// face's single bounded call (the daemon adapter's contract: a
// written-but-unacked method maps to ErrAnswerAckPending).
func (a *inprocAdapter) SendControl(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
	ack := protocol.ControlAck{Method: msg.Method, Seq: msg.Seq}
	if err := a.h.SendControl(msg); err != nil {
		return ack, err
	}
	bound := 30 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if b := time.Until(dl); b > 0 && b < bound {
			bound = b
		}
	}
	if err := a.h.WaitControlAck(ctx, msg, bound); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ack, observe.ErrAnswerAckPending
		}
		return ack, err
	}
	ack.OK = true
	return ack, nil
}

// inprocRig is the assembled runtime+observe pair with injectable
// detection windows.
type inprocRig struct {
	t         *testing.T
	inst      *v1Install
	rt        *runtime.Runtime
	obs       *observe.Observe
	sock      string
	dataDir   string
	workspace string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	lst net.Listener
	srv *http.Server

	adaptMu  sync.Mutex
	adapters map[string]*inprocAdapter
}

// inprocObsTune is one injected tweak of the rig's observe.Config
// (the rig's whole point: production components, injectable windows).
type inprocObsTune func(*observe.Config)

// optBackpressure tunes the observe backpressure ladder: after is the
// TIME trigger's window (production default 30min), steps the
// consecutive-failure trigger's threshold (production default 5).
// The time-trigger scenario shrinks after to seconds and raises steps
// so the time bound always fires first.
func optBackpressure(after time.Duration, steps int) inprocObsTune {
	return func(c *observe.Config) {
		c.BackpressureAfter = after
		if steps != 0 {
			c.BackpressureSteps = steps
		}
	}
}

// newInprocRig assembles and runs the rig. hangWindow/probeTimeout are
// the injected detection bounds; zero keeps the production defaults.
// obsTune carries additional observe.Config tweaks (e.g. the
// backpressure windows), so a scenario extends the assembly instead of
// forking the helper.
func newInprocRig(t *testing.T, inst *v1Install, hangWindow, probeTimeout time.Duration, obsTune ...inprocObsTune) *inprocRig {
	t.Helper()
	sockBase := shortTmp("/tmp", "pfv1inproc")
	sock := filepath.Join(sockBase, "o.sock")
	dataDir := t.TempDir()
	workspace := t.TempDir()

	rig := &inprocRig{
		t: t, inst: inst, sock: sock, dataDir: dataDir, workspace: workspace,
		adapters: map[string]*inprocAdapter{},
		done:     make(chan struct{}),
	}

	// The spawner inherits THIS process's environment: the plan dir must
	// be visible to the spawned agent (the daemon process gets it via its
	// own launch env).
	os.Setenv("FAKEPI_PLANS", inst.planDir)

	// ORPHAN PIN (pilot finding): every rig in this tree must derive the
	// FAKE harness binary, never the PiBin default ("pi" on PATH) — a
	// rig that silently dropped the field would spawn the operator's REAL
	// harness as an orphan that outlives the test binary and whose cwd
	// (the capsule temp dir) is deleted under it. The pin is structural:
	// fail the assembly, not the run.
	if inst.fakepiPath == "" {
		t.Fatalf("inproc rig: no fake harness binary built; refusing the PiBin default (a real harness would be spawned)")
	}

	rtCfg := runtime.Config{
		MachineID:          "m-inproc",
		WorkspaceRoot:      workspace,
		DataDir:            dataDir,
		GitRemoteBase:      inst.url + "/git",
		WorkflowsDir:       filepath.Join(inst.root, "workflows"),
		Harness:            "pi",
		PiBin:              inst.fakepiPath,
		SocketPath:         sock,
		ExecutorPromptFile: filepath.Join(inst.root, "skills", "pf-execute", "SKILL.md"),
	}
	// The tap rides the spawner's SHARED config (DefaultSpawner takes it
	// by pointer), so the binding order is no longer load-bearing — kept
	// here in the historical order for readability.
	rtCfg.RecordTap = rig.tapRecord
	sp := runtime.DefaultSpawner(&rtCfg)

	rtMsgs := make(chan protocol.RTMsg, 64)
	obMsgs := make(chan protocol.OBMsg, 64)
	attachCh := make(chan observe.Attach, 64)

	rig.rt = runtime.New(rtCfg, inst.client, sp, rtMsgs, obMsgs)
	obCfg := observe.Config{
		RT:              attachCh,
		OB:              obMsgs,
		DataDir:         dataDir,
		PollInterval:    50 * time.Millisecond,
		HangWindow:      hangWindow,
		ProbeTimeout:    probeTimeout,
		AnswerAckWindow: 3 * time.Second,
	}
	for _, tune := range obsTune {
		tune(&obCfg)
	}
	obs, err := observe.New(obCfg)
	if err != nil {
		t.Fatalf("observe.New: %v", err)
	}
	rig.obs = obs

	rig.ctx, rig.cancel = context.WithCancel(context.Background())
	go func() { _ = rig.rt.Run(rig.ctx) }()
	go func() { rig.obs.Run(rig.ctx); close(rig.done) }()
	go rig.bridge(rtMsgs, attachCh)

	// The observe lane's own UDS face: the real routes (flush/answer/
	// observe) on a real listener, so the agent's flush(reason) proxy
	// calls are production-shaped.
	lst, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("inproc UDS listen: %v", err)
	}
	rig.lst = lst
	rig.srv = &http.Server{Handler: rig.obs.Handler()}
	go func() { _ = rig.srv.Serve(lst) }()

	t.Cleanup(func() {
		// Teardown order: cancel the loops, then TERMINATE every live
		// agent handle the runtime still holds — the in-process runtime
		// does not kill agents on ctx end (Run just returns; only the
		// daemon's shutdown sequence does), so without this sweep every
		// still-live fake harness child would outlive the test binary as
		// an orphan whose cwd is deleted under it (the pilot's orphan
		// finding, pinned here for the rig itself).
		rig.cancel()
		for _, ji := range rig.rt.List() {
			if ji.Agent != nil {
				_ = ji.Agent.Terminate(5 * time.Second)
			}
		}
		_ = rig.srv.Close()
		select {
		case <-rig.done:
		case <-time.After(5 * time.Second):
		}
	})
	return rig
}

// tapRecord feeds one raw stdout record line to the work item's current
// adapter (bounded drop, the daemon's contract).
func (r *inprocRig) tapRecord(wi string, line []byte) {
	r.adaptMu.Lock()
	a := r.adapters[wi]
	r.adaptMu.Unlock()
	if a == nil {
		return
	}
	select {
	case a.recs <- line:
	default:
	}
}

// installAdapter registers the CURRENT handle as the tap target.
func (r *inprocRig) installAdapter(ji runtime.JobInfo) *inprocAdapter {
	a := &inprocAdapter{
		h:        ji.Agent,
		attempt:  ji.InstanceID,
		recs:     make(chan []byte, 1024),
		exit:     make(chan struct{}),
		pollStop: make(chan struct{}),
	}
	r.adaptMu.Lock()
	if old := r.adapters[ji.WorkItem]; old != nil {
		old.retireAdapter()
	}
	r.adapters[ji.WorkItem] = a
	r.adaptMu.Unlock()
	go a.pollDone(50 * time.Millisecond)
	return a
}

// bridge translates RT messages into observe attachments (the daemon's
// bridge shape, minus the daemon-only bookkeeping).
func (r *inprocRig) bridge(rtMsgs <-chan protocol.RTMsg, attachCh chan<- observe.Attach) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case msg, ok := <-rtMsgs:
			if !ok {
				return
			}
			at := observe.Attach{Msg: msg}
			if msg.Kind == protocol.RTStarted || msg.Kind == protocol.RTRecovered {
				if ji, live := r.rt.Info(msg.WorkItem); live && ji.Agent != nil {
					at = observe.Attach{
						Msg:    msg,
						Handle: r.installAdapter(ji),
						WI: observe.WIInfo{
							WorkItem:   ji.WorkItem,
							Project:    ji.Project,
							CapsuleDir: ji.CapsuleDir,
							BundleName: ji.BundleName,
							Git:        ji.Git,
						},
					}
				}
			}
			select {
			case attachCh <- at:
			case <-r.ctx.Done():
				return
			}
		}
	}
}

// start claims and runs one work item through the real Runtime.
func (r *inprocRig) start(wi string) {
	r.t.Helper()
	if err := r.rt.Start(r.ctx, wi); err != nil {
		r.t.Fatalf("runtime start %s: %v", wi, err)
	}
}

// capsuleDir is the rig's capsule root for wi.
func (r *inprocRig) capsuleDir(wi string) string {
	return filepath.Join(r.workspace, "capsules", wi)
}

// eventsOf reads the rig's durable events file for one work item.
func (r *inprocRig) eventsOf(t *testing.T, wi string) []protocol.EventEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dataDir, "events.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("events.log: %v", err)
	}
	var out []protocol.EventEntry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e protocol.EventEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.WI == wi {
			out = append(out, e)
		}
	}
	return out
}

// waitEvent waits until one event of the given kind appears.
func (r *inprocRig) waitEvent(t *testing.T, wi, kind string, bound time.Duration) protocol.EventEntry {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		for _, e := range r.eventsOf(t, wi) {
			if e.Kind == kind {
				return e
			}
		}
		if time.Now().After(deadline) {
			var kinds []string
			for _, e := range r.eventsOf(t, wi) {
				kinds = append(kinds, e.Kind)
			}
			t.Fatalf("timeout: no %q event for %s (saw %v)", kind, wi, kinds)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// eventBody decodes one event's body.
func eventBody(t *testing.T, e protocol.EventEntry, v any) {
	t.Helper()
	if len(e.Body) == 0 {
		t.Fatalf("event %s has no body", e.Kind)
	}
	if err := json.Unmarshal(e.Body, v); err != nil {
		t.Fatalf("event %s body: %v", e.Kind, err)
	}
}
