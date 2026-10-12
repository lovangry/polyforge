package runtime

// workspace_claim_test.go: the polyforge#79 claim-time yaml lane — the
// fresh per-claim read, the typed DAEMON-414 refusal BEFORE any capsule
// state exists, and the no-restart freshness observable (an edit governs
// the next claim on the SAME runtime instance).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// newWorkspaceTestRuntime builds a runtime whose workspace mapping names
// ONE workspace carrying the given project, over a pristine demo bundle
// (materialize can succeed).
func newWorkspaceTestRuntime(t *testing.T, hub Aihub, project string) (*Runtime, string, *scriptedSpawner) {
	t.Helper()
	setGitIdentity(t)
	wfDir, pinned := pristineBundle(t)
	hub.(*fakeHub).digest = pinned
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	sp := &scriptedSpawner{}
	cfg := Config{
		MachineID: "m-test",
		Mapping:   mappingFor(ws, project),
		DataDir:   filepath.Join(t.TempDir(), "data"),
		// polyforge#106: the driver is the only engine — the rig scripts
		// it through the single engine seam.
		AgentStarter:  scriptedEngineStarter(sp),
		MaxRecoveries: 2,
		// The storm guard's window is collapsed: test episodes are
		// separated by handleOB calls, not real time.
		RecoveryStabilityWindow: time.Nanosecond,
	}
	toOB := supervise.NewQueue[protocol.RTMsg]()
	fromOB := supervise.NewQueue[protocol.OBMsg]()
	r := New(cfg, hub, sp, toOB, fromOB)
	r.cfg.WorkflowsDir = wfDir
	return r, ws, sp
}

