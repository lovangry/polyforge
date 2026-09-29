package cli

// client_contract_test.go: the CLI side of the wire contract. Where
// client_test.go / verbs_test.go pin specific request values against
// hand-copied expectations, this file derives the expectations from the
// two wire authorities themselves:
//
//   - aihub face: api/openapi.yaml (THE frozen contract). For EVERY
//     typed Client method the constructed request (captured on an
//     httptest server) must hit a declared operation with the right
//     method and path (template match), may carry ONLY query parameters
//     the yaml declares for that operation, and a JSON body whose keys
//     are all named by the yaml request schema.
//
//   - daemon face: internal/daemon/api.go IS the wire authority (the
//     route table is parsed out of that file's mux.HandleFunc
//     registrations). For every DaemonClient method the constructed
//     UDS request must match a registered route with the right method.
//     The projected pause/wrap routes are asserted against a documented
//     stub of our own and skipped against api.go while the other lane
//     has not registered them yet (TODO-marked skip).

import (
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// openapi.yaml fixture parsing
// ---------------------------------------------------------------------------

const (
	openapiFixture   = "../../api/openapi.yaml"
	daemonAPIFixture = "../../internal/daemon/api.go"
)

type cliContract struct {
	ops     map[string]*cliOp // "method /path-template" -> contract view
	schemas map[string]any
}

type cliOp struct {
	method      string
	path        string
	queryParams map[string]bool
	bodyProps   map[string]bool
}

var (
	cliContractCache *cliContract
	cliContractErr   error
)

// queryParamNames returns the in:query parameter names of one operation,
// from both the shared path-item parameter list and the operation's own.
func queryParamNodes(pathItem, op map[string]any) []any {
	var out []any
	for _, src := range []any{pathItem["parameters"], op["parameters"]} {
		if ps, ok := src.([]any); ok {
			out = append(out, ps...)
		}
	}
	return out
}

// loadCLIContract parses api/openapi.yaml once per test binary run.
func loadCLIContract(t *testing.T) *cliContract {
	t.Helper()
	if cliContractCache != nil {
		return cliContractCache
	}
	if cliContractErr != nil {
		t.Fatalf("openapi fixture unusable (earlier parse failed): %v", cliContractErr)
	}
	raw, err := os.ReadFile(openapiFixture)
	if err != nil {
		cliContractErr = err
		t.Fatalf("read %s: %v", openapiFixture, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		cliContractErr = err
		t.Fatalf("parse %s: %v", openapiFixture, err)
	}
	c := &cliContract{ops: map[string]*cliOp{}}
	comps, _ := doc["components"].(map[string]any)
	c.schemas, _ = comps["schemas"].(map[string]any)
	paths, _ := doc["paths"].(map[string]any)
	for path, itemAny := range paths {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		for _, m := range []string{"get", "post", "put", "patch", "delete"} {
			opAny, ok := item[m]
			if !ok {
				continue
			}
			op, ok := opAny.(map[string]any)
			if !ok {
				continue
			}
			o := &cliOp{method: m, path: path, queryParams: map[string]bool{}, bodyProps: map[string]bool{}}
			for _, pAny := range queryParamNodes(item, op) {
				p, _ := pAny.(map[string]any)
				if in, _ := p["in"].(string); in == "query" {
					if name, _ := p["name"].(string); name != "" {
						o.queryParams[name] = true
					}
				}
			}
			// Request body property names, resolving one $ref hop into
			// components/schemas (the yaml keeps request schemas there).
			rb, _ := op["requestBody"].(map[string]any)
			content, _ := rb["content"].(map[string]any)
			jsonCT, _ := content["application/json"].(map[string]any)
			schema, _ := jsonCT["schema"].(map[string]any)
			if ref, ok := schema["$ref"].(string); ok && strings.HasPrefix(ref, "#/components/schemas/") {
				name := strings.TrimPrefix(ref, "#/components/schemas/")
				schema, _ = c.schemas[name].(map[string]any)
			}
			props, _ := schema["properties"].(map[string]any)
			for k := range props {
				o.bodyProps[k] = true
			}
			c.ops[m+" "+path] = o
		}
	}
	if len(c.ops) == 0 {
		t.Fatal("no operations parsed from openapi.yaml")
	}
	cliContractCache = c
	return c
}

// templateMatcher compiles an OpenAPI path template into an anchored
// regexp with {param} placeholders matched as one path segment.
func templateMatcher(t *testing.T, template string) *regexp.Regexp {
	t.Helper()
	segs := strings.Split(template, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "([^/]+)"
		} else {
			segs[i] = regexp.QuoteMeta(s)
		}
	}
	return regexp.MustCompile("^" + strings.Join(segs, "/") + "$")
}

// ---------------------------------------------------------------------------
// Aihub face: every typed Client method vs the parsed contract
// ---------------------------------------------------------------------------

// clientCase drives exactly ONE constructed request and names the
// operation it is expected to hit.
type clientCase struct {
	name  string
	opKey string
	call  func(c *Client) error
}

// TestClientConstructionMatchesOpenAPI walks every typed Client method
// (rows 1-6, 11/12, 22/23 via search, 26 and the management face
// M1-M5 — the whole aihub surface the CLI owns) and checks the captured
// request against the parsed contract: method, path template, declared
// query parameters, and body fields named by the yaml request schema.
// buildClientContractCases is the single source of truth for the
// construction-case table (consumed by both the runner and the coverage
// guard).
func buildClientContractCases() []clientCase {
	return []clientCase{
		// A. WI business (rows 1-6).
		{"row1_create", "post /api/v1/work-items", func(c *Client) error {
			_, err := c.CreateWorkItem(&CreateWorkItemRequest{
				Project: "p1", Goal: "g",
				Labels:            []string{"a"},
				Priority:          "high",
				BundleName:        strPtr("demo"),
				RepoInventory:     []RepoDecl{{Name: "r1", URL: "u"}},
				RepoInventoryBase: []RepoBaseDecl{{Name: "r1", Base: "sha"}},
			})
			return err
		}},
		{"row2_list", "get /api/v1/work-items", func(c *Client) error {
			_, err := c.ListWorkItems("p1", map[string]string{"status": "queued", "assignee": "usr_1", "milestone": "m"})
			return err
		}},
		{"row3_get_include", "get /api/v1/work-items/{id}", func(c *Client) error {
			_, err := c.GetWorkItem("wi_abc123", []string{"events", "deps"})
			return err
		}},
		{"row3_patch", "patch /api/v1/work-items/{id}", func(c *Client) error {
			_, err := c.PatchWorkItem("wi_abc123", map[string]any{"goal": "new", "labels": []string{"x"}})
			return err
		}},
		{"row4_ready_queue", "get /api/v1/ready-queue", func(c *Client) error {
			_, err := c.ReadyQueue("p1")
			return err
		}},
		{"row5_cancel", "post /api/v1/work-items/{id}/cancel", func(c *Client) error {
			_, err := c.CancelWorkItem("wi_abc123")
			return err
		}},
		{"row6_progress", "get /api/v1/work-items/{id}/progress", func(c *Client) error {
			_, err := c.GetProgress("wi_abc123")
			return err
		}},
		// G. Dependency edges (row 26).
		{"row26_add_dep", "post /api/v1/work-items/{id}/dependencies", func(c *Client) error {
			_, err := c.AddDependency("wi_a", &DependencyCreateRequest{BlockingWiID: "wi_b", Kind: "blocks"})
			return err
		}},
		{"row26_list_deps", "get /api/v1/work-items/{id}/dependencies", func(c *Client) error {
			_, err := c.ListDependencies("wi_a")
			return err
		}},
		{"row26_delete_dep", "delete /api/v1/work-items/{id}/dependencies/{blocking_id}/{kind}", func(c *Client) error {
			_, err := c.DeleteDependency("wi_a", "wi_b", "blocks")
			return err
		}},
		// B. Budget face (rows 11/12).
		{"row11_raise", "post /api/v1/work-items/{id}/budget-increments", func(c *Client) error {
			_, err := c.AddBudgetIncrement("wi_a", &BudgetIncrementRequest{Dimension: "tokens", Amount: 10, Note: "n", BasedOnSeq: 3})
			return err
		}},
		{"row12_budget", "get /api/v1/work-items/{id}/budget", func(c *Client) error {
			_, err := c.GetBudget("wi_a")
			return err
		}},
		// E. Memory search (row 23; the recall verb's backend).
		{"row23_search", "get /api/v1/memories/search", func(c *Client) error {
			_, err := c.SearchMemories("p1", "trigram probe", "experience.debug", []string{"a"}, 42)
			return err
		}},
		// F. Management face.
		{"M1_user_create", "post /api/v1/users", func(c *Client) error {
			_, err := c.CreateUser(&UserCreateRequest{DisplayName: "Ada", Kind: "human", Role: "writer"})
			return err
		}},
		{"M1_user_list", "get /api/v1/users", func(c *Client) error {
			_, err := c.ListUsers()
			return err
		}},
		{"M1_user_get", "get /api/v1/users/{id}", func(c *Client) error {
			_, err := c.GetUser("usr_1")
			return err
		}},
		{"M1_user_patch", "patch /api/v1/users/{id}", func(c *Client) error {
			_, err := c.PatchUser("usr_1", &UserPatchRequest{Role: "admin"})
			return err
		}},
		{"M2_key_create", "post /api/v1/api-keys", func(c *Client) error {
			scope := "pj1"
			_, err := c.CreateApiKey(&ApiKeyCreateRequest{UserID: "usr_1", ProjectID: &scope})
			return err
		}},
		{"M2_key_list", "get /api/v1/api-keys", func(c *Client) error {
			_, err := c.ListApiKeys()
			return err
		}},
		{"M2_key_revoke", "delete /api/v1/api-keys/{id}", func(c *Client) error {
			_, err := c.RevokeApiKey("key_1")
			return err
		}},
		{"M3_project_create", "post /api/v1/projects", func(c *Client) error {
			_, err := c.CreateProject(&ProjectCreateRequest{Name: "p2"})
			return err
		}},
		{"M3_project_list", "get /api/v1/projects", func(c *Client) error {
			_, err := c.ListProjects()
			return err
		}},
		{"M3_project_get", "get /api/v1/projects/{id}", func(c *Client) error {
			_, err := c.GetProject("pj1")
			return err
		}},
		{"M3_project_patch", "patch /api/v1/projects/{id}", func(c *Client) error {
			members := []ProjectMember{{UserID: "usr_1", Role: "maintainer"}}
			_, err := c.PatchProject("pj1", &ProjectPatchRequest{Members: &members})
			return err
		}},
		{"M4_backup", "post /api/v1/admin/backup", func(c *Client) error {
			_, err := c.Backup()
			return err
		}},
		{"M5_rotate_secret", "post /api/v1/service/rotate-secret", func(c *Client) error {
			_, err := c.RotateSecret(&RotateSecretRequest{Mode: "grace", GraceSeconds: 3600})
			return err
		}},
	}
}

// TestClientConstructionMatchesOpenAPI walks every typed Client method
// (rows 1-6, 11/12, 22/23 via search, 26 and the management face
// M1-M5 — the whole aihub surface the CLI owns) and checks the captured
// request against the parsed contract: method, path template, declared
// query parameters, and body fields named by the yaml request schema.
func TestClientConstructionMatchesOpenAPI(t *testing.T) {
	c := loadCLIContract(t)
	cases := buildClientContractCases()

	var cap captured
	srv := newCapturingServer(t, 200, `{}`, &cap)
	client := NewClient(srv.URL, "k_contract")

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			op, ok := c.ops[tc.opKey]
			if !ok {
				t.Fatalf("client method targets %q, which is NOT an operation in api/openapi.yaml", tc.opKey)
			}
			cap = captured{}
			if err := tc.call(client); err != nil {
				t.Fatalf("client call: %v", err)
			}
			// Method.
			if !strings.EqualFold(cap.Method, op.method) {
				t.Fatalf("method = %s, contract says %s", cap.Method, op.method)
			}
			// Path (template match, query stripped).
			u, err := url.ParseRequestURI(cap.Path)
			if err != nil {
				t.Fatalf("captured path %q unparsable: %v", cap.Path, err)
			}
			if !templateMatcher(t, op.path).MatchString(u.Path) {
				t.Fatalf("path %q does not match the contract template %q", u.Path, op.path)
			}
			// Query parameters must all be declared by the operation.
			for k := range u.Query() {
				if !op.queryParams[k] {
					t.Errorf("query parameter %q is NOT declared by %q in openapi.yaml (path %q)", k, tc.opKey, cap.Path)
				}
			}
			// Body fields must all be named by the yaml request schema.
			if cap.Body != "" {
				var body map[string]any
				if err := json.Unmarshal([]byte(cap.Body), &body); err != nil {
					t.Fatalf("body not a JSON object: %v (%s)", err, cap.Body)
				}
				for k := range body {
					if !op.bodyProps[k] {
						t.Errorf("body field %q is NOT named by the %q request schema in openapi.yaml (body %s)", k, tc.opKey, cap.Body)
					}
				}
			}
			// Bearer auth on the aihub face.
			if got := cap.Header.Get("Authorization"); got != "Bearer k_contract" {
				t.Errorf("Authorization = %q", got)
			}
		})
	}
}

