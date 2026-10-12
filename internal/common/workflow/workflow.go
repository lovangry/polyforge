// Package workflow defines the workflow-bundle digest contract shared by pf
// (materialization into capsules: digest verification of the copied bundle)
// and aihub (serves bundle bytes, validates digests at claim time).
//
// Design authority: docs/design/polyforge-vnext/01-workflow-skills.md,
// digest rules pinned by the golden vectors in digest_test.go (the retired v0
// implementation lives in git history).
package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileEntry is one file in the bundle digest input set.
type FileEntry struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"` // LF-normalized content SHA-256, lowercase hex
	Bytes  uint64 `json:"bytes"`
}

// ---------------------------------------------------------------------------
// Minimal frontmatter read surface (the F4 lane: candidate-chain
// consumption). The v0 validator's full structure rule set (WF-201, rule
// ids V1–V10) deliberately stayed deleted — aihub owns bundle validation
// at WI creation; the runtime needs only to READ the two fields that drive
// model selection: workflow-level defaults.model_candidates and the
// per-step model_candidates override. Nothing else is parsed, nothing is
// validated here.

// Defaults carries the workflow-level fallback model candidates.
type Defaults struct {
	ModelCandidates []string `yaml:"model_candidates"`
}

// FrontmatterStep is one frontmatter step entry, reduced to the fields
// the read surface needs (the id, the step's V8 KIND, and the per-step
// candidate override). Kind is the polyforge#79 read-surface addition:
// the workspace yaml's step_kinds is kind-KEYED, so the compose needs
// the same closed vocabulary the bundle declares. A read-surface
// addition only — aihub's bundle validation is untouched.
type FrontmatterStep struct {
	ID              string   `yaml:"id"`
	Kind            string   `yaml:"kind"`
	ModelCandidates []string `yaml:"model_candidates"`
}

// Frontmatter is the parsed workflow.md frontmatter (structure reduced to
// the minimal read surface; the Markdown body is never parsed here).
type Frontmatter struct {
	Defaults *Defaults         `yaml:"defaults"`
	Steps    []FrontmatterStep `yaml:"steps"`
}

// SplitFrontmatter splits a Markdown document into its YAML frontmatter
// block and the body that follows the closing fence. The file must begin
// with a `---` line; ok=false when it does not (a frontmatter-less
// workflow.md is legal input to this reader — the candidate chain is then
// simply absent).
func SplitFrontmatter(content []byte) (front, body []byte, ok bool) {
	s := string(content)
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return nil, nil, false
	}
	if strings.TrimRight(s[:nl], " \r") != "---" {
		return nil, nil, false
	}
	rest := s[nl+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, nil, false
	}
	block := rest[:end+1]
	// a closing fence may carry trailing spaces / CRLF noise
	tail := rest[end+4:]
	after := ""
	if nl2 := strings.IndexByte(tail, '\n'); nl2 >= 0 {
		tail, after = tail[:nl2], tail[nl2+1:]
	}
	if strings.TrimRight(tail, " \r") != "" {
		return nil, nil, false // not a fence close (e.g. ---- separator in the body)
	}
	return []byte(block), []byte(after), true
}

// ReadFrontmatter reads one workflow.md and parses its frontmatter with
// the minimal surface above. A missing file, a file with no frontmatter
// block, or an empty one answers (nil, nil) — "no candidate chain" is a
// typed absent condition, never an error. A present-but-torn YAML block
// answers (nil, err): the caller decides whether that is fatal.
func ReadFrontmatter(path string) (*Frontmatter, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	front, _, ok := SplitFrontmatter(raw)
	if !ok || len(strings.TrimSpace(string(front))) == 0 {
		return nil, nil
	}
	var fm Frontmatter
	if err := yaml.Unmarshal(front, &fm); err != nil {
		return nil, err
	}
	return &fm, nil
}

// Digest computes the canonical bundle digest: files sorted by path
// (codepoint order), each contributing path+sha+8-byte LE byte length, then
// the whole buffer is SHA-256'd. Keep byte-for-byte stable so pinned digests
// stay verifiable; golden vectors live in digest_test.go. (This is NOT the
// RFC 8785 canonical-JSON digest of the retired v0 tree.)
func Digest(files []FileEntry) string {
	sorted := make([]FileEntry, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var buf strings.Builder
	for _, f := range sorted {
		norm := strings.ReplaceAll(f.Path, "\r\n", "\n")
		buf.WriteString(norm)
		buf.WriteString(f.Sha256)
		var lenBuf [8]byte
		n := uint64(f.Bytes)
		for i := 0; i < 8; i++ {
			lenBuf[i] = byte(n >> (8 * i))
		}
		buf.Write(lenBuf[:])
	}
	sum := sha256.Sum256([]byte(buf.String()))
	return hex.EncodeToString(sum[:])
}
