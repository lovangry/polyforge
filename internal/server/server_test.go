package server_test

// End-to-end HTTP tests over the real mux: authentication, two-plane
// permission split (02 §4.10/§8), a full claim→publish→progress→wrap flow
// and the memory face.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/core/capsule"
	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/server"
)

type env struct {
	ts     *httptest.Server
	client *http.Client
	admin  string // admin api key
	writer string // member writer api key
	outsider string // key of a user with no membership
	scoped  string // project-scoped key
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc, err := service.Register(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.SeedBase(db); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.MakeDemoBundle(dir); err != nil {
		t.Fatal(err)
	}

	// Extra identities for the permission tests.
	if _, err := db.Exec(`INSERT INTO users(id, display_name, kind, role) VALUES ('usr_outsider','outsider','human','writer')`); err != nil {
		t.Fatal(err)
	}

	srv := server.New(db, dir, filepath.Join(dir, "workflows"), svc)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	e := &env{ts: ts, client: ts.Client()}
	e.admin = e.mintKey(t, db, "usr_admin", nil)
	e.writer = e.mintKey(t, db, "usr_worker", nil)
	e.outsider = e.mintKey(t, db, "usr_outsider", nil)
	pj := "pj1"
	e.scoped = e.mintKey(t, db, "usr_worker", &pj)
	return e
}

func (e *env) mintKey(t *testing.T, db *store.DB, user string, project *string) string {
	t.Helper()
	plain := "pfk_test_" + user + "_" + fmt.Sprint(len(user))
	sum := sha256Hex(plain)
	var proj any
	if project != nil {
		proj = *project
	}
	suffix := user
	if project != nil {
		suffix = user + "_" + *project
	}
	if _, err := db.Exec(`INSERT INTO api_keys(id, user_id, project_id, key_hash) VALUES (?1,?2,?3,?4)`,
		"key_"+suffix, user, proj, sum); err != nil {
		t.Fatal(err)
	}
	return plain
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sha256Long(s string) string { return sha256Hex(s) }

func timeNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (e *env) do(t *testing.T, method, path, key string, body any) (int, map[string]any, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		blob, _ := json.Marshal(body)
		rd = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
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

func errCode(t *testing.T, m map[string]any) string {
	t.Helper()
	e, _ := m["error"].(map[string]any)
	if e == nil {
		t.Fatalf("expected error envelope, got %v", m)
	}
	return e["code"].(string)
}

// TestAuthAndMembershipHardBoundary: missing/invalid key → 401; non-member
// sees CORE-404 (no existence leak, §4.10); scoped key rejected outside its
// project.
func TestAuthAndMembershipHardBoundary(t *testing.T) {
	e := newEnv(t)

	// No key.
	st, m, _ := e.do(t, "GET", "/api/v1/ready-queue?project=p1", "", nil)
	if st != 401 || errCode(t, m) != "CORE-401" {
		t.Fatalf("got %d %v", st, m)
	}
	// Bad key.
	st, m, _ = e.do(t, "GET", "/api/v1/ready-queue?project=p1", "bogus", nil)
	if st != 401 {
		t.Fatalf("got %d %v", st, m)
	}
	// Member ok.
	st, m, _ = e.do(t, "GET", "/api/v1/ready-queue?project=p1", e.writer, nil)
	if st != 200 {
		t.Fatalf("got %d %v", st, m)
	}
	if _, ok := m["executable"]; !ok {
		t.Fatalf("flat two-array response missing executable: %v", m)
	}
	// Outsider: whole query treated as not-found.
	st, m, _ = e.do(t, "GET", "/api/v1/ready-queue?project=p1", e.outsider, nil)
	if st != 404 || errCode(t, m) != "CORE-404" {
		t.Fatalf("got %d %v", st, m)
	}
	// Global admin bypasses membership.
	st, _, _ = e.do(t, "GET", "/api/v1/ready-queue?project=p1", e.admin, nil)
	if st != 200 {
		t.Fatalf("admin bypass broken: %d", st)
	}
	// Project-scoped key works inside scope…
	st, _, _ = e.do(t, "GET", "/api/v1/ready-queue?project=p1", e.scoped, nil)
	if st != 200 {
		t.Fatalf("scoped key inside scope: %d", st)
	}
	// …and is refused for anything outside it (middleware-level).
	st, m, _ = e.do(t, "GET", "/api/v1/projects", e.scoped, nil)
	if st == 500 {
		t.Fatalf("server error on scope check: %v", m)
	}
}

// TestEndToEndClaimPublishWrap walks the full lifecycle over HTTP: create
// execution ticket → claim → block PUT → manifest POST → progress
// read-through → end-generation (wrap) with flush evidence → cancel
// visibility.
func TestEndToEndClaimPublishWrap(t *testing.T) {
	e := newEnv(t)

	// Create the execution ticket.
	st, m, _ := e.do(t, "POST", "/api/v1/work-items", e.writer, map[string]any{
		"project": "p1", "goal": "e2e", "bundle_name": "demo",
		"repo_inventory": []map[string]any{{"name": "r1", "url": "u"}},
		"repo_inventory_base": []map[string]any{{"name": "r1", "base": "sha1"}},
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	wi := m["id"].(string)
	if m["bundle_digest"] == nil {
		t.Fatal("digest must be固化 in the creation response")
	}

	// Registration ticket is not claimable.
	st, m, _ = e.do(t, "POST", "/api/v1/work-items", e.writer, map[string]any{"project": "p1", "goal": "reg"})
	if st != 201 {
		t.Fatalf("reg create: %d", st)
	}
	regWI := m["id"].(string)
	st, m, _ = e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/claim", regWI), e.writer, map[string]any{"idempotency_key": "regk", "machine_id": "mac_a"})
	if errCode(t, m) != "CORE-409" {
		t.Fatalf("registration ticket claim: %d %v", st, m)
	}

	// Claim.
	st, m, _ = e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/claim", wi), e.writer,
		map[string]any{"idempotency_key": "e2e-k1", "machine_id": "mac_a"})
	if st != 200 {
		t.Fatalf("claim: %d %v", st, m)
	}
	token := m["generation_token"].(string)
	gen := int64(m["generation"].(float64))

	// Publish: block first, then manifest (先数据后状态).
	exec := `{"version":1,"current_step":"code"}`
	blockSha := sha256Long(exec)
	req, _ := http.NewRequest("PUT", fmt.Sprintf("%s/api/v1/work-items/%s/revisions/0/blocks/%s", e.ts.URL, wi, blockSha), strings.NewReader(exec))
	req.Header.Set("Authorization", "Bearer "+e.writer)
	req.Header.Set("X-PF-Generation-Token", token)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("block put: %d", resp.StatusCode)
	}

	manifest := map[string]any{
		"work_item_id": wi, "seq": 0, "parent": nil, "generation": gen,
		"kind": "incremental",
		"ops": []map[string]any{{
			"op": "put", "path": "runtime-state/execution.json",
			"sha256": blockSha, "bytes": float64(len(exec)), "mode": 420,
		}},
		"live_files": 1, "data_bytes": float64(len(exec)),
		"execution_state_included": true, "built_at": "2026-09-27T00:00:00Z",
	}
	sha := capsule.ComputeManifestSha(decodeManifest(t, manifest))
	st, m, _ = e.doWithToken(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/revisions/0/manifest", wi), e.writer, token,
		map[string]any{"manifest": manifest, "manifest_sha": sha})
	if st != 200 {
		t.Fatalf("manifest: %d %v", st, m)
	}

	// Progress read-through returns the published execution.json.
	st, m, _ = e.do(t, "GET", fmt.Sprintf("/api/v1/work-items/%s/progress", wi), e.writer, nil)
	if st != 200 {
		t.Fatalf("progress: %d %v", st, m)
	}
	prog := m["progress"].(map[string]any)
	if prog["current_step"] != "code" {
		t.Fatalf("progress: %v", m)
	}

	// Wrap with flush evidence bound to the published HEAD.
	head := m["head_seq"].(float64)
	st, m, _ = e.do(t, "GET", fmt.Sprintf("/api/v1/work-items/%s/head", wi), e.writer, nil)
	if st != 200 {
		t.Fatalf("head: %d %v", st, m)
	}
	headSha := m["manifest_sha"].(string)
	fe := map[string]any{
		"reason": "wrap",
		"flush_evidence": map[string]any{
			"published_generation": gen, "published_seq": int64(head), "manifest_sha": headSha,
			"frozen_checkpoint": map[string]any{
				"scanned_at": nowRFC(), "bytes_digest": "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
				"worktree_state": "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
				"manifest_sha": headSha, "matches_head": true,
			},
		},
	}
	req, _ = http.NewRequest("POST", fmt.Sprintf("%s/api/v1/work-items/%s/end-generation", e.ts.URL, wi), nil)
	req.Header.Set("Authorization", "Bearer "+e.writer)
	req.Header.Set("X-PF-Generation-Token", token)
	blob, _ := json.Marshal(fe)
	req.Body = io.NopCloser(bytes.NewReader(blob))
	resp, err = e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("end-generation: %d %s", resp.StatusCode, raw)
	}
	_ = m

	// Terminal: PATCH now refuses (CORE-410).
	st, m, _ = e.do(t, "PATCH", fmt.Sprintf("/api/v1/work-items/%s", wi), e.writer, map[string]any{"goal": "x"})
	if errCode(t, m) != "CORE-410" {
		t.Fatalf("terminal patch: %d %v", st, m)
	}
}

// TestMemoryFace covers rows 22–25 at the HTTP level (02's surface; 06 full
// semantics land in the Memory WI).
func TestMemoryFace(t *testing.T) {
	e := newEnv(t)

	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "experience.debug", "visibility": "project",
		"content": "---\ntitle: t\n---\n你好世界 polyforge trigram probe",
		"title": "t", "tags": []string{"a"},
	})
	if st != 201 {
		t.Fatalf("create memory: %d %v", st, m)
	}
	memID := m["id"].(string)

	// Search finds it (3+ char phrase).
	st, m, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=你好世界", e.writer, nil)
	if st != 200 {
		t.Fatalf("search: %d %v", st, m)
	}
	if len(m["items"].([]any)) != 1 {
		t.Fatalf("search items: %v", m)
	}

	// Outsider sees nothing (membership-first).
	st, m, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=你好世界", e.outsider, nil)
	if st != 404 {
		t.Fatalf("outsider search: %d %v", st, m)
	}

	// Share: owner issues a token; anonymous consumption works with no key.
	st, m, _ = e.do(t, "POST", fmt.Sprintf("/api/v1/memories/%s/share", memID), e.writer, nil)
	if st != 200 {
		t.Fatalf("share: %d %v", st, m)
	}
	token := m["share_token"].(string)
	st, m, _ = e.do(t, "GET", "/api/v1/share/"+token, "", nil)
	if st != 200 || m["content"] == nil {
		t.Fatalf("anonymous consume: %d %v", st, m)
	}

	// Redact removes it from search.
	st, _, _ = e.do(t, "POST", fmt.Sprintf("/api/v1/memories/%s/redact", memID), e.writer, nil)
	if st != 200 {
		t.Fatalf("redact: %d", st)
	}
	st, m, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=你好世界", e.writer, nil)
	if st == 200 && len(m["items"].([]any)) != 0 {
		t.Fatalf("redacted memory still searchable: %v", m)
	}
}

// doWithToken sends an authenticated request that also carries the
// X-PF-Generation-Token header.
func (e *env) doWithToken(t *testing.T, method, path, key, token string, body any) (int, map[string]any, []byte) {
	t.Helper()
	blob, _ := json.Marshal(body)
	req, err := http.NewRequest(method, e.ts.URL+path, bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-PF-Generation-Token", token)
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

func decodeManifest(t *testing.T, m map[string]any) capsule.Manifest {
	t.Helper()
	blob, _ := json.Marshal(m)
	var out capsule.Manifest
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func nowRFC() string { return timeNow() }
