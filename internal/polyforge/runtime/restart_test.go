package runtime

// Restart reconciliation regressions (v1-spec-full §十一): the matrix of
// aihub's three verdicts (wrapped / running / blocked) × local state's two
// shapes (record present / absent), plus the interrupted-seal finish and
// the aihub-unreachable keep.

import (
	"context"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func matrixRecord(r *Runtime, wi string, pid int, startedAt string) *jobRecord {
	return &jobRecord{
		WorkItemID: wi,
		Project:    "proj",
		AttemptID:  "ra_" + wi,
		InstanceID: "ra_" + wi,
		Generation: 1,
		State:      "running",
		CapsuleDir: r.capsuleDir(wi),
		GitDir:     r.gitDirFor(wi),
		SessionID:  "sess_" + wi + "_1",
		BundleName: "demo-bundle",
		AgentPID:   pid,
		AgentStart: startedAt,
		SpawnN:     1,
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
				if local {
					pid, startedAt = spawnSleeper(t, wi)
					writeStateFile(t, r, []*jobRecord{matrixRecord(r, wi, pid, startedAt)})
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
					// 不动: nothing killed, nothing revived, record kept
					if local {
						mustBeAlive(t, pid)
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
				default: // running
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

func TestRestartFinishesInterruptedSeal(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_sealrestart"
	hub.status[wi] = api.StatusRunning

	// a job the previous daemon left mid-seal after the push landed but
	// before end-generation
	j := seedJob(r, wi)
	initGitCapsule(t, j)
	pid, startedAt := spawnSleeper(t, wi)
	rec := matrixRecord(r, wi, pid, startedAt)
	rec.State = string(StateSealing)
	rec.StopReason = protocol.FinishWrapped
	rec.RemoteURL = j.git.RemoteURL
	rec.Username = j.git.Username
	rec.Secret = j.git.Secret
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
		t.Fatalf("end-generation must complete, got %+v", hub.lastEnd())
	}
	if recs := loadRecords(t, r); len(recs) != 0 {
		t.Fatalf("finished seal must drop the record, got %+v", recs)
	}
	if sp.callCount() != 0 {
		t.Fatal("finishing a seal must not revive the agent")
	}
	unseal(t, r.cfg.WorkspaceRoot)
}

func TestRestartKeepsRecordWhenAihubUnreachable(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	wi := "wi_unreach"
	hub.lookupErr[wi] = errLookupBoom
	pid, startedAt := spawnSleeper(t, wi)
	writeStateFile(t, r, []*jobRecord{matrixRecord(r, wi, pid, startedAt)})

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart must survive an unreachable aihub per record: %v", err)
	}
	mustBeAlive(t, pid) // no verdict ⇒ no action
	if recs := loadRecords(t, r); len(recs) != 1 {
		t.Fatalf("unreachable record must be kept, got %+v", recs)
	}
	if sp.callCount() != 0 {
		t.Fatal("no verdict ⇒ no spawn")
	}
	_ = killFn(-pid, 9)
}
