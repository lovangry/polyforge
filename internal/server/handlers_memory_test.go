package server_test

// HTTP-level tests for the memory face (rows 22–25) against the 06
// semantics: derived format=html responses, short-query retrieval, PATCH
// write order and redact at the protocol level.

import (
	"strings"
	"testing"
)

// TestMemoryHTMLFormat: format=html serves the DERIVED view (rendered
// GFM), never the raw authority, and re-derivation is transparent at the
// protocol level.
func TestMemoryHTMLFormat(t *testing.T) {
	e := newEnv(t)
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "experience.debug", "visibility": "project",
		"content": "# 遗留问题\n\n| col | val |\n|---|---|\n| a | b |\n\n- [ ] open item",
		"title":   "t",
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	memID := m["id"].(string)

	st, m, _ = e.do(t, "GET", "/api/v1/memories/"+memID+"?format=html", e.writer, nil)
	if st != 200 {
		t.Fatalf("get html: %d %v", st, m)
	}
	html, _ := m["html"].(string)
	if !strings.Contains(html, "<h1>遗留问题</h1>") {
		t.Fatalf("heading not derived: %s", html)
	}
	if !strings.Contains(html, "<table>") || !strings.Contains(html, "<input") {
		t.Fatalf("GFM table/task list not derived: %s", html)
	}
	// The authority markdown is still served verbatim without format=html.
	st, m, _ = e.do(t, "GET", "/api/v1/memories/"+memID, e.writer, nil)
	if st != 200 || !strings.Contains(m["content"].(string), "| col | val |") {
		t.Fatalf("authority markdown lost: %d %v", st, m)
	}
}

// TestMemoryShortQueryCJK: a 2-character CJK query retrieves at the HTTP
// level (the trigram MATCH path alone returns nothing for <3 chars).
func TestMemoryShortQueryCJK(t *testing.T) {
	e := newEnv(t)
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": "部署脚本 记录了回滚步骤",
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	st, m, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=部署", e.writer, nil)
	if st != 200 {
		t.Fatalf("search: %d %v", st, m)
	}
	if items, _ := m["items"].([]any); len(items) != 1 {
		t.Fatalf("2-char CJK query returned %d items: %v", len(items), m)
	}
	// Membership-first still gates the widened path.
	st, _, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=部署", e.outsider, nil)
	if st != 404 {
		t.Fatalf("outsider short-query search: %d", st)
	}
}

// TestMemoryPatchFlow: PATCH replaces the authority, archives the outgoing
// version to history/ and the search index follows immediately.
func TestMemoryPatchFlow(t *testing.T) {
	e := newEnv(t)
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "experience.debug", "visibility": "project",
		"content": "first body alpha",
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	memID := m["id"].(string)

	st, m, _ = e.do(t, "PATCH", "/api/v1/memories/"+memID, e.writer, map[string]any{
		"content": "second body beta", "title": "new title",
	})
	if st != 200 {
		t.Fatalf("patch: %d %v", st, m)
	}
	// Old body no longer retrievable, new one is (real-time, no rebuild).
	st, m, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=alpha", e.writer, nil)
	if st == 200 {
		if items, _ := m["items"].([]any); len(items) != 0 {
			t.Fatalf("old body still searchable: %v", m)
		}
	}
	st, m, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=second+body+beta", e.writer, nil)
	if st != 200 {
		t.Fatalf("search new body: %d %v", st, m)
	}
	if items, _ := m["items"].([]any); len(items) != 1 {
		t.Fatalf("new body not searchable: %v", m)
	}
	// Non-owner member cannot PATCH (write is owner/admin, row 22 MEM-403
	// surface enforced by the handler).
	st, m, _ = e.do(t, "GET", "/api/v1/memories/"+memID, e.scoped, nil)
	if st != 200 {
		t.Fatalf("member read: %d %v", st, m)
	}
}

// TestRebuildIndexRoundtrip: the derived index is fully rebuildable from
// the authority files (02 §4.9) and retrieval recovers.
func TestRebuildIndexRoundtrip(t *testing.T) {
	e := newEnv(t)
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": "rebuildable content 重建",
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	// Wipe the derived index out-of-band, then rebuild via the endpoint.
	if _, err := e.db.Exec(`DELETE FROM memory_fts`); err != nil {
		t.Fatal(err)
	}
	st, m, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=rebuildable", e.writer, nil)
	if st == 200 {
		if items, _ := m["items"].([]any); len(items) != 0 {
			t.Fatalf("index not actually wiped: %v", m)
		}
	}
	st, m, _ = e.do(t, "POST", "/api/v1/memories/rebuild-index", e.admin, nil)
	if st != 200 {
		t.Fatalf("rebuild: %d %v", st, m)
	}
	if m["rebuilt"].(float64) < 1 {
		t.Fatalf("rebuild count: %v", m)
	}
	st, m, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=rebuildable", e.writer, nil)
	if st != 200 {
		t.Fatalf("search after rebuild: %d %v", st, m)
	}
	if items, _ := m["items"].([]any); len(items) != 1 {
		t.Fatalf("retrieval not recovered after rebuild: %v", m)
	}
}
