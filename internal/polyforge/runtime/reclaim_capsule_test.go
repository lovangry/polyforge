package runtime

// reclaim_capsule_test.go pins the re-claim capsule path (v2-converge §6):
// a stale external git-dir never survives a quarantine, and a clone failure
// for a WI with published progress never degrades into a local init.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

func reclaimJob(t *testing.T, wi, fp string) *job {
	t.Helper()
	base := t.TempDir()
	j := &job{wi: wi, sessionFingerprint: fp}
	j.capsuleDir = filepath.Join(base, "capsules", wi)
	j.git = gitops.Opts{
		WorkItemID:  wi,
		Fingerprint: fp,
		CapsuleDir:  j.capsuleDir,
		GitDir:      filepath.Join(base, "git", wi+".git"),
		Username:    wi,
		Secret:      "push-secret",
	}
	return j
}

func headOf(t *testing.T, o gitops.Opts) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Env = o.Env()
	cmd.Dir = o.CapsuleDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// Same-machine re-claim under a new session: the previous session's capsule
// (with an unpublished local commit) is quarantined together with its git-dir;
// the new capsule's HEAD is the remote tip and a commit+push succeeds.
func TestPrepareCapsuleReclaimQuarantinesGitDirAndPushes(t *testing.T) {
	r := &Runtime{}
	j := reclaimJob(t, "wi_reclaim_gd", "fp-old")
	bare := initGitCapsule(t, j)
	if out, err := exec.Command("git", "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("bare HEAD: %v: %s", err, out)
	}
	if err := writeSessionMarker(j.capsuleDir, "fp-old"); err != nil {
		t.Fatal(err)
	}
	if _, pushed, _, err := gitops.CommitPush(j.git, "publish"); err != nil || !pushed {
		t.Fatalf("first publish: pushed=%v err=%v", pushed, err)
	}
	tip, err := exec.Command("git", "--git-dir", bare, "rev-parse", "refs/heads/main").Output()
	if err != nil {
		t.Fatal(err)
	}
	remoteTip := strings.TrimSpace(string(tip))
	// an unpublished local commit the old session never pushed
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "local-only")
	cmd.Env = j.git.Env()
	cmd.Dir = j.capsuleDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("local commit: %v: %s", err, out)
	}

	j2 := &job{wi: j.wi, capsuleDir: j.capsuleDir, git: j.git, sessionFingerprint: "fp-new"}
	j2.git.Fingerprint = "fp-new"
	if err := r.prepareCapsule(j2); err != nil {
		t.Fatalf("prepareCapsule: %v", err)
	}
	if got := headOf(t, j2.git); got != remoteTip {
		t.Fatalf("capsule HEAD = %s, want remote tip %s (stale git-dir reused?)", got, remoteTip)
	}
	if m, _ := filepath.Glob(j.git.GitDir + ".quarantined.*"); len(m) != 1 {
		t.Fatalf("the old git-dir must be quarantined beside the original, got %v", m)
	}
	if m, _ := filepath.Glob(j.capsuleDir + ".quarantined.*"); len(m) != 1 {
		t.Fatalf("the old worktree must be quarantined, got %v", m)
	}
	if err := os.WriteFile(filepath.Join(j2.capsuleDir, "next.txt"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, pushed, _, err := gitops.CommitPush(j2.git, "after re-claim"); err != nil || !pushed {
		t.Fatalf("push after re-claim: pushed=%v err=%v", pushed, err)
	}
}

// A clone failure for a WI with published progress must not init locally.
func TestPrepareCapsuleCloneFailurePublishedRefusesInit(t *testing.T) {
	r := &Runtime{}
	j := reclaimJob(t, "wi_clone_fail", "fp-old")
	bare := initGitCapsule(t, j)
	if out, err := exec.Command("git", "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("bare HEAD: %v: %s", err, out)
	}
	if _, pushed, _, err := gitops.CommitPush(j.git, "publish"); err != nil || !pushed {
		t.Fatalf("publish: pushed=%v err=%v", pushed, err)
	}
	// ls-remote still sees refs/heads/main, but the clone cannot fetch objects.
	objs := filepath.Join(bare, "objects")
	ents, _ := os.ReadDir(objs)
	for _, e := range ents {
		if e.Name() != "info" && e.Name() != "pack" {
			if err := os.RemoveAll(filepath.Join(objs, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	packs, _ := filepath.Glob(filepath.Join(objs, "pack", "*"))
	for _, p := range packs {
		_ = os.Remove(p)
	}
	j2 := reclaimJob(t, "wi_clone_fail", "fp-new")
	j2.git.RemoteURL = bare
	err := r.prepareCapsule(j2)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrDaemonCapsuleClone {
		t.Fatalf("want typed %s error, got %T: %v", api.ErrDaemonCapsuleClone, err, err)
	}
	if _, serr := os.Stat(filepath.Join(j2.capsuleDir, ".git")); serr == nil {
		t.Fatal("no local init may happen for a WI with published progress")
	}
}

// An undeterminable remote (transport error) is not "nothing published".
func TestPrepareCapsuleCloneFailureUnknownRefusesInit(t *testing.T) {
	r := &Runtime{}
	j := reclaimJob(t, "wi_clone_unknown", "fp")
	j.git.RemoteURL = "http://127.0.0.1:1/wi.git"
	err := r.prepareCapsule(j)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrDaemonCapsuleClone {
		t.Fatalf("want typed %s error, got %T: %v", api.ErrDaemonCapsuleClone, err, err)
	}
	if dirExists(j.capsuleDir) {
		t.Fatal("no capsule may be initialised on an undeterminable remote")
	}
}

// A first claim of a fresh WI (no remote content) still inits locally.
func TestPrepareCapsuleCloneFailureNothingPublishedInits(t *testing.T) {
	setGitIdentity(t)
	r := &Runtime{}
	j := reclaimJob(t, "wi_fresh", "fp")
	j.git.RemoteURL = filepath.Join(t.TempDir(), "absent.git") // repo does not exist
	if err := r.prepareCapsule(j); err != nil {
		t.Fatalf("first claim must still init locally: %v", err)
	}
	if !isRegularFile(filepath.Join(j.capsuleDir, ".git")) || !dirExists(j.git.GitDir) {
		t.Fatal("capsule must be initialised with an external git-dir")
	}
}
