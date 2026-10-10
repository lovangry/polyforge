package bridge

// bridge_test.go: the driver-bridge handle's probe semantics (the #99
// verification lesson) — a liveness probe on a live driver is acknowledged
// (the engine IS the supervised entity; step processes stay record-silent
// for a whole HangWindow during long tool calls), and every other control
// method stays refused (no stdio control channel exists).

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/common/spawn"
	"github.com/silgrid/polyforge/internal/polyforge/driver"
)

// waitUntil is the bridge tests' small poll helper.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestProbeAckedWhileDriverAlive(t *testing.T) {
	h := &Handle{done: make(chan error)}
	msg := protocol.ControlMsg{Method: protocol.ControlProbe, Nonce: "deadbeef", Seq: 7}
	if err := h.SendControl(msg); err != nil {
		t.Fatalf("probe on a live driver must be accepted, got %v", err)
	}
	if err := h.WaitControlAck(context.Background(), msg, time.Second); err != nil {
		t.Fatalf("probe ack on a live driver must succeed, got %v", err)
	}
}

func TestProbeFailsAfterDriverExit(t *testing.T) {
	h := &Handle{done: make(chan error)}
	h.mu.Lock()
	h.doneSet = true
	h.doneErr = nil
	close(h.done)
	h.mu.Unlock()
	msg := protocol.ControlMsg{Method: protocol.ControlProbe}
	if err := h.SendControl(msg); err == nil {
		t.Fatal("probe on a dead driver must fail — recovery owns it")
	}
	if err := h.WaitControlAck(context.Background(), msg, time.Second); err == nil {
		t.Fatal("probe ack on a dead driver must fail")
	}
}

func TestNonProbeControlStillRefused(t *testing.T) {
	h := &Handle{done: make(chan error)}
	// polyforge#106: the answer relay and the stop-notice delivery died
	// with the WI-Agent engine — the driver handle still refuses them BY
	// NAME (literals: the protocol constants are deleted) alongside
	// anything else that is not the probe.
	for _, method := range []string{"pf/stop_notice", "pf/answer"} {
		if err := h.SendControl(protocol.ControlMsg{Method: method}); err == nil {
			t.Fatalf("non-probe control %q must stay refused (no stdio channel)", method)
		}
	}
}

// TestTerminateJoinsDriverExit is the astra-r2 #1 regression: Terminate
// must BLOCK (bounded) until the driver goroutine actually returned —
// a cancel that merely returns lets the old driver's journal appends
// land after the relaunch reads JournalMax, re-colliding ordinals.
func TestTerminateJoinsDriverExit(t *testing.T) {
	h := &Handle{done: make(chan error), cancel: func() {}}
	// the driver goroutine exits 150ms after cancel
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		<-time.After(150 * time.Millisecond)
		h.mu.Lock()
		h.doneSet = true
		h.doneErr = context.Canceled
		close(h.done)
		h.mu.Unlock()
	}()
	start := time.Now()
	if err := h.Terminate(5 * time.Second); err != nil {
		t.Fatalf("terminate must join a clean (context-cancelled) driver exit: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("terminate returned before the driver exited (after %v)", elapsed)
	}
	select {
	case <-closed:
	default:
		t.Fatal("terminate must return only after the driver goroutine closed done")
	}
}

// TestTerminateBoundedWhenDriverNeverExits: a wedged driver answers the
// bound, loudly — the caller surfaces the incomplete teardown instead of
// assuming the writer is gone.
func TestTerminateBoundedWhenDriverNeverExits(t *testing.T) {
	h := &Handle{done: make(chan error), cancel: func() {}}
	if err := h.Terminate(40 * time.Millisecond); err == nil {
		t.Fatal("terminate must fail loudly when the driver does not exit in bound")
	}
}

// ---------------------------------------------------------------------------
// polyforge#79: the pass-through plumbing + the interrupted-cause policy.

