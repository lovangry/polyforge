package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// doctor_repos.go: the read-only git probe layer behind `pf doctor`'s
// "workspace repo clones" check. Read-only: every command inside a mapped
// workspace's `.repo/` clones and their linked worktrees is one git
// invocation with a short timeout and `--no-optional-locks` — the repos are
// never written, never fetched, never mutated by the layer.
//
// Scan units. Phase one enumerates: every directory directly under
// `<workspace>/.repo/` that carries a `.git` is the clone it is (a `.git`
// directory, or a `.git` pointer file — a linked worktree sitting directly
// in .repo/ itself). Phase two reaches: each enumerated clone's linked
// worktrees come from `git worktree list --porcelain` run in that clone —
// authoritative over the `pi-web-worktrees/*` wrapper and any other
// location, at the face of the clone they hang off. Directory entries
// without a `.git` of their own (e.g. the `pi-web-worktrees/` wrapper) are
// never units. Units are deduplicated by cleaned path, and a phase-one
// enumerated worktree keeps its own enumerated face.
//
// Local facts per unit (never blanked: a failing command is the unit's
// "not checked" reason, the doctor's own never-blank convention):
//
//   - branch: `git symbolic-ref --short HEAD`; failing = detached HEAD ("").
//     An unborn HEAD still answers its branch.
//   - dirty: `git status --porcelain` non-empty (modified/staged tracked +
//     untracked non-ignored — the ruling-4 shape).
//   - unpushed: HEAD is on no remote-tracking ref (`git branch -r --contains
//     HEAD` answers empty). Remote-tracking refs only — no network, no
//     fetch — so a repo with no remotes at all flags as unpushed. An unborn
//     HEAD carries no commit: unpushed is false, the repo is judged by
//     status alone.

// repoClonesGitTimeout bounds one read-only git invocation.
const repoClonesGitTimeout = 10 * time.Second

// A clone unit is one scanned git repo: a clone directly under a mapped
// workspace's `.repo/` or a worktree linked to one.
type repoUnit struct {
	Dir       string // the unit's own directory, cleaned and absolute
	CloneName string // the registry match face: the basename of the clone under .repo/ the unit hangs off — its own when the unit is enumerated by phase one, owning clone's when it is phase two
	Branch    string // the checked-out branch; "" = detached HEAD
	Dirty     bool   // Uncommitted: tracked modified/staged + untracked non-ignored
	Unpushed  bool   // HEAD commit on no remote-tracking ref
	Checked   string // "" = facts read; else the exact reason the probe failed
}

// workspaceRepoUnits enumerates the scan units of one mapped workspace's
// .repo/: clones directly under it (phase one) plus their linked worktrees
// (phase two). No .repo/ at all is no units, not an error — the workspace
// carries no clones.
func workspaceRepoUnits(ws string) []repoUnit {
	repoDir := filepath.Join(ws, ".repo")
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []repoUnit{{Dir: repoDir, CloneName: filepath.Base(repoDir), Checked: err.Error()}}
	}
	seen := map[string]bool{}
	units := make([]repoUnit, 0, len(entries))
	// Phase one: every entry carrying .git is the clone it is.
	for _, e := range entries {
		clone := filepath.Join(repoDir, e.Name())
		if !isRepoDir(clone) {
			continue
		}
		seen[clone] = true
		u := repoUnit{Dir: clone, CloneName: filepath.Base(clone)}
		probeRepoUnit(&u)
		units = append(units, u)
	}
	// Phase two: each clone's linked worktrees — authoritative over the
	// wrapper directory they hang off for the face, any other location; a
	// phase-one enumerated worktree keeps its own enumerated face.
	for i := range units {
		u := &units[i]
		wts, err := cloneWorktreeDirs(u.Dir)
		if err != nil {
			u.Checked = err.Error()
			continue
		}
		for _, wt := range wts {
			if wt == u.Dir || seen[wt] {
				continue
			}
			seen[wt] = true
			wu := repoUnit{Dir: wt, CloneName: u.CloneName}
			probeRepoUnit(&wu)
			units = append(units, wu)
		}
	}
	return units
}

// isRepoDir reports dir as a git repo: its .git is a directory (a clone) or
// a regular file (a linked worktree's pointer file — a worktree that sits
// directly under .repo/ is itself enumerated as the clone it is).
func isRepoDir(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && (fi.IsDir() || fi.Mode().IsRegular())
}

// cloneWorktreeDirs parses `git worktree list --porcelain` in dir and cleans
// every entry's `worktree <path>` row (including dir itself when the clone
// is the main worktree).
func cloneWorktreeDirs(dir string) ([]string, error) {
	out, err := probeGit(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "worktree "); ok {
			dirs = append(dirs, filepath.Clean(rest))
		}
	}
	return dirs, nil
}

// probeRepoUnit fills one unit's local facts, or its exact failure reason.
// Never blanks the clean face that was read before a later command failed —
// but never fakes green either: Checked non-empty means the consumer lists
// the unit and counts it neither clean nor covered.
func probeRepoUnit(u *repoUnit) {
	status, err := probeGit(u.Dir, "status", "--porcelain")
	if err != nil {
		u.Checked = err.Error()
		return
	}
	u.Dirty = strings.TrimSpace(status) != ""
	if br, brerr := probeGit(u.Dir, "symbolic-ref", "--short", "HEAD"); brerr == nil {
		u.Branch = strings.TrimSpace(br)
	} // else detached HEAD: no branch
	if _, err := probeGit(u.Dir, "rev-parse", "-q", "HEAD"); err != nil {
		return // unborn HEAD: nothing committed — judged by status alone
	}
	contained, err := probeGit(u.Dir, "branch", "-r", "--contains", "HEAD")
	if err != nil {
		u.Checked = err.Error()
		return
	}
	u.Unpushed = strings.TrimSpace(contained) == ""
}

// probeGit runs one read-only git command inside an (untrusted) mapped
// .repo/ clone: its own git-dir (its inherited GIT_* stripped), system
// config ignored, fsmonitor and hooks disabled, literal pathspecs, no
// optional locks and a hard timeout (the deliver facts' face — the doctor
// never writes into someone's clone, not even an advice file).
func probeGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), repoClonesGitTimeout)
	defer cancel()
	argv := append([]string{
		"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null",
		"--no-optional-locks", "--literal-pathspecs",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "LC_ALL=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = fmt.Errorf("git %v: %v", args, err).Error()
		}
		return "", fmt.Errorf("git %v: %v", args, msg)
	}
	return stdout.String(), nil
}
