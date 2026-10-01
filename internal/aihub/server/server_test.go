package server

// Server tests: the auth matrix (no key / wrong key / right key), the
// typed error-envelope shape, and the claim → git-credential →
// end-generation flow over real HTTP.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/aihub/git"
	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

type srvEnv struct {
	srv     *Server
	ts      *httptest.Server
	db      *core.DB
	dataDir string
	admin   *core.AdminManager
	key     string // admin api key plaintext
	wiID    string
}

func newSrvEnv(t *testing.T) *srvEnv {
	t.Helper()
	if _, err := http.NewRequest("GET", "/", nil); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	db, err := core.Open(dataDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	admin := core.NewAdminManager(db)
	u, err := admin.CreateUser("root", "root@example.com", "admin", "human")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	issued, err := admin.CreateAPIKey(u.ID, "test", "")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if _, err := admin.CreateProject(core.CreateProjectRequest{Name: "alpha"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	gitMgr, err := git.NewManager(dataDir, core.NewOwnerManager(db))
	if err != nil {
		t.Fatalf("git: %v", err)
	}
	srv := New(db, dataDir, gitMgr)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &srvEnv{srv: srv, ts: ts, db: db, dataDir: dataDir, admin: admin, key: issued.Plain}
}

// do performs an authenticated request; out receives the decoded body.
func (e *srvEnv) do(t *testing.T, method, path, key string, body any, out any) (int, []byte) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != nil && buf.Len() > 0 {
		if err := json.Unmarshal(buf.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v", buf.String(), err)
		}
	}
	return resp.StatusCode, buf.Bytes()
}

func TestAuthMatrix(t *testing.T) {
	e := newSrvEnv(t)
	// Seed one WI as admin.
	var created map[string]any
	code, body := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]string{"project": "alpha", "goal": "auth matrix"}, &created)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}

	// No key → 401 with the typed envelope.
	code, raw := e.do(t, "GET", "/api/v1/work-items", "", nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	var envl map[string]any
	if err := json.Unmarshal(raw, &envl); err != nil {
		t.Fatalf("envelope not JSON: %s", raw)
	}
	if envl["code"] != "AUTH-401" || envl["detail"] == nil {
		t.Fatalf("wrong envelope: %s", raw)
	}

	// Wrong key → 401.
	if code, _ := e.do(t, "GET", "/api/v1/work-items", "bogus-key", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", code)
	}
	// Revoked key → 401.
	u2, _ := e.admin.CreateUser("temp", "t@example.com", "writer", "human")
	k2, _ := e.admin.CreateAPIKey(u2.ID, "t", "")
	if code, _ := e.do(t, "GET", "/api/v1/work-items", k2.Plain, nil, nil); code != http.StatusOK {
		t.Fatalf("live second key: %d", code)
	}
	_ = e.admin.RevokeAPIKey(k2.Token.ID)
	if code, _ := e.do(t, "GET", "/api/v1/work-items", k2.Plain, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked key accepted: %d", code)
	}
	// Right key → 200.
	if code, _ := e.do(t, "GET", "/api/v1/work-items", e.key, nil, nil); code != http.StatusOK {
		t.Fatalf("right key: %d", code)
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	e := newSrvEnv(t)
	// 404 on an unknown WI: typed CORE-404, code+detail in the body.
	code, raw := e.do(t, "GET", "/api/v1/work-items/wi_nosuchthing", e.key, nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("status: %d %s", code, raw)
	}
	var envl struct {
		Code   string          `json:"code"`
		Detail string          `json:"detail"`
		Extra  json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(raw, &envl); err != nil || envl.Code != "CORE-404" || envl.Detail == "" {
		t.Fatalf("bad envelope: %s", raw)
	}
	// 400 with a machine-readable reason rides in Extra.
	code, raw = e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]string{"project": "alpha", "goal": ""}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("bad create status: %d %s", code, raw)
	}
	if err := json.Unmarshal(raw, &envl); err != nil || envl.Code != "CORE-400" {
		t.Fatalf("bad envelope: %s", raw)
	}
	// Non-admin on the admin face → AUTH-403.
	outsider, _ := e.admin.CreateUser("out", "out@example.com", "writer", "human")
	okey, _ := e.admin.CreateAPIKey(outsider.ID, "o", "")
	if code, raw = e.do(t, "POST", "/api/v1/users", okey.Plain,
		map[string]string{"display_name": "x", "email": "x@y", "user_type": "human"}, nil); code != http.StatusForbidden {
		t.Fatalf("non-admin on admin face: %d %s", code, raw)
	}
	if err := json.Unmarshal(raw, &envl); err != nil || envl.Code != "AUTH-403" {
		t.Fatalf("admin-face envelope: %s", raw)
	}
}

func TestClaimFlowOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	// Create an EXECUTION ticket (bundle_name set): only execution tickets
	// are claimable (#17).
	var wi struct {
		ID string `json:"id"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]any{"project": "alpha", "goal": "claim flow", "bundle_name": "demo-bundle"}, &wi); code != 201 || wi.ID == "" {
		t.Fatalf("create: %d %s", code, raw)
	}
	// A REGISTRATION ticket answers CORE-409 NOT_CLAIMABLE (#17).
	var reg struct {
		ID string `json:"id"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]any{"project": "alpha", "goal": "registry only"}, &reg); code != 201 {
		t.Fatalf("create reg: %d %s", code, raw)
	}
	var envl struct {
		Code  string          `json:"code"`
		Extra json.RawMessage `json:"extra"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+reg.ID+"/claim", e.key,
		map[string]any{"session_id": "aaaa1111bbbb2222cccc3333dddd4444"}, &envl); code != 409 || envl.Code != "CORE-409" || !strings.Contains(string(envl.Extra), "NOT_CLAIMABLE") {
		t.Fatalf("registration claim: %d %s", code, raw)
	}
	// Session claim over HTTP: the response carries claim + work_item and
	// NEVER the secret (B3) nor any hash.
	secret := "0123456789abcdef0123456789abcdef"
	var resp struct {
		Claim struct {
			WorkItemID         string `json:"work_item_id"`
			SessionFingerprint string `json:"session_fingerprint"`
			MachineID          string `json:"machine_id"`
			Owner              string `json:"owner"`
			BundleName         string `json:"bundle_name"`
			BundleDigest       string `json:"bundle_digest"`
			ClaimedAt          string `json:"claimed_at"`
		} `json:"claim"`
		WorkItem struct {
			Status             string `json:"status"`
			SessionFingerprint string `json:"session_fingerprint"`
			SessionMachine     string `json:"session_machine"`
			SessionOwner       string `json:"session_owner"`
			SessionStartedAt   string `json:"session_started_at"`
			CleanPause         bool   `json:"clean_pause"`
		} `json:"work_item"`
	}
	code, raw := e.do(t, "POST", "/api/v1/work-items/"+wi.ID+"/claim", e.key,
		map[string]string{"session_id": secret, "machine_id": "m1", "owner": "op", "idempotency_key": "http-1"}, &resp)
	if code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	if resp.Claim.SessionFingerprint != api.SessionFingerprint(secret) || resp.Claim.WorkItemID != wi.ID {
		t.Fatalf("claim shape wrong: %+v", resp.Claim)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "session_hash") {
		t.Fatalf("claim response leaks the secret or the hash: %s", raw)
	}
	// #5: the bundle pin rides the claim object itself (the runtime reads
	// it from resp.Claim, never from resp.WorkItem).
	if resp.Claim.BundleName != "demo-bundle" || resp.Claim.BundleDigest == "" {
		t.Fatalf("claim bundle fields wrong: %+v", resp.Claim)
	}
	if resp.WorkItem.Status != "running" {
		t.Fatalf("work item not running after claim: %+v", resp.WorkItem)
	}
	// The WorkItem JSON carries the m4 public audit block (never a hash).
	if resp.WorkItem.SessionFingerprint != resp.Claim.SessionFingerprint ||
		resp.WorkItem.SessionMachine != "m1" || resp.WorkItem.SessionOwner != "op" ||
		resp.WorkItem.SessionStartedAt == "" || resp.WorkItem.CleanPause {
		t.Fatalf("work item session audit fields wrong: %+v", resp.WorkItem)
	}
	// Idempotent replay (D4): the SAME key answers the SAME claim.
	var replay struct {
		Claim struct {
			SessionFingerprint string `json:"session_fingerprint"`
		} `json:"claim"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+wi.ID+"/claim", e.key,
		map[string]string{"session_id": secret, "machine_id": "m1", "owner": "op", "idempotency_key": "http-1"}, &replay); code != 200 {
		t.Fatalf("idempotent replay: %d %s", code, raw)
	}
	if replay.Claim.SessionFingerprint != resp.Claim.SessionFingerprint {
		t.Fatalf("idempotent replay returned a different session: %+v", replay.Claim)
	}
	// A second claim without a key: ALREADY_HELD.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+wi.ID+"/claim", e.key,
		map[string]string{"session_id": "ffffffffffffffffffffffffffffffff"}, &envl); code != 409 || !strings.Contains(string(envl.Extra), "ALREADY_HELD") {
		t.Fatalf("double claim not refused: %d %s", code, raw)
	}
	// The bare repo exists and the git face answers with the session
	// secret as the push password (info/refs over the same server).
	gitURL := fmt.Sprintf("%s/git/%s.git/info/refs?service=git-upload-pack", e.ts.URL, wi.ID)
	req, _ := http.NewRequest("GET", strings.Replace(gitURL, "http://", "http://"+wi.ID+":"+secret+"@", 1), nil)
	gresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("git info/refs: %v", err)
	}
	gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("git info/refs with session secret: %d", gresp.StatusCode)
	}
	// Wrong secret -> 401.
	req, _ = http.NewRequest("GET", strings.Replace(gitURL, "http://", "http://"+wi.ID+":nope@", 1), nil)
	gresp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("git info/refs: %v", err)
	}
	gresp.Body.Close()
	if gresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("git info/refs with bad secret: %d", gresp.StatusCode)
	}
	// session-end: wrapped; the session row is fully cleared, then the
	// git face refuses (no live session).
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+wi.ID+"/session-end", e.key,
		map[string]string{"session_id": secret, "status": "wrapped", "note": "done"}, nil); code != 200 {
		t.Fatalf("session-end: %d %s", code, raw)
	}
	var ended struct {
		Status             string `json:"status"`
		SessionFingerprint string `json:"session_fingerprint"`
		CleanPause         bool   `json:"clean_pause"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+wi.ID, e.key, nil, &ended); code != 200 || ended.Status != "wrapped" || ended.SessionFingerprint != "" {
		t.Fatalf("WI after session-end: %d %s %+v", code, raw, ended)
	}
	req, _ = http.NewRequest("GET", strings.Replace(gitURL, "http://", "http://"+wi.ID+":"+secret+"@", 1), nil)
	gresp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("git after wrap: %v", err)
	}
	gresp.Body.Close()
	if gresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("git face alive after session-end: %d", gresp.StatusCode)
	}
}

func TestMembershipBoundary(t *testing.T) {
	e := newSrvEnv(t)
	var wi struct {
		ID string `json:"id"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]string{"project": "alpha", "goal": "hidden object"}, &wi); code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	// An outsider with a valid key sees nothing: 404, not 403 (no
	// existence leak).
	outsider, _ := e.admin.CreateUser("out2", "out2@example.com", "writer", "human")
	okey, _ := e.admin.CreateAPIKey(outsider.ID, "o2", "")
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+wi.ID, okey.Plain, nil, nil); code != 404 {
		t.Fatalf("outsider detail: %d %s", code, raw)
	}
	// The list is trimmed for the outsider.
	var list struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items?project=alpha", okey.Plain, nil, &list); code != 200 || list.Total != 0 {
		t.Fatalf("outsider list: %d %s", code, raw)
	}
	// Membership (writer) grants read + create.
	_ = e.admin.AddMember(e.adminProjectID(t, "alpha"), outsider.ID, "writer")
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+wi.ID, okey.Plain, nil, nil); code != 200 {
		t.Fatalf("member detail: %d %s", code, raw)
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items?project=alpha", okey.Plain, nil, &list); code != 200 || list.Total != 1 {
		t.Fatalf("member list: %d %s total=%d", code, raw, list.Total)
	}
}

