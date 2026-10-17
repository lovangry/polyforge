package memory

// title.go: the row title. A memory written without a title gets one
// derived from its body, so recall listings carry a readable label for
// every entry. The derivation is a pure function of the authority file
// (explicit frontmatter title, else the body), which keeps the row a
// projection of the file (INV-7c): every row-from-file path — create,
// reconcile correction, file backfill and the startup title backfill —
// computes the same rowTitle, and rowMatchesFrontmatter compares against
// it. The frontmatter itself is never rewritten with a derived title.

import (
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// titleMaxRunes bounds a derived title, the trailing "…" included.
const titleMaxRunes = 80

// rowTitle answers the title a memories row carries for an authority
// document: the explicit title when it has non-space content (kept
// verbatim), else the title derived from the body (nil when the body has
// no usable line).
func rowTitle(explicit *string, body string) *string {
	if explicit != nil && strings.TrimSpace(*explicit) != "" {
		return explicit
	}
	return deriveTitle(body)
}

// deriveTitle answers the body's first non-empty line with its leading
// markdown markers (heading #, quote >, list - * +, task box, numbered
// 1. / 1)) stripped, trimmed and cut on a rune boundary to titleMaxRunes
// ("…" appended when cut). Code-fence delimiters and thematic breaks /
// setext underlines carry no text and are skipped. nil when no line
// survives.
func deriveTitle(body string) *string {
	for _, line := range strings.Split(body, "\n") {
		t := stripLineMarkers(line)
		if t == "" || strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") || isRuleLine(t) {
			continue
		}
		if utf8.RuneCountInString(t) > titleMaxRunes {
			rs := []rune(t)
			t = strings.TrimSpace(string(rs[:titleMaxRunes-1])) + "…"
		}
		return &t
	}
	return nil
}

// stripLineMarkers trims a line and removes its leading block markers
// until none is left ("> - # x" → "x").
func stripLineMarkers(line string) string {
	t := strings.TrimSpace(line)
	for {
		prev := t
		switch {
		case strings.HasPrefix(t, "#"):
			n := len(t) - len(strings.TrimLeft(t, "#"))
			if n == len(t) || t[n] == ' ' || t[n] == '\t' {
				t = t[n:]
			}
		case strings.HasPrefix(t, ">"):
			t = t[1:]
		case t == "-" || t == "*" || t == "+" || t == "[ ]" || t == "[x]" || t == "[X]":
			t = ""
		case len(t) >= 2 && strings.ContainsRune("-*+", rune(t[0])) && (t[1] == ' ' || t[1] == '\t'):
			t = t[2:]
		case strings.HasPrefix(t, "[ ] ") || strings.HasPrefix(t, "[x] ") || strings.HasPrefix(t, "[X] "):
			t = t[4:]
		default:
			n := len(t) - len(strings.TrimLeft(t, "0123456789"))
			if n > 0 && n <= 9 && n < len(t) && (t[n] == '.' || t[n] == ')') {
				switch {
				case n+1 == len(t): // a bare marker ("1.")
					t = ""
				case t[n+1] == ' ' || t[n+1] == '\t':
					t = t[n+2:]
				}
			}
		}
		t = strings.TrimSpace(t)
		if t == prev {
			return t
		}
	}
}

// isRuleLine reports a line made only of one of - * _ = (three or more,
// spaces allowed): a thematic break or setext underline.
func isRuleLine(t string) bool {
	s := strings.ReplaceAll(t, " ", "")
	if len(s) < 3 {
		return false
	}
	return strings.ContainsRune("-*_=", rune(s[0])) && strings.Trim(s, s[:1]) == ""
}

// backfillTitles gives every active row without a title the one its
// authority file derives (rowTitle) — the data migration for rows written
// before titles were derived. It is idempotent: a filled row no longer
// matches the selection, so a second pass reads no file and writes
// nothing. Only rows whose file verifies against content_sha are filled
// (a drifted entry is the reconcile's to correct, with the same derived
// title); the guarded UPDATE re-checks sha and emptiness at write time,
// so no per-mem lock is held across a transaction. Writes go in
// ftsRebuildBatch-row transactions with the reconcile's writer yield in
// between: each transaction holds Core's only connection.
func (s *Store) backfillTitles() error {
	type cand struct{ id, apath, sha string }
	rows, err := s.db.Query(`SELECT id, authority_path, content_sha FROM memories
		WHERE status='active' AND (title IS NULL OR trim(title) = '')`)
	if err != nil {
		return err
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.apath, &c.sha); err != nil {
			rows.Close()
			return err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type fill struct{ id, sha, title string }
	var fills []fill
	for _, c := range cands {
		raw, rerr := os.ReadFile(filepath.Join(s.dataDir, filepath.FromSlash(c.apath), "memory.md"))
		if rerr != nil || shaHex(raw) != c.sha {
			continue
		}
		fm, body, perr := parseAuthorityDoc(raw, c.id)
		if perr != nil {
			continue
		}
		if t := rowTitle(fm.Title, body); t != nil {
			fills = append(fills, fill{c.id, c.sha, *t})
		}
	}
	for i := 0; i < len(fills); i += ftsRebuildBatch {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		for _, f := range fills[i:min(i+ftsRebuildBatch, len(fills))] {
			if _, err := tx.Exec(`UPDATE memories SET title=?1 WHERE id=?2 AND content_sha=?3
				AND status='active' AND (title IS NULL OR trim(title) = '')`, f.title, f.id, f.sha); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		time.Sleep(ftsRebuildYield)
	}
	return nil
}
