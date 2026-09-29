package runtime

// Recovery-engine regressions (v1-spec-full §七) driven with REAL child
// processes as the "agents": transcript present → resume; absent → fresh;
// two consecutive resume failures → fresh; storm guard / fresh failure →
// bounded fail (RTFinished(blocked)).

import (
	"context"
	"testing"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

func recoveryMsg(j *job, reason string) protocol.OBMsg {
	return protocol.OBMsg{
		Kind: protocol.OBNeedsRecovery, AttemptID: j.instance,
		WorkItem: j.wi, Reason: reason,
	}
}

func TestRecoverTranscriptPresentResumes(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_resume"
	j := seedJob(r, wi)
	writeTranscript(t, j)
	old := installOldAgent(t, sp, j)
	oldPID := old.PID()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	if n := sp.callCount(); n != 2 { // install + the resume derivation
		t.Fatalf("expected exactly the resume derivation, got %d calls", n)
	}
	if c := sp.call(1); !c.Resume || c.TranscriptPath != j.transcript {
		t.Fatalf("derivation must be a resume of the transcript, got %+v", c)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered || msg.WorkItem != wi {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	if msg.AttemptID != "ra_"+wi+"#1" {
		t.Fatalf("recovered agent must carry a fresh lineage id, got %q", msg.AttemptID)
	}
	if msg.AgentPID == oldPID || msg.AgentPID <= 0 {
		t.Fatalf("RTRecovered must carry the NEW handle: old=%d new=%d", oldPID, msg.AgentPID)
	}
	waitDead(t, oldPID)
	if st, _ := j.snapshot(); st != StateRunning {
		t.Fatalf("recovery must return to RUNNING, got %s", st)
	}
	j.mu.Lock()
	fails := j.resumeFails
	j.mu.Unlock()
	if fails != 0 {
		t.Fatalf("a successful resume must not count failures, got %d", fails)
	}
	// tidy: terminate the recovered sleeper
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

func TestRecoverTranscriptAbsentGoesFresh(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_fresh"
	j := seedJob(r, wi) // no transcript file
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	if n := sp.callCount(); n != 2 {
		t.Fatalf("expected one fresh derivation, got %d calls", n)
	}
	if c := sp.call(1); c.Resume {
		t.Fatalf("absent transcript must route to a fresh derivation, got %+v", c)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	if c := sp.call(1); len(c.InitialPrompt) == 0 || !contains(c.InitialPrompt, "fresh derivation") {
		t.Fatal("the fresh prompt must frame the derivation and point at old transcripts")
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

func TestRecoverGarbageTranscriptIsNotReadable(t *testing.T) {
	// the discriminator is READABILITY, not existence: a zero-byte or
	// non-JSON husk left by the crash routes to fresh
	if transcriptReadable("/nonexistent/nope.jsonl") {
		t.Fatal("missing transcript must not read as resumable")
	}
	dir := t.TempDir()
	p := dir + "/t.jsonl"
	if err := writeBytes(p, nil); err != nil {
		t.Fatal(err)
	}
	if transcriptReadable(p) {
		t.Fatal("empty transcript must not read as resumable")
	}
	if err := writeBytes(p, []byte("not json at all\n")); err != nil {
		t.Fatal(err)
	}
	if transcriptReadable(p) {
		t.Fatal("garbage transcript must not read as resumable")
	}
	if err := writeBytes(p, []byte("{\"type\":\"message_end\"}\n")); err != nil {
		t.Fatal(err)
	}
	if !transcriptReadable(p) {
		t.Fatal("a JSON record transcript must read as resumable")
	}
}

func TestRecoverResumeFailsTwiceThenFresh(t *testing.T) {
	sp := &scriptedSpawner{}
	sp.failResume = 2
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_2fail"
	j := seedJob(r, wi)
	writeTranscript(t, j)
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	// cascade: resume, resume, fresh — all within the one event
	if n := sp.callCount(); n != 4 {
		t.Fatalf("expected resume+resume+fresh, got %d calls", n)
	}
	if sp.call(1).Resume != true || sp.call(2).Resume != true {
		t.Fatal("the first two derivations must be resume attempts")
	}
	if sp.call(3).Resume {
		t.Fatal("after two consecutive resume failures the cascade must go fresh")
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered after the fresh fallback, got %+v", msg)
	}
	j.mu.Lock()
	fails, dead := j.resumeFails, j.resumeDead
	j.mu.Unlock()
	if fails != 2 || !dead {
		t.Fatalf("resume failure bookkeeping wrong: fails=%d dead=%v", fails, dead)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

func TestRecoverStormGuardBoundedFail(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_storm"
	j := seedJob(r, wi)
	j.mu.Lock()
	j.recoveries = 1 // one recovery already spent: this event is the second
	j.mu.Unlock()
	installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	if n := sp.callCount(); n != 1 { // only the install: NO relaunch
		t.Fatalf("storm guard must refuse a second relaunch, got %d derivations", n)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishBlocked {
		t.Fatalf("expected RTFinished(blocked), got %+v", msg)
	}
	if hub.endCount() != 1 || hub.lastEnd().Status != protocol.FinishBlocked {
		t.Fatalf("bounded fail must end the generation blocked, got %+v", hub.lastEnd())
	}
	if r.jobOf(wi) != nil {
		t.Fatal("bounded-failed job must leave the live set")
	}
	if st, _ := j.snapshot(); st != StateDone {
		t.Fatalf("bounded fail must reach DONE, got %s", st)
	}
}

func TestRecoverFreshFailureBoundedFail(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_freshfail"
	j := seedJob(r, wi) // no transcript → fresh; fresh is scripted to fail
	installOldAgent(t, sp, j)
	sp.mu.Lock()
	sp.failFresh = 1
	sp.mu.Unlock()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishBlocked {
		t.Fatalf("a failed fresh derivation must bounded-fail, got %+v", msg)
	}
	if hub.endCount() != 1 || hub.lastEnd().Status != protocol.FinishBlocked {
		t.Fatalf("bounded fail must end the generation blocked, got %+v", hub.lastEnd())
	}
	if r.jobOf(wi) != nil {
		t.Fatal("bounded-failed job must leave the live set")
	}
}

// TestRecoverDeadOldAgentWithOrphans drives the recovery against an old
// agent that DIED leaving orphaned children in its group: the six-layer
// cleanup must prove the tree empty before the re-derivation starts.
func TestRecoverDeadOldAgentWithOrphans(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_orphans"
	j := seedJob(r, wi)

	// an "agent" that forks a long-lived child and dies without reaping it
	old, err := spawnOrphaningSleeper(wi)
	if err != nil {
		t.Fatal(err)
	}
	orphan := old.orphanPID
	j.mu.Lock()
	j.agent = old.handle
	j.agentPID = old.handle.PID()
	j.agentStartedAt = old.handle.StartedAt()
	j.mu.Unlock()

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	waitDead(t, orphan) // the orphan died with the tree, not just the leader
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
