// Package memory is the v1 Core-side memory store: FTS5-trigram search,
// authoritative memory.md files and the reconcile procedure.
//
// Semantics are ported from the pilot-validated v0 implementation
// (v0/internal/memory in git history, design authority 06-memory-search.md): the
// three-step write order (tmp+fsync → Core tx → rename), the INV-7f
// serving gate (row content_sha verified against the live file before
// anything is served), file-wins reconciliation (INV-7c) and the
// delete-then-insert FTS invariant. v1 drops the derived-HTML chain and
// the share links; the reconcile scenarios kept are exactly the three the
// lane requires: orphan-row rollback, authority-file backfill and
// redacted forward-completion.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIError mirrors the other aihub packages.
type APIError struct {
	Code   string
	Reason string
	Detail string
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return e.Code + " " + e.Reason + ": " + e.Detail
	}
	return e.Code + ": " + e.Detail
}

func errf(code, reason, format string, args ...any) *APIError {
	return &APIError{Code: code, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Visibility tiers (06 §4.2; membership-first filtering is the server's).
var visibilities = map[string]bool{"admin": true, "private": true, "project": true, "public": true, "team": true}

// maxContentBytes is the MEM-413 body limit (≤ 1 MiB).
const maxContentBytes = 1 << 20

// frontmatter is the §4.2 field set, one-to-one mirror of the memories
// columns, serialized in this exact order (deterministic authority bytes).
type frontmatter struct {
	ID             string   `yaml:"id"`
	Type           string   `yaml:"type"`
	OwnerUserID    string   `yaml:"owner_user_id"`
	Title          *string  `yaml:"title,omitempty"`
	Tags           []string `yaml:"tags,flow,omitempty"`
	Visibility     string   `yaml:"visibility"`
	WorkItemID     *string  `yaml:"work_item_id,omitempty"`
	ProjectID      string   `yaml:"project_id"`
	IdempotencyKey *string  `yaml:"idempotency_key,omitempty"`
	Related        []string `yaml:"related,flow,omitempty"`
	CreatedAt      string   `yaml:"created_at"`
	UpdatedAt      string   `yaml:"updated_at"`
}

// splitFrontmatter splits an authority document at its leading YAML
// frontmatter block. A leading "---" that never closes is a thematic
// break, not frontmatter: the whole content is the body.
func splitFrontmatter(content string) (fmBlock, body string, ok bool) {
	if !strings.HasPrefix(content, "---\n") {
		return "", content, false
	}
	idx := strings.Index(content[4:], "\n---")
	if idx < 0 {
		return "", content, false
	}
	end := 4 + idx
	rest := content[end+4:]
	if rest != "" && rest[0] != '\n' && rest[0] != '\r' {
		return "", content, false
	}
	return content[4:end], rest, true
}

// parseFrontmatterBlock parses a frontmatter block into a generic map.
func parseFrontmatterBlock(block string) (map[string]any, error) {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
		return nil, errf("MEM-400", "FRONTMATTER_INVALID", "%v", err)
	}
	if raw == nil {
		return nil, errf("MEM-400", "FRONTMATTER_INVALID", "empty frontmatter block")
	}
	return raw, nil
}

// buildAuthorityFile renders the full authority document.
func buildAuthorityFile(fm *frontmatter, body string) ([]byte, error) {
	y, err := yaml.Marshal(fm)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(y)
	if !strings.HasSuffix(sb.String(), "\n") {
		sb.WriteByte('\n')
	}
	sb.WriteString("---\n")
	sb.WriteString(body)
	return []byte(sb.String()), nil
}

// extractBody derives the searchable text (06 §4.4): strip frontmatter,
// drop structural '#','*','_','>' from regular lines; fenced and indented
// code blocks stay verbatim.
func extractBody(content string) string {
	_, body, _ := splitFrontmatter(content)
	var sb strings.Builder
	inFence := false
	var fence string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case inFence:
			if isFenceLine(trimmed, fence) {
				inFence = false
			}
			sb.WriteString(line)
		case isFenceOpener(trimmed, &fence):
			inFence = true
			sb.WriteString(line)
		case strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t"):
			sb.WriteString(line)
		default:
			sb.WriteString(stripStructural(line))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func stripStructural(line string) string {
	var sb strings.Builder
	sb.Grow(len(line))
	for _, r := range line {
		switch r {
		case '#', '*', '_', '>':
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func isFenceOpener(trimmed string, fence *string) bool {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, f) {
			*fence = f
			return true
		}
	}
	return false
}

func isFenceLine(trimmed, fence string) bool {
	if len(trimmed) < len(fence) {
		return false
	}
	marker := fence[0]
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != marker {
			return false
		}
	}
	return true
}

// writeTmpSync is write-order step ①: temp file + fsync + dir fsync. No
// rename here — the authority path is untouched until step ③.
func writeTmpSync(tmp string, body []byte) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
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
	return syncDir(filepath.Dir(tmp))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
