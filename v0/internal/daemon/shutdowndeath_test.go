package daemon

// shutdowndeath_test.go: the polyforge#32 regression set — the graceful
// shutdown's quiesce wait must OBSERVE a dead WI Agent (04 §6.5 step 3/4
// vs §7.1). The pilot measured: `pf ops shutdown` with one running job,
// the stuck agent then KILLED, and 20+ minutes later the daemon still in
// `(shutting down)` with the job at running — the quiesce failure had
// escalated into DAEMON-511 instead of converging the job through the
// daemon-exit shape §6.5 already defines (final flush on the agent's
// behalf over a PROVEN-empty writer domain, generation ended pause,
// capsule sealed), and the blocked process main swallowed further
// SIGTERMs.
//
// Matrix (agent state × shutdown phase → outcome), pinned here:
//
//   - dead BEFORE the close (a):        notices skipped, freeze via the
//     group-dead shortcut, close converges, shutdown nil.
//   - dies DURING the notice wait (a2): the exit selection short-circuits
//     the wait (no parking on a control path that will never answer),
//     same convergence as (a) — the pilot's exact scenario.
//   - alive and ACKING (b):             unchanged behavior: the control
//     calls are delivered and answered, close converges, shutdown nil.
//   - alive, NEVER acks (d):            the close escalates through its
//     EXISTING deadline (ErrQuiesceTimeout → DAEMON-511) — bounded, no
//     new hang, and no agent-death shortcut is granted to a live agent.
//   - dead, NO shutdown (c):            §7.1 crash continuation at the
//     daemon restart is untouched (re-derivation with the same session
//     id) — the exit-face wiring added an observer, nothing else.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// controlAgent is an integAgent-style fake with two personalities:
//
//   - WELL-BEHAVED (the default): control calls ack immediately — the
//     agent of the claim sequence and test (b);
//   - STUCK (armed by the test): control calls park for ackDelay (the
//     real awaitAck's 30s stand-in) and then answer "no ack". The parked
//     window is exactly where the pilot's agent died, and it does NOT
//     return early when the agent is killed: noticing the exit is the
//     SHUTDOWN PATH's job (that is the whole fix), not the fake's.
//
// It exposes the reaper's exit face (Wait) so the daemon can select on
// agent death.
type controlAgent struct {
	pid       int
	startedAt string
	child     *exec.Cmd
	ackDelay  time.Duration

	mu       sync.Mutex
	dead     bool
	notifies int
	quiesces int
	exit     chan struct{} // closed on kill: the reaper's exit signal
	entered  chan struct{} // closed when the first STUCK control call parks
	stopOnce sync.Once
	killOnce sync.Once

	// stuck is armed by the test between the working publish and the
	// shutdown; written only before the Shutdown goroutine starts.
	stuck bool
}

func (f *controlAgent) PID() int          { return f.pid }
func (f *controlAgent) StartedAt() string { return f.startedAt }

func (f *controlAgent) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.dead
}

// Wait implements AgentExitWaiter: the reaper's exit face the shutdown
// path selects on (controlWatch / agentGone).
func (f *controlAgent) Wait() error {
	<-f.exit
	return nil
}

// GroupEmpty models a clean exit with no residual group: empty exactly
// when the agent is dead.
func (f *controlAgent) GroupEmpty() bool { return !f.Alive() }

func (f *controlAgent) NotifyStop(ctx context.Context) error {
	f.mu.Lock()
	f.notifies++
	f.mu.Unlock()
	if !f.stuck {
		return nil
	}
	f.enter()
	time.Sleep(f.ackDelay) // the parked ack window (dead or not: the fix must not rely on this ending)
	return fmt.Errorf("mainagent: no ack for pf/stop_notice")
}

func (f *controlAgent) Quiesce(ctx context.Context) error {
	f.mu.Lock()
	f.quiesces++
	f.mu.Unlock()
	if !f.stuck {
		return nil
	}
	f.enter()
	time.Sleep(f.ackDelay)
	return fmt.Errorf("mainagent: no ack for pf/quiesce")
}

func (f *controlAgent) Resume(ctx context.Context) error { return nil }
func (f *controlAgent) DeliverAnswer(AnswerRelay) error  { return nil }
func (f *controlAgent) Terminate(timeout time.Duration) error {
	f.kill()
	return nil
}

func (f *controlAgent) enter() {
	f.stopOnce.Do(func() { close(f.entered) })
}

// kill simulates the pilot's `kill` of the stuck agent process: the real
// child dies (a REAL pid for the §7.1 restart path) and the reaper's
// exit signal fires.
func (f *controlAgent) kill() {
	f.killOnce.Do(func() {
		f.mu.Lock()
		f.dead = true
		f.mu.Unlock()
		if f.child != nil && f.child.Process != nil {
			_ = f.child.Process.Kill()
			_, _ = f.child.Process.Wait()
		}
		close(f.exit)
	})
}

