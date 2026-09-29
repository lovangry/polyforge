package server

// Server tests: the auth matrix (no key / wrong key / right key), the
// typed error-envelope shape, and the claim → git-credential →
// end-generation flow over real HTTP.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/aihub/git"
)

type srvEnv struct {
	srv   *Server
	ts    *httptest.Server
	db    *core.DB
	admin *core.AdminManager
	key   string // admin api key plaintext
	wiID  string
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
	return &srvEnv{srv: srv, ts: ts, db: db, admin: admin, key: issued.Plain}
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
	// Create a WI.
	var wi struct {
		ID string `json:"id"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]string{"project": "alpha", "goal": "claim flow"}, &wi); code != 201 || wi.ID == "" {
		t.Fatalf("create: %d %s", code, raw)
	}
	// Claim over HTTP: the response carries claim + credential + work_item.
	var resp struct {
		Claim struct {
			AttemptID  string `json:"attempt_id"`
			Generation int64  `json:"generation"`
			Token      string `json:"token"`
		} `json:"claim"`
		Credential struct {
			AttemptID string `json:"attempt_id"`
			Username  string `json:"username"`
			Secret    string `json:"secret"`
		} `json:"credential"`
		WorkItem struct {
			Status string `json:"status"`
		} `json:"work_item"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+wi.ID+"/claim", e.key,
		map[string]string{"intent": "claim", "idempotency_key": "http-1"}, &resp); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	if resp.Claim.AttemptID == "" || resp.Claim.Generation != 1 {
		t.Fatalf("claim shape wrong: %+v", resp)
	}
	if resp.Credential.Username != wi.ID || resp.Credential.Secret == "" {
		t.Fatalf("credential shape wrong: %+v", resp)
	}
	if resp.WorkItem.Status != "running" {
		t.Fatalf("work item not running after claim: %+v", resp.WorkItem)
	}
	// The bare repo exists and the git face answers with the credential
	// (info/refs over the same server).
	gitURL := fmt.Sprintf("%s/git/%s.git/info/refs?service=git-upload-pack", e.ts.URL, wi.ID)
	req, _ := http.NewRequest("GET", strings.Replace(gitURL, "http://", "http://"+wi.ID+":"+resp.Credential.Secret+"@", 1), nil)
	gresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("git info/refs: %v", err)
	}
	gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("git info/refs with credential: %d", gresp.StatusCode)
	}
	// Wrong secret → 401.
	req, _ = http.NewRequest("GET", strings.Replace(gitURL, "http://", "http://"+wi.ID+":nope@", 1), nil)
	gresp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("git info/refs: %v", err)
	}
	gresp.Body.Close()
	if gresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("git info/refs with bad secret: %d", gresp.StatusCode)
	}
	// end-generation: wrapped; then the git face refuses (no active attempt).
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+wi.ID+"/end-generation", e.key,
		map[string]any{"attempt_id": resp.Claim.AttemptID, "generation": 1, "status": "wrapped", "note": "done"}, nil); code != 200 {
		t.Fatalf("end-generation: %d %s", code, raw)
	}
	req, _ = http.NewRequest("GET", strings.Replace(gitURL, "http://", "http://"+wi.ID+":"+resp.Credential.Secret+"@", 1), nil)
	gresp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("git after wrap: %v", err)
	}
	gresp.Body.Close()
	if gresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("git face alive after wrap: %d", gresp.StatusCode)
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
