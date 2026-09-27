//go:build ignore

// openapi-lint validates api/openapi.yaml — the frozen wire contract of the
// aihub core HTTP API — against the canonical endpoint inventory of
// docs/design/polyforge-vnext/02-aihub-core.md §5.1 (26 business rows +
// management face M1–M5 + the row-26 dependency-edge routes) and a set of
// structural consistency rules. It is deliberately dependency-light: only
// gopkg.in/yaml.v3 (already a direct module dependency) plus the standard
// library. Kept out of the normal build path via the `ignore` build tag;
// run from the repo root via `go run ./api/openapi-lint.go` (Makefile
// target `openapi-lint`). Exits non-zero on any drift.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// expectedOp names one canonical (method, path) operation. Row is the
// canonical inventory row from 02 §5.1 the operation belongs to ("M1".."M5"
// for the management face). The set below is derived verbatim from
// 02 §5.1:
//
//	26 business rows = WI business 6 (rows 1–6, row 3 folding GET+PATCH on
//	one path) + execution right 7 (rows 7–13) + sync 7 (rows 14–20) +
//	machines 1 (row 21) + memory 4 rows (22–25) + dependency edges 1 row
//	(row 26, three routes) — plus the management face M1–M5.
//
// The Memory row 22 (POST/GET/PATCH over /api/v1/memories and
// /api/v1/memories/{id}) and row 24 (redact/share/anonymous consume) are
// operation GROUPS in the design: each listed method+path pair below is one
// OpenAPI operation, and the groups are counted as single rows.
var expectedOps = []struct {
	Row   string
	Method string
	Path  string
}{
	// A. WI business (rows 1–6).
	{"1", "post", "/api/v1/work-items"},
	{"2", "get", "/api/v1/work-items"},
	{"3", "get", "/api/v1/work-items/{id}"},
	{"3", "patch", "/api/v1/work-items/{id}"},
	{"4", "get", "/api/v1/ready-queue"},
	{"5", "post", "/api/v1/work-items/{id}/cancel"},
	{"6", "get", "/api/v1/work-items/{id}/progress"},
	// G. Dependency edges (row 26, design amendment polyforge#3).
	{"26", "post", "/api/v1/work-items/{id}/dependencies"},
	{"26", "get", "/api/v1/work-items/{id}/dependencies"},
	{"26", "delete", "/api/v1/work-items/{id}/dependencies/{blocking_id}/{kind}"},
	// B. Execution right (rows 7–13).
	{"7", "post", "/api/v1/work-items/{id}/claim"},
	{"8", "post", "/api/v1/work-items/{id}/force-takeover"},
	{"9", "post", "/api/v1/work-items/{id}/end-generation"},
	{"10", "post", "/api/v1/work-items/{id}/unblock"},
	{"11", "post", "/api/v1/work-items/{id}/budget-increments"},
	{"12", "get", "/api/v1/work-items/{id}/budget"},
	{"13", "get", "/api/v1/work-items/{id}/repo-inventory"},
	// C. Sync (rows 14–20).
	{"14", "put", "/api/v1/work-items/{id}/revisions/{seq}/blocks/{sha256}"},
	{"15", "post", "/api/v1/work-items/{id}/revisions/{seq}/manifest"},
	{"16", "get", "/api/v1/work-items/{id}/head"},
	{"17", "get", "/api/v1/work-items/{id}/revisions/{seq}/blocks/{sha256}"},
	{"18", "delete", "/api/v1/work-items/{id}/capsule-data"},
	{"19", "get", "/api/v1/work-items/{id}/revisions"},
	{"20", "post", "/api/v1/work-items/{id}/capsule-data/revert-tombstone"},
	// D. Machines (row 21).
	{"21", "post", "/api/v1/machines/heartbeat"},
	// E. Memory (rows 22–25).
	{"22", "post", "/api/v1/memories"},
	{"22", "get", "/api/v1/memories"},
	{"22", "get", "/api/v1/memories/{id}"},
	{"22", "patch", "/api/v1/memories/{id}"},
	{"23", "get", "/api/v1/memories/search"},
	{"24", "post", "/api/v1/memories/{id}/redact"},
	{"24", "post", "/api/v1/memories/{id}/share"},
	{"24", "get", "/api/v1/share/{token}"},
	{"25", "post", "/api/v1/memories/rebuild-index"},
	// F. Management face (M1–M5).
	{"M1", "post", "/api/v1/users"},
	{"M1", "get", "/api/v1/users"},
	{"M1", "get", "/api/v1/users/{id}"},
	{"M1", "patch", "/api/v1/users/{id}"},
	{"M2", "post", "/api/v1/api-keys"},
	{"M2", "get", "/api/v1/api-keys"},
	{"M2", "delete", "/api/v1/api-keys/{id}"},
	{"M3", "post", "/api/v1/projects"},
	{"M3", "get", "/api/v1/projects"},
	{"M3", "get", "/api/v1/projects/{id}"},
	{"M3", "patch", "/api/v1/projects/{id}"},
	{"M4", "post", "/api/v1/admin/backup"},
	{"M5", "post", "/api/v1/service/rotate-secret"},
}