func (f *controlAgent) counts() (notifies, quiesces int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifies, f.quiesces
}

type controlSpawner struct {
	mu     sync.Mutex
	spawns []SpawnParams
	agents []*controlAgent
}

func (s *controlSpawner) Spawn(ctx context.Context, p SpawnParams) (AgentHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spawns = append(s.spawns, p)
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		return nil, err
	}
	a := &controlAgent{
		pid: child.Process.Pid, startedAt: NowStamp(), child: child,
		entered: make(chan struct{}), exit: make(chan struct{}),
	}
	s.agents = append(s.agents, a)
	return a, nil
}

// spawnControlledJob drives the 04 §6.1 claim sequence for e.wi and makes
// the agent "work" one step (the spawnWorkingJob shape) so the later
// final flush has a real HEAD to compare against.
func spawnControlledJob(t *testing.T, e *denv, spawner *controlSpawner) *controlAgent {
	t.Helper()
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	agent := spawner.agents[0]
	capc, _, err := capsule.Open(filepath.Join(e.dataDir, "home", "ws"), e.wi)
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	if err := capc.SaveExecution(&capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: e.wi, Generation: 1,
		CurrentStep: "code", WorkflowStatus: capsule.StatusRunning,
		Steps: map[string]*capsule.Step{"code": {Status: capsule.StepInProgress}}, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.PublishRequest(ctx, e.wi, "step_completed"); err != nil {
		t.Fatalf("working publish: %v", err)
	}
	return agent
}

// assertShutdownConverged is the shared convergence verdict of the death
// and acking tests: the shutdown returns nil (NOT the DAEMON-511 park),
// the WI is paused server-side, the job record is gone, and the capsule
// is sealed read-only — the 04 §6.5 daemon-exit shape.
func assertShutdownConverged(t *testing.T, e *denv, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("shutdown must converge on the daemon-exit shape, got: %v", err)
	}
	if s := e.wiStatus(t); s != "paused" {
		t.Fatalf("wi status after shutdown: %s", s)
	}
	if jobs := e.daemonStateJobs(t); len(jobs) != 0 {
		t.Fatalf("jobs[] must be empty after the converged close: %+v", jobs)
	}
	capDir := filepath.Join(e.dataDir, "home", "ws", "capsules", e.wi)
	fi, serr := os.Stat(capDir)
	if serr != nil {
		t.Fatalf("sealed capsule must remain on disk: %v", serr)
	}
	if fi.Mode().Perm()&0o222 != 0 {
		t.Fatal("capsule must be sealed read-only after the close")
	}
}

// (a) Shutdown requested, the agent ALREADY dead: the close skips the
// pointless notices, takes the 05 §2.7 group-dead shortcut for the
// freeze, and the whole sequence converges inside a bounded window —
// never the 20-minute park.
func TestShutdownDeadAgentConverges(t *testing.T) {
	e := newDaemonEnv(t)
	spawner := &controlSpawner{}
	e.d.spawner = spawner
	agent := spawnControlledJob(t, e, spawner)
	agent.kill() // the operator kills the stuck agent BEFORE the shutdown
	n0, q0 := agent.counts()

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- e.d.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		assertShutdownConverged(t, e, err)
	case <-time.After(15 * time.Second):
		t.Fatal("shutdown with an already-dead agent must converge in a bounded window")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("dead-agent shutdown took %s; the bounded windows were exceeded", elapsed)
	}
	if n, q := agent.counts(); n != n0 || q != q0 {
		t.Fatalf("no stop-notice or quiesce may be sent to a dead agent: notices %d→%d, quiesces %d→%d", n0, n, q0, q)
	}
}

// (a2) THE PILOT SCENARIO: shutdown requested, the agent alive but stuck
// (control calls park), then KILLED mid-notice-wait. The exit selection
// must short-circuit the parked wait (the ack window is 15s — the
// shutdown may not wait it out), and the close must converge through the
// same daemon-exit shape instead of escalating into DAEMON-511.
func TestShutdownAgentDiesMidWaitShortCircuits(t *testing.T) {
	e := newDaemonEnv(t)
	spawner := &controlSpawner{}
	e.d.spawner = spawner
	agent := spawnControlledJob(t, e, spawner)
	// Arm the stuck personality AFTER the working publish: the claim-time
	// publishes keep their healthy acks, the shutdown's notice parks.
	agent.stuck = true
	agent.ackDelay = 15 * time.Second

	done := make(chan error, 1)
	go func() { done <- e.d.Shutdown(context.Background()) }()

	select {
	case <-agent.entered: // the close is now parked in the notice ack wait
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown never reached the agent notice wait")
	}
	agent.kill() // the pilot's kill, INSIDE the window

	select {
	case err := <-done:
		assertShutdownConverged(t, e, err)
	case <-time.After(10 * time.Second):
		t.Fatal("agent death inside the shutdown window must short-circuit " +
			"the parked quiesce wait and converge the shutdown (polyforge#32 pilot)")
	}
}

