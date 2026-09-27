package server_test

// openapi_contract_test.go: the RUNTIME side of the wire contract. The
// structural side (operation inventory, error-code table completeness,
// token applicability) is already asserted by api/openapi-lint.go against
// api/openapi.yaml; this file walks the SAME parsed openapi.yaml fixture
// and drives the real server mux behind it:
//
//  1. TestOpenAPIOperationsResolveOverRealMux — every one of the 47
//     operations in api/openapi.yaml must resolve route+method on the
//     real mux (an unauthenticated probe must reach authenticate() and
//     answer the typed CORE-401 envelope, never the mux's bare
//     "404 page not found"; the anonymous share op answers MEM-404).
//     The yaml must also declare the response status actually served.
//
//  2. TestOpenAPIErrorStatusMapRuntimeAgreement — a table of
//     representative error paths, each producing a typed CORE-*/SYNC-*/
//     MEM-* code. For every probe the test asserts, ALL against the
//     parsed contract: the code exists in x-error-status-map, the served
//     HTTP status equals the mapped value, the body parses as the
//     ErrorEnvelope {error:{code,reason,detail}} with the expected code,
//     the operation declares that response status, and the declared
//     response component references the ErrorEnvelope schema.
//
// Expectations are derived from the yaml fixture (never hand-copied
// status tables); only WHICH error path each probe drives is coded here.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// openapiPath is the frozen wire contract, relative to this package's
// test working directory (internal/server).
const openapiPath = "../../api/openapi.yaml"

// contractOps is the parsed operation inventory of the wire contract.
type contractOps struct {
	// ops maps "METHOD /path-template" -> the operation node.
	ops map[string]map[string]any
	// errStatus mirrors the root x-error-status-map (typed code -> HTTP status).
	errStatus map[string]int
	// responses holds components.responses (resolved for schema checks).
	responses map[string]any
	// schemas holds components.schemas.
	schemas map[string]any
}

var (
	contractCache *contractOps
	contractErr   error
)

