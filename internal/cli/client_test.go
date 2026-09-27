package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captured holds what a fake aihub saw for one request.
type captured struct {
	Method string
	Path   string // path with query string
	Header http.Header
	Body   string
}

// newCapturingServer spins an httptest aihub that records one request and
// answers with the given canned body.
func newCapturingServer(t *testing.T, status int, response string, cap *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*cap = captured{
			Method: r.Method,
			Path:   r.URL.RequestURI(),
			Header: r.Header.Clone(),
		}
		if r.Body != nil {
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			cap.Body = string(buf[:n])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// envelope renders the typed wire error envelope (api/openapi.yaml).
func envelope(code, reason, detail string) string {
	return `{"error":{"code":` + quoteJSON(code) +
		`,"reason":` + quoteJSON(reason) +
		`,"detail":` + quoteJSON(detail) + `}}`
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---------------------------------------------------------------------------
// Request construction, one case per verb surface.
// ---------------------------------------------------------------------------

func TestClientAuthAndCreateWorkItem(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 201, `{"id":"wi_abc","slug":"p#1","goal":"g"}`, &cap)
	c := NewClient(srv.URL, "k_secret")
	content := "background"
	raw, err := c.CreateWorkItem(&CreateWorkItemRequest{
		Project:           "polyforge",
		Goal:              "do the thing",
		Content:           &content,
		Labels:            []string{"a", "b"},
		Priority:          "high",
		BundleName:        strPtr("reflow"),
		RepoInventory:     []RepoDecl{{Name: "polyforge", URL: "git@example:polyforge.git"}},
		RepoInventoryBase: []RepoBaseDecl{{Name: "polyforge", Base: "0123abc"}},
	})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	if !strings.Contains(string(raw), "wi_abc") {
		t.Error("raw body passthrough broken")
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/work-items" {
		t.Errorf("got %s %s", cap.Method, cap.Path)
	}
	if got := cap.Header.Get("Authorization"); got != "Bearer k_secret" {
		t.Errorf("Authorization = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(cap.Body), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	for _, k := range []string{"project", "goal", "content", "labels", "priority", "bundle_name", "repo_inventory", "repo_inventory_base"} {
		if _, ok := body[k]; !ok {
			t.Errorf("body missing %q: %s", k, cap.Body)
		}
	}
	if body["bundle_name"] != "reflow" {
		t.Errorf("bundle_name = %v", body["bundle_name"])
	}
}

func TestClientListWorkItemsQuery(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"items":[]}`, &cap)
	c := NewClient(srv.URL, "k")
	if _, err := c.ListWorkItems("pi", map[string]string{
		"status": "queued", "assignee": "usr_1", "milestone": "m1",
	}); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "GET" {
		t.Errorf("Method = %s", cap.Method)
	}
	for _, want := range []string{"project=pi", "status=queued", "assignee=usr_1", "milestone=m1"} {
		if !strings.Contains(cap.Path, want) {
			t.Errorf("path %q missing %q", cap.Path, want)
		}
	}
	if !strings.Contains(cap.Path, "waiting_on") && strings.Contains(cap.Path, "waiting_on=") {
		t.Error("empty filters must be omitted")
	}
}

func TestClientGetPatchCancelProgress(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{}`, &cap)
	c := NewClient(srv.URL, "k")

	if _, err := c.GetWorkItem("wi_abc123", []string{"events", "deps"}); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "GET" || cap.Path != "/api/v1/work-items/wi_abc123?include=events%2Cdeps" {
		t.Errorf("get: %s %s", cap.Method, cap.Path)
	}

	if _, err := c.PatchWorkItem("wi_abc123", map[string]any{"goal": "new"}); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "PATCH" || cap.Path != "/api/v1/work-items/wi_abc123" {
		t.Errorf("patch: %s %s", cap.Method, cap.Path)
	}
	if !strings.Contains(cap.Body, `"goal":"new"`) {
		t.Errorf("patch body = %s", cap.Body)
	}

	if _, err := c.CancelWorkItem("wi_abc123"); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/work-items/wi_abc123/cancel" {
		t.Errorf("cancel: %s %s", cap.Method, cap.Path)
	}

	if _, err := c.GetProgress("wi_abc123"); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "GET" || cap.Path != "/api/v1/work-items/wi_abc123/progress" {
		t.Errorf("progress: %s %s", cap.Method, cap.Path)
	}
}

