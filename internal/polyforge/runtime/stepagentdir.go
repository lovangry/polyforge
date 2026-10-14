package runtime

// stepagentdir.go: the dedicated step agent directory (audit-subtraction
// pass B, addendum 2 / owner ruling A). A step pi process runs with
// PI_CODING_AGENT_DIR pointing at a directory the DAEMON owns under its
// data dir — never inside a capsule (capsules are committed and pushed to
// aihub and are agent-writable) and never the operator's own agent
// directory (whose settings.json is exactly where user-level packages
// such as npm:pi-background-tasks are declared).
//
// The directory is a MINIMAL snapshot, enumerated in the capsule plan
// (owner ruling A2) — exactly this set, nothing else:
//
//	auth.json            model service credentials — COPY from the
//	                     operator agent dir (mode 0600, daemon-owned)
//	models.json          model service configuration — COPY (source mode
//	                     preserved), only when the source has one
//	agents/              subagent definition files — recursive COPY of
//	                     the source tree; absent source dir ⇒ absent here
//	extensions/subagent/ the subagent TOOL — recursive COPY of exactly
//	                     that one extension directory; absent source ⇒
//	                     absent here
//
// The subagent tool is not a pi builtin: it is the extension at
// <operator agent dir>/extensions/subagent/, and pi auto-discovers global
// extensions only from <agent dir>/extensions/. So the dedicated dir
// carries that ONE extension directory and no other — the operator's
// other extensions (extensions/polyforge is for interactive sessions)
// stay out, and so do settings.json / packages, which is where the
// background-task tools are declared.
//
// auth.json supports API-key entries. An OAuth entry works until pi
// refreshes it: pi writes the refreshed token into the COPY, and the next
// sync sees the copy differ from the operator's file and overwrites it
// with the operator's (older) entry — the step-side refresh is lost. No
// mechanism handles that; use API-key credentials for step agents.
//
// Copies, not links: a snapshot never follows later owner edits (a global
// reinstall included), and no symlink makes the dedicated dir a window
// onto the whole operator dir. The allowlisted members are refreshed
// before every spawn; anything else under the directory is PRUNED only at
// daemon start (SyncStepAgentDir), when no step process of this daemon is
// running — a per-spawn prune could pull files from under a live step
// process. A file that is present in the source but cannot be READ
// (torn, unreadable) or a destination that cannot be written is a LOUD
// typed error — the spawn is refused, never silently degraded. A source
// with no auth.json at all (the harness adapter may hold its credentials
// elsewhere; the in-repo fake harness needs none) is a warning, not a
// refusal — but an explicitly configured source (flag/env) whose
// auth.json is missing IS a refusal: the operator named the credentials'
// home and it is not there.
//
// Deliberately ABSENT (the isolation set): settings.json (user-level
// packages), keybindings.json, mcp.json, AGENTS.md/CLAUDE.md/SYSTEM.md/
// APPEND_SYSTEM.md, every extension but subagent, skills/, prompts/,
// themes/.
//
// Sessions live in a SIBLING directory (step-agent-sessions), not here,
// so the agent dir itself stays exactly the enumerated minimal set: the
// step process gets PI_CODING_AGENT_SESSION_DIR=<data>/step-agent-sessions
// and the runtime's transcript locator root points at the same place.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/common/atomicfile"
)

// The dedicated directories' names under the daemon data dir.
const (
	StepAgentDirName         = "step-agent"
	StepAgentSessionsDirName = "step-agent-sessions"
)

// The minimal-set file names (owner ruling A2).
const (
	stepAgentAuthFile   = "auth.json"
	stepAgentModelsFile = "models.json"
	stepAgentAgentsDir  = "agents"
	stepAgentExtDir     = "extensions"
	stepAgentSubagent   = "subagent" // the ONE extension carried: extensions/subagent
)

// StepAgentDir is the dedicated step agent directory under the daemon
// data dir (A1: under the data dir, NEVER inside a capsule).
func (c *Config) StepAgentDir() string {
	return filepath.Join(c.DataDir, StepAgentDirName)
}

// StepAgentSessionsDir is the step processes' harness session store
// (PI_CODING_AGENT_SESSION_DIR and the live-transcript locator root).
func (c *Config) StepAgentSessionsDir() string {
	return filepath.Join(c.DataDir, StepAgentSessionsDirName)
}