func (e *srvEnv) adminProjectID(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := e.db.QueryRow(`SELECT id FROM projects WHERE name=?1`, name).Scan(&id); err != nil {
		t.Fatalf("project id: %v", err)
	}
	return id
}

func TestMemoryOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	var created struct {
		ID string `json:"id"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/memories", e.key, map[string]any{
		"project": "alpha", "type": "experience.debug", "visibility": "project",
		"content": "the quick brown fox over http",
	}, &created); code != 201 || created.ID == "" {
		t.Fatalf("create memory: %d %s", code, raw)
	}
	var search struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/memories/search?q=brown+fox", e.key, nil, &search); code != 200 || search.Total != 1 {
		t.Fatalf("search: %d %s", code, raw)
	}
	// Redact → GET is a 404.
	if code, _ := e.do(t, "POST", "/api/v1/memories/"+created.ID+"/redact", e.key, nil, nil); code != 200 {
		t.Fatalf("redact: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/v1/memories/"+created.ID, e.key, nil, nil); code != 404 {
		t.Fatalf("redacted memory served: %d", code)
	}
}

// ---------------------------------------------------------------------------
// Management face over HTTP (#10): the six endpoints the CLI verbs call.

func TestAdminFaceEndpointsOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	// A non-admin caller for the 403 matrix.
	outsider, _ := e.admin.CreateUser("plain", "plain@example.com", "writer", "human")
	okey, _ := e.admin.CreateAPIKey(outsider.ID, "o", "")

	// users get / patch.
	var u struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/users/"+outsider.ID, e.key, nil, &u); code != 200 || u.ID != outsider.ID {
		t.Fatalf("users get: %d %s", code, raw)
	}
	if code, raw := e.do(t, "GET", "/api/v1/users/usr_nosuch", e.key, nil, nil); code != 404 {
		t.Fatalf("users get unknown: %d %s", code, raw)
	}
	if code, raw := e.do(t, "PATCH", "/api/v1/users/"+outsider.ID, e.key,
		map[string]string{"display_name": "renamed", "role": "admin"}, &u); code != 200 || u.DisplayName != "renamed" || u.Role != "admin" {
		t.Fatalf("users patch: %d %s", code, raw)
	}
	if code, _ := e.do(t, "PATCH", "/api/v1/users/"+outsider.ID, e.key, map[string]string{}, nil); code != 400 {
		t.Fatalf("users patch empty body: %d", code)
	}
	if code, _ := e.do(t, "PATCH", "/api/v1/users/"+outsider.ID, e.key, map[string]string{"role": "bogus"}, nil); code != 400 {
		t.Fatalf("users patch bad role: %d", code)
	}
	// Back to writer: the 403 matrix below depends on the non-admin role.
	if code, _ := e.do(t, "PATCH", "/api/v1/users/"+outsider.ID, e.key, map[string]string{"role": "writer"}, nil); code != 200 {
		t.Fatalf("users patch back to writer: %d", code)
	}

	// api-keys list: metadata only, never material or hash.
	issued, _ := e.admin.CreateAPIKey(outsider.ID, "listed", "")
	var keys struct {
		Items []struct {
			ID     string `json:"id"`
			UserID string `json:"user_id"`
		} `json:"items"`
		Total int `json:"total"`
	}
	code, raw := e.do(t, "GET", "/api/v1/api-keys", e.key, nil, &keys)
	if code != 200 || keys.Total < 2 {
		t.Fatalf("api-keys list: %d %s total=%d", code, raw, keys.Total)
	}
	if strings.Contains(string(raw), issued.Plain) || strings.Contains(string(raw), "key_hash") {
		t.Fatalf("api-keys list leaks material: %s", raw)
	}
	found := false
	for _, k := range keys.Items {
		if k.ID == issued.Token.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("issued key not listed: %s", raw)
	}

	// projects patch: visibility first.
	var p struct {
		ID      string `json:"id"`
		Visible bool   `json:"visible"`
		Members []struct {
			UserID string `json:"user_id"`
			Role   string `json:"role"`
		} `json:"members"`
	}
	if code, raw := e.do(t, "PATCH", "/api/v1/projects/alpha", e.key,
		map[string]any{"visible": false}, &p); code != 200 || p.Visible {
		t.Fatalf("projects patch visible: %d %s", code, raw)
	}
	// Membership: upsert u2 as maintainer; removal runs after upsert.
	u2, _ := e.admin.CreateUser("maint", "maint@example.com", "writer", "human")
	mk, _ := e.admin.CreateAPIKey(u2.ID, "m", "")
	if code, raw := e.do(t, "PATCH", "/api/v1/projects/alpha", e.key,
		map[string]any{"members": []map[string]string{{"user_id": u2.ID, "role": "maintainer"}}}, &p); code != 200 || len(p.Members) != 1 {
		t.Fatalf("members add: %d %s", code, raw)
	}
	// The maintainer (non-admin) may patch; a writer member may not.
	if code, raw := e.do(t, "PATCH", "/api/v1/projects/alpha", mk.Plain,
		map[string]any{"visible": true}, &p); code != 200 || !p.Visible {
		t.Fatalf("maintainer patch: %d %s", code, raw)
	}
	// A bad member role or unknown user is a clean 400/404.
	if code, _ := e.do(t, "PATCH", "/api/v1/projects/alpha", e.key,
		map[string]any{"members": []map[string]string{{"user_id": u2.ID, "role": "owner"}}}, nil); code != 400 {
		t.Fatalf("bad member role: %d", code)
	}
	if code, _ := e.do(t, "PATCH", "/api/v1/projects/alpha", e.key,
		map[string]any{"members": []map[string]string{{"user_id": "usr_nosuch", "role": "writer"}}}, nil); code != 404 {
		t.Fatalf("unknown member user: %d", code)
	}
	// Explicit remove list.
	if code, raw := e.do(t, "PATCH", "/api/v1/projects/alpha", e.key,
		map[string]any{"remove_members": []string{u2.ID}}, &p); code != 200 || len(p.Members) != 0 {
		t.Fatalf("members remove: %d %s", code, raw)
	}
	if code, _ := e.do(t, "PATCH", "/api/v1/projects/alpha", mk.Plain, map[string]any{}, nil); code != 403 {
		t.Fatalf("removed maintainer still patches: %d", code)
	}
	if code, _ := e.do(t, "PATCH", "/api/v1/projects/alpha", okey.Plain, map[string]any{}, nil); code != 403 {
		t.Fatalf("writer patches project: %d", code)
	}
	if code, _ := e.do(t, "PATCH", "/api/v1/projects/nope", e.key, map[string]any{}, nil); code != 404 {
		t.Fatalf("unknown project patch: %d", code)
	}

	// backup: snapshot file lands under <dataDir>/backups.
	var bk struct {
		Backup string `json:"backup"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/admin/backup", e.key, map[string]any{}, &bk); code != 200 || bk.Backup == "" {
		t.Fatalf("backup: %d %s", code, raw)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "backups", bk.Backup)); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}

	// rotate-secret: grace then rollback, with the mode validation.
	var rot struct {
		Epoch int    `json:"epoch"`
		Mode  string `json:"mode"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/service/rotate-secret", e.key,
		map[string]any{"mode": "grace", "grace_seconds": 300}, &rot); code != 200 || rot.Epoch != 1 || rot.Mode != "grace" {
		t.Fatalf("rotate grace: %d %s", code, raw)
	}
	if code, _ := e.do(t, "POST", "/api/v1/service/rotate-secret", e.key,
		map[string]any{"mode": "grace", "grace_seconds": 0}, nil); code != 400 {
		t.Fatalf("grace without window: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/v1/service/rotate-secret", e.key,
		map[string]any{"mode": "rollback", "grace_seconds": 5}, nil); code != 400 {
		t.Fatalf("rollback with window: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/v1/service/rotate-secret", e.key,
		map[string]any{"mode": "bogus"}, nil); code != 400 {
		t.Fatalf("bogus mode: %d", code)
	}
	if code, raw := e.do(t, "POST", "/api/v1/service/rotate-secret", e.key,
		map[string]any{"mode": "rollback"}, &rot); code != 200 || rot.Epoch != 2 {
		t.Fatalf("rotate rollback: %d %s", code, raw)
	}
	// Rollback retired epoch 1 immediately.
	var retired sql.NullString
	if err := e.db.QueryRow(`SELECT retired_at FROM service_secret_epochs WHERE epoch=1`).Scan(&retired); err != nil || !retired.Valid {
		t.Fatalf("epoch 1 not retired by rollback: %v %v", retired, err)
	}

	// Non-admin callers are refused on every new admin endpoint.
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/users/" + outsider.ID, nil},
		{"PATCH", "/api/v1/users/" + outsider.ID, map[string]string{"display_name": "x"}},
		{"GET", "/api/v1/api-keys", nil},
		{"POST", "/api/v1/admin/backup", map[string]any{}},
		{"POST", "/api/v1/service/rotate-secret", map[string]any{"mode": "grace", "grace_seconds": 60}},
	} {
		if code, raw := e.do(t, tc.method, tc.path, okey.Plain, tc.body, nil); code != 403 {
			t.Fatalf("non-admin %s %s: %d %s", tc.method, tc.path, code, raw)
		}
	}
}

// TestApiKeyScopeFieldCompat pins #11: the CLI sends the scope as
// project_id, the server used to read project_scope — the key was minted
// GLOBAL. Both field names now work (and must actually scope the key).
func TestApiKeyScopeFieldCompat(t *testing.T) {
	e := newSrvEnv(t)
	if _, err := e.admin.CreateProject(core.CreateProjectRequest{Name: "beta"}); err != nil {
		t.Fatal(err)
	}
	// Seed one WI per project.
	var wi struct {
		ID string `json:"id"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]any{"project": "alpha", "goal": "alpha wi", "bundle_name": "b"}, &wi); code != 201 {
		t.Fatalf("alpha wi: %d %s", code, raw)
	}
	alphaID := wi.ID
	if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]any{"project": "beta", "goal": "beta wi", "bundle_name": "b"}, &wi); code != 201 {
		t.Fatalf("beta wi: %d %s", code, raw)
	}
	betaID := wi.ID

	target, _ := e.admin.CreateUser("scoped", "scoped@example.com", "writer", "human")
	// The target is a writer on alpha: scoping must be the ONLY thing that
	// keeps the key off beta (a plain membership denial would 404-fold).
	if err := e.admin.AddMember(e.adminProjectID(t, "alpha"), target.ID, "writer"); err != nil {
		t.Fatal(err)
	}
	if err := e.admin.AddMember(e.adminProjectID(t, "beta"), target.ID, "writer"); err != nil {
		t.Fatal(err)
	}
	// BIDIRECTIONAL: the real client (which sends project_id) against the
	// real server.
	client := aihubclient.New(e.ts.URL, e.key)
	raw, err := client.CreateApiKey(aihubclient.ApiKeyCreateRequest{UserID: target.ID, ProjectID: strPtrTest("alpha")})
	if err != nil {
		t.Fatalf("client create key: %v", err)
	}
	var issued struct {
		Plain string `json:"plain"`
	}
	if err := json.Unmarshal(raw, &issued); err != nil || issued.Plain == "" {
		t.Fatalf("no plaintext in issue response: %s", raw)
	}
	// The key is genuinely scoped: its own project reads fine, the other
	// project is an AUTH-403 (scope check fires before membership).
	if code, _ := e.do(t, "GET", "/api/v1/work-items/"+alphaID, issued.Plain, nil, nil); code != 200 {
		t.Fatalf("scoped key denied on its own project: %d", code)
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+betaID, issued.Plain, nil, nil); code != 403 {
		t.Fatalf("scoped key reached another project: %d %s", code, raw)
	}
	// The legacy field name (project_scope) still works, by name or id.
	var legacy struct {
		Plain string `json:"plain"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/api-keys", e.key,
		map[string]any{"user_id": target.ID, "project_scope": "beta"}, &legacy); code != 201 || legacy.Plain == "" {
		t.Fatalf("legacy scope field: %d %s", code, raw)
	}
	if code, _ := e.do(t, "GET", "/api/v1/work-items/"+betaID, legacy.Plain, nil, nil); code != 200 {
		t.Fatalf("legacy-scoped key denied on beta: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/v1/work-items/"+alphaID, legacy.Plain, nil, nil); code != 403 {
		t.Fatalf("legacy-scoped key reached alpha: %d", code)
	}
	// Both names present and agreeing is accepted...
	var both struct {
		Plain string `json:"plain"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/api-keys", e.key,
		map[string]any{"user_id": target.ID, "project_id": "alpha", "project_scope": "alpha"}, &both); code != 201 || both.Plain == "" {
		t.Fatalf("both fields agreeing: %d %s", code, raw)
	}
	// ...disagreeing is a 400.
	if code, _ := e.do(t, "POST", "/api/v1/api-keys", e.key,
		map[string]any{"user_id": target.ID, "project_id": "alpha", "project_scope": "beta"}, nil); code != 400 {
		t.Fatalf("both fields disagreeing: %d", code)
	}
}

