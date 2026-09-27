package memory

// Authority-file helpers: frontmatter parse/build (06 §4.2), the §4.4 body
// extraction rule set for the FTS index, and the low-level file primitives
// of the §5.2 write order (tmp+fsync, rename, dir fsync).
//
// The authority file is memory.md (frontmatter + body). Frontmatter mirrors
// the memories-table columns one-to-one (06 §4.2); it is machine-written by
// the server in a FIXED field order (struct marshaling via yaml.v3), so the
// authority bytes are a deterministic function of the field values — same
// inputs, same file, same content_sha.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatter is the §4.2 field set, serialized in this exact order.
// Deterministic output (06 §4.3 requirement for the derived chain) is
// guaranteed by struct marshaling: yaml.v3 emits struct fields in
// declaration order with no timestamps or random ids.
//
// Inference (recorded): 06 §4.2 pins the field list as the one-to-one
// mirror of the memories columns; any additional key a client places in a
// content frontmatter block is NOT carried into the authority file (there
// is no column to mirror it into). The v1 mirror is exactly this struct.
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
// frontmatter block. A leading "---" that is never closed is NOT a
// frontmatter block (a bare "---" line is a thematic break in Markdown):
// the whole content is the body in that case.
func splitFrontmatter(content string) (fmBlock, body string, ok bool) {
	if !strings.HasPrefix(content, "---\n") {
		return "", content, false
	}
	idx := strings.Index(content[4:], "\n---")
	if idx < 0 {
		return "", content, false
	}
	end := 4 + idx
	rest := content[end+4:] // byte right after the closing "---"
	// The closing marker must be a line on its own: the next byte (if any)
	// must be a newline, otherwise "---foo" was a body line, not a close.
	if rest != "" && rest[0] != '\n' && rest[0] != '\r' {
		return "", content, false
	}
	return content[4:end], rest, true
}

// parseFrontmatterBlock parses a frontmatter block into a generic map.
// Callers validate the fields they consume (06 §4.2: missing required
// fields are MEM-400 FRONTMATTER_INVALID at the point of use).
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

// buildAuthorityFile renders the full authority document: frontmatter
// header + body. Deterministic for equal field values.
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

// extractBody derives the searchable text from an authority document
// (06 §4.4): strip the frontmatter block, then remove the Markdown
// structural characters '#', '*', '_', '>' from regular lines, while
// fenced and indented code blocks are kept verbatim (their text is content,
// not structure).
func extractBody(content string) string {
	_, body, _ := splitFrontmatter(content)
	var sb strings.Builder
	inFence := false
	var fence string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case inFence:
			// Inside a fenced block: keep the line byte-for-byte, watch for
			// the closing fence of the same character.
			if isFenceLine(trimmed, fence) {
				inFence = false
			}
			sb.WriteString(line)
		case isFenceOpener(trimmed, &fence):
			inFence = true
			sb.WriteString(line)
		case strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t"):
			// Indented code block: verbatim.
			sb.WriteString(line)
		default:
			sb.WriteString(stripStructural(line))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// stripStructural removes the §4.4 structural characters from one line.
func stripStructural(line string) string {
	var sb strings.Builder
	sb.Grow(len(line))
	for _, r := range line {
		switch r {
		case '#', '*', '_', '>':
			// structural: dropped
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// isFenceOpener reports whether the trimmed line opens a fenced code block
// and, if so, records the fence marker ("```" or "~~~").
func isFenceOpener(trimmed string, fence *string) bool {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, f) {
			*fence = f
			return true
		}
	}
	return false
}

// isFenceLine reports whether the trimmed line consists only of the
// fence's marker character (three or more of them).
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

// writeTmpSync is step ① of the 06 §5.2 write order: create/truncate the
// temp file, write the bytes, fsync the file, then fsync the parent
// directory so the temp entry itself is durable. NO rename happens here —
// the authority path is intentionally untouched until step ③.
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

// syncDir fsyncs a directory so renames/creations inside it are durable.
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

// atomicWrite writes body to tmp (fsync) and renames it over dst (fsync
// dir). Used for derived artifacts (memory.html) where the §5.2 numbered
// order does not apply; the authority path uses the explicit step
// primitives instead.
func atomicWrite(tmp, dst string, body []byte) error {
	if err := writeTmpSync(tmp, body); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
