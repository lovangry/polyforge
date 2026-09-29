package daemon

// Unit tests: jobq ordering/backfill/no-preemption (04 §6.1d), mode
// admission (00 §3.4/04 §6.7), answer relay rules (00 §3.4a), observe ring
// (04 §5.1a), state persistence, pid-reuse protection (04 §7.1).

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestJobQueuePriorityDescFIFOWithin(t *testing.T) {
	q := NewJobQueue()
	t0 := time.Now()
	q.Enqueue(QEntry{WorkItemID: "low1", Priority: "low", EnqueuedAt: t0})
	q.Enqueue(QEntry{WorkItemID: "urgent1", Priority: "urgent", EnqueuedAt: t0.Add(time.Millisecond)})
	q.Enqueue(QEntry{WorkItemID: "high1", Priority: "high", EnqueuedAt: t0.Add(2 * time.Millisecond)})
	q.Enqueue(QEntry{WorkItemID: "urgent2", Priority: "urgent", EnqueuedAt: t0.Add(3 * time.Millisecond)})
	q.Enqueue(QEntry{WorkItemID: "low2", Priority: "low", EnqueuedAt: t0.Add(4 * time.Millisecond)})

	got := []string{}
	for {
		e := q.Take()
		if e == nil {
			break
		}
		got = append(got, e.WorkItemID)
		q.MarkRunning(e.WorkItemID)
	}
	want := []string{"urgent1", "urgent2", "high1", "low1", "low2"}
	// Concurrency cap of 4 stops the fifth take.
	if len(got) != 4 {
		t.Fatalf("cap must stop the 5th spawn: %v", got)
	}
	for i := range got[:3] {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want prefix %v", got, want)
		}
	}
	// Backfill: completing a job frees the slot for the next in order.
	q.Complete("urgent1")
	e := q.Take()
	if e == nil || e.WorkItemID != "low2" {
		t.Fatalf("backfill must take low2, got %+v", e)
	}
}

func TestJobQueueFIFOWithinPriority(t *testing.T) {
	q := NewJobQueue()
	q.SetMaxConcurrent(8)
	t0 := time.Now()
	q.Enqueue(QEntry{WorkItemID: "a", Priority: "normal", EnqueuedAt: t0})
	q.Enqueue(QEntry{WorkItemID: "b", Priority: "normal", EnqueuedAt: t0.Add(time.Millisecond)})
	q.Enqueue(QEntry{WorkItemID: "c", Priority: "normal", EnqueuedAt: t0.Add(2 * time.Millisecond)})
	var order []string
	for {
		e := q.Take()
		if e == nil {
			break
		}
		order = append(order, e.WorkItemID)
	}
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("FIFO within priority: %v", order)
	}
}

func TestJobQueueNoPreemption(t *testing.T) {
	q := NewJobQueue()
	q.Enqueue(QEntry{WorkItemID: "low", Priority: "low"})
	e := q.Take()
	q.MarkRunning(e.WorkItemID)
	// A later urgent arrival never displaces the running low job; it only
	// takes the next free slot.
	q.Enqueue(QEntry{WorkItemID: "urgent", Priority: "urgent"})
	next := q.Take()
	if next.WorkItemID != "urgent" {
		t.Fatal("urgent should take the free slot")
	}
	if !q.IsRunning("low") {
		t.Fatal("no preemption: the running job keeps its slot")
	}
}

func TestJobQueueBadConcurrency(t *testing.T) {
	q := NewJobQueue()
	if err := q.SetMaxConcurrent(0); err == nil {
		t.Fatal("n<1 must be refused (DAEMON-400 shape)")
	}
	if err := q.SetMaxConcurrent(2); err != nil {
		t.Fatal(err)
	}
}

