package runtime

// Start/Stop end-to-end: claim → clone+external-git-dir → materialize →
// spawn → RTStarted, then seal → final push → end-generation → RTFinished,
// against a real capsule git repo and a real (scripted) child process.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

func TestStartStopEndToEnd(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)

	// aihub's bare repo for this WI lives under a local dir standing in
	// for the git proxy base
	base := t.TempDir()
	r.cfg.GitRemoteBase = base
	wi := "wi_e2e"
	bare := filepath.Join(base, wi+".git")
	if out, err := run(t, "git", "init", "--bare", bare); err != nil {
		t.Fatalf("bare init: %s", out)
	}
	// every push carries --push-option=attempt-id=… (§六 备层): the
	// receiving end must advertise push options
	if out, err := run(t, "git", "--git-dir", bare, "config", "receive.advertisePushOptions", "true"); err != nil {
		t.Fatalf("bare advertisePushOptions: %s", out)
	}

	ctx := context.Background()
	if err := r.Start(ctx, wi); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// RTStarted carries the agent handle + attempt-id
	started := recvRT(t, toOB)
	if started.Kind != protocol.RTStarted || started.WorkItem != wi {
		t.Fatalf("expected RTStarted for %s, got %+v", wi, started)
	}
	if started.AttemptID != "ra_"+wi || started.AgentPID <= 0 {
		t.Fatalf("RTStarted missing handle/attempt: %+v", started)
	}

	capDir := r.capsuleDir(wi)
	// capsule layout materialized
	for _, rel := range []string{
		capsule.CapsuleJSON, capsule.SessionJSON, ".gitignore",
		capsule.DirAgents, capsule.DirWorkflows, capsule.DirRuntimeState, capsule.DirSession,
	} {
		if _, err := os.Stat(filepath.Join(capDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("materialized %s missing: %v", rel, err)
		}
	}
	// capsule.json / session.json parse and carry the attempt identity
	raw, err := os.ReadFile(filepath.Join(capDir, capsule.CapsuleJSON))
	if err != nil {
		t.Fatal(err)
	}
	var idn capsule.Identity
	if err := json.Unmarshal(raw, &idn); err != nil || idn.WorkItemID != wi || idn.Generation == 0 {
		t.Fatalf("capsule.json wrong: %v %+v", err, idn)
	}
	// git-dir is EXTERNAL: .git is a one-line pointer, not a directory
	pointer := filepath.Join(capDir, ".git")
	if !isRegularFile(pointer) {
		t.Fatalf("capsule .git must be a pointer file (external git-dir ruling)")
	}
	if !dirExists(r.gitDirFor(wi)) {
		t.Fatalf("external git-dir missing at %s", r.gitDirFor(wi))
	}
	// state.json recorded the job
	if recs := loadRecords(t, r); len(recs) != 1 || recs[0].WorkItemID != wi {
		t.Fatalf("state.json should hold one record, got %+v", recs)
	}

	// Stop: seal → push → end-generation → RTFinished(wrapped)
	if err := os.WriteFile(filepath.Join(capDir, "final.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(ctx, wi, protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	fin := recvRT(t, toOB)
	if fin.Kind != protocol.RTFinished || fin.Reason != protocol.FinishWrapped {
		t.Fatalf("expected RTFinished(wrapped), got %+v", fin)
	}
	if hub.endCount() != 1 || hub.lastEnd().Status != protocol.FinishWrapped {
		t.Fatalf("end-generation not called with wrapped: %+v", hub.lastEnd())
	}
	if !bareHasMain(t, bare) {
		t.Fatal("final push never landed on the bare repo")
	}
	// job is gone everywhere
	if r.jobOf(wi) != nil {
		t.Fatal("job still in the live map after Stop")
	}
	if recs := loadRecords(t, r); len(recs) != 0 {
		t.Fatalf("state.json should be empty after Stop, got %+v", recs)
	}
	// the sealed scene is read-only: dirs 0555, files 0444 (an
	// anti-mistake guard — chmod does not bind a same-UID/root writer,
	// so the assertion is on the mode bits, not on a failed write)
	if st, err := os.Stat(capDir); err != nil || st.Mode().Perm() != 0o555 {
		t.Fatalf("capsule root must be sealed 0555, got %v (%v)", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(filepath.Join(capDir, "final.txt")); err != nil || st.Mode().Perm() != 0o444 {
		t.Fatalf("sealed file must be 0444, got %v (%v)", st.Mode().Perm(), err)
	}
	// the stopped agent (a real child) was torn down
	pid := started.AgentPID
	waitDead(t, pid)

	unseal(t, capDir)
	unseal(t, r.cfg.WorkspaceRoot)
}

func TestStopUnknownWorkItem(t *testing.T) {
	r, _, _ := newTestRuntime(t, newFakeHub(), &scriptedSpawner{})
	if err := r.Stop(context.Background(), "wi_none", protocol.FinishWrapped); err == nil {
		t.Fatal("Stop of an unknown work item must fail")
	}
}

// TestStopEndGenerationFailureLeavesSealing pins the §五 wrap semantics: a
// failed end-generation must NOT silently half-seal — the job stays
// SEALING (persisted with its reason) for the restart path to finish.
func TestStopEndGenerationFailureLeavesSealing(t *testing.T) {
	hub := newFakeHub()
	hub.lookupErr["wi_seal"] = errLookupBoom
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	j := seedJob(r, "wi_seal")
	initGitCapsule(t, j)

	err := r.Stop(context.Background(), "wi_seal", protocol.FinishWrapped)
	if err == nil {
		t.Fatal("Stop must fail when end-generation fails")
	}
	state, _ := j.snapshot()
	if state != StateSealing {
		t.Fatalf("job must stay SEALING, got %s", state)
	}
	recs := loadRecords(t, r)
	if len(recs) != 1 || recs[0].State != string(StateSealing) || recs[0].StopReason != protocol.FinishWrapped {
		t.Fatalf("sealing intent not persisted: %+v", recs)
	}
	assertNoRT(t, toOB) // no WIFinished — the seal never landed
	unseal(t, r.cfg.WorkspaceRoot)
}

func run(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// TestPushStanding pins the honest final-push retry: a standing local
// commit whose first push failed must be re-drivable directly (the case
// gitops.CommitPush answers with the standing HEAD and pushed=false).
func TestPushStanding(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	j := seedJob(r, "wi_push")
	bare := initGitCapsule(t, j)

	// an unreachable remote answers a typed error and nothing lands
	bad := j.git
	bad.RemoteURL = filepath.Join(t.TempDir(), "nowhere.git")
	if pushed, err := gitops.Push(bad); err == nil || pushed {
		t.Fatal("push to a nonexistent remote must fail")
	}

	// the real remote accepts the standing HEAD
	pushed, err := gitops.Push(j.git)
	if err != nil || !pushed {
		t.Fatalf("gitops.Push of the standing HEAD = (%v, %v)", pushed, err)
	}
	if !bareHasMain(t, bare) {
		t.Fatal("the standing commit must land on the bare repo")
	}
	unseal(t, r.cfg.WorkspaceRoot)
}