func strPtrTest(s string) *string { return &s }

// TestClaimResponseCarriesProjectRepos: the claim endpoint serves the
// project's task-repo inventory on the claim object (the runtime's
// claim→clone lane reads it from resp.Claim).
func TestClaimResponseCarriesProjectRepos(t *testing.T) {
	e := newSrvEnv(t)
	if _, err := e.admin.CreateProject(core.CreateProjectRequest{
		Name:  "bravo",
		Repos: []api.ProjectRepo{{Name: "app", URL: "file:///srv/app.git"}},
	}); err != nil {
		t.Fatalf("project: %v", err)
	}
	var wi struct {
		ID string `json:"id"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]any{"project": "bravo", "goal": "repos over http", "bundle_name": "demo-bundle"}, &wi); code != 201 || wi.ID == "" {
		t.Fatalf("create: %d %s", code, raw)
	}
	var resp struct {
		Claim struct {
			Repos []api.ProjectRepo `json:"repos"`
		} `json:"claim"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+wi.ID+"/claim", e.key,
		map[string]string{"session_id": "55556666777788885555666677778888"}, &resp); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	if len(resp.Claim.Repos) != 1 || resp.Claim.Repos[0].Name != "app" || resp.Claim.Repos[0].URL != "file:///srv/app.git" {
		t.Fatalf("claim repos wrong over HTTP: %+v", resp.Claim.Repos)
	}
}

