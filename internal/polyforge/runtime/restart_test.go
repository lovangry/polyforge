package runtime

// Restart reconciliation regressions, re-keyed onto the v1.5 session model
// (contract §1.2/§1.3): the three-way verdict of SessionVerify —
// matches+running (crash-mid-run → recover), matches+paused+!clean_pause
// (graceful shutdown → attach+resume), matches=false (superseded/ended →
// release or drop) — plus blocked (untouched, M2), terminal (drop), the
// interrupted-seal finish and the aihub-unreachable keep.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func matrixRecord(r *Runtime, wi string, pid int, startedAt string) *jobRecord {
	sess := fakeSession(wi)
	return &jobRecord{
		WorkItemID:         wi,
		Project:            "proj",
		SessionID:          sess,
		SessionFingerprint: api.SessionFingerprint(sess),
		State:              "running",
		CapsuleDir:         testCapsuleDir(r, wi),
		GitDir:             r.gitDirFor(wi),
		BundleName:         "demo-bundle",
		AgentPID:           pid,
		AgentStart:         startedAt,
		Transcript:         sessionPathFor(testCapsuleDir(r, wi), api.SessionFingerprint(sess)),
		SpawnN:             1,
	}
}

func TestRestartReconciliationMatrix(t *testing.T) {
	verdicts := []struct {
		status string
		name   string
	}{
		{api.StatusWrapped, "wrapped"},
		{api.StatusRunning, "running"},
		{api.StatusBlocked, "blocked"},
	}
	for _, v := range verdicts {
		for _, local := range []bool{true, false} {
			shape := "absent"
			if local {
				shape = "present"
			}
			t.Run(v.name+"-local-"+shape, func(t *testing.T) {
				hub := newFakeHub()
				sp := &scriptedSpawner{}
				r, toOB, _ := newTestRuntime(t, hub, sp)
				wi := "wi_mx_" + v.name + "_" + shape
				hub.status[wi] = v.status

				var pid int
				var startedAt string
				var sess string
				if local {
					pid, startedAt = spawnSleeper(t, wi)
					rec := matrixRecord(r, wi, pid, startedAt)
					sess = rec.SessionID
					writeStateFile(t, r, []*jobRecord{rec})
					// wrapped/blocked: the session was cleared server-side;
					// running: our session is still the holder.
					if v.status == api.StatusRunning {
						hub.hold(wi, sess, api.StatusRunning)
						// the restart-recovery lane proceeds only with the
						// capsule present at the active base (#103): this
						// shape models a crash-mid-run, capsule in place.
						if err := os.MkdirAll(rec.CapsuleDir, 0o755); err != nil {
							t.Fatal(err)
						}
					}
				}

				if err := r.Restart(context.Background()); err != nil {
					t.Fatalf("Restart: %v", err)
				}

				switch {
				case v.status == api.StatusWrapped:
					// 不复活: residue collected, record dropped, nothing revived
					if local {
						waitDead(t, pid)
						if recs := loadRecords(t, r); len(recs) != 0 {
							t.Fatalf("record must be dropped, got %+v", recs)
						}
					}
					if sp.callCount() != 0 {
						t.Fatal("wrapped must never revive")
					}
					assertNoRT(t, toOB)
				case v.status == api.StatusBlocked:
					// M2: nothing revived, record kept; crash orphans (and only
					// they) are reaped — the sleeper carries the WI marker
					if local {
						waitDead(t, pid)
						if recs := loadRecords(t, r); len(recs) != 1 || recs[0].WorkItemID != wi {
							t.Fatalf("blocked record must be kept, got %+v", recs)
						}
					}
					if sp.callCount() != 0 {
						t.Fatal("blocked must not spawn")
					}
					assertNoRT(t, toOB)
					if local {
						_ = killFn(-pid, 9) // tidy the sleeper
					}
				default: // running (verify matches)
					if local {
						// 杀旧 + Recover: old tree dead, one fresh derivation,
						// Observe hears RTRecovered, bookkeeping updated
						waitDead(t, pid)
						if n := sp.callCount(); n != 1 {
							t.Fatalf("expected exactly one recovery derivation, got %d", n)
						}
						if sp.call(0).Resume {
							t.Fatal("no transcript on record: the restart recovery must be fresh")
						}
						msg := recvRT(t, toOB)
						if msg.Kind != protocol.RTRecovered || msg.WorkItem != wi {
							t.Fatalf("expected RTRecovered, got %+v", msg)
						}
						if msg.SessionID != sess {
							t.Fatalf("RTRecovered must carry the session secret in-process, got %q", msg.SessionID)
						}
						recs := loadRecords(t, r)
						if len(recs) != 1 || recs[0].AgentPID == pid || recs[0].AgentPID <= 0 {
							t.Fatalf("record must carry the new agent pid, got %+v", recs)
						}
						if j := r.jobOf(wi); j == nil {
							t.Fatal("recovered job must be live")
						} else if h := func() AgentHandle {
							j.mu.Lock()
							defer j.mu.Unlock()
							return j.agent
						}(); h != nil {
							_ = h.Terminate(2e9)
						}
					} else {
						// no local record: nothing to reconcile
						if sp.callCount() != 0 {
							t.Fatal("absent local state must not spawn")
						}
						assertNoRT(t, toOB)
					}
				}
			})
		}
	}
}

