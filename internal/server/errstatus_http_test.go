package server_test

// HTTP-level tests for the typed-code → HTTP-status mapping pinned by
// internal/server/server.go errStatus: the 403 family (SYNC-403 STALE_OWNER
// fencing on the sync face, MEM-403 owner-only memory writes, 05 §5.2 /
// 06 §5.4), MEM-413 (content over the 1 MiB limit, 06 §5.4) and MEM-501
// (DERIVE_FAILED on format=html reads, 06 §5.4). The typed code rides in
// the error envelope body; every assertion checks BOTH the HTTP status and
// the body code — none of these may fall to the errStatus default 500.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newExecTicket creates a claimable execution ticket in p1 and returns its id.
func newExecTicket(t *testing.T, e *env) string {
	t.Helper()
	st, m, _ := e.do(t, "POST", "/api/v1/work-items", e.writer, map[string]any{
		"project": "p1", "goal": "errstatus probe", "bundle_name": "demo",
		"repo_inventory":      []map[string]any{{"name": "r1", "url": "u"}},
		"repo_inventory_base": []map[string]any{{"name": "r1", "base": "sha1"}},
	})
	if st != 201 {
		t.Fatalf("create ticket: %d %v", st, m)
	}
	return m["id"].(string)
}

// doRaw sends a request with a raw (non-JSON-marshaled) body plus the
// X-PF-Generation-Token header, for the octet-stream sync face.
func (e *env) doRaw(t *testing.T, method, path, key, token, contentType, body string) (int, map[string]any, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if token != "" {
		req.Header.Set("X-PF-Generation-Token", token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, raw
}

// TestSync403StaleOwnerFencing: a block PUT and a manifest POST from a
// fenced-out (stale) owner are SYNC-403 with HTTP 403 (05 §5.2).
func TestSync403StaleOwnerFencing(t *testing.T) {
	e := newEnv(t)
	wi := newExecTicket(t, e)

	st, m, _ := e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/claim", wi), e.writer,
		map[string]any{"idempotency_key": "sync403-k1", "machine_id": "mac_a"})
	if st != 200 {
		t.Fatalf("claim: %d %v", st, m)
	}
	staleToken := m["generation_token"].(string)

	// Admin force-takeover opens generation 2; the first owner's token is
	// now a stale owner (05 §2.9 fencing).
	st, m, _ = e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/force-takeover", wi), e.admin,
		map[string]any{"machine_id": "mac_b"})
	if st != 200 {
		t.Fatalf("force-takeover: %d %v", st, m)
	}
	if int64(m["generation"].(float64)) <= 1 {
		t.Fatalf("takeover must bump the generation: %v", m)
	}

	// Stale-owner block PUT (row 14) → HTTP 403 + SYNC-403 body.
	block := `{"version":1,"current_step":"code"}`
	st, m, _ = e.doRaw(t, "PUT",
		fmt.Sprintf("/api/v1/work-items/%s/revisions/0/blocks/%s", wi, sha256Long(block)),
		e.writer, staleToken, "application/octet-stream", block)
	if st != 403 || errCode(t, m) != "SYNC-403" {
		t.Fatalf("stale-owner block PUT: want 403/SYNC-403, got %d %v", st, m)
	}

	// The same fencing fires on the manifest face (row 15). The manifest is
	// rejected by fencing before any content validation, so a minimal body
	// suffices.
	st, m, _ = e.doWithToken(t, "POST",
		fmt.Sprintf("/api/v1/work-items/%s/revisions/0/manifest", wi),
		e.writer, staleToken, map[string]any{
			"manifest": map[string]any{
				"work_item_id": wi, "seq": 0, "parent": nil, "generation": 1,
				"kind": "incremental", "ops": []map[string]any{}, "live_files": 0,
				"data_bytes": 0, "execution_state_included": true,
			},
			"manifest_sha": "sha256:" + strings.Repeat("0", 64),
		})
	if st != 403 || errCode(t, m) != "SYNC-403" {
		t.Fatalf("stale-owner manifest POST: want 403/SYNC-403, got %d %v", st, m)
	}
}