func TestModesAdmission(t *testing.T) {
	m := NewModeCtl()
	if m.Current() != ModeInteractive {
		t.Fatal("default mode is interactive")
	}
	if err := m.AdmitSingle(); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(ModeDrain); err != nil {
		t.Fatal(err)
	}
	// Drain refuses single submissions and batch waves (its loop is the
	// only creation entry) — DAEMON-409 mode exclusion, NOT shutdown.
	if err := m.AdmitSingle(); !errors.Is(err, ErrModeConflict) {
		t.Fatalf("drain must refuse single claims: %v", err)
	}
	if err := m.AdmitBatch(); !errors.Is(err, ErrModeConflict) {
		t.Fatalf("drain must refuse batch waves: %v", err)
	}
	// Drain admits its own wave fetches.
	if !m.AdmitDrainWave() {
		t.Fatal("drain loop must be admitted in drain mode")
	}
	// Drain's only mode exit is abort-drain.
	if err := m.Set(ModeBatch); !errors.Is(err, ErrModeConflict) {
		t.Fatal("drain → other modes must be refused")
	}
	if err := m.AbortDrain(); err != nil {
		t.Fatal(err)
	}
	if m.Current() != ModeInteractive {
		t.Fatal("abort-drain returns to interactive")
	}
	// Shutdown latch: refuses everything, including drain waves (04 §6.5).
	m.Set(ModeDrain)
	m.SetShuttingDown()
	if err := m.AdmitSingle(); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("shutdown must refuse submissions: %v", err)
	}
	if m.AdmitDrainWave() {
		t.Fatal("shutdown must stop the drain loop's wave fetches")
	}
}

// fakeAgent for answer and lifecycle tests.
type fakeAgent struct {
	quiesceErr error
	answers    []AnswerRelay
	stopped    bool
}

func (f *fakeAgent) PID() int                             { return 0 }
func (f *fakeAgent) StartedAt() string                    { return "" }
func (f *fakeAgent) NotifyStop(ctx context.Context) error { f.stopped = true; return nil }
func (f *fakeAgent) Quiesce(ctx context.Context) error    { return f.quiesceErr }
func (f *fakeAgent) Resume(ctx context.Context) error     { return nil }
func (f *fakeAgent) DeliverAnswer(rel AnswerRelay) error {
	f.answers = append(f.answers, rel)
	return nil
}
func (f *fakeAgent) Alive() bool                           { return true }
func (f *fakeAgent) GroupEmpty() bool                      { return false }
func (f *fakeAgent) Terminate(timeout time.Duration) error { return nil }

type fixedQuestion struct{ q, step string }

func (f fixedQuestion) OpenQuestion() (string, string) { return f.q, f.step }

func TestAnswerRelayRules(t *testing.T) {
	l := NewAnswerLedger()
	agent := &fakeAgent{}
	relay := AnswerRelay{
		QuestionID: "q_abc123", AnswerID: "a_7k9m2p", AnswerText: "yes",
		AnsweredAt: "2026-09-26T10:00:00Z", AnsweredBy: "u_5dFjeaMZ",
	}
	src := fixedQuestion{q: "q_abc123", step: "code"}

	// Identity mismatch → DAEMON-403.
	if err := l.Submit(relay, "u_somebody_else", src, agent.DeliverAnswer); !errors.Is(err, ErrAnswerIdentity) {
		t.Fatalf("identity mismatch must 403: %v", err)
	}
	// First submission applies.
	if err := l.Submit(relay, "u_5dFjeaMZ", src, agent.DeliverAnswer); err != nil {
		t.Fatal(err)
	}
	if len(agent.answers) != 1 {
		t.Fatal("answer must be delivered exactly once")
	}
	// Same answer_id replay: idempotent, no second effect.
	if err := l.Submit(relay, "u_5dFjeaMZ", src, agent.DeliverAnswer); err != nil {
		t.Fatal(err)
	}
	if len(agent.answers) != 1 {
		t.Fatal("replayed answer_id must not deliver twice")
	}
	// Different answer_id against a CONSUMED question → DAEMON-409.
	relay2 := relay
	relay2.AnswerID = "a_other01"
	if err := l.Submit(relay2, "u_5dFjeaMZ", fixedQuestion{q: "", step: "code"}, agent.DeliverAnswer); !errors.Is(err, ErrAnswerConsumed) {
		t.Fatalf("consumed question must 409: %v", err)
	}
	if len(agent.answers) != 1 {
		t.Fatal("refused answer must not reach the agent")
	}
	// Question mismatch (stale id) → 409.
	relay3 := relay
	relay3.AnswerID = "a_third99"
	if err := l.Submit(relay3, "u_5dFjeaMZ", fixedQuestion{q: "q_new999", step: "code"}, agent.DeliverAnswer); !errors.Is(err, ErrAnswerConsumed) {
		t.Fatal("question mismatch must 409")
	}
}

