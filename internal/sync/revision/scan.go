package revision

// scan.go: the full scan of a capsule's sync surface (05 §4.1 / §6 step 3).
//
// Sync roots: runtime-state/execution.json, artifacts/, logs/ (incl.
// logs/session/), repos/<name>/ working trees (git-aware: union of tracked
// and untracked files, gitignored excluded, .git excluded), plus the
// optional harness/<adapter>/session.json. Excluded (SYNC-INV-10 / 05 §4.1):
// harness/<adapter>/session-data/, .pi/agents/, .claude/agents/, workflow.md,
// workflow/, sync/, capsule.json, repos/*/.git.
//
// SCAN-INV-6 ORDERING: the caller fixes data/artifact files FIRST and reads
// runtime-state/execution.json LAST — BuildOps enforces the manifest-level
// order; the scan itself returns an ordered structure so the ordering
// contract is carried in the data, not in caller discipline.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/workflow/digest"
)

// FileInfo describes one live file of the sync surface.
type FileInfo struct {
	Sha256    string // digest of ORIGINAL content (or of the symlink target text)
	Bytes     int64
	Mode      int64 // POSIX permission bits; 0 for symlinks
	IsSymlink bool
	Target    string // symlink target (relative form as stored)
}

// ScanResult is the frozen-window scan output. Files is the full live set;
// ReadOrder records the SYNC-INV-6 ordering (execution.json last).
type ScanResult struct {
	Files map[string]FileInfo // capsule-relative slash path → info
}

// ScanCapsule enumerates and digests the whole sync surface under
// capsuleDir. It reads each file's bytes once (content digest) — the freeze
// window (05 §2.7) is held by the caller for the duration.
func ScanCapsule(capsuleDir string) (*ScanResult, error) {
	res := &ScanResult{Files: map[string]FileInfo{}}
	add := func(rel string, fi FileInfo) {
		res.Files[rel] = fi
	}

	// artifacts/ and logs/: plain recursive walks (symlinks recorded, not
	// followed).
	for _, root := range []string{"artifacts", "logs"} {
		if err := walkFiles(capsuleDir, root, add); err != nil {
			return nil, err
		}
	}
	// repos/<name>/: git-aware when a .git exists, plain walk otherwise.
	reposDir := filepath.Join(capsuleDir, "repos")
	entries, err := os.ReadDir(reposDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		repoDir := filepath.Join(reposDir, name)
		if _, gerr := os.Stat(filepath.Join(repoDir, ".git")); gerr == nil {
			if err := walkGitRepo(capsuleDir, name, repoDir, add); err != nil {
				return nil, err
			}
		} else {
			if err := walkFiles(capsuleDir, "repos/"+name, add); err != nil {
				return nil, err
			}
		}
		// The aggregate inventory snapshot is a synced file in its own right.
		invRel := "repos/inventory.json"
		if fi, ok, ferr := statFile(capsuleDir, invRel); ferr == nil && ok {
			add(invRel, fi)
		}
	}
	// runtime-state/execution.json: the sync surface's single member of
	// runtime-state/ (05 §4.1). Other files under runtime-state/ (tmp
	// leftovers) are not synced.
	execRel := "runtime-state/execution.json"
	if fi, ok, err := statFile(capsuleDir, execRel); err == nil && ok {
		add(execRel, fi)
	}
	// harness/<adapter>/session.json: the optional-sync member (05 §4.1).
	harnessDir := filepath.Join(capsuleDir, "harness")
	if adapters, aerr := os.ReadDir(harnessDir); aerr == nil {
		for _, a := range adapters {
			if !a.IsDir() {
				continue
			}
			rel := "harness/" + a.Name() + "/session.json"
			if fi, ok, err := statFile(capsuleDir, rel); err == nil && ok {
				add(rel, fi)
			}
		}
	}
	return res, nil
}

// statFile digests one capsule-relative file if it exists and is not
// excluded.
func statFile(capsuleDir, rel string) (FileInfo, bool, error) {
	if capsule.IsExcludedFromSync(rel) {
		return FileInfo{}, false, nil
	}
	abs := filepath.Join(capsuleDir, filepath.FromSlash(rel))
	lst, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return FileInfo{}, false, nil
	}
	if err != nil {
		return FileInfo{}, false, err
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		tgt, terr := os.Readlink(abs)
		if terr != nil {
			return FileInfo{}, false, terr
		}
		return SymlinkFileInfo(tgt), true, nil
	}
	if !lst.Mode().IsRegular() {
		return FileInfo{}, false, nil // sockets/fifos/devices are not synced
	}
	fi, err := digestFile(abs)
	if err != nil {
		return FileInfo{}, false, err
	}
	return fi, true, nil
}

func walkFiles(capsuleDir, root string, add func(string, FileInfo)) error {
	base := filepath.Join(capsuleDir, filepath.FromSlash(root))
	return filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if p == base {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, capsuleDir+string(os.PathSeparator)))
		if capsule.IsExcludedFromSync(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			tgt, terr := os.Readlink(p)
			if terr != nil {
				return terr
			}
			add(rel, SymlinkFileInfo(tgt))
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		fi, derr := digestFile(p)
		if derr != nil {
			return derr
		}
		add(rel, fi)
		return nil
	})
}

