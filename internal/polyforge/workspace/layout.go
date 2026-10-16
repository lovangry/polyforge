package workspace

// layout.go: the workspace runtime-artifact convention (polyforge#177).
//
// Anything meant to be SHARED — designs, reviews, decision sheets,
// audits, experience — lives in aihub (a work item's content or a
// memory), never in a file of the workspace. The workspace itself holds
// only what is local:
//
//	<workspace>/
//	  .polyforge.yaml   the human-written model candidates
//	  CLAUDE.md         the workspace notes (polyforge-managed block + human part)
//	  .repo/<repo>/     clones for humans and interactive agents
//	  capsules/<wi>/    work item workspaces (daemon-managed)
//	  .polyforge/       polyforge-generated files only (no documents)
//	  scratch/          disposable output; entries older than 14 days are stale
//
// `pf init` creates scratch/ and writes the convention into CLAUDE.md
// (Init); `pf doctor` warns about entries outside it (StrayEntries) and
// about a CLAUDE.md without the layout section (LayoutSection), and
// reports — or, with --prune-scratch, removes — stale scratch entries
// (StaleScratch / PruneScratch). Nothing here moves or deletes anything
// except PruneScratch, and that only inside scratch/.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The local layout's names.
const (
	ScratchDir   = "scratch"
	CapsulesDir  = "capsules"
	ReposDir     = ".repo"
	GeneratedDir = ".polyforge"
	NotesFile    = "CLAUDE.md"
)

// ScratchMaxAge is the age past which a scratch entry is stale: its
// newest change (mtime or ctime, of the entry and everything under it)
// is older.
const ScratchMaxAge = 14 * 24 * time.Hour

// rootAllowed are the non-dot root entries of the convention. Every
// dot-entry is tolerated too (harnesses and tools own .pi, .claude,
// .mcp.json, .gitignore, .git, …) except the config backups StrayEntries
// names — those are disposable.
var rootAllowed = map[string]bool{
	CapsulesDir:       true,
	ScratchDir:        true,
	NotesFile:         true,
	"CLAUDE.local.md": true,
	"AGENTS.md":       true,
	// A Go workspace file at the root ties the .repo/ clones together.
	"go.work":     true,
	"go.work.sum": true,
}

// generatedAllowed are the entries tolerated under .polyforge/. Both are
// legacy v1 artifacts — nothing in v2 generates them any more (the v1
// repo-map generator and watcher logs are gone); they stay allowed so
// existing workspaces do not warn, and may be removed by hand.
var generatedAllowed = map[string]bool{
	"repo-map": true,
	"watchers": true,
}

// Stray is one entry outside the convention and where it should go.
type Stray struct {
	Path string // workspace-relative, e.g. "notes.md" or ".polyforge/design.md"
	Dest string // the suggested destination (one of the Dest* values)
}

// The suggested destinations of a stray entry.
const (
	DestAihub   = "aihub (its work item's content, or a doc.* memory)"
	DestScratch = "scratch/"
	DestRepo    = ".repo/"
	DestDir     = "scratch/ (share any documents in it through aihub)"
)

// docExt are the extensions treated as prose documents.
var docExt = map[string]bool{".md": true, ".markdown": true, ".txt": true, ".pdf": true, ".html": true, ".rst": true, ".adoc": true}