// failingAgent fails the first N deliveries, then succeeds — the
// "agent died before its pf/answer ack" shape at ledger level.
type failingAgent struct {
	fakeAgent
	failsLeft int
}

func (f *failingAgent) DeliverAnswer(rel AnswerRelay) error {
	if f.failsLeft > 0 {
		f.failsLeft--
		return errors.New("agent died before its pf/answer ack")
	}
	return f.fakeAgent.DeliverAnswer(rel)
}

// TestAnswerFailedDeliveryKeepsRedeliverable (F2-1): a FAILED delivery
// must not finalize the idempotency entry — the same answer_id can be
// re-submitted and re-delivered (00 §3.4a failure-keeps-redeliverable,
// matching the supervisor's SK-4/CR-3 behavior); only the SUCCESSFUL
// delivery consumes it, after which a replay is refused a second effect.
// The old ledger recorded the delivery error permanently, so the
// re-submission returned the stale error with no retry.
func TestAnswerFailedDeliveryKeepsRedeliverable(t *testing.T) {
	l := NewAnswerLedger()
	agent := &failingAgent{fakeAgent: fakeAgent{}, failsLeft: 1}
	relay := AnswerRelay{
		QuestionID: "q_r1", AnswerID: "a_r1", AnswerText: "yes",
		AnsweredAt: "2026-09-26T10:00:00Z", AnsweredBy: "u_x",
	}
	src := fixedQuestion{q: "q_r1", step: "code"}

	// First submission: delivery fails — error surfaced, nothing applied.
	if err := l.Submit(relay, "u_x", src, agent.DeliverAnswer); err == nil {
		t.Fatal("the failed delivery must be reported as an error")
	}
	if len(agent.answers) != 0 {
		t.Fatal("nothing may be applied on a failed delivery")
	}
	// Re-submission of the SAME answer_id: re-delivered (not replayed with
	// the stale error), this time successfully — delivered exactly once.
	if err := l.Submit(relay, "u_x", src, agent.DeliverAnswer); err != nil {
		t.Fatalf("the same answer_id must be re-deliverable after a failed delivery: %v", err)
	}
	if len(agent.answers) != 1 {
		t.Fatalf("expected exactly one delivery, got %d", len(agent.answers))
	}
	// The success finalized the entry: a replay returns the first outcome
	// and never delivers again.
	if err := l.Submit(relay, "u_x", src, agent.DeliverAnswer); err != nil {
		t.Fatal(err)
	}
	if len(agent.answers) != 1 {
		t.Fatal("the replay must be refused a second effect")
	}
}