func writeWorkspaceYaml(t *testing.T, ws, content string) string {
	t.Helper()
	p := filepath.Join(ws, ".polyforge.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// An invalid .polyforge.yaml refuses the claim with the typed DAEMON-414
// BEFORE any capsule directory exists: the session ends blocked, the
// claiming journal settles, no live job remains — and after the operator
// fixes the file, the NEXT claim on the SAME runtime runs clean (the
// no-restart freshness observable).
func TestWorkspaceYamlRefusesClaimBeforeMaterialization(t *testing.T) {
	hub := newFakeHub()
	r, ws, sp := newWorkspaceTestRuntime(t, hub, "proj")

	writeWorkspaceYaml(t, ws, "projects:\n  proj:\n    repos: [\n") // torn
	err := r.Start(context.Background(), "wi_w079ref")
	if err == nil {
		t.Fatal("an invalid workspace yaml must refuse the claim")
	}
	ae, ok := err.(*api.Error)
	if !ok || ae.Code != api.ErrDaemonWorkspaceYaml {
		t.Fatalf("expected DAEMON-414, got %v (%T)", err, err)
	}
	if !strings.Contains(ae.Detail, ".polyforge.yaml") {
		t.Fatalf("the refusal must name the file, got %q", ae.Detail)
	}
	if n := hub.endCount(); n != 1 {
		t.Fatalf("the refused claim must close the session exactly once, got %d", n)
	}
	if end := hub.lastEnd(); end.Status != api.StatusBlocked || end.Note == "" {
		t.Fatalf("DAEMON-414 must close blocked with a note, got %+v", end)
	}
	if _, serr := os.Stat(testCapsuleDir(r, "wi_w079ref")); !os.IsNotExist(serr) {
		t.Fatalf("the refused claim must materialize nothing (stat: %v)", serr)
	}
	if r.jobOf("wi_w079ref") != nil {
		t.Fatal("a refused claim must leave no live job")
	}
	if items := r.loadClaiming(); len(items) != 0 {
		t.Fatalf("the claiming journal must settle with the refusal, got %+v", items)
	}
	if n := sp.callCount(); n != 0 {
		t.Fatalf("the refusal must derive no agent, got %d spawns", n)
	}

	// The operator fixes the yaml; the SAME runtime instance serves the
	// next claim — no restart (the read lives in the claim path).
	writeWorkspaceYaml(t, ws, "projects:\n  proj:\n    repos: []\n")
	if err := r.Start(context.Background(), "wi_w079fix"); err != nil {
		t.Fatalf("the fixed yaml must let the next claim run: %v", err)
	}
	if r.jobOf("wi_w079fix") == nil {
		t.Fatal("the fixed-yaml claim must run to a live agent")
	}
	if n := hub.endCount(); n != 1 {
		t.Fatalf("the clean claim must not close any session, got %d ends", n)
	}
	if err := r.Stop(context.Background(), "wi_w079fix", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// Every strict-schema violation family refuses through the same lane.
func TestWorkspaceYamlRefusalFamilies(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"torn", "projects:\n  proj:\n    repos: [\n"},
		{"unknown top-level key", "project: proj\nprojects:\n  proj:\n    repos: []\n"},
		{"step_kinds key outside the V8 set", "workflow_defaults:\n  step_kinds:\n    build: [a]\n"},
		{"project entry without repos", "projects:\n  proj: {}\n"},
		{"unsafe repo name", "projects:\n  proj:\n    repos:\n      - name: ../esc\n        path: .repo/a\n"},
		{"absolute repo path", "projects:\n  proj:\n    repos:\n      - name: a\n        path: /tmp/a\n"},
		{"parent-escaping repo path", "projects:\n  proj:\n    repos:\n      - name: a\n        path: ../a\n"},
		{"empty model id", "workflow_defaults:\n  model_candidates: [\"\"]\n"},
		{"duplicate key", "projects:\n  proj:\n    repos: []\n  dup:\n    repos: []\n  dup:\n    repos: []\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := newFakeHub()
			r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
			writeWorkspaceYaml(t, ws, c.yaml)
			err := r.Start(context.Background(), "wi_w079fam")
			ae, ok := err.(*api.Error)
			if !ok || ae.Code != api.ErrDaemonWorkspaceYaml {
				t.Fatalf("expected DAEMON-414 for %s, got %v", c.name, err)
			}
			if _, serr := os.Stat(testCapsuleDir(r, "wi_w079fam")); !os.IsNotExist(serr) {
				t.Fatalf("%s: the refused claim materialized a capsule", c.name)
			}
		})
	}
}

// A workspace WITHOUT a yaml runs exactly as at HEAD: the claim
// proceeds, the job carries no yaml snapshot.
func TestAbsentWorkspaceYamlEqualsToday(t *testing.T) {
	hub := newFakeHub()
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	if _, err := os.Stat(filepath.Join(ws, ".polyforge.yaml")); !os.IsNotExist(err) {
		t.Fatalf("fixture defect: %v", err)
	}
	if err := r.Start(context.Background(), "wi_w079abs"); err != nil {
		t.Fatalf("an absent yaml must not refuse the claim: %v", err)
	}
	j := r.jobOf("wi_w079abs")
	if j == nil {
		t.Fatal("the claim must run to a live agent")
	}
	j.mu.Lock()
	snap := j.workspaceYaml
	root := j.workspaceRoot
	j.mu.Unlock()
	if snap != nil {
		t.Fatalf("no yaml must answer a nil snapshot, got %+v", snap)
	}
	if root != testWorkspace(r) {
		t.Fatalf("workspace root %q, want the mapped root", root)
	}
	if err := r.Stop(context.Background(), "wi_w079abs", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A comments-only yaml is absent-equivalent: the claim proceeds.
func TestCommentsOnlyWorkspaceYamlIsAbsentEquivalent(t *testing.T) {
	hub := newFakeHub()
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	writeWorkspaceYaml(t, ws, "# just comments\n# nothing more\n")
	if err := r.Start(context.Background(), "wi_w079cmt"); err != nil {
		t.Fatalf("a comments-only yaml must not refuse the claim: %v", err)
	}
	j := r.jobOf("wi_w079cmt")
	if j == nil {
		t.Fatal("the claim must run to a live agent")
	}
	j.mu.Lock()
	snap := j.workspaceYaml
	j.mu.Unlock()
	if snap != nil {
		t.Fatalf("a comments-only yaml must answer a nil snapshot, got %+v", snap)
	}
	if err := r.Stop(context.Background(), "wi_w079cmt", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// The single-lane convergence: a FRESH claim and a RE-CLAIM over a
// retained paused record both read the yaml on their own claim path — a
// yaml valid for claim one that is broken for the re-claim refuses it, and
// vice versa (the re-claim re-enters startClaimed over a paused record
// once force-end-session returned the WI to queued).
func TestWorkspaceYamlReadOnFreshClaimAndReclaim(t *testing.T) {
	hub := newFakeHub()
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")

	writeWorkspaceYaml(t, ws, "projects:\n  proj:\n    repos: []\n")
	if err := r.Start(context.Background(), "wi_w079lane"); err != nil {
		t.Fatalf("fresh claim: %v", err)
	}
	// Park the live job (the only shape a re-claim re-enters startClaimed
	// from): the operator-pause close retains the record in StatePaused.
	if err := r.Stop(context.Background(), "wi_w079lane", protocol.FinishPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	hub.forceEnd("wi_w079lane")
	// The re-claim re-enters startClaimed: it re-reads the yaml — now
	// broken — and refuses with DAEMON-414.
	writeWorkspaceYaml(t, ws, "workflow_defaults:\n  step_kinds:\n    bogus: [a]\n")
	terr := r.Start(context.Background(), "wi_w079lane")
	if terr == nil {
		t.Fatal("the re-claim must re-read the yaml and refuse on an invalid one")
	}
	if ae, ok := terr.(*api.Error); !ok || ae.Code != api.ErrDaemonWorkspaceYaml {
		t.Fatalf("expected DAEMON-414 on the re-claim, got %v", terr)
	}
	// Fixed again: the re-claim proceeds.
	writeWorkspaceYaml(t, ws, "projects:\n  proj:\n    repos: []\n")
	if err := r.Start(context.Background(), "wi_w079lane"); err != nil {
		t.Fatalf("re-claim after the fix: %v", err)
	}
	if err := r.Stop(context.Background(), "wi_w079lane", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