// StrayEntries lists the entries of the workspace root and of
// .polyforge/ that sit outside the convention, sorted by path. It reads
// only; a missing root answers an error, a missing .polyforge/ nothing.
func StrayEntries(root string) ([]Stray, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Stray
	for _, e := range entries {
		name := e.Name()
		if rootAllowed[name] {
			continue
		}
		if strings.HasPrefix(name, ".") {
			if strings.HasPrefix(name, FileName+".") { // .polyforge.yaml.bak-…: a disposable backup
				out = append(out, Stray{Path: name, Dest: DestScratch})
			}
			continue
		}
		out = append(out, Stray{Path: name, Dest: destFor(filepath.Join(root, name), e)})
	}
	gen, err := os.ReadDir(filepath.Join(root, GeneratedDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		return nil, err
	}
	for _, e := range gen {
		if generatedAllowed[e.Name()] {
			continue
		}
		out = append(out, Stray{Path: GeneratedDir + "/" + e.Name(), Dest: destFor(filepath.Join(root, GeneratedDir, e.Name()), e)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func destFor(path string, e fs.DirEntry) string {
	if e.IsDir() {
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			return DestRepo
		}
		return DestDir
	}
	if docExt[strings.ToLower(filepath.Ext(e.Name()))] {
		return DestAihub
	}
	return DestScratch
}

// ScratchReport is the outcome of a scratch/ scan or prune.
type ScratchReport struct {
	Stale   []string // stale entry names, sorted (PruneScratch: the ones it removed)
	Skipped []string // "name: reason" for entries that could not be judged or removed
}

// scratchRoot answers the resolved path of <root>/scratch, or "" when
// there is none. Anything but a plain directory — a symlink above all,
// whose target may be anything — is refused: it is never scanned or
// pruned.
func scratchRoot(root string) (string, error) {
	p := filepath.Join(root, ScratchDir)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a plain directory (a symlink or a file) — not scanned or pruned", p)
	}
	// the workspace path itself may run through symlinks; removal
	// targets are checked against the resolved scratch dir
	return filepath.EvalSymlinks(p)
}

// StaleScratch answers the scratch/ entries whose newest change — the
// later of mtime and ctime, of the entry and, for a directory, of every
// path under it (symlinks are not followed) — is older than maxAge at
// now. ctime counts because a download or an extracted archive keeps
// its old mtimes yet arrived just now. An entry that cannot be read (or
// vanished meanwhile) is skipped and reported, never fatal. A workspace
// without scratch/ has none.
func StaleScratch(root string, now time.Time, maxAge time.Duration) (ScratchReport, error) {
	var rep ScratchReport
	dir, err := scratchRoot(root)
	if err != nil || dir == "" {
		return rep, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return rep, err
	}
	cutoff := now.Add(-maxAge)
	for _, e := range entries {
		newest, err := newestChange(filepath.Join(dir, e.Name()))
		if err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		if newest.Before(cutoff) {
			rep.Stale = append(rep.Stale, e.Name())
		}
	}
	sort.Strings(rep.Stale)
	return rep, nil
}

// ChangeTime answers a file's ctime (zero where the platform does not
// report it). A variable only so tests — which cannot backdate a ctime —
// can judge by mtime alone.
var ChangeTime = changeTime

// newestChange walks path (without following symlinks) and answers the
// newest mtime or ctime seen.
func newestChange(path string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		for _, t := range []time.Time{info.ModTime(), ChangeTime(info)} {
			if t.After(newest) {
				newest = t
			}
		}
		return nil
	})
	return newest, err
}

// PruneScratch removes the stale scratch/ entries and reports the ones
// it removed. Each entry is re-judged immediately before its removal, so
// one that got fresh content since a preceding StaleScratch report is
// kept. A race remains between that re-check and RemoveAll (a file
// written into a stale directory in that window is removed with it);
// scratch/ is disposable by convention, so that window is accepted.
// Every target is verified to sit directly under the resolved scratch
// dir; nothing outside it is ever touched.
func PruneScratch(root string, now time.Time, maxAge time.Duration) (ScratchReport, error) {
	var rep ScratchReport
	dir, err := scratchRoot(root)
	if err != nil || dir == "" {
		return rep, err
	}
	found, err := StaleScratch(root, now, maxAge)
	if err != nil {
		return rep, err
	}
	rep.Skipped = found.Skipped
	cutoff := now.Add(-maxAge)
	for _, name := range found.Stale {
		target := filepath.Join(dir, name)
		if rel, err := filepath.Rel(dir, target); err != nil || rel != name || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: not an entry directly under %s", name, dir))
			continue
		}
		newest, err := newestChange(target)
		if err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if !newest.Before(cutoff) {
			continue // changed since the scan: no longer stale
		}
		if err := os.RemoveAll(target); err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		rep.Stale = append(rep.Stale, name)
	}
	return rep, nil
}

// The CLAUDE.md markers: the polyforge-managed block (shared with the
// repo listing a workspace may already carry) and, inside it, the
// layout section `pf init` owns.
const (
	managedOpen  = `<!-- polyforge:managed:version="1.0" -->`
	managedOpenP = "<!-- polyforge:managed"
	managedClose = "<!-- /polyforge:managed -->"
	layoutOpen   = "<!-- polyforge:layout -->"
	layoutClose  = "<!-- /polyforge:layout -->"
)

// LayoutBlock is the convention as `pf init` writes it into CLAUDE.md
// (markers included).
func LayoutBlock() string {
	return layoutOpen + `
## Workspace layout (polyforge)

Anything meant to be shared — designs, reviews, decision sheets, audits,
experience — goes to aihub, never into a file here:

- a design → its work item's content while the work item is open
  (` + "`pf file edit <wi> --content @file`" + `), else a memory
  ` + "`pf remember --project <p> --type doc.design --work-item <wi> @file`" + `;
- a review / decision sheet / audit → ` + "`pf remember --project <p> --type doc.review`" + ` /
  ` + "`doc.decision`" + ` / ` + "`doc.audit`" + ` (with ` + "`--work-item`" + ` when it belongs to one);
- experience → ` + "`experience.*`" + ` / ` + "`fact.*`" + ` memories.

Locally only:

- ` + "`.repo/<repo>/`" + ` — repository clones;
- ` + "`capsules/<wi>/`" + ` — work item workspaces (the daemon manages them);
- ` + "`scratch/`" + ` — disposable output (logs, briefs, e2e output, downloads);
  ` + "`pf doctor`" + ` reports entries older than 14 days, ` + "`pf doctor --prune-scratch`" + ` removes them;
- ` + "`.polyforge/`" + ` — polyforge-generated files only.

Never write documents into the workspace root or ` + "`.polyforge/`" + `;
` + "`pf doctor`" + ` warns about anything else found there.
` + layoutClose + "\n"
}

// InitResult reports what Init did.
type InitResult struct {
	Workspace string `json:"workspace"`
	Scratch   string `json:"scratch"`   // created | exists
	NotesFile string `json:"claude_md"` // created | inserted | updated | unchanged
}