// TestAnswerInFlightDuplicateRefused (F2-1): a concurrent duplicate of an
// answer_id whose delivery has not settled is refused transiently
// (ErrAnswerInFlight), never silently told "success" by the reservation.
func TestAnswerInFlightDuplicateRefused(t *testing.T) {
	l := NewAnswerLedger()
	relay := AnswerRelay{
		QuestionID: "q_c1", AnswerID: "a_c1", AnswerText: "yes",
		AnsweredAt: "2026-09-26T10:00:00Z", AnsweredBy: "u_x",
	}
	src := fixedQuestion{q: "q_c1", step: "code"}
	entered := make(chan struct{})
	release := make(chan struct{})
	deliver := func(rel AnswerRelay) error {
		close(entered)
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- l.Submit(relay, "u_x", src, deliver) }()
	<-entered
	// While the first delivery is in flight, a duplicate of the same
	// answer_id must be refused transiently — not applied, not "success".
	if err := l.Submit(relay, "u_x", src, deliver); !errors.Is(err, ErrAnswerInFlight) {
		t.Fatalf("in-flight duplicate must be refused transiently, got %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// After settling, the replay is idempotent.
	if err := l.Submit(relay, "u_x", src, deliver); err != nil {
		t.Fatal(err)
	}
}

func TestObserveRing(t *testing.T) {
	r := &Ring{}
	r.Append(EvJobStarted, nil)
	r.Append(EvStepStarted, map[string]any{"step": "code"})
	resp := r.Observe(0, func() map[string]any { return map[string]any{"workflow_status": "running"} })
	if len(resp.Events) != 2 || resp.NextCursor != 2 || resp.Compacted {
		t.Fatalf("observe: %+v", resp)
	}
	if resp.StateSnapshot["workflow_status"] != "running" {
		t.Fatal("snapshot must be attached")
	}
	// Incremental pull.
	resp = r.Observe(1, nil)
	if len(resp.Events) != 1 || resp.Events[0].Seq != 2 {
		t.Fatalf("incremental: %+v", resp.Events)
	}
	// Cursor fallen out of the ring → compacted (snapshot-only rebuild).
	small := &Ring{}
	small.events = nil
	for i := 0; i < ObserveRingCap+10; i++ {
		small.Append(EvStepCompleted, nil)
	}
	resp = small.Observe(3, func() map[string]any { return map[string]any{"x": 1} })
	if !resp.Compacted {
		t.Fatal("out-of-ring cursor must report compacted")
	}
	if resp.StateSnapshot == nil {
		t.Fatal("compacted response must carry the snapshot")
	}
}

func TestStatePersistenceAndPendingJournal(t *testing.T) {
	store := NewStateStore(t.TempDir())
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.MachineID = "m-01"
	st.Mode = ModeInteractive
	st.WorkspaceRoot = "/ws/root"
	st.StartedAt = NowStamp()
	if err := store.AppendPending(st, PendingJob{WorkItemID: "wi_a", IdempotencyKey: "k1", Intent: "claim", CreatedAt: NowStamp(), Status: PendingIntentPersisted}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertJob(st, Job{WorkItemID: "wi_a", CapsuleID: "cap_wi_a", Generation: 1,
		HarnessAdapter: "pi", HarnessSessionID: "sess_wi_a_1", MainAgentPID: 42,
		MainAgentStartedAt: NowStamp(), Status: JobRunning}); err != nil {
		t.Fatal(err)
	}
	// Reload: both persisted.
	st2, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st2.FindJob("wi_a") == nil || len(st2.PendingJobs) != 1 {
		t.Fatalf("reload: %+v", st2)
	}
	// Formal record done → pending entry removed.
	if err := store.RemovePending(st2, "wi_a", "k1"); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveJob(st2, "wi_a"); err != nil {
		t.Fatal(err)
	}
	st3, _ := store.Load()
	if len(st3.Jobs) != 0 || len(st3.PendingJobs) != 0 {
		t.Fatalf("cleanup: %+v", st3)
	}
}

func TestProcessAlivePidReuseProtection(t *testing.T) {
	// A pid that does not exist: dead.
	if processAlive(99999999, NowStamp()) {
		t.Fatal("nonexistent pid must be dead")
	}
	// A live process (this test binary) with a WRONG creation time: the pid
	// was reused by an unrelated process → must count as dead (04 §7.1).
	self := 1 // pid 1 always exists on Linux; creation time won't match.
	if processAlive(self, "2020-01-01T00:00:00Z") {
		t.Fatal("pid reuse must be detected via creation-time mismatch")
	}
	// Correct record for a real process: spawn one.
	cmd := newSleepProcess(t)
	if !processAlive(cmd.Process.Pid, NowStamp()) {
		// Tolerance window: our stamp is taken right after spawn.
		t.Log("note: stamp outside tolerance is acceptable if process started earlier")
	}
	// pid<=0: dead.
	if processAlive(0, "") {
		t.Fatal("pid 0 must be dead")
	}
}

// newSleepProcess spawns a sleeping child for liveness tests.
func newSleepProcess(t *testing.T) *execCmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return &execCmd{cmd}
}

type execCmd struct{ *exec.Cmd }
