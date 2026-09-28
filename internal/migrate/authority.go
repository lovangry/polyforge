package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// authorityFM is the 06 §4.2 frontmatter field set, declared in the exact
// order and with the exact yaml tags of internal/memory's private
// frontmatter struct: the migration writes authority bytes that the
// server's parseAuthorityDoc accepts EXACTLY (required fields, correct
// types, same deterministic serialization — yaml.v3 struct marshaling in
// declaration order, omitempty/flow included). Any drift here would leave
// the file skipped by the §5.3 rebuild ("invalid frontmatter never
// becomes an illegal row"), which the four-step verification then catches
// as a count mismatch — the format is still mirrored, not improvised.
type authorityFM struct {
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

// renderAuthorityFile renders the full authority document with the same
// framing the server's buildAuthorityFile produces: "---\n" + frontmatter
// YAML + "---\n" + body. Deterministic for equal field values (06 §4.3:
// the derived chain assumes same inputs → same bytes).
func renderAuthorityFile(fm *authorityFM, body string) ([]byte, error) {
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

// authorityRelPath is the data-dir-relative placement of one migrated
// memory: active stock in the project tree, redacted stock under
// _redacted/ (06 §4.1; redact is terminal, the entry stays soft-deleted).
func authorityRelPath(projectID, memID string, redacted bool) string {
	if redacted {
		return "memory/" + projectID + "/_redacted/" + memID
	}
	return "memory/" + projectID + "/" + memID
}

// placeAuthorityFile writes one memory.md directly under the data dir
// (A12: CLI直写, NOT via the Memory API — no memory.md.tmp, no history/
// write-order transients; the migration runs in the deployment's silent
// window). The write is durable (fsync file + parent dir) so a crash
// never leaves a truncated authority file behind.
func placeAuthorityFile(dataDir, relPath string, content []byte) error {
	dir := filepath.Join(dataDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	full := filepath.Join(dir, "memory.md")
	f, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
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

// readPlacedFM re-reads one placed authority file's frontmatter — the
// verification's blanked-list reconciliation step judges the FILES, not
// the tool's in-memory intent. Missing required framing → error.
func readPlacedFM(dataDir, relPath string) (*authorityFM, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(relPath), "memory.md"))
	if err != nil {
		return nil, err
	}
	block, _, ok := splitFM(string(raw))
	if !ok {
		return nil, fmt.Errorf("%s: no frontmatter block", relPath)
	}
	var fm authorityFM
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return nil, fmt.Errorf("%s: %v", relPath, err)
	}
	return &fm, nil
}

// splitFM mirrors internal/memory's splitFrontmatter framing rules: a
// leading "---" line that is never closed is a thematic break, not
// frontmatter.
func splitFM(content string) (block, body string, ok bool) {
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