// tokenRequiredOps are the operations whose wire contract requires the
// X-PF-Generation-Token header (02 §5.1 tier 3, as enforced by the
// implementation: rows 9, 14, 15 — NOT row 12, whose token check the
// implementation omits).
var tokenRequiredOps = map[string]bool{
	"post /api/v1/work-items/{id}/end-generation":                    true,
	"put /api/v1/work-items/{id}/revisions/{seq}/blocks/{sha256}":     true,
	"post /api/v1/work-items/{id}/revisions/{seq}/manifest":           true,
}

// anonymousOp is the only operation that must explicitly opt out of the
// global BearerAuth security (02 §5.1 row 24: anonymous read-only share
// consumption).
const anonymousOp = "get /api/v1/share/{token}"

// expectedErrorCodes is the union of the typed error codes of
// 02 §5.3 (CORE), 05 §5.2 (SYNC) and 06 §5.4 (MEM), plus CORE-500/MEM-500
// (writeErr's default) and the literal "404" pre-bootstrap sentinel used by
// capsule.Head. Every one of them must appear in the root-level
// x-error-status-map so the HTTP status mapping stays complete.
var expectedErrorCodes = []string{
	"CORE-400", "CORE-401", "CORE-403", "CORE-404", "CORE-409", "CORE-410",
	"CORE-413", "CORE-425", "CORE-500", "CORE-503", "CORE-504", "CORE-507",
	"CORE-508", "CORE-509",
	"SYNC-400", "SYNC-403", "SYNC-404", "SYNC-409", "SYNC-410",
	"MEM-400", "MEM-403", "MEM-404", "MEM-409", "MEM-413", "MEM-500",
	"MEM-501", "MEM-502", "MEM-503",
	"404",
}

// businessRows/managementRows pin the counting rule: 26 business rows (with
// row 3 counting GET+PATCH once and rows 22/24/26 being operation groups)
// and 5 management rows.
const (
	wantBusinessOpsPerRowBasis = 26 // distinct canonical row labels in the business face
	wantManagementRows         = 5
)