func TestClientReadyQueue(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"project":"pi","executable":[],"blocked":[]}`, &cap)
	if _, err := NewClient(srv.URL, "k").ReadyQueue("pi"); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "GET" || cap.Path != "/api/v1/ready-queue?project=pi" {
		t.Errorf("got %s %s", cap.Method, cap.Path)
	}
}

func TestClientDependencies(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"ok":true}`, &cap)
	c := NewClient(srv.URL, "k")

	if _, err := c.AddDependency("wi_a", &DependencyCreateRequest{BlockingWiID: "wi_b", Kind: "blocks"}); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/work-items/wi_a/dependencies" {
		t.Errorf("add: %s %s", cap.Method, cap.Path)
	}
	if !strings.Contains(cap.Body, `"blocking_wi_id":"wi_b"`) {
		t.Errorf("add body = %s", cap.Body)
	}

	if _, err := c.ListDependencies("wi_a"); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/api/v1/work-items/wi_a/dependencies" {
		t.Errorf("list: %s", cap.Path)
	}

	if _, err := c.DeleteDependency("wi_a", "wi_b", "blocks"); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "DELETE" || cap.Path != "/api/v1/work-items/wi_a/dependencies/wi_b/blocks" {
		t.Errorf("delete: %s %s", cap.Method, cap.Path)
	}
}

func TestClientBudget(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"max_seq":3}`, &cap)
	c := NewClient(srv.URL, "k")

	if _, err := c.GetBudget("wi_a"); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/api/v1/work-items/wi_a/budget" {
		t.Errorf("get: %s", cap.Path)
	}

	raw, err := c.AddBudgetIncrement("wi_a", &BudgetIncrementRequest{
		Dimension: "tokens", Amount: 1000, Note: "n", BasedOnSeq: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/work-items/wi_a/budget-increments" {
		t.Errorf("raise: %s %s", cap.Method, cap.Path)
	}
	if !strings.Contains(cap.Body, `"based_on_seq":3`) {
		t.Errorf("raise body = %s", cap.Body)
	}
	if !strings.Contains(string(raw), "max_seq") {
		t.Error("raw passthrough broken")
	}
}

func TestClientMemorySearch(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{"items":[]}`, &cap)
	_, err := NewClient(srv.URL, "k").SearchMemories("pi", "routing verbs", "experience.debug", []string{"a", "b"}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if cap.Method != "GET" {
		t.Errorf("Method = %s", cap.Method)
	}
	for _, want := range []string{"q=routing+verbs", "project=pi", "type=experience.debug", "tags=a%2Cb", "limit=42"} {
		if !strings.Contains(cap.Path, want) {
			t.Errorf("path %q missing %q", cap.Path, want)
		}
	}
}

