package capsule

// safety.go: the Revision recovery write-path safety rules (03 §6.3, fix
// B07). String-level ".." checks are NOT sufficient: a symlink such as
// repos/ieops/evil -> ../../runtime-state contains no ".." beyond a legal
// relative form, yet following its parent directory would land writes
// outside the sync root. The authoritative rules:
//
//  1. Per-level parent check: for every write target, lstat every path
//     component from the capsule root down; each intermediate level must be
//     a real directory (never a symlink). One symlink anywhere in the chain
//     → reject the whole operation with CAP-406, do not resolve-and-continue.
//  2. Symlink targets declared by Revision data must resolve INSIDE the
//     sync root, and never into runtime-state/ or sync/ (03 §6.3 rule 2).
//  3. Whole-operation atomicity is the CALLER's duty (apply_full_replace
//     rolls back on any CAP-406; 03 §6.3 rule 3).

import (
	"os"
	"path/filepath"
	"strings"
)

// forbiddenWritePrefixes are capsule areas no recovery write may touch:
// harness/ is never restored from history (05 §7.4), the materialization
// targets are rebuilt by the WI Agent step 0 (SYNC-INV-10), sync/ and
// capsule.json are not part of the sync surface at all.
var forbiddenWritePrefixes = []string{
	"harness/",
	".pi/", ".claude/",
	"sync/",
	"capsule.json",
	"workflow.md", "workflow/",
}

// ValidateWriteTarget checks a recovery write target rel (slash-separated,
// relative to the capsule root) against 03 §6.3 rules 1 and 2.
//
// For a plain file target: every existing parent level under the capsule
// root must be a real directory (lstat, not stat — a symlink level aborts
// with CAP-406 before anything is written through it).
//
// For a symlink target: target must be a RELATIVE path that, resolved
// against the symlink's own directory, stays inside the capsule root and
// never lands under runtime-state/, sync/, harness/ or any other forbidden
// prefix.
func (c *Capsule) ValidateWriteTarget(rel, symlinkTarget string) error {
	if err := validateRelPath(rel); err != nil {
		return err
	}
	for _, p := range forbiddenWritePrefixes {
		if rel == strings.TrimSuffix(p, "/") || strings.HasPrefix(rel, p) {
			return errf(ErrSymlinkEscape, "recovery write into excluded area %q: %s", p, rel)
		}
	}
	// Rule 1: walk every parent level under the capsule root.
	parts := strings.Split(rel, "/")
	cur := c.Dir
	for i := 0; i < len(parts)-1; i++ {
		cur = filepath.Join(cur, parts[i])
		fi, err := os.Lstat(cur)
		if errors_isNotExist(err) {
			continue // level does not exist yet: apply will create it as a dir
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errf(ErrSymlinkEscape, "parent level %q is a symlink", strings.Join(parts[:i+1], "/"))
		}
		if !fi.IsDir() {
			return errf(ErrSymlinkEscape, "parent level %q is not a directory", strings.Join(parts[:i+1], "/"))
		}
	}
	if symlinkTarget == "" {
		return nil
	}
	// Rule 2: declared symlink target must resolve inside the capsule root.
	return c.ValidateSymlinkTarget(rel, symlinkTarget)
}

// ValidateSymlinkTarget resolves target relative to the symlink's own
// directory and requires the result to stay inside the capsule root and out
// of the forbidden areas (03 §6.3 rule 2).
func (c *Capsule) ValidateSymlinkTarget(rel, target string) error {
	if target == "" {
		return errf(ErrSymlinkEscape, "symlink %s has empty target", rel)
	}
	if filepath.IsAbs(target) || strings.HasPrefix(target, "/") {
		return errf(ErrSymlinkEscape, "symlink %s target %q is absolute", rel, target)
	}
	if err := validateRelPath(target); err != nil {
		return errf(ErrSymlinkEscape, "symlink %s target %q: %v", rel, target, err)
	}
	symlinkDir := filepath.Dir(filepath.Join(c.Dir, filepath.FromSlash(rel)))
	resolved := filepath.Clean(filepath.Join(symlinkDir, filepath.FromSlash(target)))
	root := filepath.Clean(c.Dir)
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return errf(ErrSymlinkEscape, "symlink %s target %q resolves outside the capsule root", rel, target)
	}
	relResolved, err := filepath.Rel(root, resolved)
	if err != nil {
		return errf(ErrSymlinkEscape, "symlink %s target %q: %v", rel, target, err)
	}
	relResolved = filepath.ToSlash(relResolved)
	if relResolved == "." {
		return errf(ErrSymlinkEscape, "symlink %s target is the capsule root itself", rel)
	}
	for _, p := range forbiddenWritePrefixes {
		prefix := strings.TrimSuffix(p, "/")
		if relResolved == prefix || strings.HasPrefix(relResolved, p) {
			return errf(ErrSymlinkEscape, "symlink %s target resolves into excluded area %q", rel, p)
		}
	}
	return nil
}

// IsExcludedFromSync reports whether a capsule-relative path is OUTSIDE the
// sync surface (05 §4.1 / SYNC-INV-10): used by the scan (never package such
// paths), the watcher (never observe) and the recovery (never write back).
// rel must be slash-separated relative to the capsule root; paths under
// repos/<name>/.git/ are excluded; repos/<name>/inventory.json IS synced
// (it is a file in the repos tree like any other — only .git is excluded).
func IsExcludedFromSync(rel string) bool {
	if rel == "" || rel == "capsule.json" {
		return true
	}
	for _, p := range []string{"sync/", ".pi/", ".claude/"} {
		if strings.HasPrefix(rel, p) || rel == strings.TrimSuffix(p, "/") {
			return true
		}
	}
	if rel == "workflow.md" || strings.HasPrefix(rel, "workflow/") {
		return true
	}
	if strings.HasPrefix(rel, "harness/") {
		// harness/<adapter>/session.json is the only optional-sync member;
		// session-data/ and everything else are excluded (05 §4.1).
		rest := strings.TrimPrefix(rel, "harness/")
		if strings.Count(rest, "/") == 1 && strings.HasSuffix(rest, "/session.json") {
			return false
		}
		return true
	}
	// .git inside repos/<name>/: excluded. The pattern is repos/<n>/.git/**.
	if strings.HasPrefix(rel, "repos/") {
		rest := strings.TrimPrefix(rel, "repos/")
		idx := strings.Index(rest, "/")
		if idx > 0 {
			tail := rest[idx+1:]
			if tail == ".git" || strings.HasPrefix(tail, ".git/") {
				return true
			}
		}
	}
	return false
}

func errors_isNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}