// walkGitRepo enumerates a repo working tree via git: the union of tracked
// and untracked-not-ignored paths (05 §4.1: "git status --porcelain 列出的
// 全部路径（含 untracked）与 git ls-files 已提交文件的并集"), .git excluded.
func walkGitRepo(capsuleDir, name, repoDir string, add func(string, FileInfo)) error {
	cmd := exec.Command("git", "-C", repoDir, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if err != nil {
		// Not a usable git work tree (e.g. bare husk): fall back to a plain
		// walk so the scan never silently under-reports.
		return walkFiles(capsuleDir, "repos/"+name, add)
	}
	paths := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	sort.Strings(paths)
	for _, relInRepo := range paths {
		if relInRepo == "" {
			continue
		}
		rel := "repos/" + name + "/" + relInRepo
		if capsule.IsExcludedFromSync(rel) {
			continue
		}
		fi, ok, serr := statFile(capsuleDir, rel)
		if serr != nil {
			return serr
		}
		if ok {
			add(rel, fi)
		}
	}
	return nil
}

func digestFile(p string) (FileInfo, error) {
	f, err := os.Open(p)
	if err != nil {
		return FileInfo{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return FileInfo{}, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return FileInfo{}, err
	}
	sum := h.Sum(nil)
	mode := int64(st.Mode().Perm())
	return FileInfo{Sha256: hex.EncodeToString(sum), Bytes: n, Mode: mode}, nil
}

// SymlinkFileInfo is the canonical FileInfo of one symlink member, used by
// BOTH the scan and the §7.4 recovery replay (05 §6.1: a symlink's
// aggregate-digest contribution is sha256 of its TARGET TEXT with mode 0;
// recovery must seed the publish state with byte-identical values or every
// post-recovery flush would report a false "changed" diff).
func SymlinkFileInfo(target string) FileInfo {
	sum := sha256.Sum256([]byte(target))
	return FileInfo{Sha256: hexOf(sum), Bytes: int64(len(target)), IsSymlink: true, Target: target}
}

func hexOf(sum [32]byte) string {
	const hexDigits = "0123456789abcdef"
	buf := make([]byte, 64)
	for i, b := range sum {
		buf[i*2] = hexDigits[b>>4]
		buf[i*2+1] = hexDigits[b&0x0f]
	}
	return string(buf)
}

// ---------------------------------------------------------------------------
// Aggregate digests (05 §6.1 flush-evidence inputs)

// BytesDigestEntry is one element of the bytes_digest input list.
type BytesDigestEntry struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Mode   int64  `json:"mode"`
}

// BytesDigest is the aggregate digest of the ENTIRE sync surface scanned in
// a freeze window: RFC 8785 canonical JSON of the path-sorted
// (path, sha256, mode) list, sha256, hex. Symlink entries contribute
// sha256(target text) with mode 0 (recorded resolution of the design's
// "全部同步面字节").
func (s *ScanResult) BytesDigest() (string, error) {
	entries := make([]BytesDigestEntry, 0, len(s.Files))
	for path, fi := range s.Files {
		entries = append(entries, BytesDigestEntry{Path: path, Sha256: fi.Sha256, Mode: fi.Mode})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return digestOfList(entries)
}

// WorktreeRepoState is one inventory repo's record in the worktree_state
// input list (05 §6.1: {name, base, head, dirty_count}).
type WorktreeRepoState struct {
	Name       string `json:"name"`
	Base       string `json:"base"`
	Head       string `json:"head"`
	DirtyCount int64  `json:"dirty_count"`
}

// WorktreeDigest digests the per-repo git state of the capsule's inventory
// repos. With no repos the input is the empty list (EMPTY_SYNC_BYTES_DIGEST).
func WorktreeDigest(capsuleDir string) (string, error) {
	states, err := WorktreeStates(capsuleDir)
	if err != nil {
		return "", err
	}
	return digestOfList(states)
}

// WorktreeStates collects {name, base, head, dirty_count} per inventory repo
// by reading repos/inventory.json and each repo's git state.
func WorktreeStates(capsuleDir string) ([]WorktreeRepoState, error) {
	out := []WorktreeRepoState{}
	raw, err := os.ReadFile(filepath.Join(capsuleDir, "repos", "inventory.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	var snap struct {
		Repos []struct {
			Name string `json:"name"`
			Base string `json:"base"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	for _, r := range snap.Repos {
		repoDir := filepath.Join(capsuleDir, "repos", r.Name)
		st := WorktreeRepoState{Name: r.Name, Base: r.Base}
		if out1, err := gitOutput(repoDir, "rev-parse", "HEAD"); err == nil {
			st.Head = strings.TrimSpace(out1)
		}
		if out2, err := gitOutput(repoDir, "status", "--porcelain"); err == nil {
			for _, line := range strings.Split(out2, "\n") {
				if strings.TrimSpace(line) != "" {
					st.DirtyCount++
				}
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

func digestOfList(v any) (string, error) {
	canonical, err := digest.MarshalCanonical(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// to32 converts a sha256 slice sum into its fixed array form.
func to32(sum []byte) [32]byte {
	var a [32]byte
	copy(a[:], sum)
	return a
}