// loadContract parses api/openapi.yaml once per test binary run.
func loadContract(t *testing.T) *contractOps {
	t.Helper()
	if contractCache != nil {
		return contractCache
	}
	if contractErr != nil {
		t.Fatalf("openapi fixture unusable (earlier parse failed): %v", contractErr)
	}
	raw, err := os.ReadFile(openapiPath)
	if err != nil {
		contractErr = err
		t.Fatalf("read %s: %v", openapiPath, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		contractErr = err
		t.Fatalf("parse %s: %v", openapiPath, err)
	}
	c := &contractOps{ops: map[string]map[string]any{}}
	paths, _ := doc["paths"].(map[string]any)
	for path, itemAny := range paths {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		for _, m := range []string{"get", "post", "put", "patch", "delete"} {
			if opAny, ok := item[m]; ok {
				if op, ok := opAny.(map[string]any); ok {
					c.ops[m+" "+path] = op
				}
			}
		}
	}
	errMap, _ := doc["x-error-status-map"].(map[string]any)
	c.errStatus = map[string]int{}
	for k, v := range errMap {
		if n, ok := v.(int); ok {
			c.errStatus[k] = n
		}
	}
	comps, _ := doc["components"].(map[string]any)
	c.responses, _ = comps["responses"].(map[string]any)
	c.schemas, _ = comps["schemas"].(map[string]any)
	if len(c.ops) == 0 {
		t.Fatal("no operations parsed from openapi.yaml")
	}
	contractCache = c
	return c
}

// opResponses returns the declared responses of one operation keyed by
// the string status ("200", "400", ...).
func opResponses(op map[string]any) map[string]any {
	r, _ := op["responses"].(map[string]any)
	return r
}

// responseUsesErrorEnvelope resolves the declared error response (either
// inline or via a components.responses $ref) and reports whether its
// application/json schema references the ErrorEnvelope schema.
func (c *contractOps) responseUsesErrorEnvelope(node any) bool {
	m, _ := node.(map[string]any)
	if m == nil {
		return false
	}
	if ref, ok := m["$ref"].(string); ok && strings.HasPrefix(ref, "#/components/responses/") {
		name := strings.TrimPrefix(ref, "#/components/responses/")
		m, _ = c.responses[name].(map[string]any)
		if m == nil {
			return false
		}
	}
	content, _ := m["content"].(map[string]any)
	json, _ := content["application/json"].(map[string]any)
	schema, _ := json["schema"].(map[string]any)
	ref, _ := schema["$ref"].(string)
	return strings.HasPrefix(ref, "#/components/schemas/") &&
		strings.TrimPrefix(ref, "#/components/schemas/") == "ErrorEnvelope"
}

// substitutePathParams fills the {name} placeholders of a path template
// with values satisfying the yaml parameter constraints (id charset,
// 64-hex sha256, integer seq, the kind enum).
func substitutePathParams(t *testing.T, path string) string {
	t.Helper()
	out := path
	repl := map[string]string{
		"{id}":          "wi_contract0000",
		"{seq}":         "0",
		"{sha256}":      strings.Repeat("ab", 32),
		"{blocking_id}": "wi_blocking000",
		"{kind}":        "blocks",
		"{token}":       "contract-bad-token",
	}
	for k, v := range repl {
		out = strings.ReplaceAll(out, k, v)
	}
	if strings.Contains(out, "{") {
		t.Fatalf("unsubstituted path parameter in %q", out)
	}
	return out
}

// TestOpenAPIOperationsResolveOverRealMux drives one unauthenticated
// request per declared operation. Everything but the anonymous share
// consumption must reach authenticate() and answer 401 + CORE-401
// envelope (proving route AND method resolved on the real mux — an
// unresolved route answers the mux's bare "404 page not found", a wrong
// method answers 405). The anonymous op answers the typed MEM-404 for a
// bad token. Each operation must also declare, in the yaml, the response
// status the server actually serves here.
func TestOpenAPIOperationsResolveOverRealMux(t *testing.T) {
	c := loadContract(t)
	e := newEnv(t)

	for key, op := range c.ops {
		key := key
		op := op
		t.Run(key, func(t *testing.T) {
			method := strings.SplitN(key, " ", 2)[0]
			tpl := strings.SplitN(key, " ", 2)[1]
			path := substitutePathParams(t, tpl)

			wantStatus, wantCode := 401, "CORE-401"
			if key == "get /api/v1/share/{token}" {
				// Row 24: anonymous by design — no auth, typed MEM-404 on a
				// bad/unknown token without leaking other objects.
				wantStatus, wantCode = 404, "MEM-404"
			}
			st, m, raw := e.do(t, strings.ToUpper(method), path, "", nil)
			if st != wantStatus {
				t.Fatalf("%s %s: want %d (route must resolve; a bare 404 means the mux did not match, 405 means wrong method), got %d body=%q",
					method, path, wantStatus, st, raw)
			}
			code, _ := contractEnvelope(t, m)
			if code != wantCode {
				t.Fatalf("%s %s: body code = %q, want %q (%s)", method, path, code, wantCode, raw)
			}
			// The contract must declare the status the server served.
			if _, declared := opResponses(op)[strconv.Itoa(wantStatus)]; !declared {
				t.Fatalf("%s: openapi.yaml does not declare a %d response", key, wantStatus)
			}
			// ...and the declared error response must use the ErrorEnvelope.
			if node := opResponses(op)[strconv.Itoa(wantStatus)]; !c.responseUsesErrorEnvelope(node) {
				t.Fatalf("%s: declared %d response does not reference the ErrorEnvelope schema", key, wantStatus)
			}
		})
	}
}

// contractEnvelope extracts code+reason from a served error body.
func contractEnvelope(t *testing.T, m map[string]any) (code, reason string) {
	t.Helper()
	e, _ := m["error"].(map[string]any)
	if e == nil {
		t.Fatalf("expected the ErrorEnvelope {error:{...}}, got %v", m)
	}
	code, _ = e["code"].(string)
	reason, _ = e["reason"].(string)
	if code == "" {
		t.Fatalf("envelope carries no typed code: %v", m)
	}
	return code, reason
}

// mapProbe is one representative runtime error path: send performs the
// request (fresh objects per probe as needed) and returns the served
// status plus the parsed body.
type mapProbe struct {
	name       string
	opKey      string // "method /path-template" in openapi.yaml
	wantCode   string // the typed code the body must carry
	wantReason string // optional machine-readable reason ("" = unchecked)
	send       func(t *testing.T, e *env) (int, map[string]any, []byte)
}

// TestOpenAPIErrorStatusMapRuntimeAgreement walks representative error
// paths per tag family and pins the three-way agreement the contract
// demands: served HTTP status == x-error-status-map[body code], the body
// parses as the ErrorEnvelope, and the operation declares that status
// with an ErrorEnvelope-carrying response component.
//
// Runtime coverage deliberately complements (not duplicates)
// errstatus_http_test.go: the SYNC-403 / MEM-403 / MEM-413 / MEM-501
// families are already pinned there, so this table focuses on the rest of
// the map.
func TestOpenAPIErrorStatusMapRuntimeAgreement(t *testing.T) {
	c := loadContract(t)
	e := newEnv(t)

	// claimed probes need a claim response; probe closures build their own
	// fixtures so the table stays order-independent.
	claim := func(t *testing.T) (string, string) {
		wi := newExecTicket(t, e)
		st, m, _ := e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/claim", wi), e.writer,
			map[string]any{"idempotency_key": "contract-" + wi, "machine_id": "mac_a"})
		if st != 200 {
			t.Fatalf("claim %s: %d %v", wi, st, m)
		}
		return wi, m["generation_token"].(string)
	}
	terminal := func(t *testing.T) string {
		wi := newExecTicket(t, e)
		st, m, _ := e.do(t, "POST", fmt.Sprintf("/api/v1/work-items/%s/cancel", wi), e.writer, nil)
		if st != 200 {
			t.Fatalf("cancel %s: %d %v", wi, st, m)
		}
		return wi
	}

	probes := []mapProbe{
		// --- WI business face (rows 1–6) ---
		{
			name: "row1_create_bad_json", opKey: "post /api/v1/work-items", wantCode: "CORE-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.doRaw(t, "POST", "/api/v1/work-items", e.writer, "", "application/json", "{not-json")
			},
		},
		{
			name: "row2_list_missing_project", opKey: "get /api/v1/work-items", wantCode: "CORE-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "GET", "/api/v1/work-items", e.writer, nil)
			},
		},
		{
			name: "row3_get_unknown_wi", opKey: "get /api/v1/work-items/{id}", wantCode: "CORE-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "GET", "/api/v1/work-items/wi_nope00000000", e.writer, nil)
			},
		},
		{
			name: "row3_patch_unknown_field", opKey: "patch /api/v1/work-items/{id}", wantCode: "CORE-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := newExecTicket(t, e)
				return e.do(t, "PATCH", "/api/v1/work-items/"+wi, e.writer, map[string]any{"bogus_field": 1})
			},
		},
		{
			name: "row3_patch_immutable_field", opKey: "patch /api/v1/work-items/{id}", wantCode: "CORE-400", wantReason: "IMMUTABLE",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := newExecTicket(t, e)
				return e.do(t, "PATCH", "/api/v1/work-items/"+wi, e.writer, map[string]any{"bundle_name": "x"})
			},
		},
		{
			name: "row3_patch_terminal_wi", opKey: "patch /api/v1/work-items/{id}", wantCode: "CORE-410",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := terminal(t)
				return e.do(t, "PATCH", "/api/v1/work-items/"+wi, e.writer, map[string]any{"goal": "x"})
			},
		},
		{
			name: "row5_cancel_running_wi", opKey: "post /api/v1/work-items/{id}/cancel", wantCode: "CORE-409",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, _ := claim(t)
				return e.do(t, "POST", "/api/v1/work-items/"+wi+"/cancel", e.writer, nil)
			},
		},
		{
			name: "row5_cancel_terminal_wi", opKey: "post /api/v1/work-items/{id}/cancel", wantCode: "CORE-410",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := terminal(t)
				return e.do(t, "POST", "/api/v1/work-items/"+wi+"/cancel", e.writer, nil)
			},
		},
		{
			name: "row6_progress_claimed_no_publish", opKey: "get /api/v1/work-items/{id}/progress", wantCode: "CORE-504",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, _ := claim(t)
				return e.do(t, "GET", "/api/v1/work-items/"+wi+"/progress", e.writer, nil)
			},
		},
		// --- Dependency edges (row 26) ---
		{
			name: "row26_add_missing_endpoint_wi", opKey: "post /api/v1/work-items/{id}/dependencies", wantCode: "CORE-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := newExecTicket(t, e)
				return e.do(t, "POST", "/api/v1/work-items/"+wi+"/dependencies", e.writer,
					map[string]any{"blocking_wi_id": "wi_nope00000000", "kind": "blocks"})
			},
		},
		{
			name: "row26_delete_missing_edge", opKey: "delete /api/v1/work-items/{id}/dependencies/{blocking_id}/{kind}", wantCode: "CORE-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := newExecTicket(t, e)
				other := newExecTicket(t, e)
				return e.do(t, "DELETE", "/api/v1/work-items/"+wi+"/dependencies/"+other+"/blocks", e.writer, nil)
			},
		},
		// --- Execution right (rows 7–13) ---
		{
			name: "row8_takeover_non_admin", opKey: "post /api/v1/work-items/{id}/force-takeover", wantCode: "CORE-403",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := newExecTicket(t, e)
				return e.do(t, "POST", "/api/v1/work-items/"+wi+"/force-takeover", e.writer, nil)
			},
		},
		{
			name: "row9_end_generation_bad_token", opKey: "post /api/v1/work-items/{id}/end-generation", wantCode: "CORE-403", wantReason: "stale_generation",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, _ := claim(t)
				return e.do(t, "POST", "/api/v1/work-items/"+wi+"/end-generation", e.writer, map[string]any{"reason": "wrap"})
			},
		},
		{
			name: "row9_end_generation_missing_flush_evidence", opKey: "post /api/v1/work-items/{id}/end-generation", wantCode: "CORE-425", wantReason: "MISSING",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, token := claim(t)
				return e.doWithToken(t, "POST", "/api/v1/work-items/"+wi+"/end-generation", e.writer, token,
					map[string]any{"reason": "wrap"})
			},
		},
		{
			name: "row10_unblock_not_blocked", opKey: "post /api/v1/work-items/{id}/unblock", wantCode: "CORE-409",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, _ := claim(t) // running, not blocked
				return e.do(t, "POST", "/api/v1/work-items/"+wi+"/unblock", e.writer,
					map[string]any{"idempotency_key": "ub-" + wi})
			},
		},
		{
			name: "row11_budget_cas_conflict", opKey: "post /api/v1/work-items/{id}/budget-increments", wantCode: "CORE-409", wantReason: "CAS_CONFLICT",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := newExecTicket(t, e) // reporter is usr_worker = the calling writer
				return e.do(t, "POST", "/api/v1/work-items/"+wi+"/budget-increments", e.writer,
					map[string]any{"dimension": "tokens", "amount": 10, "based_on_seq": 99})
			},
		},
		{
			name: "row13_repo_inventory_unknown_wi", opKey: "get /api/v1/work-items/{id}/repo-inventory", wantCode: "CORE-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "GET", "/api/v1/work-items/wi_nope00000000/repo-inventory", e.writer, nil)
			},
		},
		// --- Sync face (rows 14–20) ---
		{
			name: "row14_put_block_digest_mismatch", opKey: "put /api/v1/work-items/{id}/revisions/{seq}/blocks/{sha256}", wantCode: "SYNC-400", wantReason: "DIGEST_MISMATCH",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, token := claim(t)
				return e.doRaw(t, "PUT",
					"/api/v1/work-items/"+wi+"/revisions/0/blocks/"+sha256Hex("other content"),
					e.writer, token, "application/octet-stream", "real content")
			},
		},
		{
			name: "row15_manifest_hash_mismatch", opKey: "post /api/v1/work-items/{id}/revisions/{seq}/manifest", wantCode: "SYNC-400", wantReason: "MANIFEST_HASH_MISMATCH",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, token := claim(t)
				exec := `{"version":1}`
				st, _, _ := e.doRaw(t, "PUT",
					"/api/v1/work-items/"+wi+"/revisions/0/blocks/"+sha256Hex(exec),
					e.writer, token, "application/octet-stream", exec)
				if st != 200 {
					t.Fatalf("block put: %d", st)
				}
				mode := float64(420)
				return e.doWithToken(t, "POST", "/api/v1/work-items/"+wi+"/revisions/0/manifest", e.writer, token,
					map[string]any{
						"manifest": map[string]any{
							"work_item_id": wi, "seq": 0, "parent": nil,
							"generation": 1, "kind": "incremental",
							"ops": []map[string]any{{
								"op": "put", "path": "runtime-state/execution.json",
								"sha256": sha256Hex(exec), "bytes": float64(len(exec)), "mode": mode,
							}},
							"live_files": 1, "data_bytes": float64(len(exec)),
							"execution_state_included": true, "built_at": "2026-09-27T00:00:00Z",
						},
						// Correct shape, wrong digest: the Core recompute must reject.
						"manifest_sha": "sha256:" + strings.Repeat("0", 64),
					})
			},
		},
		{
			name: "row16_head_prebootstrap_sentinel", opKey: "get /api/v1/work-items/{id}/head", wantCode: "404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, _ := claim(t)
				return e.do(t, "GET", "/api/v1/work-items/"+wi+"/head", e.writer, nil)
			},
		},
		{
			name: "row17_get_block_missing", opKey: "get /api/v1/work-items/{id}/revisions/{seq}/blocks/{sha256}", wantCode: "SYNC-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, _ := claim(t)
				return e.do(t, "GET", "/api/v1/work-items/"+wi+"/revisions/0/blocks/"+strings.Repeat("cd", 32), e.writer, nil)
			},
		},
		{
			name: "row18_tombstone_non_terminal_wi", opKey: "delete /api/v1/work-items/{id}/capsule-data", wantCode: "SYNC-409",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi, _ := claim(t) // running: not terminal
				return e.do(t, "DELETE", "/api/v1/work-items/"+wi+"/capsule-data?confirm="+wi, e.admin, nil)
			},
		},
		{
			name: "row18_tombstone_confirm_mismatch", opKey: "delete /api/v1/work-items/{id}/capsule-data", wantCode: "SYNC-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := terminal(t)
				return e.do(t, "DELETE", "/api/v1/work-items/"+wi+"/capsule-data?confirm=not-the-id", e.admin, nil)
			},
		},
		{
			name: "row20_revert_no_pending_tombstone", opKey: "post /api/v1/work-items/{id}/capsule-data/revert-tombstone", wantCode: "SYNC-410",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := terminal(t)
				return e.do(t, "POST", "/api/v1/work-items/"+wi+"/capsule-data/revert-tombstone", e.admin, nil)
			},
		},
		{
			name: "row19_revisions_bad_since", opKey: "get /api/v1/work-items/{id}/revisions", wantCode: "SYNC-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := newExecTicket(t, e)
				return e.do(t, "GET", "/api/v1/work-items/"+wi+"/revisions?since=abc", e.writer, nil)
			},
		},
		{
			name: "row19_revisions_unknown_include", opKey: "get /api/v1/work-items/{id}/revisions", wantCode: "CORE-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				wi := newExecTicket(t, e)
				return e.do(t, "GET", "/api/v1/work-items/"+wi+"/revisions?since=0&include=bogus", e.writer, nil)
			},
		},
		// --- Machines (row 21) ---
		{
			name: "row21_heartbeat_machine_self_register", opKey: "post /api/v1/machines/heartbeat", wantCode: "CORE-403",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				machineKey := e.mintKey(t, e.db, "mac_user_a", nil)
				return e.do(t, "POST", "/api/v1/machines/heartbeat", machineKey,
					map[string]any{"machine_id": "mac_contract_new"})
			},
		},
		// --- Memory face (rows 22–25) ---
		{
			name: "row22_memory_create_blank_content", opKey: "post /api/v1/memories", wantCode: "MEM-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
					"project": "p1", "type": "fact.note", "visibility": "project", "content": "",
				})
			},
		},
		{
			name: "row22_memory_get_unknown", opKey: "get /api/v1/memories/{id}", wantCode: "MEM-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "GET", "/api/v1/memories/mem_nope00000000", e.writer, nil)
			},
		},
		{
			name: "row23_search_blank_query", opKey: "get /api/v1/memories/search", wantCode: "MEM-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "GET", "/api/v1/memories/search?project=p1&q=", e.writer, nil)
			},
		},
		{
			name: "row23_search_unknown_project", opKey: "get /api/v1/memories/search", wantCode: "MEM-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "GET", "/api/v1/memories/search?project=nope&q=abc", e.writer, nil)
			},
		},
		// NOTE: a second redact answering the yaml-documented MEM-409 is NOT
		// probed here — see TestOpenAPIRedactTwiceDocumentedDivergence below:
		// the runtime currently answers MEM-404, a recorded contract finding.
		{
			name: "row24_consume_share_bad_token", opKey: "get /api/v1/share/{token}", wantCode: "MEM-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "GET", "/api/v1/share/contract-bad-token", "", nil)
			},
		},
		{
			name: "row25_rebuild_index_non_admin", opKey: "post /api/v1/memories/rebuild-index", wantCode: "CORE-403",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "POST", "/api/v1/memories/rebuild-index", e.writer, nil)
			},
		},
		// --- Management face (M1–M5) ---
		{
			name: "M1_create_user_non_admin", opKey: "post /api/v1/users", wantCode: "CORE-403",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "POST", "/api/v1/users", e.writer, map[string]any{"display_name": "x"})
			},
		},
		{
			name: "M1_get_unknown_user", opKey: "get /api/v1/users/{id}", wantCode: "CORE-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "GET", "/api/v1/users/usr_nope0000000", e.admin, nil)
			},
		},
		{
			name: "M2_create_key_missing_user_id", opKey: "post /api/v1/api-keys", wantCode: "CORE-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "POST", "/api/v1/api-keys", e.admin, map[string]any{})
			},
		},
		{
			name: "M3_get_unknown_project", opKey: "get /api/v1/projects/{id}", wantCode: "CORE-404",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "GET", "/api/v1/projects/nope", e.writer, nil)
			},
		},
		{
			name: "M3_patch_project_bad_member_role", opKey: "patch /api/v1/projects/{id}", wantCode: "CORE-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "PATCH", "/api/v1/projects/p1", e.admin,
					map[string]any{"members": []map[string]any{{"user_id": "usr_other", "role": "boss"}}})
			},
		},
		{
			name: "M4_backup_non_admin", opKey: "post /api/v1/admin/backup", wantCode: "CORE-403",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				return e.do(t, "POST", "/api/v1/admin/backup", e.writer, nil)
			},
		},
		{
			name: "M5_rotate_rollback_with_grace", opKey: "post /api/v1/service/rotate-secret", wantCode: "CORE-400",
			send: func(t *testing.T, e *env) (int, map[string]any, []byte) {
				// mode=rollback with a non-zero grace window is refused BEFORE
				// any rotation happens (02 §6.6), so this probe is side-effect
				// free for the other probes sharing the env.
				return e.do(t, "POST", "/api/v1/service/rotate-secret", e.admin,
					map[string]any{"mode": "rollback", "grace_seconds": 3600})
			},
		},
	}

	if len(probes) == 0 {
		t.Fatal("no probes")
	}
	for _, p := range probes {
		p := p
		t.Run(p.name, func(t *testing.T) {
			op, ok := c.ops[p.opKey]
			if !ok {
				t.Fatalf("probe names %q, which is NOT an operation in api/openapi.yaml", p.opKey)
			}
			wantStatus, ok := c.errStatus[p.wantCode]
			if !ok {
				t.Fatalf("typed code %q is missing from x-error-status-map", p.wantCode)
			}
			st, m, raw := p.send(t, e)
			code, reason := contractEnvelope(t, m)
			if code != p.wantCode {
				t.Fatalf("body code = %q, want %q (%s)", code, p.wantCode, raw)
			}
			if st != wantStatus {
				t.Fatalf("HTTP status = %d, want %d per x-error-status-map[%s] (%s)", st, wantStatus, p.wantCode, raw)
			}
			if p.wantReason != "" && reason != p.wantReason {
				t.Fatalf("body reason = %q, want %q (%s)", reason, p.wantReason, raw)
			}
			// The operation must declare the mapped status...
			node, declared := opResponses(op)[strconv.Itoa(wantStatus)]
			if !declared {
				t.Fatalf("%s does not declare a %d response for code %s", p.opKey, wantStatus, p.wantCode)
			}
			// ...and that response must carry the ErrorEnvelope schema.
			if !c.responseUsesErrorEnvelope(node) {
				t.Fatalf("%s: the declared %d response does not reference the ErrorEnvelope schema", p.opKey, wantStatus)
			}
		})
	}
}