// matches + paused + clean_pause=false → SessionAttach → resume from the
// capsule: the graceful-shutdown shape (the hub's paused-with-session is
// exactly "daemon shut down gracefully, will attach on restart", M3).
func TestRestartAttachResumesPausedSession(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_attach"
	pid, startedAt := spawnSleeper(t, wi)
	rec := matrixRecord(r, wi, pid, startedAt)
	hub.hold(wi, rec.SessionID, api.StatusPaused) // cleanPause stays false
	writeStateFile(t, r, []*jobRecord{rec})
	// the local capsule scene exists with a readable transcript: the
	// attach-resume continues it
	if err := os.MkdirAll(rec.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, jobFromRecord(rec))

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if n := len(hub.attaches); n != 1 {
		t.Fatalf("the paused record must attach exactly once, got %d", n)
	}
	if got := hub.attaches[0].SessionID; got != rec.SessionID {
		t.Fatalf("attach must present the record's session, got %q", got)
	}
	waitDead(t, pid) // the shutdown's leftover tree is collected
	if n := sp.callCount(); n != 1 {
		t.Fatalf("expected exactly one engine derivation, got %d", n)
	}
	// polyforge#106: the driver IS the engine — the attach resume derives
	// a fresh driver process (its continuation semantics are the capsule
	// state it reads, not a --session transcript argv). The transcript
	// the graceful shutdown synced into the capsule stays the context
	// record the derivation reads.
	if c := sp.call(0); c.Resume || c.TranscriptPath != "" {
		t.Fatalf("the attach derivation is a fresh driver process, got %+v", c)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered || msg.WorkItem != wi {
		t.Fatalf("expected RTRecovered after attach, got %+v", msg)
	}
	if hub.status[wi] != api.StatusRunning {
		t.Fatalf("attach must have flipped the hub status to running, got %q", hub.status[wi])
	}
	if j2 := r.jobOf(wi); j2 != nil && j2.agent != nil {
		_ = j2.agent.Terminate(2e9)
	}
}

// matches + paused + clean_pause=true (operator pause): NEVER auto-
// resumed — no attach, no spawn. Finding 2 (v1.5 review): the record is
// RETAINED (the old contract dropped it, which destroyed the session
// secret locally and made `pf resume`'s attach promise a lie); it sits
// in the Paused state with the clean_pause marker until the operator
// clears the hub flag, and the capsule stays.
func TestRestartCleanPauseNotAutoResumed(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_cleanpause"
	pid, startedAt := spawnSleeper(t, wi)
	rec := matrixRecord(r, wi, pid, startedAt)
	rec.State = string(StatePaused) // the pause close retains this shape
	rec.CleanPause = true
	hub.hold(wi, rec.SessionID, api.StatusPaused)
	hub.cleanPause[wi] = true
	if err := os.MkdirAll(rec.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateFile(t, r, []*jobRecord{rec})

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if n := len(hub.attaches); n != 0 {
		t.Fatalf("a clean pause must never attach, got %d", n)
	}
	if sp.callCount() != 0 {
		t.Fatal("a clean pause must never auto-resume")
	}
	assertNoRT(t, toOB)
	// Finding 2: the record is KEPT (the secret survives locally for the
	// later attach), its state reconciled to Paused.
	recs := loadRecords(t, r)
	if len(recs) != 1 || recs[0].WorkItemID != wi {
		t.Fatalf("the cleanly-paused record must be retained, got %+v", recs)
	}
	if recs[0].State != string(StatePaused) {
		t.Fatalf("the retained record must be in the paused state, got %q", recs[0].State)
	}
	if recs[0].SessionID != rec.SessionID {
		t.Fatalf("the retained record must carry the session secret, got %q", recs[0].SessionID)
	}
	if _, err := os.Stat(testCapsuleDir(r, wi)); err != nil {
		t.Fatalf("the capsule scene must stay on disk: %v", err)
	}
	_ = killFn(-pid, 9) // tidy
}

// TestRestartAttachesRetainedPausedRecord (Finding 2, v1.5 review): the
// retained Paused record of an operator pause, once the operator cleared
// clean_pause on the hub (`pf resume`), attaches on the holder daemon's
// next start: SessionAttach flips the hub to running and the recovery
// engine resumes the local capsule — the exact flow the CLI's resume
// help promises.
func TestRestartAttachesRetainedPausedRecord(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_attachretained"
	rec := matrixRecord(r, wi, 0, "") // the pause close already killed the tree
	rec.State = string(StatePaused)
	rec.CleanPause = true
	hub.hold(wi, rec.SessionID, api.StatusPaused) // clean_pause cleared by `pf resume`
	writeStateFile(t, r, []*jobRecord{rec})
	if err := os.MkdirAll(rec.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, jobFromRecord(rec))

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if n := len(hub.attaches); n != 1 {
		t.Fatalf("the retained paused record must attach exactly once, got %d", n)
	}
	if got := hub.attaches[0].SessionID; got != rec.SessionID {
		t.Fatalf("attach must present the record's session, got %q", got)
	}
	// the attach must derive exactly one engine derivation (polyforge#106:
	// a fresh driver process — its continuation semantics are the capsule
	// state, not a --session transcript argv)
	if n := sp.callCount(); n != 1 || sp.call(0).Resume {
		t.Fatalf("the attach must derive exactly one fresh engine derivation, got %d calls resume=%v", sp.callCount(), sp.call(0).Resume)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered || msg.WorkItem != wi {
		t.Fatalf("expected RTRecovered after the retained-pause attach, got %+v", msg)
	}
	if hub.status[wi] != api.StatusRunning {
		t.Fatalf("attach must have flipped the hub status to running, got %q", hub.status[wi])
	}
	if j2 := r.jobOf(wi); j2 == nil {
		t.Fatal("the resumed job must be live")
	} else if h := func() AgentHandle {
		j2.mu.Lock()
		defer j2.mu.Unlock()
		return j2.agent
	}(); h != nil {
		_ = h.Terminate(2e9)
	}
}

// matches=false + the WI held by ANOTHER session → release: kill the
// local tree, quarantine the capsule in place, drop the record — and NO
// hub transition (superseded, not terminal).
func TestRestartReleasesForeignHolder(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_foreign"
	pid, startedAt := spawnSleeper(t, wi)
	rec := matrixRecord(r, wi, pid, startedAt)
	// a force-end + re-claim happened while we were down: another session holds it
	other := fakeSession(wi + "-other")
	hub.hold(wi, other, api.StatusRunning)
	// the local capsule scene exists
	capDir := testCapsuleDir(r, wi)
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateFile(t, r, []*jobRecord{rec})

	var tapped []string
	r.cfg.EventTap = func(w, kind string, body map[string]any) {
		tapped = append(tapped, kind)
	}

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	waitDead(t, pid) // the zombie tree is collected
	// the record is gone
	if recs := loadRecords(t, r); len(recs) != 0 {
		t.Fatalf("the released record must be dropped, got %+v", recs)
	}
	// NO hub transition: no end, no attach, no claim
	if hub.endCount() != 0 || len(hub.attaches) != 0 || hub.claimCount() != 0 {
		t.Fatalf("release must make no hub transition (ends=%d attaches=%d claims=%d)",
			hub.endCount(), len(hub.attaches), hub.claimCount())
	}
	// the capsule is quarantined under the contract's sibling naming
	ents, err := os.ReadDir(filepath.Join(testWorkspace(r), "capsules"))
	if err != nil {
		t.Fatal(err)
	}
	var quarantined bool
	for _, e := range ents {
		if e.Name() == wi {
			t.Fatal("the capsule must not remain at its live path")
		}
		if strings.HasPrefix(e.Name(), wi+".quarantined.") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Fatalf("no %s.quarantined.<ts> sibling found: %v", wi, ents)
	}
	if len(tapped) != 1 || tapped[0] != "session_released" {
		t.Fatalf("the release must leave a durable event, got %v", tapped)
	}
	if sp.callCount() != 0 {
		t.Fatal("a released WI must never be revived locally")
	}
	assertNoRT(t, toOB)
}

func TestRestartFinishesInterruptedSeal(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_sealrestart"

	// a job the previous daemon left mid-seal after the push landed but
	// before session-end
	j := seedJob(r, wi)
	initGitCapsule(t, j)
	pid, startedAt := spawnSleeper(t, wi)
	rec := matrixRecord(r, wi, pid, startedAt)
	rec.State = string(StateSealing)
	rec.StopReason = protocol.FinishWrapped
	rec.CapsuleDir = j.capsuleDir
	rec.GitDir = j.git.GitDir
	rec.RemoteURL = j.git.RemoteURL
	rec.Transcript = j.transcript
	hub.hold(wi, rec.SessionID, api.StatusRunning)
	writeStateFile(t, r, []*jobRecord{rec})
	// drop the in-memory seed so Restart owns the job
	r.dropJob(j)

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	waitDead(t, pid)
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishWrapped {
		t.Fatalf("interrupted seal must finish wrapped, got %+v", msg)
	}
	if hub.endCount() != 1 || hub.lastEnd().Status != protocol.FinishWrapped {
		t.Fatalf("session-end must complete, got %+v", hub.lastEnd())
	}
	if recs := loadRecords(t, r); len(recs) != 0 {
		t.Fatalf("finished seal must drop the record, got %+v", recs)
	}
	if sp.callCount() != 0 {
		t.Fatal("finishing a seal must not revive the agent")
	}
	unseal(t, testWorkspace(r))
}

// TestRestartVerifyOutageParksBlocked (the C6 posture): a failed
// restart-time session verify — aihub unreachable — parks the WI in the
// ordinary blocked state. No local verify-pending state, no background
// retry: the agent tree is killed, the blocked verdict (its reason naming
// the unreachable-aihub restart and the incomplete session verify) is
// attempted at the hub, and with the hub still down the record is kept
// end_pending for the durable retry. Once aihub answers, the verdict
// lands and the owner's `pf resume` takes the ordinary blocked → resume
// path.
func TestRestartVerifyOutageParksBlocked(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_verify_outage"
	hub.lookupErr[wi] = errLookupBoom
	pid, startedAt := spawnSleeper(t, wi)
	rec := matrixRecord(r, wi, pid, startedAt)
	writeStateFile(t, r, []*jobRecord{rec})

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart must survive an unreachable aihub per record: %v", err)
	}
	// the parked WI holds no agent: the tree is killed, nothing spawns
	waitDead(t, pid)
	if sp.callCount() != 0 {
		t.Fatal("a verify outage must not spawn")
	}
	// the blocked verdict is pending at the (unreachable) hub: the record
	// is kept end_pending, carrying the outage reason verbatim
	recs := loadRecords(t, r)
	if len(recs) != 1 || recs[0].State != string(StateEndPending) {
		t.Fatalf("the verify-outage record must be kept end_pending, got %+v", recs)
	}
	if !strings.Contains(recs[0].EndNote, "unreachable") || !strings.Contains(recs[0].EndNote, "verify") {
		t.Fatalf("the parked reason must name the unreachable-aihub restart and the incomplete verify: %q", recs[0].EndNote)
	}
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTFinished || msg.Reason != protocol.FinishBlocked {
		t.Fatalf("expected RTFinished(blocked), got %+v", msg)
	}

	// aihub answers: the durable retry lands the blocked verdict, and the
	// job leaves the live set (the owner's `pf resume` takes it from here)
	hub.mu.Lock()
	delete(hub.lookupErr, wi)
	hub.mu.Unlock()
	waitFor(t, "the end-pending blocked close to settle once aihub answers", func() bool {
		_, ok := r.Info(wi)
		return !ok
	})
	if hub.endCount() != 1 || hub.lastEnd().Status != protocol.FinishBlocked {
		t.Fatalf("the blocked close must land once the hub answers, got %+v", hub.lastEnd())
	}
	if !strings.Contains(hub.lastEnd().Note, "unreachable") {
		t.Fatalf("the hub-side blocked reason must name the outage: %q", hub.lastEnd().Note)
	}
}

// A blocked verdict reaps the crash orphans named only by the spawn
// journal (record pid 0), while keeping the record and reviving nothing.
func TestRestartBlockedReapsJournaledOrphansOnly(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_blk_reap"
	hub.status[wi] = api.StatusBlocked
	pid, startedAt := startTree(t, wi)
	bystander, _ := spawnSleeper(t, "wi_other") // different WI: must survive
	defer func() { _ = killFn(-bystander, 9) }()
	rec := matrixRecord(r, wi, 0, "")
	if err := os.MkdirAll(rec.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJournalPID(t, rec.CapsuleDir, pid, startedAt)
	writeStateFile(t, r, []*jobRecord{rec})

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	waitDead(t, pid)
	mustBeAlive(t, bystander)
	if recs := loadRecords(t, r); len(recs) != 1 || recs[0].WorkItemID != wi {
		t.Fatalf("blocked record must be kept, got %+v", recs)
	}
	if sp.callCount() != 0 {
		t.Fatal("blocked must not spawn")
	}
	assertNoRT(t, toOB)
}