// OperatorAgentDirDefault is the default source of the minimal set: the
// operator's own agent directory.
func OperatorAgentDirDefault() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".pi", "agent")
	}
	return filepath.Join(home, ".pi", "agent")
}

// StepAgentDirError is the loud typed refusal of the dedicated-directory
// sync: the spawn (or daemon start) that hit it is refused, never
// silently degraded.
type StepAgentDirError struct {
	Dir string
	Err error
}

func (e *StepAgentDirError) Error() string {
	return fmt.Sprintf("runtime: the dedicated step agent directory %s could not be synced from the operator agent dir — the spawn is refused rather than run degraded: %v", e.Dir, e.Err)
}

func (e *StepAgentDirError) Unwrap() error { return e.Err }

// refreshStepAgentMinimalSet refreshes the allowlisted members of the
// dedicated directory at dst from src, each copied only when the source
// presents it and removed when the source no longer does. It never
// touches anything outside the allowlist (that is pruneStepAgentDir's
// job, daemon start only).
func refreshStepAgentMinimalSet(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	// auth.json: the one mandatory member whenever the source can
	// present it. Missing source file ⇒ no copy (the caller warned);
	// unreadable source file ⇒ the loud refusal.
	srcAuth := filepath.Join(src, stepAgentAuthFile)
	if _, err := os.Stat(srcAuth); err == nil {
		if err := copyStepAgentFile(srcAuth, filepath.Join(dst, stepAgentAuthFile), 0o600); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	// models.json: only when the source has one (source mode kept); a
	// stale destination copy is removed so "only if present in the
	// source" holds both ways.
	srcModels := filepath.Join(src, stepAgentModelsFile)
	dstModels := filepath.Join(dst, stepAgentModelsFile)
	if _, err := os.Stat(srcModels); err == nil {
		if err := copyStepAgentFile(srcModels, dstModels, 0); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return &StepAgentDirError{Dir: dst, Err: err}
	} else if err := os.Remove(dstModels); err != nil && !os.IsNotExist(err) {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	// agents/ and extensions/subagent/: recursive per-file copies;
	// absent source dir ⇒ absent destination dir.
	if err := mirrorStepAgentTree(filepath.Join(src, stepAgentAgentsDir), filepath.Join(dst, stepAgentAgentsDir)); err != nil {
		return err
	}
	return mirrorStepAgentTree(
		filepath.Join(src, stepAgentExtDir, stepAgentSubagent),
		filepath.Join(dst, stepAgentExtDir, stepAgentSubagent))
}

// pruneStepAgentDir removes everything under dst that is not in the
// minimal set: at the top level anything but auth.json / models.json /
// agents / extensions, under extensions/ anything but subagent (and
// extensions/ itself when subagent is absent). Daemon start only.
func pruneStepAgentDir(dst string) error {
	ents, err := os.ReadDir(dst)
	if err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	for _, e := range ents {
		switch e.Name() {
		case stepAgentAuthFile, stepAgentModelsFile, stepAgentAgentsDir, stepAgentExtDir:
		default:
			if err := os.RemoveAll(filepath.Join(dst, e.Name())); err != nil {
				return &StepAgentDirError{Dir: dst, Err: err}
			}
		}
	}
	extDir := filepath.Join(dst, stepAgentExtDir)
	exts, err := os.ReadDir(extDir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	keep := false
	for _, e := range exts {
		if e.Name() == stepAgentSubagent && e.IsDir() {
			keep = true
			continue
		}
		if err := os.RemoveAll(filepath.Join(extDir, e.Name())); err != nil {
			return &StepAgentDirError{Dir: dst, Err: err}
		}
	}
	if !keep {
		if err := os.RemoveAll(extDir); err != nil {
			return &StepAgentDirError{Dir: dst, Err: err}
		}
	}
	return nil
}

// mirrorStepAgentTree copies the tree at src to dst when src exists and
// removes dst when it does not.
func mirrorStepAgentTree(src, dst string) error {
	if _, err := os.Stat(src); err == nil {
		return copyStepAgentTree(src, dst)
	} else if !os.IsNotExist(err) {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	if err := os.RemoveAll(dst); err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	return nil
}

// copyStepAgentFile copies src→dst when the pair differs (the compare is
// size + mtime — after a copy the destination's mtime is set to the
// source's, so an unchanged pair is a no-op on the next sync) and leaves
// dst with the given mode — or the source's own permission bits when
// mode is 0. A readable-but-torn source is a loud refusal.
func copyStepAgentFile(src, dst string, mode os.FileMode) error {
	ss, err := os.Stat(src)
	if err != nil {
		return &StepAgentDirError{Dir: filepath.Dir(dst), Err: err}
	}
	if mode == 0 {
		mode = ss.Mode().Perm()
	}
	if ds, derr := os.Stat(dst); derr == nil && ds.Size() == ss.Size() && ds.ModTime().Equal(ss.ModTime()) {
		return nil // unchanged since the last sync
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return &StepAgentDirError{Dir: filepath.Dir(dst), Err: err}
	}
	if err := atomicfile.Write(dst, raw, mode); err != nil {
		return &StepAgentDirError{Dir: filepath.Dir(dst), Err: err}
	}
	if err := os.Chtimes(dst, ss.ModTime(), ss.ModTime()); err != nil {
		return &StepAgentDirError{Dir: filepath.Dir(dst), Err: err}
	}
	return nil
}

// copyStepAgentTree mirrors src's tree into dst per file (same
// compare/copy discipline as copyStepAgentFile, source modes kept); a
// source file that disappeared is removed from dst. The root itself may
// be a symlink (it is resolved); symlinks INSIDE the tree are skipped —
// never followed, never copied as links.
func copyStepAgentTree(src, dst string) error {
	root, err := filepath.EvalSymlinks(src)
	if err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	src = root
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(src, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		if de.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !de.Type().IsRegular() {
			return nil // a symlink (or other non-regular entry) inside the tree is skipped
		}
		seen[rel] = true
		if cerr := copyStepAgentFile(p, filepath.Join(dst, rel), 0); cerr != nil {
			return cerr
		}
		return nil
	})
	if err != nil {
		if _, ok := err.(*StepAgentDirError); ok {
			return err
		}
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	// remove destination files whose source disappeared
	err = filepath.WalkDir(dst, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dst, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if de.IsDir() {
			return nil // pruned below when empty
		}
		if !seen[rel] {
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	return nil
}

// warnStepAgentDirNoCredentials is the carried-on notice for a source
// without auth.json (the harness adapter may hold credentials elsewhere).
func warnStepAgentDirNoCredentials(src string) {
	fmt.Fprintf(os.Stderr, "runtime: the operator agent dir %s carries no auth.json — the dedicated step agent directory is synced without model service credentials (the harness adapter may hold them elsewhere)\n", src)
}

// SyncStepAgentDir is the daemon-start sync: refresh the allowlisted
// members, then prune everything else (no step process of this daemon is
// running yet, so nothing can be pulled from under one).
func (c *Config) SyncStepAgentDir() error {
	if err := c.refreshStepAgentDir(); err != nil {
		return err
	}
	return pruneStepAgentDir(c.StepAgentDir())
}

// refreshStepAgentDir is the per-spawn re-check: the allowlisted members
// only, never a prune. An EXPLICIT source (flag/env) without a readable
// auth.json refuses; the implicit default without file credentials
// carries on with the warning — the harness adapter may hold its
// credentials elsewhere (the in-repo fake harness is exactly that shape).
func (c *Config) refreshStepAgentDir() error {
	if _, err := os.Stat(filepath.Join(c.OperatorAgentDir, stepAgentAuthFile)); err != nil {
		if !os.IsNotExist(err) {
			return &StepAgentDirError{Dir: c.StepAgentDir(), Err: err}
		}
		if c.OperatorAgentDirExplicit {
			return &StepAgentDirError{Dir: c.StepAgentDir(), Err: fmt.Errorf("the configured operator agent dir %s carries no readable auth.json — a step agent without credentials is a dead end, never a silent degradation", c.OperatorAgentDir)}
		}
		warnStepAgentDirNoCredentials(c.OperatorAgentDir)
	}
	return refreshStepAgentMinimalSet(c.OperatorAgentDir, c.StepAgentDir())
}

// FillDefaults applies the Config's documented defaults to a config the
// daemon assembles by hand (runtime.New does it internally for the
// runtime's own copy; the daemon needs the filled values — the operator
// agent dir and the step-agent session root — before it starts the
// runtime, to sync the dedicated directory at daemon start and to point
// the live-transcript locator at the step processes' session store).
func (c *Config) FillDefaults() { c.fill() }
