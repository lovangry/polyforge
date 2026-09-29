// Package digest computes the workflow bundle digest. Design authority:
// docs/design/polyforge-vnext/01-workflow-skills.md §4.5 (唯一规则：RFC 8785
// canonical JSON，MUST NOT 裸换行拼接).
package digest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
)

// FileEntry is one per-file record in the digest input object (01 §4.5 step 5).
type FileEntry struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Bytes  uint64 `json:"bytes"`
}

type digestObject struct {
	Files []FileEntry `json:"files"`
}

// EmptySyncBytesDigest is the aggregate digest of an empty sync surface:
// sha256(canonical JSON of the empty list) — the constant shared with the
// flush-evidence empty-chain branch (02 §6.4 / 05 §6.1, verified value:
// sha256("[]")).
const EmptySyncBytesDigest = "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"

// ComputeBundleDigest walks the bundle at bundleDir, collects workflow.md and
// every file under agents/ (only .md files are allowed there, 01 §4.1 V7),
// LF-normalizes each file's bytes (BOM rejected), orders them by Unicode code
// point, and returns sha256(canonical JSON of the files object), hex lowercase.
//
// The result is the value persisted as work_items.bundle_digest (02 §4.4).
func ComputeBundleDigest(bundleDir string) (string, error) {
	entries, err := collectEntries(bundleDir)
	if err != nil {
		return "", err
	}
	obj := digestObject{Files: entries}
	canonical, err := MarshalCanonical(obj)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func collectEntries(bundleDir string) ([]FileEntry, error) {
	if _, err := os.Stat(filepath.Join(bundleDir, "workflow.md")); err != nil {
		return nil, fmt.Errorf("digest: workflow.md missing in bundle %s: %w", bundleDir, err)
	}
	var paths []string
	agentsDir := filepath.Join(bundleDir, "agents")
	err := filepath.WalkDir(agentsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == agentsDir {
				return nil // empty bundle without agents/ still yields workflow.md alone
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(bundleDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".md") {
			return fmt.Errorf("digest: agents/ allows only .md files (V7), found %q", rel)
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	paths = append(paths, "workflow.md")
	// Unicode code point ascending order (01 §4.5 step 3). For UTF-8 strings,
	// byte-wise comparison equals code-point order.
	sort.Slice(paths, func(i, j int) bool { return paths[i] < paths[j] })

	entries := make([]FileEntry, 0, len(paths))
	for _, rel := range paths {
		content, err := os.ReadFile(filepath.Join(bundleDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		normalized, err := lfNormalize(content)
		if err != nil {
			return nil, fmt.Errorf("digest: %s: %w", rel, err)
		}
		sum := sha256.Sum256(normalized)
		entries = append(entries, FileEntry{
			Path:   rel,
			Sha256: hex.EncodeToString(sum[:]),
			Bytes:  uint64(len(normalized)),
		})
	}
	return entries, nil
}

func lfNormalize(content []byte) ([]byte, error) {
	// 01 §4.5 step 2: CRLF/CR → LF; BOM rejected.
	if bytes.HasPrefix(content, []byte{0xEF, 0xBB, 0xBF}) {
		return nil, fmt.Errorf("BOM not allowed")
	}
	if !bytes.Contains(content, []byte{'\r'}) {
		return content, nil
	}
	out := bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))
	return out, nil
}

// MarshalCanonical serializes v as RFC 8785 canonical JSON: object keys sorted
// by UTF-16 code unit, no insignificant whitespace, strings escaped per
// ECMAScript JSON.stringify, integers in shortest form.
//
// Supported value domain: strings, bools, nil, slices, maps with string keys
// and integral numbers (json.Number / int / int64 / uint64 / float64 with an
// exactly integral value). Non-integral numbers are rejected loudly: every
// digest input in the v1 design (bundle entries, budget scalars, bytes
// digests) is strings-and-integers only, and emitting a non-canonical float
// would silently break reproducibility (01 §4.5 step 6 note: full RFC 8785
// number serialization required for the general case — outside the v1 input
// domain, so refused here rather than approximated).
func MarshalCanonical(v any) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(mustJSON(v)))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("canonical: decode: %w", err)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Encode-only value domain (structs of string/int fields); marshal
		// errors surface earlier from the caller's types.
		panic(err)
	}
	return b
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, t)
	case json.Number:
		s, err := canonicalNumber(t)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonical: unsupported value type %T", v)
	}
	return nil
}

func canonicalNumber(n json.Number) (string, error) {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		// Integral literal; ensure shortest form (no leading zeros/plus).
		var i int64
		if err := json.Unmarshal([]byte(s), &i); err == nil {
			return fmt.Sprintf("%d", i), nil
		}
		var u uint64
		if err := json.Unmarshal([]byte(s), &u); err == nil {
			return fmt.Sprintf("%d", u), nil
		}
		return "", fmt.Errorf("canonical: bad integer %q", s)
	}
	var f float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return "", fmt.Errorf("canonical: bad number %q", s)
	}
	if f != float64(int64(f)) && f != float64(uint64(f)) {
		return "", fmt.Errorf("canonical: non-integral number %q outside v1 digest input domain", s)
	}
	if f < 0 {
		return fmt.Sprintf("%d", int64(f)), nil
	}
	return fmt.Sprintf("%d", uint64(f)), nil
}

func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

func utf16Less(a, b string) bool {
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}