func TestClientManagementFace(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 200, `{}`, &cap)
	c := NewClient(srv.URL, "k")

	if _, err := c.CreateUser(&UserCreateRequest{DisplayName: "Ada", Kind: "human", Role: "writer"}); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/users" {
		t.Errorf("users create: %s %s", cap.Method, cap.Path)
	}

	if _, err := c.ListUsers(); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/api/v1/users" {
		t.Errorf("users list: %s", cap.Path)
	}

	if _, err := c.PatchUser("usr_1", &UserPatchRequest{Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "PATCH" || cap.Path != "/api/v1/users/usr_1" {
		t.Errorf("users patch: %s %s", cap.Method, cap.Path)
	}

	scope := "proj_1"
	if _, err := c.CreateApiKey(&ApiKeyCreateRequest{UserID: "usr_1", ProjectID: &scope}); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/api/v1/api-keys" {
		t.Errorf("api-keys create: %s", cap.Path)
	}
	if !strings.Contains(cap.Body, `"project_id":"proj_1"`) {
		t.Errorf("api-keys body = %s", cap.Body)
	}

	if _, err := c.RevokeApiKey("key_1"); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "DELETE" || cap.Path != "/api/v1/api-keys/key_1" {
		t.Errorf("api-keys revoke: %s %s", cap.Method, cap.Path)
	}

	members := []ProjectMember{{UserID: "usr_1", Role: "maintainer"}}
	if _, err := c.PatchProject("proj_1", &ProjectPatchRequest{Members: &members}); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "PATCH" || cap.Path != "/api/v1/projects/proj_1" {
		t.Errorf("projects patch: %s %s", cap.Method, cap.Path)
	}
	if !strings.Contains(cap.Body, `"role":"maintainer"`) {
		t.Errorf("members body = %s", cap.Body)
	}

	if _, err := c.Backup(); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/api/v1/admin/backup" {
		t.Errorf("backup: %s %s", cap.Method, cap.Path)
	}

	if _, err := c.RotateSecret(&RotateSecretRequest{Mode: "grace", GraceSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/api/v1/service/rotate-secret" {
		t.Errorf("rotate: %s", cap.Path)
	}
	if !strings.Contains(cap.Body, `"mode":"grace"`) {
		t.Errorf("rotate body = %s", cap.Body)
	}
}

// ---------------------------------------------------------------------------
// Error mapping against the REAL wire envelope.
// ---------------------------------------------------------------------------

func TestErrorEnvelopeMapping(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 409, envelope("CORE-409", "ALREADY_CLAIMED", "wi is running"), &cap)
	_, err := NewClient(srv.URL, "k").CancelWorkItem("wi_x")
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Code != "CORE-409" {
		t.Errorf("Code = %q", ae.Code)
	}
	if ae.Reason != "ALREADY_CLAIMED" {
		t.Errorf("Reason = %q", ae.Reason)
	}
	if ae.Detail != "wi is running" {
		t.Errorf("Detail = %q", ae.Detail)
	}
	if ae.Status != 409 {
		t.Errorf("Status = %d", ae.Status)
	}
	want := "CORE-409 ALREADY_CLAIMED: wi is running"
	if ae.Error() != want {
		t.Errorf("Error() = %q, want %q", ae.Error(), want)
	}
}

func TestErrorEnvelopeStatusCodeDivergence(t *testing.T) {
	// api/openapi.yaml x-error-status-map: SYNC-403 surfaces as HTTP 500
	// carrying the typed code in the body. The CODE is authoritative.
	var cap captured
	srv := newCapturingServer(t, 500, envelope("SYNC-403", "stale_generation", "fenced out"), &cap)
	_, err := NewClient(srv.URL, "k").GetProgress("wi_x")
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Code != "SYNC-403" {
		t.Errorf("Code = %q, want SYNC-403 (body wins over status)", ae.Code)
	}
	if ae.Status != 500 {
		t.Errorf("Status = %d", ae.Status)
	}
}

func TestErrorNonEnvelopeBody(t *testing.T) {
	var cap captured
	srv := newCapturingServer(t, 502, "upstream proxy exploded", &cap)
	_, err := NewClient(srv.URL, "k").GetProgress("wi_x")
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Code != "HTTP-502" {
		t.Errorf("Code = %q, want HTTP-502", ae.Code)
	}
	if ae.Detail != "upstream proxy exploded" {
		t.Errorf("Detail = %q", ae.Detail)
	}
}

func TestErrorKeepsRawForJSONMode(t *testing.T) {
	var cap captured
	body := envelope("CORE-410", "", "terminal")
	srv := newCapturingServer(t, 410, body, &cap)
	_, err := NewClient(srv.URL, "k").CancelWorkItem("wi_x")
	ae := err.(*APIError)
	if string(ae.Raw) != body {
		t.Errorf("Raw = %q, want the verbatim envelope", ae.Raw)
	}
}
