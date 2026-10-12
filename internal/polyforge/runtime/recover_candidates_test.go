package runtime

// recover_candidates_test.go: the recovery lane's polyforge#79
// rehydration of the effective candidate map. A LIVE job relaunches
// under its claim-time snapshot (an edit governs the NEXT claim, never
// an in-flight job's recomputation); a restart-RECONSTRUCTED job (nil
// snapshot) re-derives the lists from the workspace yaml FRESH through
// the same claim-time compose, so the archived-attempt recomputation
// indexes the re-derived list; a yaml that fails to re-read falls back
// to the capsule.json audit; an audit-less capsule rehydrates to nil
// (bundle-only, today's behavior). The interrupted-dispatch archive
// signal rides the relaunch as the raw recovery cause.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// recordingStarter wraps the scripted engine starter and records every
// AgentStartInput a relaunch carries — the recovery lane's rehydration
// is observable at exactly this seam.
type recordingStarter struct {
	mu     sync.Mutex
	inputs []AgentStartInput
	base   AgentStartFunc
}

func (rs *recordingStarter) start(ctx context.Context, in AgentStartInput) (AgentHandle, error) {
	rs.mu.Lock()
	rs.inputs = append(rs.inputs, in)
	rs.mu.Unlock()
	return rs.base(ctx, in)
}

func (rs *recordingStarter) last() AgentStartInput {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.inputs) == 0 {
		return AgentStartInput{}
	}
	return rs.inputs[len(rs.inputs)-1]
}