// (b) Shutdown with a healthy, acking agent: the unchanged baseline. The
// control calls are delivered and answered, the close converges through
// the normal quiesce-confirmation path, no re-derivation happens.
func TestShutdownAckingAgentUnchanged(t *testing.T) {
	e := newDaemonEnv(t)
	spawner := &controlSpawner{}
	e.d.spawner = spawner
	agent := spawnControlledJob(t, e, spawner)

	if err := e.d.Shutdown(context.Background()); err != nil {
		t.Fatalf("healthy-agent shutdown must converge as before: %v", err)
	}
	if n, _ := agent.counts(); n == 0 {
		t.Fatal("the acking agent must have received the stop notice")
	}
	if s := e.wiStatus(t); s != "paused" {
		t.Fatalf("wi status after shutdown: %s", s)
	}
	if jobs := e.daemonStateJobs(t); len(jobs) != 0 {
		t.Fatalf("jobs[] must be empty after the close: %+v", jobs)
	}
	if n := len(spawner.spawns); n != 1 {
		t.Fatalf("no re-derivation may happen during the shutdown: %d spawns", n)
	}
}

// (d) An agent that stays ALIVE but never acks: no dead-writer shortcut is
// granted to a live agent; the close escalates through its EXISTING
// deadline (the quiesce window) into the designed DAEMON-511 verdict —
// bounded, and never a new hang. The windows are tightened via
// Config.QuiesceTimeout so the test observes the deadline, not a 30s
// wall-clock wait.
func TestShutdownLiveUnresponsiveAgentConvergesViaDeadline(t *testing.T) {
	e := newDaemonEnv(t)
	e.d.cfg.QuiesceTimeout = 250 * time.Millisecond
	spawner := &controlSpawner{}
	e.d.spawner = spawner
	agent := spawnControlledJob(t, e, spawner)
	agent.stuck = true
	agent.ackDelay = 400 * time.Millisecond // alive, stuck, will never ack

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- e.d.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrFlushFailed) {
			t.Fatalf("a live never-acking agent must escalate to the DAEMON-511 verdict, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the never-acking LIVE agent must not hang the shutdown: " +
			"it converges through its existing quiesce deadline")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("the deadline convergence took %s; the existing windows were exceeded", elapsed)
	}
	if !agent.Alive() {
		t.Fatal("the unresponsive agent must not have been killed: the deadline path does not collect a live agent")
	}
	// The job stays recorded (interrupting) — the DAEMON-511 shape keeps the
	// scene for the human the design demands.
	if jobs := e.daemonStateJobs(t); len(jobs) != 1 || jobs[0].Status != JobInterrupting {
		t.Fatalf("a DAEMON-511 close must leave the job recorded interrupting: %+v", jobs)
	}
}

// (c) NO shutdown, agent dies: §7.1 crash continuation at the daemon
// restart still happens — the exit-face wiring only ADDED an observer
// for the shutdown path; the restart path keeps consuming procfs
// liveness and re-derives the dead agent with the SAME session id.
func TestShutdownDeathNoShutdownStillCrashContinues(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("procfs not available: dead-agent recovery proof is Linux-only")
	}
	e := newDaemonEnv(t)
	_, agent := spawnWorkingJob(t, e) // the integAgent with a REAL child pid

	// The agent dies with NO shutdown in flight (a real kill: the recorded
	// pid is really gone).
	agent.dead = true
	if agent.child != nil && agent.child.Process != nil {
		agent.child.Process.Kill()
		agent.child.Wait()
	}

	// A daemon "restart" over the same data dir: crash recovery must
	// re-derive the dead agent (04 §7.1 step 4), reusing the session id.
	home := filepath.Join(e.dataDir, "home")
	cfg := Config{
		AIHubURL: e.ts.url, APIKey: "pfk_daemon_it", MachineID: "mac_a",
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		DrainProject:  "p1", Adapter: "pi", Version: "it",
	}
	spawner2 := newIntegSpawner()
	d2, err := New(cfg, spawner2)
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RecoverFromState(context.Background()); err != nil {
		t.Fatalf("crash recovery: %v", err)
	}
	if n := len(spawner2.spawns); n != 1 {
		t.Fatalf("the dead agent must be re-derived exactly once (§7.1 crash continuation), got %d", n)
	}
	if spawner2.spawns[0].SessionID == "" ||
		spawner2.spawns[0].SessionID != e.spawner.spawns[0].SessionID {
		t.Fatalf("the re-derivation must reuse the recorded session id: %q vs %q",
			spawner2.spawns[0].SessionID, e.spawner.spawns[0].SessionID)
	}
	if !strings.Contains(spawner2.spawns[0].InitialPrompt, "re-derivation") {
		t.Fatal("the re-derivation must carry the crash-continuation prompt")
	}
}