// TestMemorySearchExcerpt (F6): the search face's additive excerpt
// extension — a caller that names an excerpt length gets each hit with a
// bounded content prefix attached (content_excerpt); a plain search
// keeps the exact v0 answer shape (no excerpt key at all).
func TestMemorySearchExcerpt(t *testing.T) {
	e := newSrvEnv(t)
	if code, raw := e.do(t, "POST", "/api/v1/memories", e.key, map[string]any{
		"project": "alpha", "type": "experience.debug", "visibility": "project",
		"content": "the lesson about bounded retry ladders",
	}, nil); code != 201 {
		t.Fatalf("create memory: %d %s", code, raw)
	}

	// Plain search: the v0 shape, no excerpt key on the row.
	var plain struct {
		Items []map[string]any `json:"items"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/memories/search?q=ladders", e.key, nil, &plain); code != 200 || len(plain.Items) != 1 {
		t.Fatalf("plain search: %d %s", code, raw)
	}
	if _, has := plain.Items[0]["content_excerpt"]; has {
		t.Fatalf("plain search must not carry an excerpt: %v", plain.Items[0])
	}

	// GET with an excerpt: a bounded rune prefix of the authority content.
	var with struct {
		Items []struct {
			ID             string `json:"id"`
			ContentExcerpt string `json:"content_excerpt"`
		} `json:"items"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/memories/search?q=ladders&excerpt=9", e.key, nil, &with); code != 200 || len(with.Items) != 1 {
		t.Fatalf("excerpt search: %d %s", code, raw)
	}
	if with.Items[0].ContentExcerpt != "the lesso" {
		t.Fatalf("content_excerpt = %q, want the first 9 runes", with.Items[0].ContentExcerpt)
	}

	// POST with an excerpt: the same extension on the POST face.
	if code, raw := e.do(t, "POST", "/api/v1/memories/search", e.key, map[string]any{
		"q": "ladders", "excerpt": 40,
	}, &with); code != 200 || len(with.Items) != 1 || with.Items[0].ContentExcerpt != "the lesson about bounded retry ladders" {
		t.Fatalf("POST excerpt search: %d %s (%+v)", code, raw, with.Items)
	}
}