// A LIVE job relaunches under its CLAIM-TIME snapshot: the yaml edited
// mid-flight governs the NEXT claim, never this job's recovery
// recomputation, and the interrupted-dispatch archive signal rides the
// relaunch as the raw recovery cause.
func TestRecoveryRelaunchCarriesClaimSnapshot(t *testing.T) {
	setGitIdentity(t)
	wfDir, pinned := frontmatterBundle(t, w079Frontmatter)
	hub := newFakeHub()
	hub.digest = pinned
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	sp := &scriptedSpawner{}
	cfg := Config{
		MachineID:               "m-test",
		Mapping:                 mappingFor(ws, "proj"),
		DataDir:                 filepath.Join(t.TempDir(), "data"),
		MaxRecoveries:           2,
		RecoveryStabilityWindow: time.Nanosecond,
	}
	toOB := supervise.NewQueue[protocol.RTMsg]()
	fromOB := supervise.NewQueue[protocol.OBMsg]()
	r := New(cfg, hub, sp, toOB, fromOB)
	r.cfg.WorkflowsDir = wfDir
	rec := &recordingStarter{base: scriptedEngineStarter(sp)}
	r.cfg.AgentStarter = rec.start

	writeWorkspaceYaml(t, ws, "models:\n  proj:\n    code: [w79/code-a, w79/code-b]\n")
	if err := r.Start(context.Background(), "wi_w079rec"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := r.jobOf("wi_w079rec")
	if j == nil {
		t.Fatal("the claim must run to a live agent")
	}
	j.mu.Lock()
	claimSnap := j.stepCandidates
	j.mu.Unlock()
	if claimSnap["fix"] == nil || claimSnap["fix"][0] != "w79/code-a" {
		t.Fatalf("claim snapshot for fix %v, want [w79/code-a ...]", claimSnap["fix"])
	}
	// Drain the claim's own RTStarted before the recovery's message.
	if got := recvRT(t, toOB); got.Kind != protocol.RTStarted {
		t.Fatalf("expected the claim's RTStarted, got %+v", got)
	}

	// The mid-flight edit: the yaml on disk now pins a DIFFERENT list.
	writeWorkspaceYaml(t, ws, "models:\n  proj:\n    code: [w79/edited-a]\n")

	// The death → recovery: the relaunch must carry the claim snapshot,
	// NOT a re-derivation of the edited yaml.
	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
	msg := recvRT(t, toOB)
	if msg.Kind != protocol.RTRecovered {
		t.Fatalf("expected RTRecovered, got %+v", msg)
	}
	relaunch := rec.last()
	if len(relaunch.StepCandidates["fix"]) != 2 ||
		relaunch.StepCandidates["fix"][0] != "w79/code-a" ||
		relaunch.StepCandidates["fix"][1] != "w79/code-b" {
		t.Fatalf("relaunch StepCandidates for fix %v, want the claim snapshot [w79/code-a w79/code-b] — an edited yaml governs only the NEXT claim (the disk yaml now pins [w79/edited-a])", relaunch.StepCandidates["fix"])
	}
	if relaunch.InterruptedCause != protocol.RecoverDeath {
		t.Fatalf("relaunch InterruptedCause %q, want the raw recovery cause %q (the archive signal)", relaunch.InterruptedCause, protocol.RecoverDeath)
	}
}

// A restart-RECONSTRUCTED job (nil snapshot — the jobFromRecord shape)
// rehydrates the capsule.json audit and never re-reads the yaml: even a
// yaml now on disk that pins another list, or is invalid, changes nothing.
func TestRecoveryReconstructedJobRehydratesAuditNotYaml(t *testing.T) {
	setGitIdentity(t)
	wfDir, _ := frontmatterBundle(t, w079Frontmatter)
	sp := &scriptedSpawner{}
	cfg := Config{
		MachineID:               "m-test",
		Mapping:                 mappingFor(filepath.Join(t.TempDir(), "ws")),
		DataDir:                 filepath.Join(t.TempDir(), "data"),
		AgentStarter:            scriptedEngineStarter(sp),
		MaxRecoveries:           2,
		RecoveryStabilityWindow: time.Nanosecond,
	}
	toOB := supervise.NewQueue[protocol.RTMsg]()
	fromOB := supervise.NewQueue[protocol.OBMsg]()
	r := New(cfg, newFakeHub(), sp, toOB, fromOB)
	r.cfg.WorkflowsDir = wfDir
	ws, err := r.cfg.Mapping.WorkspaceFor("proj")
	if err != nil {
		t.Fatal(err)
	}
	j := seedJob(r, "wi_w079rec2")
	audit := map[string][]string{
		"fix":   {"w79/aud-a", "w79/aud-b"},
		"grill": {"prov/weak-a"},
	}
	blob, err := json.Marshal(map[string]any{"effective_candidates": audit})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.capsuleDir, capsule.CapsuleJSON), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, y := range []string{
		"models:\n  proj:\n    code: [w79/edited]\n",
		"models:\n  proj:\n    code: [\n", // invalid: recovery must not care
	} {
		writeWorkspaceYaml(t, ws, y)
		got := r.effectiveCandidatesForRecovery(j)
		if len(got["fix"]) != 2 || got["fix"][0] != "w79/aud-a" || got["fix"][1] != "w79/aud-b" {
			t.Fatalf("rehydrated fix list %v, want the audit's [w79/aud-a w79/aud-b] regardless of the disk yaml %q", got["fix"], y)
		}
	}
}

// An audit-less capsule (no yaml, no audit — the no-yaml claim shape)
// rehydrates to nil: bundle-only behavior, exactly today's.
func TestRecoveryAuditlessCapsuleRehydratesNil(t *testing.T) {
	setGitIdentity(t)
	wfDir, _ := frontmatterBundle(t, w079Frontmatter)
	sp := &scriptedSpawner{}
	cfg := Config{
		MachineID:               "m-test",
		Mapping:                 mappingFor(filepath.Join(t.TempDir(), "ws")),
		DataDir:                 filepath.Join(t.TempDir(), "data"),
		AgentStarter:            scriptedEngineStarter(sp),
		MaxRecoveries:           2,
		RecoveryStabilityWindow: time.Nanosecond,
	}
	toOB := supervise.NewQueue[protocol.RTMsg]()
	fromOB := supervise.NewQueue[protocol.OBMsg]()
	r := New(cfg, newFakeHub(), sp, toOB, fromOB)
	r.cfg.WorkflowsDir = wfDir
	j := seedJob(r, "wi_w079rec4")
	if got := r.effectiveCandidatesForRecovery(j); got != nil {
		t.Fatalf("audit-less rehydration %v, want nil (bundle-only)", got)
	}
}
