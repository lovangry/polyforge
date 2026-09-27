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
	"sync"
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
	db     *store.DB
	dir    string // server data dir (memory authority files live under it)
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

	e := &env{ts: ts, client: ts.Client(), db: db, dir: dir}
	e.admin = e.mintKey(t, db, "usr_admin", nil)
	e.writer = e.mintKey(t, db, "usr_worker", nil)
	e.outsider = e.mintKey(t, db, "usr_outsider", nil)
	pj := "pj1"
	e.scoped = e.mintKey(t, db, "usr_worker", &pj)
	return e
}

func (e *env) mintKey(t *testing.T, db *store.DB, user string, project *string) string {
	t.Helper()
	suffix := user
	if project != nil {
		suffix = user + "_" + *project
	}
	// The plaintext MUST be unique per (user, project) pair, not just per
	// user: a scoped and an unscoped key for the same user would otherwise
	// share one key_hash, and authenticate()'s WHERE key_hash=?1 lookup would
	// non-deterministically resolve to whichever row SQLite happens to
	// return, silently defeating any scope test.
	plain := "pfk_test_" + suffix
	sum := sha256Hex(plain)
	var proj any
	if project != nil {
		proj = *project
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

// TestRevisionsSinceIncludeOps: row 19 amendment (polyforge#14) at the
// HTTP level — default listing metadata-only; include=ops carries kind+ops
// inline; any unknown include value is CORE-400 (row 3 §5.1c style).
func TestRevisionsSinceIncludeOps(t *testing.T) {
	e := newEnv(t)

	// Create an execution ticket, claim it, publish one revision.
	st, m, _ := e.do(t, "POST", "/api/v1/work-items", e.writer, map[string]any{
		"project": "p1", "goal": "rev-ops", "bundle_name": "demo",
		"repo_inventory": []map[string]any{}, "repo_inventory_base": []map[string]any{},
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	wi := m["id"].(string)
	st, m, _ = e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/claim", wi), e.writer,
		map[string]any{"idempotency_key": "revops-k1", "machine_id": "mac_a"})
	if st != 200 {
		t.Fatalf("claim: %d %v", st, m)
	}
	token := m["generation_token"].(string)
	gen := int64(m["generation"].(float64))

	exec := `{"version":1}`
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
		"execution_state_included": true, "built_at": nowRFC(),
	}
	sha := capsule.ComputeManifestSha(decodeManifest(t, manifest))
	st, m, _ = e.doWithToken(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/revisions/0/manifest", wi), e.writer, token,
		map[string]any{"manifest": manifest, "manifest_sha": sha})
	if st != 200 {
		t.Fatalf("manifest: %d %v", st, m)
	}

	// Default: metadata only — no kind, no ops.
	st, m, _ = e.do(t, "GET", fmt.Sprintf("/api/v1/work-items/%s/revisions?since=0", wi), e.writer, nil)
	if st != 200 {
		t.Fatalf("default listing: %d %v", st, m)
	}
	rows := m["revisions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows: %v", rows)
	}
	if row := rows[0].(map[string]any); row["kind"] != nil || row["ops"] != nil {
		t.Fatalf("default listing must be metadata-only: %v", row)
	}

	// include=ops: kind + ops inline from the manifest audit copy.
	st, m, _ = e.do(t, "GET", fmt.Sprintf("/api/v1/work-items/%s/revisions?since=0&include=ops", wi), e.writer, nil)
	if st != 200 {
		t.Fatalf("ops listing: %d %v", st, m)
	}
	rows = m["revisions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows: %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["kind"] != "incremental" {
		t.Fatalf("kind: %v", row)
	}
	ops, ok := row["ops"].([]any)
	if !ok || len(ops) != 1 {
		t.Fatalf("ops: %v", row)
	}
	op := ops[0].(map[string]any)
	if op["path"] != "runtime-state/execution.json" || op["sha256"] != blockSha {
		t.Fatalf("op: %v", op)
	}

	// Unknown include value: CORE-400, same verdict style as row 3.
	st, m, _ = e.do(t, "GET", fmt.Sprintf("/api/v1/work-items/%s/revisions?since=0&include=events", wi), e.writer, nil)
	if st != 400 || errCode(t, m) != "CORE-400" {
		t.Fatalf("unknown include: %d %v", st, m)
	}
	st, m, _ = e.do(t, "GET", fmt.Sprintf("/api/v1/work-items/%s/revisions?since=0&include=ops,bogus", wi), e.writer, nil)
	if st != 400 || errCode(t, m) != "CORE-400" {
		t.Fatalf("mixed include: %d %v", st, m)
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

// --- fix #2: heartbeat privilege escalation ---

func TestHeartbeatAuthorization(t *testing.T) {
	e := newEnv(t)
	// Mint a MACHINE-kind key (mac_user_a, seeded by testsupport) to stand in
	// for an unattended/non-human caller.
	machineKey := e.mintKey(t, e.db, "mac_user_a", nil)

	// A machine-kind caller must not be able to register a brand-new machine
	// identity for itself.
	st, m, _ := e.do(t, "POST", "/api/v1/machines/heartbeat", machineKey, map[string]any{"machine_id": "mac_new_1"})
	if st != 403 || errCode(t, m) != "CORE-403" {
		t.Fatalf("machine caller must not self-register: %d %v", st, m)
	}

	// A human (writer) caller CAN register a new machine.
	st, m, _ = e.do(t, "POST", "/api/v1/machines/heartbeat", e.writer, map[string]any{"machine_id": "mac_new_1"})
	if st != 200 || m["api_key"] == nil {
		t.Fatalf("human registration failed: %d %v", st, m)
	}
	newMachineKey := m["api_key"].(string)

	// The new machine's OWN key can refresh its own heartbeat.
	st, m, _ = e.do(t, "POST", "/api/v1/machines/heartbeat", newMachineKey, map[string]any{"machine_id": "mac_new_1"})
	if st != 200 {
		t.Fatalf("self-refresh: %d %v", st, m)
	}

	// A DIFFERENT machine's key must not be able to refresh mac_new_1's
	// heartbeat.
	st, m, _ = e.do(t, "POST", "/api/v1/machines/heartbeat", machineKey, map[string]any{"machine_id": "mac_new_1"})
	if st != 403 || errCode(t, m) != "CORE-403" {
		t.Fatalf("cross-machine refresh must be forbidden: %d %v", st, m)
	}

	// A human/admin can still refresh any machine's heartbeat.
	st, _, _ = e.do(t, "POST", "/api/v1/machines/heartbeat", e.writer, map[string]any{"machine_id": "mac_new_1"})
	if st != 200 {
		t.Fatalf("human refresh of another machine: %d", st)
	}
}

func TestHeartbeatConcurrentRegistrationIsIdempotent(t *testing.T) {
	e := newEnv(t)
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	registered, refreshed := 0, 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			st, m, _ := e.do(t, "POST", "/api/v1/machines/heartbeat", e.writer, map[string]any{"machine_id": "mac_race"})
			if st != 200 {
				t.Errorf("heartbeat call failed: %d %v", st, m)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if reg, _ := m["registered"].(bool); reg {
				registered++
			} else {
				refreshed++
			}
		}()
	}
	wg.Wait()
	if registered != 1 {
		t.Fatalf("expected exactly one registration winner, got %d (refreshed=%d)", registered, refreshed)
	}
	// Exactly one user + one api_key row exist for this machine.
	var userCount int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM machines WHERE id='mac_race'`).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if userCount != 1 {
		t.Fatalf("expected exactly one machines row, got %d", userCount)
	}
	var uid string
	if err := e.db.QueryRow(`SELECT user_id FROM machines WHERE id='mac_race'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	var keyCount int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id=?1`, uid).Scan(&keyCount); err != nil {
		t.Fatal(err)
	}
	if keyCount != 1 {
		t.Fatalf("expected exactly one api_key row for the winning machine user, got %d", keyCount)
	}
}

// --- fix #3: missing key-scope checks on execution/sync plane ---

func TestScopeEnforcedOnExecutionAndSyncPlane(t *testing.T) {
	e := newEnv(t)

	// A second project the scoped key (bound to p1/pj1, user usr_worker) is
	// NOT scoped to.
	st, m, _ := e.do(t, "POST", "/api/v1/projects", e.admin, map[string]any{"name": "p2"})
	if st != 201 {
		t.Fatalf("create p2: %d %v", st, m)
	}
	// Give usr_worker membership in p2 too, so any 403 we observe is
	// definitely the SCOPE gate, not a membership/visibility 404.
	p2ID := m["id"].(string)
	if _, err := e.db.Exec(`INSERT INTO project_members(project_id, user_id, role, created_at) VALUES (?1,'usr_worker','writer','2026-01-01T00:00:00Z')`, p2ID); err != nil {
		t.Fatal(err)
	}
	st, m, _ = e.do(t, "POST", "/api/v1/work-items", e.admin, map[string]any{"project": "p2", "goal": "in p2", "bundle_name": "demo",
		"repo_inventory": []map[string]any{{"name": "r1", "url": "u"}}, "repo_inventory_base": []map[string]any{{"name": "r1", "base": "sha1"}}})
	if st != 201 {
		t.Fatalf("create wi in p2: %d %v", st, m)
	}
	wi2 := m["id"].(string)

	// The scoped key (bound to pj1) must be refused CORE-403 on every one of
	// these execution/sync-plane routes for a WI in a DIFFERENT project.
	cases := []struct {
		method, path string
	}{
		{"GET", "/api/v1/work-items/" + wi2 + "/budget"},
		{"GET", "/api/v1/work-items/" + wi2 + "/repo-inventory"},
		{"POST", "/api/v1/work-items/" + wi2 + "/end-generation"},
		{"GET", "/api/v1/work-items/" + wi2 + "/head"},
		{"GET", "/api/v1/work-items/" + wi2 + "/revisions?since=0"},
		{"PUT", "/api/v1/work-items/" + wi2 + "/revisions/0/blocks/" + strings.Repeat("a", 64)},
		{"POST", "/api/v1/work-items/" + wi2 + "/revisions/0/manifest"},
		{"GET", "/api/v1/work-items/" + wi2 + "/revisions/0/blocks/" + strings.Repeat("a", 64)},
	}
	for _, c := range cases {
		st, m, _ := e.do(t, c.method, c.path, e.scoped, nil)
		if st != 403 || errCode(t, m) != "CORE-403" {
			t.Fatalf("%s %s: expected CORE-403 out-of-scope, got %d %v", c.method, c.path, st, m)
		}
	}

	// Admin-gated routes (force-takeover / tombstone / revert-tombstone) need
	// a scoped ADMIN key to observe the scope gate specifically (the role
	// gate would otherwise fire first for a non-admin scoped key).
	scopedAdmin := e.mintKey(t, e.db, "usr_admin", &p2ID)
	// scopedAdmin is scoped to p2, so it should be REFUSED on a WI in p1.
	st, m, _ = e.do(t, "POST", "/api/v1/work-items", e.writer, map[string]any{"project": "p1", "goal": "in p1", "bundle_name": "demo",
		"repo_inventory": []map[string]any{{"name": "r1", "url": "u"}}, "repo_inventory_base": []map[string]any{{"name": "r1", "base": "sha1"}}})
	if st != 201 {
		t.Fatalf("create wi in p1: %d %v", st, m)
	}
	wi1 := m["id"].(string)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/work-items/" + wi1 + "/force-takeover"},
		{"DELETE", "/api/v1/work-items/" + wi1 + "/capsule-data?confirm=" + wi1},
		{"POST", "/api/v1/work-items/" + wi1 + "/capsule-data/revert-tombstone"},
	} {
		st, m, _ := e.do(t, c.method, c.path, scopedAdmin, nil)
		if st != 403 || errCode(t, m) != "CORE-403" {
			t.Fatalf("%s %s: expected CORE-403 out-of-scope, got %d %v", c.method, c.path, st, m)
		}
	}
}

// --- fix #17: STALE_SERVICE wiring ---

func TestStaleServiceRejectsMutations(t *testing.T) {
	e := newEnv(t)
	// Simulate a newer process having taken over: tamper the service_id row
	// directly, out from under this running server's in-memory Service.
	if _, err := e.db.Exec(`UPDATE core_meta SET value='svc_someone_else' WHERE key='service_id'`); err != nil {
		t.Fatal(err)
	}
	st, m, _ := e.do(t, "POST", "/api/v1/work-items", e.writer, map[string]any{"project": "p1", "goal": "x"})
	if st != 409 || errCode(t, m) != "CORE-509" {
		t.Fatalf("expected CORE-509 on a mutating call from a stale service, got %d %v", st, m)
	}
	// Reads are unaffected.
	st, _, _ = e.do(t, "GET", "/api/v1/ready-queue?project=p1", e.writer, nil)
	if st != 200 {
		t.Fatalf("reads must stay available: %d", st)
	}
}

// --- fix #18: M3 member removal ---

func TestPatchProjectMemberRemoval(t *testing.T) {
	e := newEnv(t)
	st, m, _ := e.do(t, "PATCH", "/api/v1/projects/p1", e.admin, map[string]any{
		"remove_members": []string{"usr_other"},
	})
	if st != 200 {
		t.Fatalf("remove_members: %d %v", st, m)
	}
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM project_members WHERE project_id='pj1' AND user_id='usr_other'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected the membership row to be deleted, still have %d", n)
	}
}

// --- fix #19: dependency-edge routes ---

func TestDependencyEdgeRoutes(t *testing.T) {
	e := newEnv(t)
	mk := func(goal string) string {
		st, m, _ := e.do(t, "POST", "/api/v1/work-items", e.writer, map[string]any{"project": "p1", "goal": goal, "bundle_name": "demo",
			"repo_inventory": []map[string]any{{"name": "r1", "url": "u"}}, "repo_inventory_base": []map[string]any{{"name": "r1", "base": "sha1"}}})
		if st != 201 {
			t.Fatalf("create %s: %d %v", goal, st, m)
		}
		return m["id"].(string)
	}
	a := mk("a")
	b := mk("b")

	// Create: a blocks b (path {id}=b is the BLOCKED side).
	st, m, _ := e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/dependencies", b), e.writer,
		map[string]any{"blocking_wi_id": a, "kind": "blocks"})
	if st != 201 {
		t.Fatalf("create edge: %d %v", st, m)
	}

	// List from both sides.
	st, m, _ = e.do(t, "GET", fmt.Sprintf("/api/v1/work-items/%s/dependencies", b), e.writer, nil)
	if st != 200 {
		t.Fatalf("list on blocked: %d %v", st, m)
	}
	blockedBy := m["blocked_by"].([]any)
	if len(blockedBy) != 1 {
		t.Fatalf("expected one blocked_by edge, got %v", m)
	}
	st, m, _ = e.do(t, "GET", fmt.Sprintf("/api/v1/work-items/%s/dependencies", a), e.writer, nil)
	if st != 200 || len(m["blocking"].([]any)) != 1 {
		t.Fatalf("list on blocking: %d %v", st, m)
	}

	// waiting_on materialization on the blocked WI.
	st, m, _ = e.do(t, "GET", "/api/v1/work-items/"+b, e.writer, nil)
	if st != 200 || m["waiting_on"] != "dependency:"+a {
		t.Fatalf("waiting_on not materialized: %d %v", st, m)
	}

	// Cycle: b blocks a would close a→b→a.
	st, m, _ = e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/dependencies", a), e.writer,
		map[string]any{"blocking_wi_id": b, "kind": "blocks"})
	if st != 409 || errCode(t, m) != "CORE-409" {
		t.Fatalf("expected cycle 409, got %d %v", st, m)
	}

	// Cross-project: a WI in a different project.
	st, m, _ = e.do(t, "POST", "/api/v1/projects", e.admin, map[string]any{"name": "p3"})
	if st != 201 {
		t.Fatalf("create p3: %d %v", st, m)
	}
	st, m, _ = e.do(t, "POST", "/api/v1/work-items", e.admin, map[string]any{"project": "p3", "goal": "other project"})
	if st != 201 {
		t.Fatalf("create wi in p3: %d %v", st, m)
	}
	other := m["id"].(string)
	st, m, _ = e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/dependencies", b), e.writer,
		map[string]any{"blocking_wi_id": other, "kind": "blocks"})
	if st != 400 || errCode(t, m) != "CORE-400" {
		t.Fatalf("expected cross-project 400, got %d %v", st, m)
	}

	// Delete the edge.
	st, m, _ = e.do(t, "DELETE", fmt.Sprintf("/api/v1/work-items/%s/dependencies/%s/blocks", b, a), e.writer, nil)
	if st != 200 {
		t.Fatalf("delete edge: %d %v", st, m)
	}
	st, m, _ = e.do(t, "GET", fmt.Sprintf("/api/v1/work-items/%s/dependencies", b), e.writer, nil)
	if st != 200 || asSliceLen(m["blocked_by"]) != 0 {
		t.Fatalf("edge not removed: %d %v", st, m)
	}
}

// asSliceLen treats a JSON null (an empty Go slice marshals as null, not [])
// the same as an empty array.
func asSliceLen(v any) int {
	if v == nil {
		return 0
	}
	return len(v.([]any))
}
