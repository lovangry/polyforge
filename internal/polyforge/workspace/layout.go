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
//	  .polyforge/       polyforge-generated files only (repo-map, watcher state)
//	  scratch/          disposable output; entries older than 14 days are stale
//
// `pf init` creates scratch/ and writes the convention into CLAUDE.md
// (Init); `pf doctor` warns about entries outside it (StrayEntries) and
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
// newest modification (the entry and everything under it) is older.
const ScratchMaxAge = 14 * 24 * time.Hour

// rootAllowed are the non-dot root entries of the convention. Every
// dot-entry is tolerated too (harnesses and tools own .pi, .claude,
// .mcp.json, .gitignore, .git, …) except the config backups rootStray
// names — those are disposable.
var rootAllowed = map[string]bool{
	CapsulesDir:       true,
	ScratchDir:        true,
	NotesFile:         true,
	"CLAUDE.local.md": true,
	"AGENTS.md":       true,
}

// generatedAllowed are the entries polyforge itself generates under
// .polyforge/.
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

// StaleScratch answers the scratch/ entries (names, sorted) whose newest
// modification — the entry's own and, for a directory, every path under
// it (symlinks are not followed) — is older than maxAge at now. A
// workspace without scratch/ has none.
func StaleScratch(root string, now time.Time, maxAge time.Duration) ([]string, error) {
	dir := filepath.Join(root, ScratchDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-maxAge)
	var stale []string
	for _, e := range entries {
		newest, err := newestMtime(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if newest.Before(cutoff) {
			stale = append(stale, e.Name())
		}
	}
	sort.Strings(stale)
	return stale, nil
}

// newestMtime walks path (without following symlinks) and answers the
// newest modification time seen.
func newestMtime(path string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}

// PruneScratch removes the named scratch/ entries (as StaleScratch
// answered them) and answers the ones it removed. Only plain names are
// accepted: nothing outside scratch/ is ever touched.
func PruneScratch(root string, names []string) ([]string, error) {
	var removed []string
	for _, name := range names {
		if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
			return removed, fmt.Errorf("prune scratch: %q is not a scratch entry name", name)
		}
		if err := os.RemoveAll(filepath.Join(root, ScratchDir, name)); err != nil {
			return removed, err
		}
		removed = append(removed, name)
	}
	return removed, nil
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
// a file with one gets that section replaced.
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
	switch st, err := os.Stat(scratch); {
	case err == nil && st.IsDir():
		res.Scratch = "exists"
	case err == nil:
		return res, fmt.Errorf("%s exists and is not a directory", scratch)
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(scratch, 0o755); err != nil {
			return res, err
		}
		res.Scratch = "created"
	default:
		return res, err
	}

	notes := filepath.Join(root, NotesFile)
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
	return res, os.WriteFile(notes, []byte(text), mode)
}

// upsertLayout answers the CLAUDE.md text with the layout section in
// place and how it got there.
func upsertLayout(text string, absent bool) (string, string, error) {
	block := LayoutBlock()
	if absent {
		return managedOpen + "\n" + block + managedClose + "\n", "created", nil
	}
	if i := strings.Index(text, layoutOpen); i >= 0 {
		j := strings.Index(text[i:], layoutClose)
		if j < 0 {
			return "", "", fmt.Errorf("%s has no closing %s — fix the file by hand, then re-run", layoutOpen, layoutClose)
		}
		end := i + j + len(layoutClose)
		if end < len(text) && text[end] == '\n' {
			end++
		}
		if text[i:end] == block {
			return text, "unchanged", nil
		}
		return text[:i] + block + text[end:], "updated", nil
	}
	if i := strings.Index(text, managedOpenP); i >= 0 {
		j := strings.Index(text[i:], managedClose)
		if j < 0 {
			return "", "", fmt.Errorf("the polyforge-managed block has no closing %s — fix the file by hand, then re-run", managedClose)
		}
		at := i + j
		before := text[:at]
		if !strings.HasSuffix(before, "\n") {
			before += "\n"
		}
		if !strings.HasSuffix(before, "\n\n") {
			before += "\n"
		}
		return before + block + text[at:], "inserted", nil
	}
	sep := "\n"
	if text == "" {
		sep = ""
	}
	return managedOpen + "\n" + block + managedClose + "\n" + sep + text, "inserted", nil
}