// TestInterruptedFromCausePolicy pins the operator filter: an
// operator-shaped recovery cause archives nothing (a deliberate stop is
// not a model verdict); everything else classifies through the driver's
// fault table and archives with that class (unknown free text defaults
// provider — the bounded side).
func TestInterruptedFromCausePolicy(t *testing.T) {
	if got := interruptedFromCause(""); got != nil {
		t.Fatalf("an empty cause archives nothing, got %+v", got)
	}
	if got := interruptedFromCause("operator daemon shutdown"); got != nil {
		t.Fatalf("an operator-shaped cause archives nothing, got %+v", got)
	}
	for cause, want := range map[string]driver.FaultClass{
		"agent_death":    driver.FaultProvider, // free text defaults provider
		"daemon_restart": driver.FaultProvider,
		"provider_error": driver.FaultProvider,
		"hang":           driver.FaultProvider,
	} {
		got := interruptedFromCause(cause)
		if got == nil {
			t.Fatalf("cause %q must archive, got nil", cause)
		}
		if got.FaultClass != want {
			t.Fatalf("cause %q classified %q, want %q", cause, got.FaultClass, want)
		}
		if got.Detail != cause {
			t.Fatalf("archive detail %q, want the cause verbatim %q", got.Detail, cause)
		}
	}
}

// fakeRTSpawner is a scripted rt.Spawner: it records every Spawn's
// Params and parks the derivation until its terminate channel closes.
type fakeRTSpawner struct {
	mu     sync.Mutex
	params []spawn.Params
	block  chan struct{}
}

func (s *fakeRTSpawner) Spawn(ctx context.Context, p spawn.Params) (spawn.Handle, error) {
	s.mu.Lock()
	s.params = append(s.params, p)
	s.mu.Unlock()
	if s.block == nil {
		s.block = make(chan struct{})
	}
	return &fakeRTHandle{block: s.block}, nil
}

func (s *fakeRTSpawner) last() spawn.Params {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.params[len(s.params)-1]
}

type fakeRTHandle struct {
	block chan struct{}
	once  sync.Once
}

func (h *fakeRTHandle) PID() int                              { return 0 }
func (h *fakeRTHandle) StartedAt() string                     { return "fake" }
func (h *fakeRTHandle) Alive() bool                           { return true }
func (h *fakeRTHandle) Done() bool                            { return false }
func (h *fakeRTHandle) SendControl(protocol.ControlMsg) error { return nil }
func (h *fakeRTHandle) WaitControlAck(ctx context.Context, m protocol.ControlMsg, bound time.Duration) error {
	return nil
}
func (h *fakeRTHandle) Terminate(bound time.Duration) error {
	h.once.Do(func() { close(h.block) })
	return nil
}

// TestBridgeForwardsStepCandidates pins the pass-through: the driver's
// FIRST dispatch pins its model from the effective list the runtime
// supplied (the post-precedence compose), not the bundle's own list.
func TestBridgeForwardsStepCandidates(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "workflows", "wf")
	if err := os.MkdirAll(filepath.Join(bundle, "steps"), 0o755); err != nil {
		t.Fatal(err)
	}
	front := "---\nname: wf\ndefaults:\n  model_candidates: [bundle-weak]\nsteps:\n  - { id: fix, kind: code }\n---\n# body\n"
	if err := os.WriteFile(filepath.Join(bundle, "workflow.md"), []byte(front), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "steps", "fix.md"), []byte("do the fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"harness":"pi","session_id":"fp123"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	sp := &fakeRTSpawner{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Start(ctx, Config{
		WI:         "wi_pass",
		CapsuleDir: dir,
		BundleName: "wf",
		SpawnN:     1,
		Spawner:    sp,
		StepCandidates: map[string][]string{
			"fix": {"eff-a", "eff-b"},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Terminate(2 * time.Second)
	waitUntil(t, "the first dispatch", func() bool {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		return len(sp.params) > 0
	})
	if got := sp.last().Model; got != "eff-a" {
		t.Fatalf("first dispatch model %q, want the effective head eff-a", got)
	}
}