// TestClientConstructionCoversAllAihubMethods guards the table above
// against drift: every typed aihub request constructor the CLI owns must
// appear in the contract table, so a new method cannot be added without a
// contract case (and a case cannot go stale).
func TestClientConstructionCoversAllAihubMethods(t *testing.T) {
	want := []string{
		"row1_create", "row2_list", "row3_get_include", "row3_patch",
		"row4_ready_queue", "row5_cancel", "row6_progress",
		"row26_add_dep", "row26_list_deps", "row26_delete_dep",
		"row11_raise", "row12_budget", "row23_search",
		"M1_user_create", "M1_user_list", "M1_user_get", "M1_user_patch",
		"M2_key_create", "M2_key_list", "M2_key_revoke",
		"M3_project_create", "M3_project_list", "M3_project_get", "M3_project_patch",
		"M4_backup", "M5_rotate_secret",
	}
	c := loadCLIContract(t)
	have := map[string]bool{}
	for _, tc := range buildClientContractCases() {
		have[tc.name] = true
		if _, ok := c.ops[tc.opKey]; !ok {
			t.Errorf("case %q targets unknown operation %q", tc.name, tc.opKey)
		}
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("contract case %q is missing from the client construction table", name)
		}
	}
	if len(have) != len(want) {
		t.Errorf("table has %d cases, the pinned inventory has %d — remove or re-name stale cases", len(have), len(want))
	}
}