// TestMemorySearchTypeWildcard: the type filter's trailing-".*" prefix
// interpretation (X.* → prefix "X.") is shared by the GET and POST faces
// of the search endpoint; non-wildcard values keep exact matching.
func TestMemorySearchTypeWildcard(t *testing.T) {
	e := newSrvEnv(t)
	mk := func(memType string) {
		t.Helper()
		if code, raw := e.do(t, "POST", "/api/v1/memories", e.key, map[string]any{
			"project": "alpha", "type": memType, "visibility": "project",
			"content": "the shared xylophone token",
		}, nil); code != 201 {
			t.Fatalf("create %s: %d %s", memType, code, raw)
		}
	}
	mk("experience.debug")
	mk("experience.approach")
	mk("fact.note")

	var res struct {
		Items []struct {
			Type string `json:"type"`
		} `json:"items"`
		Total int `json:"total"`
	}
	// GET face: experience.* returns both experience.* rows, not fact.note.
	if code, raw := e.do(t, "GET", "/api/v1/memories/search?q=xylophone&type=experience.*", e.key, nil, &res); code != 200 || len(res.Items) != 2 {
		t.Fatalf("GET experience.*: %d %s (%d items)", code, raw, len(res.Items))
	}
	for _, it := range res.Items {
		if !strings.HasPrefix(it.Type, "experience.") {
			t.Fatalf("GET experience.* matched type %q", it.Type)
		}
	}
	// POST face: the same interpretation.
	if code, raw := e.do(t, "POST", "/api/v1/memories/search", e.key, map[string]any{
		"q": "xylophone", "type": "experience.*",
	}, &res); code != 200 || len(res.Items) != 2 {
		t.Fatalf("POST experience.*: %d %s (%d items)", code, raw, len(res.Items))
	}
	for _, it := range res.Items {
		if !strings.HasPrefix(it.Type, "experience.") {
			t.Fatalf("POST experience.* matched type %q", it.Type)
		}
	}
	// GET face, non-wildcard: exact type only.
	if code, raw := e.do(t, "GET", "/api/v1/memories/search?q=xylophone&type=experience.debug", e.key, nil, &res); code != 200 || len(res.Items) != 1 || res.Items[0].Type != "experience.debug" {
		t.Fatalf("GET exact: %d %s (%+v)", code, raw, res.Items)
	}
	// POST face, non-wildcard: exact type only.
	if code, raw := e.do(t, "POST", "/api/v1/memories/search", e.key, map[string]any{
		"q": "xylophone", "type": "experience.debug",
	}, &res); code != 200 || len(res.Items) != 1 || res.Items[0].Type != "experience.debug" {
		t.Fatalf("POST exact: %d %s (%+v)", code, raw, res.Items)
	}
	// fact.* never matches experience.* rows, on either face.
	if code, raw := e.do(t, "GET", "/api/v1/memories/search?q=xylophone&type=fact.*", e.key, nil, &res); code != 200 || len(res.Items) != 1 || res.Items[0].Type != "fact.note" {
		t.Fatalf("GET fact.*: %d %s (%+v)", code, raw, res.Items)
	}
	if code, raw := e.do(t, "POST", "/api/v1/memories/search", e.key, map[string]any{
		"q": "xylophone", "type": "fact.*",
	}, &res); code != 200 || len(res.Items) != 1 || res.Items[0].Type != "fact.note" {
		t.Fatalf("POST fact.*: %d %s (%+v)", code, raw, res.Items)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/work-items/{id}/progress — the §5.2 read-through face.

// runGitIn runs one git command inside dir (test helper for seeding bare
// repos); failure is fatal.
func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", args, err, out)
	}
}