func main() {
	if err := run("api/openapi.yaml"); err != nil {
		fmt.Fprintf(os.Stderr, "openapi-lint: FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// 1. OpenAPI version and single http server.
	if v, _ := doc["openapi"].(string); v != "3.1.0" {
		fail("openapi must be \"3.1.0\", got %q", v)
	}
	servers, _ := doc["servers"].([]any)
	if len(servers) != 1 {
		fail("exactly one server entry required (single port, 02 §5.1 A5), got %d", len(servers))
	} else {
		srv, _ := servers[0].(map[string]any)
		if url, _ := srv["url"].(string); !strings.HasPrefix(url, "http://") {
			fail("server url must be plain http (v1 intranet, 05 §8), got %q", url)
		}
	}

	// 2. Bearer security scheme present.
	comps, _ := doc["components"].(map[string]any)
	schemes, _ := comps["securitySchemes"].(map[string]any)
	bearer, ok := schemes["BearerAuth"].(map[string]any)
	if !ok {
		fail("components.securitySchemes.BearerAuth missing")
	} else {
		if t, _ := bearer["type"].(string); t != "http" {
			fail("BearerAuth.type must be http, got %q", t)
		}
		if s, _ := bearer["scheme"].(string); s != "bearer" {
			fail("BearerAuth.scheme must be bearer, got %q", s)
		}
	}

	// 3. Global security declared (operations inherit; the anonymous share
	//    op opts out explicitly).
	if _, ok := doc["security"]; !ok {
		fail("root-level security (BearerAuth) missing")
	}

	// 4. Operation set vs the canonical inventory.
	got := map[string]string{} // "METHOD path" -> row label (from expectedOps)
	gotRows := map[string]int{}
	paths, _ := doc["paths"].(map[string]any)
	if paths == nil {
		return fmt.Errorf("paths section missing")
	}
	nonOpKeys := map[string]bool{"parameters": true, "summary": true, "description": true, "servers": true, "$ref": true}
	for path, itemAny := range paths {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		for method, opAny := range item {
			m := strings.ToLower(method)
			if nonOpKeys[method] || !(m == "get" || m == "post" || m == "put" || m == "patch" || m == "delete") {
				continue
			}
			key := m + " " + path
			op, ok := opAny.(map[string]any)
			if !ok {
				fail("%s: operation body is not a mapping", key)
				continue
			}
			row, known := lookupRow(key)
			if !known {
				fail("%s: operation is NOT in the canonical inventory (02 §5.1) — remove it or amend the inventory", key)
				continue
			}
			got[key] = row
			gotRows[row]++

			// Every operation description must cite its canonical row and be non-empty.
			desc, _ := op["description"].(string)
			if strings.TrimSpace(desc) == "" {
				fail("%s: missing description", key)
			} else if !strings.Contains(desc, "Canonical") {
				fail("%s: description does not cite its canonical row (want \"Canonical row N (02 section 5.1)\")", key)
			}
			if oid, _ := op["operationId"].(string); oid == "" {
				fail("%s: missing operationId", key)
			}

			// Anonymous share consumption must opt out of security; every
			// other operation must NOT opt out.
			if secAny, hasSec := op["security"]; hasSec {
				sec, _ := secAny.([]any)
				if key == anonymousOp {
					if len(sec) != 0 {
						fail("%s: must be anonymous (security: [])", key)
					}
				} else {
					fail("%s: must not override the global BearerAuth security (only %s may)", key, anonymousOp)
				}
			} else if key == anonymousOp {
				fail("%s: must declare security: [] (anonymous by design)", key)
			}

			// Token applicability: tier-3 operations must carry the
			// X-PF-Generation-Token header parameter (directly or via the
			// shared path/operation parameter list), marked required.
			if tokenRequiredOps[key] && !hasGenerationToken(item, op) {
				fail("%s: must require the X-PF-Generation-Token header (02 §5.1 tier 3)", key)
			}
			if !tokenRequiredOps[key] && hasGenerationToken(item, op) {
				fail("%s: must NOT require the X-PF-Generation-Token header (tier 1/2 operation)", key)
			}
		}
	}
	for _, e := range expectedOps {
		key := e.Method + " " + e.Path
		if _, ok := got[key]; !ok {
			fail("missing canonical operation %s (row %s)", key, e.Row)
		}
	}
	// Business-row counting basis: every one of the 26 business row labels
	// must be represented, plus the 5 management rows.
	rowLabels := map[string]bool{}
	for _, e := range expectedOps {
		rowLabels[e.Row] = true
	}
	business, management := 0, 0
	for row := range rowLabels {
		if strings.HasPrefix(row, "M") && row != "26" && len(row) == 2 {
			management++
		} else {
			business++
		}
	}
	if business != wantBusinessOpsPerRowBasis {
		fail("business canonical rows = %d, want %d", business, wantBusinessOpsPerRowBasis)
	}
	if management != wantManagementRows {
		fail("management rows = %d, want %d", management, wantManagementRows)
	}

	// 5. $ref resolution: every local ref must point at an existing
	//    component node.
	errs := 0
	var walk func(node any)
	walk = func(node any) {
		switch t := node.(type) {
		case map[string]any:
			for k, v := range t {
				if k == "$ref" {
					if s, ok := v.(string); ok {
						if !refExists(doc, s) {
							fail("dangling $ref %q", s)
						}
					}
				}
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}
	walk(doc)
	_ = errs

	// 6. Error-code status map: every typed code of 02 §5.3 / 05 §5.2 /
	//    06 §5.4 must be mapped (the map mirrors errStatus in
	//    internal/server/server.go).
	errMapAny, ok := doc["x-error-status-map"].(map[string]any)
	if !ok {
		fail("root-level x-error-status-map missing")
		errMapAny = map[string]any{}
	}
	for _, code := range expectedErrorCodes {
		if _, ok := errMapAny[code]; !ok {
			fail("x-error-status-map missing code %s", code)
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		var b strings.Builder
		fmt.Fprintf(&b, "%d problem(s):", len(problems))
		for _, p := range problems {
			fmt.Fprintf(&b, "\n  - %s", p)
		}
		return fmt.Errorf("%s", b.String())
	}

	fmt.Printf("openapi-lint: OK — %d operations (26 business rows incl. row-26 dependency routes + 5 management rows), %d paths, %d error codes mapped\n",
		len(expectedOps), len(paths), len(expectedErrorCodes))
	return nil
}

func lookupRow(key string) (string, bool) {
	for _, e := range expectedOps {
		if e.Method+" "+e.Path == key {
			return e.Row, true
		}
	}
	return "", false
}

// hasGenerationToken reports whether the operation (or its shared path-item
// parameters) references the GenerationToken header parameter with
// required: true.
func hasGenerationToken(pathItem, op map[string]any) bool {
	check := func(params []any) bool {
		for _, pAny := range params {
			p, ok := pAny.(map[string]any)
			if !ok {
				continue
			}
			name, _ := p["name"].(string)
			req, _ := p["required"].(bool)
			if ref, ok := p["$ref"].(string); ok && strings.HasSuffix(ref, "/GenerationToken") {
				return true
			}
			if name == "X-PF-Generation-Token" && req {
				return true
			}
		}
		return false
	}
	if ps, ok := op["parameters"].([]any); ok && check(ps) {
		return true
	}
	if ps, ok := pathItem["parameters"].([]any); ok && check(ps) {
		return true
	}
	return false
}

// refExists resolves a local "#/..." pointer inside the parsed document.
func refExists(doc map[string]any, ref string) bool {
	if !strings.HasPrefix(ref, "#/") {
		return true // remote or full-document ref: not checked here
	}
	cur := any(doc)
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[seg]
		if !ok {
			return false
		}
	}
	return true
}
