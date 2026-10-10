package runtime

// workspace_repos_test.go: the polyforge#79 local repo source — the yaml
// projects section replaces the aihub registry inventory WHOLLY for the
// projects it lists, clones ride the existing gitClone seam from the
// resolved local checkout, and the recovery contract (an existing
// repos/<name> is never re-cloned or reset) holds for locally-sourced
// clones.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// w079GitRepo creates a real git repo (one commit carrying markerName
// with markerContent) at dir and answers its HEAD SHA.
func w079GitRepo(t *testing.T, dir, markerName, markerContent string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=pf-test", "GIT_AUTHOR_EMAIL=pf-test@invalid",
			"GIT_COMMITTER_NAME=pf-test", "GIT_COMMITTER_EMAIL=pf-test@invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, markerName), []byte(markerContent), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "w079 init")
	return strings.TrimSpace(git("rev-parse", "HEAD"))
}

// w079Remote is one local fake registry remote served over file://.
func w079Remote(t *testing.T, markerName, markerContent string) (url, head string) {
	t.Helper()
	dir := t.TempDir()
	return "file://" + dir, w079GitRepo(t, dir, markerName, markerContent)
}

// A listed project's repos clone from the resolved local checkout, not
// the registry URL: the claim answer's inventory is wholly replaced, the
// baseline is the local HEAD, and the job carries the resolved sources.
func TestListedReposCloneFromLocalCheckout(t *testing.T) {
	registryURL, _ := w079Remote(t, "registry.txt", "registry source\n")
	hub := newFakeHub()
	hub.repos = []api.ProjectRepo{{Name: "w079src", URL: registryURL}}
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	localHead := w079GitRepo(t, filepath.Join(ws, ".repo", "w079src"), "local.txt", "local source\n")
	writeWorkspaceYaml(t, ws, "projects:\n  proj:\n    repos:\n      - name: w079src\n        path: .repo/w079src\n")

	if err := r.Start(context.Background(), "wi_w079src"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	capDir := testCapsuleDir(r, "wi_w079src")
	marker, err := os.ReadFile(filepath.Join(capDir, "repos", "w079src", "local.txt"))
	if err != nil || string(marker) != "local source\n" {
		t.Fatalf("repos/w079src was not cloned from the local checkout: %q, %v", marker, err)
	}
	if _, err := os.Stat(filepath.Join(capDir, "repos", "w079src", "registry.txt")); err == nil {
		t.Fatal("repos/w079src carries the registry remote's content — the registry URL was used")
	}
	// The claim-time baseline is the local checkout's HEAD.
	raw, err := os.ReadFile(filepath.Join(capDir, "capsule.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idn capsule.Identity
	if err := json.Unmarshal(raw, &idn); err != nil {
		t.Fatal(err)
	}
	if idn.Repos["w079src"] != localHead {
		t.Fatalf("baseline %q, want the local HEAD %s", idn.Repos["w079src"], localHead)
	}
	// The job carries the resolved source for the audit.
	j := r.jobOf("wi_w079src")
	j.mu.Lock()
	src := j.repoSources["w079src"]
	j.mu.Unlock()
	if src != filepath.Join(ws, ".repo", "w079src") {
		t.Fatalf("repo source %q, want the resolved local checkout", src)
	}
	if err := r.Stop(context.Background(), "wi_w079src", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A listed project with repos: [] clones NOTHING — the empty list is the
// authoritative inventory and the registry entry is not consulted.
func TestEmptyReposListReplacesRegistryWholly(t *testing.T) {
	urlA, _ := w079Remote(t, "a.txt", "registry A\n")
	urlB, _ := w079Remote(t, "b.txt", "registry B\n")
	hub := newFakeHub()
	hub.repos = []api.ProjectRepo{{Name: "w079one", URL: urlA}, {Name: "w079two", URL: urlB}}
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	writeWorkspaceYaml(t, ws, "projects:\n  proj:\n    repos: []\n")

	if err := r.Start(context.Background(), "wi_w079empty"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reposDir := filepath.Join(testCapsuleDir(r, "wi_w079empty"), "repos")
	if ents, err := os.ReadDir(reposDir); err == nil && len(ents) != 0 {
		t.Fatalf("repos/ holds %v, want nothing", ents)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", reposDir, err)
	}
	raw, err := os.ReadFile(filepath.Join(testCapsuleDir(r, "wi_w079empty"), "capsule.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idn capsule.Identity
	if err := json.Unmarshal(raw, &idn); err != nil {
		t.Fatal(err)
	}
	if len(idn.Repos) != 0 {
		t.Fatalf("capsule.json Repos %v, want empty", idn.Repos)
	}
	if err := r.Stop(context.Background(), "wi_w079empty", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A listed repo whose resolved local checkout is missing fails through
// the existing typed clone-failure lane (DAEMON-507, blocked end) — the
// registry URL is NEVER used as a fallback.
func TestMissingLocalCheckoutBlocksNeverFallsBack(t *testing.T) {
	registryURL, _ := w079Remote(t, "registry.txt", "registry fallback\n")
	hub := newFakeHub()
	hub.repos = []api.ProjectRepo{{Name: "w079gone", URL: registryURL}}
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	writeWorkspaceYaml(t, ws, "projects:\n  proj:\n    repos:\n      - name: w079gone\n        path: .repo/absent\n")

	err := r.Start(context.Background(), "wi_w079gone")
	if err == nil {
		t.Fatal("a missing local checkout must fail the claim")
	}
	ae, ok := err.(*api.Error)
	if !ok || ae.Code != api.ErrDaemonSpawnFail {
		t.Fatalf("expected DAEMON-507 through the existing clone-failure lane, got %v", err)
	}
	if !strings.Contains(ae.Detail, "w079gone") {
		t.Fatalf("the failure must name the repo, got %q", ae.Detail)
	}
	if end := hub.lastEnd(); end.Status != api.StatusBlocked {
		t.Fatalf("the clone failure must close blocked, got %+v", end)
	}
	if _, serr := os.Stat(filepath.Join(testCapsuleDir(r, "wi_w079gone"), "repos", "w079gone")); !os.IsNotExist(serr) {
		t.Fatal("the registry URL was consulted for a yaml-listed repo")
	}
}

// A present projects section that does not list the claimed project
// refuses the claim (DAEMON-415) — no silent registry fallback.
func TestUnlistedProjectRefusesClaim(t *testing.T) {
	registryURL, _ := w079Remote(t, "registry.txt", "registry marker\n")
	hub := newFakeHub()
	hub.repos = []api.ProjectRepo{{Name: "w079reg", URL: registryURL}}
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	writeWorkspaceYaml(t, ws, "projects:\n  w079other:\n    repos: []\n")

	err := r.Start(context.Background(), "wi_w079unl")
	ae, ok := err.(*api.Error)
	if !ok || ae.Code != api.ErrDaemonProjectNotListed {
		t.Fatalf("expected DAEMON-415, got %v", err)
	}
	for _, tok := range []string{".polyforge.yaml", "proj"} {
		if !strings.Contains(ae.Detail, tok) {
			t.Fatalf("the refusal must name %q, got %q", tok, ae.Detail)
		}
	}
	if _, serr := os.Stat(testCapsuleDir(r, "wi_w079unl")); !os.IsNotExist(serr) {
		t.Fatal("the refused claim materialized a capsule")
	}
	if n := hub.endCount(); n != 1 {
		t.Fatalf("the refused claim must close the session exactly once, got %d", n)
	}
}

// A yaml WITHOUT a projects section leaves the registry inventory
// standing — exactly as at HEAD.
func TestNoProjectsSectionKeepsRegistryInventory(t *testing.T) {
	registryURL, registryHead := w079Remote(t, "registry.txt", "registry marker\n")
	hub := newFakeHub()
	hub.repos = []api.ProjectRepo{{Name: "w079reg", URL: registryURL}}
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	writeWorkspaceYaml(t, ws, "workflow_defaults:\n  model_candidates: [a]\n")

	if err := r.Start(context.Background(), "wi_w079reg"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(testCapsuleDir(r, "wi_w079reg"), "capsule.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idn capsule.Identity
	if err := json.Unmarshal(raw, &idn); err != nil {
		t.Fatal(err)
	}
	if idn.Repos["w079reg"] != registryHead {
		t.Fatalf("baseline %q, want the registry HEAD %s (a workflow-only yaml takes over nothing on the repo side)", idn.Repos["w079reg"], registryHead)
	}
	j := r.jobOf("wi_w079reg")
	j.mu.Lock()
	sources := j.repoSources
	j.mu.Unlock()
	if len(sources) != 0 {
		t.Fatalf("a workflow-only yaml records no repo sources, got %v", sources)
	}
	if err := r.Stop(context.Background(), "wi_w079reg", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// The recovery contract holds for locally-sourced clones: an existing
// repos/<name> at materialize time is never re-cloned, re-fetched or
// reset — presence is verified, the recorded baseline carries forward.
// Pinned against cloneTaskRepos directly (the in-place materialize
// rerun shape; the full crash/restart walk lives in the e2e suite).
func TestRecoveryContractForLocallySourcedClones(t *testing.T) {
	hub := newFakeHub()
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	localHead := w079GitRepo(t, filepath.Join(ws, ".repo", "w079src"), "local.txt", "original scene\n")

	capsuleDir := testCapsuleDir(r, "wi_w079keep")
	if err := os.MkdirAll(filepath.Join(capsuleDir, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	j := &job{
		wi:          "wi_w079keep",
		project:     "proj",
		capsuleDir:  capsuleDir,
		repos:       []api.ProjectRepo{{Name: "w079src", URL: filepath.Join(ws, ".repo", "w079src")}},
		repoSources: map[string]string{"w079src": filepath.Join(ws, ".repo", "w079src")},
	}
	ctx := context.Background()

	// First materialize: the clone lands and the baseline is recorded.
	shas, err := r.cloneTaskRepos(ctx, j)
	if err != nil {
		t.Fatalf("cloneTaskRepos: %v", err)
	}
	if shas["w079src"] != localHead {
		t.Fatalf("baseline %q, want the local HEAD %s", shas["w079src"], localHead)
	}
	reposDir := capsuleDir
	worktree := filepath.Join(reposDir, "repos", "w079src")
	// The agent's uncommitted edits in the worktree are the live scene.
	if err := os.WriteFile(filepath.Join(worktree, "keep.txt"), []byte("in flight\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The recorded baseline (the prior capsule.json) carries forward.
	priorJSON := `{"repos": {"w079src": "` + localHead + `"}}`
	if err := os.WriteFile(filepath.Join(reposDir, "capsule.json"), []byte(priorJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// The local checkout MOVES ON — a re-clone would be detectable.
	w079GitRepo(t, filepath.Join(ws, ".repo", "w079src"), "moved.txt", "moved on\n")

	// The in-place re-materialize (the crashed-Start husk shape): the
	// worktree is untouched and the recorded baseline carried forward.
	shas2, err := r.cloneTaskRepos(ctx, j)
	if err != nil {
		t.Fatalf("cloneTaskRepos rerun: %v", err)
	}
	if shas2["w079src"] != localHead {
		t.Fatalf("baseline %q after the rerun, want the recorded claim-time %q", shas2["w079src"], localHead)
	}
	if b, err := os.ReadFile(filepath.Join(worktree, "keep.txt")); err != nil || string(b) != "in flight\n" {
		t.Fatalf("the in-flight marker was lost — the worktree was re-cloned or reset: %q, %v", b, err)
	}
	if head := strings.TrimSpace(gitHeadSHARaw(t, worktree)); head != localHead {
		t.Fatalf("worktree HEAD %q after the rerun, want the claim-time %q", head, localHead)
	}
}

func gitHeadSHARaw(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