// Init gives an existing workspace directory its local layout: scratch/
// and the layout section of CLAUDE.md. It is idempotent and never
// touches CLAUDE.md outside the layout section: a file without one gets
// it inside its polyforge-managed block (or a new managed block on top);
// a file with one gets that section replaced. CLAUDE.md is written
// atomically (temp file + rename, mode kept); a symlinked CLAUDE.md has
// its target written, the link stays.
func Init(root string) (InitResult, error) {
	res := InitResult{Workspace: root}
	st, err := os.Stat(root)
	if err != nil {
		return res, err
	}
	if !st.IsDir() {
		return res, fmt.Errorf("%s is not a directory", root)
	}
	scratch := filepath.Join(root, ScratchDir)
	switch st, err := os.Lstat(scratch); {
	case err == nil && st.IsDir():
		res.Scratch = "exists"
	case err == nil:
		return res, fmt.Errorf("%s exists and is not a plain directory (a symlink or a file)", scratch)
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(scratch, 0o755); err != nil {
			return res, err
		}
		res.Scratch = "created"
	default:
		return res, err
	}

	notes := filepath.Join(root, NotesFile)
	if fi, err := os.Lstat(notes); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(notes)
		if err != nil {
			return res, fmt.Errorf("%s is a symlink that does not resolve: %w", notes, err)
		}
		notes = target
	}
	raw, err := os.ReadFile(notes)
	var mode fs.FileMode = 0o644
	switch {
	case errors.Is(err, fs.ErrNotExist):
		raw = nil
	case err != nil:
		return res, err
	default:
		if fi, serr := os.Stat(notes); serr == nil {
			mode = fi.Mode().Perm()
		}
	}
	text, how, err := upsertLayout(string(raw), raw == nil)
	if err != nil {
		return res, fmt.Errorf("%s: %w", notes, err)
	}
	res.NotesFile = how
	if how == "unchanged" {
		return res, nil
	}
	return res, writeAtomic(notes, []byte(text), mode)
}

// writeAtomic replaces path with data through a temp file in the same
// directory (fsync, chmod, rename).
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// LayoutSection reports whether the workspace's CLAUDE.md exists and,
// when it does, whether its polyforge-managed block carries the layout
// section (the same placement rule as upsertLayout: a layout marker
// outside the managed block does not count). A missing CLAUDE.md is
// (false, false, nil): there is nothing `pf init` would be repairing.
func LayoutSection(root string) (exists, has bool, err error) {
	raw, err := os.ReadFile(filepath.Join(root, NotesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	text := string(raw)
	i := strings.Index(text, managedOpenP)
	if i < 0 {
		return true, false, nil
	}
	inner := text[i:]
	if j := strings.Index(inner, managedClose); j >= 0 {
		inner = inner[:j]
	}
	return true, strings.Contains(inner, layoutOpen), nil
}

// upsertLayout answers the CLAUDE.md text with the layout section in
// place and how it got there. The layout markers count only inside the
// polyforge-managed block (one quoted in the human part is ignored);
// more than one layout section there is refused. A CRLF file gets a CRLF
// section.
func upsertLayout(text string, absent bool) (string, string, error) {
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
	}
	block := strings.ReplaceAll(LayoutBlock(), "\n", nl)
	if absent {
		return managedOpen + nl + block + managedClose + nl, "created", nil
	}
	i := strings.Index(text, managedOpenP)
	if i < 0 {
		sep := nl
		if text == "" {
			sep = ""
		}
		return managedOpen + nl + block + managedClose + nl + sep + text, "inserted", nil
	}
	j := strings.Index(text[i:], managedClose)
	if j < 0 {
		return "", "", fmt.Errorf("the polyforge-managed block has no closing %s — fix the file by hand, then re-run", managedClose)
	}
	at := i + j // the managed block's closing marker
	inner := text[i:at]
	switch n := strings.Count(inner, layoutOpen); {
	case n > 1:
		return "", "", fmt.Errorf("the polyforge-managed block carries %d %s sections — keep one, then re-run", n, layoutOpen)
	case n == 1:
		s := i + strings.Index(inner, layoutOpen)
		k := strings.Index(text[s:at], layoutClose)
		if k < 0 {
			return "", "", fmt.Errorf("%s has no closing %s inside the polyforge-managed block — fix the file by hand, then re-run", layoutOpen, layoutClose)
		}
		end := s + k + len(layoutClose)
		if strings.HasPrefix(text[end:], nl) {
			end += len(nl)
		}
		if text[s:end] == block {
			return text, "unchanged", nil
		}
		return text[:s] + block + text[end:], "updated", nil
	}
	if strings.Contains(inner, layoutClose) {
		return "", "", fmt.Errorf("a stray %s sits in the polyforge-managed block — fix the file by hand, then re-run", layoutClose)
	}
	before := text[:at]
	if !strings.HasSuffix(before, "\n") {
		before += nl
	}
	if !strings.HasSuffix(before, nl+nl) {
		before += nl
	}
	return before + block + text[at:], "inserted", nil
}
