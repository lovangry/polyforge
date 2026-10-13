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
//	auth.json     model service credentials — COPY from the operator
//	              agent dir (mode 0600, daemon-owned)
//	models.json   model service configuration — COPY, only when the
//	              source has one
//	agents/       subagent definition files — recursive COPY of the
//	              source tree; absent source dir ⇒ absent here
//
// Copies, not links: a snapshot never follows later owner edits (a global
// reinstall included), and no symlink makes the dedicated dir a window
// onto the whole operator dir. Sync happens at daemon start AND is
// re-checked before every spawn; a file that is present in the source but
// cannot be READ (torn, unreadable) or a destination that cannot be
// written is a LOUD typed error — the spawn is refused, never silently
// degraded. A source with no auth.json at all (the harness adapter may
// hold its credentials elsewhere; the in-repo fake harness needs none) is
// a warning, not a refusal — but an explicitly configured source
// (flag/env) whose auth.json is missing IS a refusal: the operator named
// the credentials' home and it is not there.
//
// Deliberately ABSENT (the isolation set): settings.json (user-level
// packages), keybindings.json, mcp.json, AGENTS.md/CLAUDE.md/SYSTEM.md/
// APPEND_SYSTEM.md, extensions/, skills/, prompts/, themes/ — the refresh
// sweeps anything else it finds under the directory, so the minimal-set
// invariant is restored on every sync.
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

// syncStepAgentMinimalSet (re)builds the dedicated directory at dst as a
// minimal snapshot of src and leaves it ready for a spawn: exactly
// auth.json / models.json / agents/, each copied only when the source
// presents it, and nothing else surviving under dst.
func syncStepAgentMinimalSet(src, dst string) error {
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
	// models.json: only when the source has one; a stale destination
	// copy is removed so "only if present in the source" holds both
	// ways.
	srcModels := filepath.Join(src, stepAgentModelsFile)
	dstModels := filepath.Join(dst, stepAgentModelsFile)
	if _, err := os.Stat(srcModels); err == nil {
		if err := copyStepAgentFile(srcModels, dstModels, 0o644); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return &StepAgentDirError{Dir: dst, Err: err}
	} else if err := os.Remove(dstModels); err != nil && !os.IsNotExist(err) {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	// agents/: recursive copy per-file; absent source dir ⇒ the
	// destination dir is absent too.
	srcAgents := filepath.Join(src, stepAgentAgentsDir)
	dstAgents := filepath.Join(dst, stepAgentAgentsDir)
	if _, err := os.Stat(srcAgents); err == nil {
		if err := copyStepAgentTree(srcAgents, dstAgents); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return &StepAgentDirError{Dir: dst, Err: err}
	} else if err := os.RemoveAll(dstAgents); err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	// minimal-set invariant: nothing but the enumerated set survives a
	// sync, whatever put an extra entry there.
	ents, err := os.ReadDir(dst)
	if err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	for _, e := range ents {
		switch e.Name() {
		case stepAgentAuthFile, stepAgentModelsFile, stepAgentAgentsDir:
		default:
			if err := os.RemoveAll(filepath.Join(dst, e.Name())); err != nil {
				return &StepAgentDirError{Dir: dst, Err: err}
			}
		}
	}
	return nil
}

// copyStepAgentFile copies src→dst when the pair differs (the compare is
// size + mtime — after a copy the destination's mtime is set to the
// source's, so an unchanged pair is a no-op on the next sync) and leaves
// dst with the given mode. A readable-but-torn source is a loud refusal.
func copyStepAgentFile(src, dst string, mode os.FileMode) error {
	ss, err := os.Stat(src)
	if err != nil {
		return &StepAgentDirError{Dir: filepath.Dir(dst), Err: err}
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
// compare/copy discipline as copyStepAgentFile); a source file that
// disappeared is removed from dst.
func copyStepAgentTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return &StepAgentDirError{Dir: dst, Err: err}
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(src, func(p string, de os.DirEntry, err error) error {
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
			return nil // a link in the source is followed as its bytes, never copied as a link
		}
		seen[rel] = true
		if cerr := copyStepAgentFile(p, filepath.Join(dst, rel), 0o644); cerr != nil {
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

// SyncStepAgentDir re-checks the dedicated directory against its source
// (the per-spawn half of the sync contract; the daemon-start half is the
// same call). An EXPLICIT source (flag/env) without a readable auth.json
// refuses; the implicit default without file credentials carries on with
// the warning — the harness adapter may hold its credentials elsewhere
// (the in-repo fake harness is exactly that shape).
func (c *Config) SyncStepAgentDir() error {
	authMissing := false
	if _, err := os.Stat(filepath.Join(c.OperatorAgentDir, stepAgentAuthFile)); err != nil {
		if !os.IsNotExist(err) {
			return &StepAgentDirError{Dir: c.StepAgentDir(), Err: err}
		}
		authMissing = true
	}
	if authMissing {
		if c.OperatorAgentDirExplicit {
			return &StepAgentDirError{Dir: c.StepAgentDir(), Err: fmt.Errorf("the configured operator agent dir %s carries no readable auth.json — a step agent without credentials is a dead end, never a silent degradation", c.OperatorAgentDir)}
		}
		warnStepAgentDirNoCredentials(c.OperatorAgentDir)
	}
	return syncStepAgentMinimalSet(c.OperatorAgentDir, c.StepAgentDir())
}

// FillDefaults applies the Config's documented defaults to a config the
// daemon assembles by hand (runtime.New does it internally for the
// runtime's own copy; the daemon needs the filled values — the operator
// agent dir and the step-agent session root — before it starts the
// runtime, to sync the dedicated directory at daemon start and to point
// the live-transcript locator at the step processes' session store).
func (c *Config) FillDefaults() { c.fill() }