// tryGitIn runs one git command and reports success (non-fatal variant
// of runGitIn, for optional setup steps).
func tryGitIn(t *testing.T, dir string, args ...string) bool {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("git %s (tolerated): %v: %s", args, err, out)
		return false
	}
	return true
}

// pushProgressRev builds a throwaway worktree whose runtime-state/execution.json
// is content, commits it with the seq trailer and pushes to the WI's bare repo.
// The committer date is pinned so head_updated_at is assertable. When the
// bare repo already holds revisions, the worktree builds on its main tip so
// the push stays fast-forward.
func pushProgressRev(t *testing.T, gitRoot, wiID, content string, seq int64, at string) {
	t.Helper()
	wt := t.TempDir()
	bare := filepath.Join(gitRoot, wiID+".git")
	runGitIn(t, wt, "init", "--initial-branch=main", ".")
	if tryGitIn(t, wt, "fetch", bare, "main") {
		tryGitIn(t, wt, "reset", "--hard", "FETCH_HEAD")
	}
	if err := os.MkdirAll(filepath.Join(wt, "runtime-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "runtime-state", "execution.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, wt, "add", "-A")
	cmd := exec.Command("git", "commit", "-m", fmt.Sprintf("rev\n\nPolyforge-Seq: %d", seq))
	cmd.Dir = wt
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_COMMITTER_DATE="+at, "GIT_AUTHOR_DATE="+at,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	runGitIn(t, wt, "push", filepath.Join(gitRoot, wiID+".git"), "HEAD:refs/heads/main")
}

func TestProgressReadThrough(t *testing.T) {
	e := newSrvEnv(t)
	var wi struct {
		ID string `json:"id"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]any{"project": "alpha", "goal": "progress read-through", "bundle_name": "b"}, &wi); code != 201 || wi.ID == "" {
		t.Fatalf("create: %d %s", code, raw)
	}

	// Unknown WI → typed CORE-404.
	code, raw := e.do(t, "GET", "/api/v1/work-items/wi_nosuchthing/progress", e.key, nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown WI: %d %s", code, raw)
	}
	var envl api.Error
	if err := json.Unmarshal(raw, &envl); err != nil || envl.Code != api.ErrCoreNotFound {
		t.Fatalf("unknown WI envelope: %s (%v)", raw, err)
	}

	// Known WI, never published → CORE-504 (no invented HEAD data).
	code, raw = e.do(t, "GET", "/api/v1/work-items/"+wi.ID+"/progress", e.key, nil, nil)
	if code != 504 { // status derives from the numeric suffix: CORE-504 → HTTP 504
		t.Fatalf("never published: %d %s", code, raw)
	}
	if err := json.Unmarshal(raw, &envl); err != nil || envl.Code != api.ErrCoreProgressUnavailable {
		t.Fatalf("never published envelope: %s (%v)", raw, err)
	}

	// Publish one revision directly into the bare repo (a local push: no
	// attempt-id option, so the pre-receive attempt gate stays open — the
	// same path the git-package tests use).
	if err := e.srv.Git.EnsureBare(wi.ID); err != nil {
		t.Fatalf("EnsureBare: %v", err)
	}
	execJSON := `{"capsule_id":"cap_1","work_item_id":"` + wi.ID + `","generation":3,` +
		`"current_step":"fix","workflow_status":"running","steps":{},"version":7}`
	pushProgressRev(t, filepath.Join(e.dataDir, "git"), wi.ID, execJSON, 1, "2026-10-01T00:00:00Z")

	var p api.ProgressResponse
	if code, raw = e.do(t, "GET", "/api/v1/work-items/"+wi.ID+"/progress", e.key, nil, &p); code != http.StatusOK {
		t.Fatalf("after publish: %d %s", code, raw)
	}
	if p.WorkItemID != wi.ID || p.HeadSeq != 1 || p.HeadGeneration != 3 {
		t.Errorf("head fields: %+v", p)
	}
	if p.HeadUpdatedAt != "2026-10-01T00:00:00Z" {
		t.Errorf("head_updated_at = %q", p.HeadUpdatedAt)
	}
	if p.FreshnessNote != api.ProgressFreshnessNote {
		t.Errorf("freshness_note = %q", p.FreshnessNote)
	}
	// The progress blob passes through VERBATIM.
	if string(p.Progress) != execJSON {
		t.Errorf("progress not verbatim:\n got: %s\nwant: %s", p.Progress, execJSON)
	}

	// A second publish moves the head: every call reads through live.
	execJSON2 := `{"capsule_id":"cap_1","work_item_id":"` + wi.ID + `","generation":3,` +
		`"current_step":"verify","workflow_status":"running","steps":{},"version":8}`
	pushProgressRev(t, filepath.Join(e.dataDir, "git"), wi.ID, execJSON2, 2, "2026-10-01T00:09:00Z")
	if code, raw = e.do(t, "GET", "/api/v1/work-items/"+wi.ID+"/progress", e.key, nil, &p); code != http.StatusOK {
		t.Fatalf("second publish: %d %s", code, raw)
	}
	if p.HeadSeq != 2 || string(p.Progress) != execJSON2 {
		t.Errorf("second head: seq=%d progress=%s", p.HeadSeq, p.Progress)
	}

	// A non-member (non-admin) key cannot read another project's WI: the
	// object stays hidden behind the same CORE-404 non-visibility the
	// other object-data GET routes enforce.
	outsider, _ := e.admin.CreateUser("stranger", "s@example.com", "writer", "human")
	okey, _ := e.admin.CreateAPIKey(outsider.ID, "s", "")
	code, raw = e.do(t, "GET", "/api/v1/work-items/"+wi.ID+"/progress", okey.Plain, nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("non-member: %d %s", code, raw)
	}
	if err := json.Unmarshal(raw, &envl); err != nil || envl.Code != api.ErrCoreNotFound {
		t.Fatalf("non-member envelope: %s (%v)", raw, err)
	}
	// A project member (viewer) CAN read: membership is the canRead rule.
	member, _ := e.admin.CreateUser("mate", "m@example.com", "writer", "human")
	mkey, _ := e.admin.CreateAPIKey(member.ID, "m", "")
	if _, err := e.admin.PatchProject("alpha", core.ProjectPatchRequest{
		Members: &[]api.ProjectMember{{UserID: member.ID, Role: "viewer"}},
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if code, _ = e.do(t, "GET", "/api/v1/work-items/"+wi.ID+"/progress", mkey.Plain, nil, nil); code != http.StatusOK {
		t.Fatalf("member viewer: %d", code)
	}
}