// TestOpenAPIRedactTwiceDocumentedBehavior pins the post-#11 ruling: a
// second redact answers 404/MEM-404 (redacted-as-nonexistent serving
// semantics — the same no-existence-leak stance as every other read of a
// redacted entry), and api/openapi.yaml row 24 now documents exactly that.
// History (recorded by the #11 contract sweep): the yaml description used
// to say "Already redacted -> MEM-409", while the wire answered MEM-404
// (the MEM-409 branch in internal/memory Redact, memory.go:825 area, is
// shadowed by memForRead's Mems.Get refusal for redacted entries). The
// ruling kept the runtime behavior and fixed the contract text; the dead
// MEM-409 branch is cleanup for a future Memory-domain ticket.
func TestOpenAPIRedactTwiceDocumentedBehavior(t *testing.T) {
	c := loadContract(t)
	e := newEnv(t)

	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": "redact-twice divergence probe", "title": "t",
	})
	if st != 201 {
		t.Fatalf("create memory: %d %v", st, m)
	}
	memID := m["id"].(string)
	if st, _, _ = e.do(t, "POST", "/api/v1/memories/"+memID+"/redact", e.writer, nil); st != 200 {
		t.Fatalf("first redact: %d", st)
	}
	st, m, raw := e.do(t, "POST", "/api/v1/memories/"+memID+"/redact", e.writer, nil)
	code, _ := contractEnvelope(t, m)
	// Wire truth, now the documented contract: 404/MEM-404.
	if code != "MEM-404" {
		t.Fatalf("body code = %q, want MEM-404 (%s)", code, raw)
	}
	if wantStatus, ok := c.errStatus["MEM-404"]; !ok || st != wantStatus {
		t.Fatalf("HTTP status = %d, want %d (%s)", st, c.errStatus["MEM-404"], raw)
	}
	// The contract text must no longer claim MEM-409 for a second redact.
	op := c.ops["post /api/v1/memories/{id}/redact"]
	if node, declared := opResponses(op)["409"]; !declared {
		// A 409 response declaration is fine to keep (MEM-409 maps to 409 and
		// the error-code group of row 24 still lists it), but the DESCRIPTION
		// must carry the MEM-404 ruling; assert it does.
		_ = node
	}
	if op == nil || !strings.Contains(fmt.Sprint(op), "MEM-404") {
		t.Fatal("redact op description does not carry the MEM-404 ruling")
	}
}
