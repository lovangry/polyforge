package bridge

// bridge_test.go: the driver-bridge handle — Terminate joins the driver's
// exit, the interrupted-cause archive policy (an unclassified cause never
// advances the candidate pointer), the candidate pass-through, and the
// driver-event / supervision-link wiring. (The liveness probe the bridge
// used to answer died with the guessing supervision lanes.)

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/spawn"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
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

// TestTerminateJoinsDriverExit is the astra-r2 #1 regression: Terminate
// must BLOCK (bounded) until the driver goroutine actually returned —
// a cancel that merely returns lets the old driver's journal appends
// land after the relaunch reads the journal, re-colliding ordinals.
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
// fault table and archives with that class. Unknown free text (a daemon
// restart, a driver relaunch) archives with NO class: unclassified never
// advances the candidate pointer (v2-converge 2.3).
func TestInterruptedFromCausePolicy(t *testing.T) {
	if got := interruptedFromCause(""); got != nil {
		t.Fatalf("an empty cause archives nothing, got %+v", got)
	}
	if got := interruptedFromCause("operator daemon shutdown"); got != nil {
		t.Fatalf("an operator-shaped cause archives nothing, got %+v", got)
	}
	for _, cause := range []string{"agent_death", "daemon_restart", "driver_stalled", "rate limit hit"} {
		got := interruptedFromCause(cause)
		if got == nil {
			t.Fatalf("cause %q must archive, got nil", cause)
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

func (h *fakeRTHandle) PID() int          { return 0 }
func (h *fakeRTHandle) StartedAt() string { return "fake" }
func (h *fakeRTHandle) Alive() bool       { return true }
func (h *fakeRTHandle) Done() bool        { return false }
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
	front := "---\nname: wf\ndefaults:\n  model_candidates: [pi:bundle-weak]\nsteps:\n  - { id: fix }\n---\n# body\n"
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
			"fix": {"pi:eff-a", "pi:eff-b"},
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

// recLog records driver events.
type recLog struct {
	mu    sync.Mutex
	kinds []string
}

func (l *recLog) Append(wi, kind string, body any) error {
	l.mu.Lock()
	l.kinds = append(l.kinds, kind)
	l.mu.Unlock()
	return nil
}

func (l *recLog) has(kind string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range l.kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// TestBridgeWiresDriverEventsAndSupervisionLink pins the v2 event base
// and the contract wiring: the driver's events reach the Log the daemon
// supplied (not a stderr logger) and the step_started fact reaches the
// WI's link, carrying the dispatch's model, harness and transcript.
func TestBridgeWiresDriverEventsAndSupervisionLink(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "workflows", "wf")
	if err := os.MkdirAll(filepath.Join(bundle, "steps"), 0o755); err != nil {
		t.Fatal(err)
	}
	front := "---\nname: wf\nsteps:\n  - { id: fix }\n---\n# body\n"
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
	log := &recLog{}
	link := supervise.NewLink("wi_wire")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Start(ctx, Config{
		WI: "wi_wire", CapsuleDir: dir, BundleName: "wf", SpawnN: 1, Spawner: sp,
		Log: log, Link: link, StepCandidates: map[string][]string{"fix": {"pi:m-a"}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Terminate(2 * time.Second)
	waitUntil(t, "the step_started event in the supplied log", func() bool { return log.has("step_started") })
	var started *supervise.StepStarted
	waitUntil(t, "the step_started fact on the link", func() bool {
		for _, f := range link.Facts.Drain() {
			if st, ok := f.(supervise.StepStarted); ok {
				c := st
				started = &c
			}
		}
		return started != nil
	})
	if started.Step != "fix" || started.Model != "m-a" || started.Harness != "pi" || started.SpawnN != 1 {
		t.Fatalf("step_started fact = %+v", *started)
	}
	if started.Thresholds.StepTimeout <= 0 || started.Thresholds.Tool <= 0 || started.Thresholds.Silence <= 0 {
		t.Fatalf("the fact must carry the thresholds, got %+v", started.Thresholds)
	}
}
