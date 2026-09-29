// Package workflow defines the workflow-bundle schema shared by pf
// (materialization into capsules) and aihub (serves bundle bytes, validates
// digests at claim time).
//
// Design authority: docs/design/polyforge-vnext/01-workflow-skills.md,
// frozen digest rules from the v0 implementation (/v0/internal/workflow).
package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Frontmatter is the parsed YAML-like header of workflow.md (01 §4).
// Field names are wire-stable.
type Frontmatter struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Steps       []Step          `json:"steps"`
	Defaults    Defaults        `json:"defaults"`
	Rewind      Rewind          `json:"rewind"`
}

// Step is one step definition in a bundle.
type Step struct {
	ID             string          `json:"id"`
	Skill          string          `json:"skill"`              // agents/<skill>.md body
	Title          string          `json:"title,omitempty"`
	OnFail         OnFail          `json:"on_fail"`
	Budget         *StepBudget     `json:"budget,omitempty"`    // step-level overrides
	ModelCandidates []ModelCandidate `json:"model_candidates,omitempty"`
}

// OnFail declares the failure policy of a step (01 §6).
type OnFail struct {
	Action  string `json:"action"` // rewind|fail|continue
	RewindTo string `json:"rewind_to,omitempty"`
	Note    string `json:"note,omitempty"`
}

// StepBudget bounds a single step.
type StepBudget struct {
	USD        *float64 `json:"usd,omitempty"`
	WallMs     *int64   `json:"wall_ms,omitempty"`
	Tokens     *int64   `json:"tokens,omitempty"`
}

// ModelCandidate is one (harness, model, effort) triple tried in order on
// infrastructure failure (01 §6.3: provider/network failure advances the
// ordinal; business FAIL never does).
type ModelCandidate struct {
	Harness string  `json:"harness"` // pi|cc
	Model   string  `json:"model"`
	Effort  string  `json:"effort,omitempty"`
}

// Defaults carries bundle-wide defaults merged with step-level overrides.
type Defaults struct {
	ModelCandidates []ModelCandidate `json:"model_candidates,omitempty"`
	Budget          *StepBudget      `json:"budget,omitempty"`
}

// Rewind declares the rewind budget of the flow (01 §6.2).
type Rewind struct {
	Max   int64 `json:"max"`
	Budget json.RawMessage `json:"budget,omitempty"`
}

// FileEntry is one file in the bundle digest input set.
type FileEntry struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"` // LF-normalized content SHA-256, lowercase hex
	Bytes  uint64 `json:"bytes"`
}

// Digest computes the canonical bundle digest: files sorted by path
// (codepoint order), each contributing len(path)+path+len(sha)+sha+8-byte
// LE byte length, then the whole buffer is SHA-256'd (frozen v0 algorithm —
// golden vectors live in /v0/internal/workflow/digest tests; keep byte-for-
// byte compatible so migrated chains stay verifiable).
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

// ValidationError is a bundle validation failure.
type ValidationError struct {
	Path string
	Msg  string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Msg)
}

// RuleError is a cross-field rule violation (01 §4 rules).
type RuleError struct {
	Rule string
	Msg  string
}

func (e *RuleError) Error() string { return e.Rule + ": " + e.Msg }