// ---------------------------------------------------------------------------
// Daemon face: DaemonClient framing vs internal/daemon/api.go routes
// ---------------------------------------------------------------------------

var daemonRouteRe = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) ([^"]+)",`)

// loadDaemonRoutes parses the REGISTERED route table out of the wire
// authority file internal/daemon/api.go (the daemon face's contract IS
// its implementation, 04 §5.1 — there is no openapi for the UDS face).
func loadDaemonRoutes(t *testing.T) map[string]*regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile(daemonAPIFixture)
	if err != nil {
		t.Fatalf("read %s: %v", daemonAPIFixture, err)
	}
	routes := map[string]*regexp.Regexp{}
	for _, m := range daemonRouteRe.FindAllStringSubmatch(string(raw), -1) {
		method, path := m[1], m[2]
		routes[method+" "+path] = templateMatcher(t, path)
	}
	if len(routes) == 0 {
		t.Fatal("no mux.HandleFunc routes parsed from internal/daemon/api.go")
	}
	return routes
}

// TestDaemonClientFramingMatchesDaemonAPI pins every DaemonClient request
// against the routes actually registered in internal/daemon/api.go: the
// constructed (method, path) must match a registered route template.
// Body shapes the api.go handlers decode are asserted alongside (hSubmit
// reads {"priority"}, hAnswer decodes the AnswerRelay five-field message
// with the X-PF-User header, hBatchSubmit reads {"wi_ids"}, hMode reads
// {"action",...}).
func TestDaemonClientFramingMatchesDaemonAPI(t *testing.T) {
	routes := loadDaemonRoutes(t)
	mustMatchRoute := func(t *testing.T, method, path string) {
		t.Helper()
		for key, re := range routes {
			if strings.SplitN(key, " ", 2)[0] != method {
				continue
			}
			if re.MatchString(strings.SplitN(path, "?", 2)[0]) {
				return
			}
		}
		t.Fatalf("%s %s matches NO route registered in internal/daemon/api.go (registered: %v)", method, path, routeKeys(routes))
	}

	var cap captured
	d := newUnixDaemon(t, 200, `{}`, &cap)

	t.Run("submit_family", func(t *testing.T) {
		for intent, call := range map[string]func() error{
			"claim":    func() error { _, err := d.Claim("wi_abc", ""); return err },
			"resume":   func() error { _, err := d.Resume("wi_abc", "high"); return err },
			"takeover": func() error { _, err := d.Takeover("wi_abc", ""); return err },
		} {
			cap = captured{}
			if err := call(); err != nil {
				t.Fatalf("%s: %v", intent, err)
			}
			if cap.Method != "POST" {
				t.Fatalf("%s: method %s", intent, cap.Method)
			}
			mustMatchRoute(t, "POST", cap.Path)
			if cap.Path != "/jobs/wi_abc/"+intent {
				t.Errorf("%s: path %s", intent, cap.Path)
			}
			var body map[string]any
			if err := decodeJSON([]byte(cap.Body), &body); err != nil {
				t.Fatalf("%s: body %q: %v", intent, cap.Body, err)
			}
			if _, ok := body["priority"]; !ok {
				t.Errorf("%s: body lacks the priority field hSubmit decodes: %s", intent, cap.Body)
			}
		}
	})

	t.Run("observe", func(t *testing.T) {
		cap = captured{}
		if _, err := d.Observe("wi_abc", 7); err != nil {
			t.Fatal(err)
		}
		if cap.Method != "GET" {
			t.Fatalf("method %s", cap.Method)
		}
		mustMatchRoute(t, "GET", cap.Path)
		if cap.Path != "/jobs/wi_abc/observe?after=7" {
			t.Errorf("path %s", cap.Path)
		}
	})

	t.Run("answer_relay", func(t *testing.T) {
		cap = captured{}
		payload := AnswerPayload{
			QuestionID: "q_1", AnswerID: "a_1", AnswerText: "yes",
			AnsweredAt: "2026-09-27T00:00:00Z", AnsweredBy: "usr_1",
		}
		if _, err := d.Answer("wi_abc", payload); err != nil {
			t.Fatal(err)
		}
		if cap.Method != "POST" {
			t.Fatalf("method %s", cap.Method)
		}
		mustMatchRoute(t, "POST", cap.Path)
		if cap.Path != "/jobs/wi_abc/answer" {
			t.Errorf("path %s", cap.Path)
		}
		// The relay message IS the body (api.go hAnswer decodes AnswerRelay
		// directly — exactly the five fields, no wrapper) and the caller
		// identity rides X-PF-User.
		var body map[string]any
		if err := decodeJSON([]byte(cap.Body), &body); err != nil {
			t.Fatalf("body %q: %v", cap.Body, err)
		}
		for _, k := range []string{"question_id", "answer_id", "answer_text", "answered_at", "answered_by"} {
			if _, ok := body[k]; !ok {
				t.Errorf("answer relay body missing field %q: %s", k, cap.Body)
			}
		}
		if len(body) != 5 {
			t.Errorf("answer relay body must be the five-field message itself, got %d keys: %s", len(body), cap.Body)
		}
		if got := cap.Header.Get("X-PF-User"); got != "usr_1" {
			t.Errorf("X-PF-User = %q", got)
		}
	})

	t.Run("batch_submit", func(t *testing.T) {
		cap = captured{}
		if _, err := d.BatchSubmit([]string{"wi_a", "wi_b"}); err != nil {
			t.Fatal(err)
		}
		if cap.Method != "POST" {
			t.Fatalf("method %s", cap.Method)
		}
		mustMatchRoute(t, "POST", cap.Path)
		if cap.Path != "/batch_submit" {
			t.Errorf("path %s", cap.Path)
		}
		var body map[string]any
		if err := decodeJSON([]byte(cap.Body), &body); err != nil {
			t.Fatalf("body: %v", err)
		}
		if _, ok := body["wi_ids"]; !ok {
			t.Errorf("batch body lacks wi_ids (hBatchSubmit): %s", cap.Body)
		}
	})

	t.Run("mode", func(t *testing.T) {
		cap = captured{}
		if _, err := d.Mode(ModeRequest{Action: "set", Mode: "drain"}); err != nil {
			t.Fatal(err)
		}
		if cap.Method != "POST" {
			t.Fatalf("method %s", cap.Method)
		}
		mustMatchRoute(t, "POST", cap.Path)
		var body map[string]any
		if err := decodeJSON([]byte(cap.Body), &body); err != nil {
			t.Fatalf("body: %v", err)
		}
		if _, ok := body["action"]; !ok {
			t.Errorf("mode body lacks action (hMode): %s", cap.Body)
		}
	})

	t.Run("status", func(t *testing.T) {
		cap = captured{}
		if _, err := d.Status(); err != nil {
			t.Fatal(err)
		}
		if cap.Method != "GET" {
			t.Fatalf("method %s", cap.Method)
		}
		mustMatchRoute(t, "GET", cap.Path)
	})

	t.Run("publish_request", func(t *testing.T) {
		cap = captured{}
		if _, err := d.PublishRequest("wi_abc", "milestone"); err != nil {
			t.Fatal(err)
		}
		if cap.Method != "POST" {
			t.Fatalf("method %s", cap.Method)
		}
		mustMatchRoute(t, "POST", cap.Path)
		if cap.Path != "/jobs/wi_abc/publish_request" {
			t.Errorf("path %s", cap.Path)
		}
	})

	t.Run("ship", func(t *testing.T) {
		cap = captured{}
		if _, err := d.Ship("wi_abc"); err != nil {
			t.Fatal(err)
		}
		if cap.Method != "POST" {
			t.Fatalf("method %s", cap.Method)
		}
		mustMatchRoute(t, "POST", cap.Path)
		if cap.Path != "/jobs/wi_abc/ship" {
			t.Errorf("path %s", cap.Path)
		}
		// Empty-object body (hShip decodes nothing; the wi id is the whole
		// input — the branch is the fixed name polyforge/<wi_id>).
		if strings.TrimSpace(cap.Body) != "{}" {
			t.Errorf("ship body = %q, want the empty object", cap.Body)
		}
	})

	t.Run("shutdown", func(t *testing.T) {
		cap = captured{}
		if _, err := d.Shutdown(); err != nil {
			t.Fatal(err)
		}
		if cap.Method != "POST" {
			t.Fatalf("method %s", cap.Method)
		}
		mustMatchRoute(t, "POST", cap.Path)
	})
}

// routeKeys renders the parsed daemon route table for failure messages.
func routeKeys(routes map[string]*regexp.Regexp) []string {
	out := make([]string, 0, len(routes))
	for k := range routes {
		out = append(out, k)
	}
	return out
}

// TestDaemonPauseWrapProjectedShape asserts the PROJECTED pause/wrap
// framing (internal/cli/daemon.go file header: the daemon face has no
// pause/wrap row yet; the natural projection is POST /jobs/{id}/pause
// with an empty body and POST /jobs/{id}/wrap with {"reason"}) against
// this test's own documented stub, and against internal/daemon/api.go's
// registered routes ONLY once the other lane has registered them.
func TestDaemonPauseWrapProjectedShape(t *testing.T) {
	routes := loadDaemonRoutes(t)
	var cap captured
	// The documented stub: a capturing UDS server standing in for the
	// future daemon pause/wrap routes.
	d := newUnixDaemon(t, 200, `{"ok":true}`, &cap)

	cap = captured{}
	if _, err := d.Pause("wi_abc"); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/jobs/wi_abc/pause" {
		t.Fatalf("pause projection: %s %s", cap.Method, cap.Path)
	}
	if cap.Body != "{}" {
		t.Errorf("pause body = %s, want the empty object", cap.Body)
	}

	cap = captured{}
	if _, err := d.Wrap("wi_abc", "fail"); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/jobs/wi_abc/wrap" {
		t.Fatalf("wrap projection: %s %s", cap.Method, cap.Path)
	}
	var body map[string]any
	if err := decodeJSON([]byte(cap.Body), &body); err != nil {
		t.Fatalf("wrap body: %v", err)
	}
	if body["reason"] != "fail" {
		t.Errorf("wrap body = %s, want reason=fail", cap.Body)
	}

	// The against-the-authority half: pause/wrap must be registered in
	// internal/daemon/api.go before the projection is wire truth.
	_, pauseRegistered := routes["POST /jobs/{id}/pause"]
	_, wrapRegistered := routes["POST /jobs/{id}/wrap"]
	if !pauseRegistered || !wrapRegistered {
		missing := []string{}
		if !pauseRegistered {
			missing = append(missing, "POST /jobs/{id}/pause")
		}
		if !wrapRegistered {
			missing = append(missing, "POST /jobs/{id}/wrap")
		}
		t.Skipf("TODO(contract-test): internal/daemon/api.go does not yet register %s (registered: %v); the projected shape above is asserted against this test's own documented stub only, per the pause/wrap lane note",
			strings.Join(missing, " and "), routeKeys(routes))
	}
}