// TestMem403OwnerOnlyByNonOwner: a project member who is not the entry's
// owner gets MEM-403 with HTTP 403 on the owner-only memory operations
// (PATCH / redact / share, 06 §5.4 A18).
func TestMem403OwnerOnlyByNonOwner(t *testing.T) {
	e := newEnv(t)

	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "experience.debug", "visibility": "project",
		"content": "owner-only probe body", "title": "t",
	})
	if st != 201 {
		t.Fatalf("create memory: %d %v", st, m)
	}
	memID := m["id"].(string)

	// usr_other is a seeded pj1 member but NOT the owner (usr_worker is).
	member := e.mintKey(t, e.db, "usr_other", nil)

	// The member can read the project-visibility entry (sanity: any 403 below
	// is the owner gate, not visibility).
	st, _, _ = e.do(t, "GET", "/api/v1/memories/"+memID, member, nil)
	if st != 200 {
		t.Fatalf("member read of project-visibility memory: %d", st)
	}

	for _, c := range []struct{ method, path string }{
		{"PATCH", "/api/v1/memories/" + memID},
		{"POST", "/api/v1/memories/" + memID + "/redact"},
		{"POST", "/api/v1/memories/" + memID + "/share"},
	} {
		st, m, _ = e.do(t, c.method, c.path, member, map[string]any{"content": "hijack"})
		if st != 403 || errCode(t, m) != "MEM-403" {
			t.Fatalf("%s %s by non-owner: want 403/MEM-403, got %d %v", c.method, c.path, st, m)
		}
	}

	// Sanity: the owner itself still succeeds on the owner-only op.
	st, m, _ = e.do(t, "POST", "/api/v1/memories/"+memID+"/share", e.writer, nil)
	if st != 200 || m["share_token"] == nil {
		t.Fatalf("owner share: %d %v", st, m)
	}
}

// TestMem413ContentOverLimit: create and PATCH over the 1 MiB authority
// limit are MEM-413 with HTTP 413 (06 §5.4). The JSON body stays well under
// the 16 MiB decode cap, so the typed limit — not the transport cap — fires.
func TestMem413ContentOverLimit(t *testing.T) {
	e := newEnv(t)
	over := strings.Repeat("x", 1<<20+1) // 1 MiB + 1 byte

	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": over, "title": "too big",
	})
	if st != 413 || errCode(t, m) != "MEM-413" {
		t.Fatalf("oversized create: want 413/MEM-413, got %d %v", st, m)
	}

	// Within the limit it succeeds (content minus the frontmatter overhead
	// the authority document wraps around it), so the boundary is the
	// authority-document cap, not something coarser.
	st, m, _ = e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": strings.Repeat("y", (1<<20)-4096), "title": "just under 1 MiB",
	})
	if st != 201 {
		t.Fatalf("exact-limit create: %d %v", st, m)
	}
	memID := m["id"].(string)

	st, m, _ = e.do(t, "PATCH", "/api/v1/memories/"+memID, e.writer, map[string]any{"content": over})
	if st != 413 || errCode(t, m) != "MEM-413" {
		t.Fatalf("oversized patch: want 413/MEM-413, got %d %v", st, m)
	}
}

// TestMem501DeriveFailed: a failed HTML re-derivation on a format=html read
// is MEM-501 with HTTP 501 (06 §5.4), while the authority content is still
// served. Derivation failure is forced deterministically by putting a
// DIRECTORY where memory.html must be written: os.Rename cannot move a file
// onto a directory (EISDIR), so the atomic write fails regardless of uid.
func TestMem501DeriveFailed(t *testing.T) {
	e := newEnv(t)

	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": "derive-failure probe", "title": "t",
	})
	if st != 201 {
		t.Fatalf("create memory: %d %v", st, m)
	}
	memID := m["id"].(string)

	// Sanity: format=html works before the fault is injected.
	st, _, _ = e.do(t, "GET", "/api/v1/memories/"+memID+"?format=html", e.writer, nil)
	if st != 200 {
		t.Fatalf("html read before fault: %d", st)
	}

	// Force the derivation failure: replace memory.html with a directory.
	var apath string
	if err := e.db.QueryRow(`SELECT authority_path FROM memories WHERE id=?1`, memID).Scan(&apath); err != nil {
		t.Fatal(err)
	}
	htmlPath := filepath.Join(e.dir, filepath.FromSlash(apath), "memory.html")
	if err := os.Remove(htmlPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(htmlPath, 0o755); err != nil {
		t.Fatal(err)
	}

	st, m, _ = e.do(t, "GET", "/api/v1/memories/"+memID+"?format=html", e.writer, nil)
	if st != 501 || errCode(t, m) != "MEM-501" {
		t.Fatalf("derive-failed html read: want 501/MEM-501, got %d %v", st, m)
	}

	// The authority content is unaffected (06 §5.4: DERIVE_FAILED never
	// touches the authority document).
	st, m, _ = e.do(t, "GET", "/api/v1/memories/"+memID, e.writer, nil)
	if st != 200 || !strings.Contains(m["content"].(string), "derive-failure probe") {
		t.Fatalf("authority read after derive failure: %d %v", st, m)
	}
}
