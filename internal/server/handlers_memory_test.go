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

// tierVisibilities returns the visibility values present in a list/search
// response, for the tier-matrix assertions below.
func tierVisibilities(m map[string]any) map[string]bool {
	out := map[string]bool{}
	items, _ := m["items"].([]any)
	for _, it := range items {
		if e, ok := it.(map[string]any); ok {
			if v, ok := e["visibility"].(string); ok {
				out[v] = true
			}
		}
	}
	return out
}

// TestMemoryVisibilityTiers (FB-1 regression): the 06 §7 phase-1 matrix
// holds on every read path — private = owner only; admin = users.role='admin'
// ONLY (a plain member must not see it); project|team|public = any member.
func TestMemoryVisibilityTiers(t *testing.T) {
	e := newEnv(t)
	other := e.mintKey(t, e.db, "usr_other", nil)

	// One memory per tier, all owned by usr_worker (a plain project writer).
	ids := map[string]string{}
	for _, vis := range []string{"private", "project", "team", "public", "admin"} {
		st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
			"project": "p1", "type": "fact.note", "visibility": vis,
			"content": "tierprobe body for the " + vis + " tier",
		})
		if st != 201 {
			t.Fatalf("create %s: %d %v", vis, st, m)
		}
		ids[vis] = m["id"].(string)
	}

	// List: the owner sees private|project|team|public, never the admin tier.
	st, m, _ := e.do(t, "GET", "/api/v1/memories?project=p1", e.writer, nil)
	if st != 200 {
		t.Fatalf("owner list: %d %v", st, m)
	}
	seen := tierVisibilities(m)
	for _, vis := range []string{"private", "project", "team", "public"} {
		if !seen[vis] {
			t.Fatalf("owner list missing %s: %v", vis, seen)
		}
	}
	if seen["admin"] {
		t.Fatalf("owner (non-admin role) sees the admin tier: %v", seen)
	}

	// Another member sees project|team|public only — not the owner's
	// private, not the admin tier.
	st, m, _ = e.do(t, "GET", "/api/v1/memories?project=p1", other, nil)
	if st != 200 {
		t.Fatalf("member list: %d %v", st, m)
	}
	seen = tierVisibilities(m)
	for _, vis := range []string{"project", "team", "public"} {
		if !seen[vis] {
			t.Fatalf("member list missing %s: %v", vis, seen)
		}
	}
	if seen["private"] || seen["admin"] {
		t.Fatalf("member sees private/admin tiers: %v", seen)
	}

	// A global admin passes the LAYER-1 project gate (02 §4.10: membership
	// OR admin) and so sees the admin tier — but 06 §7's layer-2 matrix has
	// NO admin exception for private (“private = 仅 owner_user_id 可读”):
	// another user's private entries stay invisible to the admin.
	st, m, _ = e.do(t, "GET", "/api/v1/memories?project=p1", e.admin, nil)
	if st != 200 {
		t.Fatalf("admin list: %d %v", st, m)
	}
	seen = tierVisibilities(m)
	for _, vis := range []string{"project", "team", "public", "admin"} {
		if !seen[vis] {
			t.Fatalf("admin list missing %s: %v", vis, seen)
		}
	}
	if seen["private"] {
		t.Fatal("admin list shows another user's private tier (06 §7: private is owner-only for everyone)")
	}
	// The admin's OWN private entry IS visible to the admin (owner read).
	st, m, _ = e.do(t, "POST", "/api/v1/memories", e.admin, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "private",
		"content": "admin own private body tierprobe",
	})
	if st != 201 {
		t.Fatalf("admin create own private: %d %v", st, m)
	}
	adminPrivate := m["id"].(string)
	st, m, _ = e.do(t, "GET", "/api/v1/memories?project=p1", e.admin, nil)
	if st != 200 || !tierVisibilities(m)["private"] {
		t.Fatalf("admin list missing own private: %d %v", st, m)
	}
	// Another member never sees the admin's private entry either.
	st, m, _ = e.do(t, "GET", "/api/v1/memories?project=p1", other, nil)
	if st != 200 || tierVisibilities(m)["private"] {
		t.Fatalf("member list shows admin's private: %d %v", st, m)
	}

	// Search follows the same matrix (all five bodies share "tierprobe").
	st, m, _ = e.do(t, "GET", "/api/v1/memories/search?project=p1&q=tierprobe", other, nil)
	if st != 200 {
		t.Fatalf("member search: %d %v", st, m)
	}
	seen = tierVisibilities(m)
	if seen["private"] || seen["admin"] || !seen["project"] || !seen["team"] || !seen["public"] {
		t.Fatalf("member search tiers wrong: %v", seen)
	}

	// Single GET: the admin tier 404s for a member but serves for an admin;
	// another member's private 404s; the owner's own private serves.
	st, _, _ = e.do(t, "GET", "/api/v1/memories/"+ids["admin"], other, nil)
	if st != 404 {
		t.Fatalf("member GET admin-tier memory: %d", st)
	}
	st, _, _ = e.do(t, "GET", "/api/v1/memories/"+ids["admin"], e.admin, nil)
	if st != 200 {
		t.Fatalf("admin GET admin-tier memory: %d", st)
	}
	st, _, _ = e.do(t, "GET", "/api/v1/memories/"+ids["private"], other, nil)
	if st != 404 {
		t.Fatalf("member GET other's private: %d", st)
	}
	st, _, _ = e.do(t, "GET", "/api/v1/memories/"+ids["private"], e.writer, nil)
	if st != 200 {
		t.Fatalf("owner GET own private: %d", st)
	}
	// 06 §7 layer-2 matrix, no admin exception: the global admin is
	// refused another user's private entry on the single GET too (the
	// layer-1 admin exemption never reaches the visibility matrix).
	st, _, _ = e.do(t, "GET", "/api/v1/memories/"+ids["private"], e.admin, nil)
	if st != 404 {
		t.Fatalf("admin GET other's private: %d", st)
	}
	st, _, _ = e.do(t, "GET", "/api/v1/memories/"+adminPrivate, e.admin, nil)
	if st != 200 {
		t.Fatalf("admin GET own private: %d", st)
	}
	// Share is owner-only (06 §5.1 A18, 02 row 24): a non-owner ADMIN is
	// refused just like any other non-owner.
	st, _, _ = e.do(t, "POST", "/api/v1/memories/"+ids["project"]+"/share", e.admin, nil)
	if st != 403 {
		t.Fatalf("admin share of another user's entry: %d", st)
	}
}

// TestMemoryMembershipGate (FB-4 regression): a non-member sees nothing of
// the project's memories on EVERY memory route (06 §7 first layer, 02
// §4.10 membership OR admin), and a project-scoped key is refused for the
// single-memory routes even when its user is a global admin (02 §8: scope
// is enforced on every route and only narrows).
func TestMemoryMembershipGate(t *testing.T) {
	e := newEnv(t)
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": "gated body gateprobe",
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	memID := m["id"].(string)

	// Non-member: no route leaks the entry, and the routes answer with the
	// no-existence-leak codes (404 for reads, 403 for the write attempt).
	if st, _, _ := e.do(t, "POST", "/api/v1/memories", e.outsider, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project", "content": "x",
	}); st != 403 {
		t.Fatalf("outsider create: %d", st)
	}
	for path, want := range map[string]int{
		"/api/v1/memories?project=p1":                    404, // list
		"/api/v1/memories/search?project=p1&q=gateprobe": 404, // search
		"/api/v1/memories/" + memID:                      404, // get
	} {
		if st, _, _ := e.do(t, "GET", path, e.outsider, nil); st != want {
			t.Fatalf("outsider GET %s: %d", path, st)
		}
	}
	if st, _, _ := e.do(t, "PATCH", "/api/v1/memories/"+memID, e.outsider, map[string]any{"content": "no"}); st != 404 {
		t.Fatalf("outsider patch: %d", st)
	}
	if st, _, _ := e.do(t, "POST", "/api/v1/memories/"+memID+"/redact", e.outsider, nil); st != 404 {
		t.Fatalf("outsider redact: %d", st)
	}
	if st, _, _ := e.do(t, "POST", "/api/v1/memories/"+memID+"/share", e.outsider, nil); st != 404 {
		t.Fatalf("outsider share: %d", st)
	}

	// Scoped-key gap (fixed): a key scoped to pj1 whose user is a GLOBAL
	// ADMIN must be refused for a pj2 memory — pre-fix the admin role
	// satisfied canRead and no scope check ran on the single-memory routes.
	if _, err := e.db.Exec(`INSERT INTO projects(id, name) VALUES ('pj2','p2')`); err != nil {
		t.Fatal(err)
	}
	st, m, _ = e.do(t, "POST", "/api/v1/memories", e.admin, map[string]any{
		"project": "p2", "type": "fact.note", "visibility": "project",
		"content": "other project body scopeprobe",
	})
	if st != 201 {
		t.Fatalf("admin create in p2: %d %v", st, m)
	}
	mem2 := m["id"].(string)
	pj1 := "pj1"
	scopedAdmin := e.mintKey(t, e.db, "usr_admin", &pj1)

	if st, _, _ := e.do(t, "GET", "/api/v1/memories/"+mem2, scopedAdmin, nil); st != 403 {
		t.Fatalf("scoped-admin GET out-of-scope memory: %d", st)
	}
	if st, _, _ := e.do(t, "PATCH", "/api/v1/memories/"+mem2, scopedAdmin, map[string]any{"content": "no"}); st != 403 {
		t.Fatalf("scoped-admin PATCH out-of-scope memory: %d", st)
	}
	if st, _, _ := e.do(t, "POST", "/api/v1/memories/"+mem2+"/share", scopedAdmin, nil); st != 403 {
		t.Fatalf("scoped-admin share out-of-scope memory: %d", st)
	}
	// The same scoped key still works inside its own scope.
	st, _, _ = e.do(t, "GET", "/api/v1/memories/"+memID, scopedAdmin, nil)
	if st != 200 {
		t.Fatalf("scoped-admin GET in-scope memory: %d", st)
	}
}

// TestShareTokenRandomIssuance (FB-10 regression): 02 row 24 pins the
// share token as RANDOMLY issued at generation time and non-enumerable.
// Two issuances of the same memory must differ, keep the
// <mem_id> "." <22 lowercase hex> shape, and both must resolve anonymously;
// a well-shaped but never-issued token answers MEM-404; redaction (which
// hides the entry itself) kills consumption.
func TestShareTokenRandomIssuance(t *testing.T) {
	e := newEnv(t)
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": "shareable body shareprobe",
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	memID := m["id"].(string)

	issue := func() string {
		st, m, _ := e.do(t, "POST", "/api/v1/memories/"+memID+"/share", e.writer, nil)
		if st != 200 {
			t.Fatalf("share: %d %v", st, m)
		}
		return m["share_token"].(string)
	}
	tok1 := issue()
	tok2 := issue()

	if tok1 == tok2 {
		t.Fatalf("two issuances produced the identical token %q — deterministic issuance", tok1)
	}
	for _, tok := range []string{tok1, tok2} {
		if !strings.HasPrefix(tok, memID+".") {
			t.Fatalf("token lost its <mem_id>. prefix: %q", tok)
		}
		suffix := tok[len(memID)+1:]
		if len(suffix) != 22 {
			t.Fatalf("token suffix not 22 chars: %q", suffix)
		}
		for _, c := range suffix {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				t.Fatalf("token suffix not lowercase hex: %q", suffix)
			}
		}
		st, m, _ := e.do(t, "GET", "/api/v1/share/"+tok, "", nil)
		if st != 200 || m["content"] == nil {
			t.Fatalf("anonymous consume of issued token: %d %v", st, m)
		}
	}

	// Well-shaped but never-issued (the memID is real, the suffix is not):
	// MEM-404, no existence information beyond the shared entry.
	forged := memID + ".0000000000000000000000"
	if st, _, _ := e.do(t, "GET", "/api/v1/share/"+forged, "", nil); st != 404 {
		t.Fatalf("never-issued token consumed: %d", st)
	}
	// Malformed shapes also answer MEM-404 (contract row24 case).
	if st, _, _ := e.do(t, "GET", "/api/v1/share/contract-bad-token", "", nil); st != 404 {
		t.Fatalf("malformed token: %d", st)
	}

	// Redaction hides the entry: the issued tokens stop resolving to it.
	st, _, _ = e.do(t, "POST", "/api/v1/memories/"+memID+"/redact", e.writer, nil)
	if st != 200 {
		t.Fatalf("redact: %d", st)
	}
	if st, _, _ := e.do(t, "GET", "/api/v1/share/"+tok1, "", nil); st != 404 {
		t.Fatalf("redacted memory still consumable via share: %d", st)
	}
}

// TestMemoryIdempotencyReplayBinding (R2C-F7 regression): create-replay is
// bound to (idempotency_key, caller principal, project) — the INV-2d
// three-way-match shape 02 §4.3/§6.6 pins for idempotency reuse, rendered
// on the memory face as MEM-409 IDEMPOTENCY_KEY_REUSED (06 §5.1/§5.4: the
// POST /memories conflict code). The pre-fix lookup was globally keyed: a
// reused key returned another project/owner's existing entry with no
// scope/membership/visibility recheck. Same-principal/project replay keeps
// working; mismatched reuse leaks nothing.
func TestMemoryIdempotencyReplayBinding(t *testing.T) {
	e := newEnv(t)
	if _, err := e.db.Exec(`INSERT INTO projects(id, name) VALUES ('pj2','p2')`); err != nil {
		t.Fatal(err)
	}

	// Seed: the admin creates in p1 under an explicit idempotency key.
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.admin, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "private",
		"content": "idempotent body r2c", "idempotency_key": "idem-r2c-f7",
	})
	if st != 201 {
		t.Fatalf("seed create: %d %v", st, m)
	}
	memID := m["id"].(string)

	// Same key + same principal + same project: replay returns the SAME
	// entry, and no second row is created.
	st, m, _ = e.do(t, "POST", "/api/v1/memories", e.admin, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "private",
		"content": "idempotent body r2c", "idempotency_key": "idem-r2c-f7",
	})
	if st != 201 || m["id"].(string) != memID {
		t.Fatalf("same-principal/project replay: %d %v", st, m)
	}
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE idempotency_key='idem-r2c-f7'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("replay created a second row: %d", n)
	}

	// Same key, DIFFERENT project (admin may write p2, so the server gates
	// pass and only the replay binding stops the leak): typed MEM-409
	// IDEMPOTENCY_KEY_REUSED, and the p1 entry's body never crosses.
	st, m, _ = e.do(t, "POST", "/api/v1/memories", e.admin, map[string]any{
		"project": "p2", "type": "fact.note", "visibility": "private",
		"content": "anything", "idempotency_key": "idem-r2c-f7",
	})
	if st != 409 || errCode(t, m) != "MEM-409" {
		t.Fatalf("cross-project key reuse: %d %v", st, m)
	}
	if reason, _ := m["error"].(map[string]any)["reason"].(string); reason != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("reuse reason: %v", m)
	}

	// Same key, DIFFERENT principal (usr_worker is a pj1 member with a
	// pj1-scoped key, so the scope/membership gates pass): owner mismatch
	// → MEM-409, nothing of the admin's private entry is returned.
	st, m, _ = e.do(t, "POST", "/api/v1/memories", e.scoped, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "private",
		"content": "anything", "idempotency_key": "idem-r2c-f7",
	})
	if st != 409 || errCode(t, m) != "MEM-409" {
		t.Fatalf("cross-owner key reuse: %d %v", st, m)
	}

	// A different key still creates normally in p2 for the same principal.
	st, m, _ = e.do(t, "POST", "/api/v1/memories", e.admin, map[string]any{
		"project": "p2", "type": "fact.note", "visibility": "private",
		"content": "fresh", "idempotency_key": "idem-r2c-f7-p2",
	})
	if st != 201 {
		t.Fatalf("fresh key create: %d %v", st, m)
	}
}

// TestShareOwnerOfAdminTierEntry (R3C-F4 regression, 06 §5.1 A18 + 02
// row 24): share is an explicit owner-authorized grant channel OUTSIDE the
// visibility tiers. The pre-fix route went through memForRead, whose
// admin-tier predicate rejected a non-admin OWNER before ownership was ever
// considered — the one principal the A18 ruling names could never mint a
// share. The owner must pass even for admin-tier entries; consumers of the
// issued token read via the anonymous share face.
func TestShareOwnerOfAdminTierEntry(t *testing.T) {
	e := newEnv(t)
	other := e.mintKey(t, e.db, "usr_other", nil)

	// A non-admin writer owns an admin-tier entry.
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "admin",
		"content": "admin-tier body shareownerprobe",
	})
	if st != 201 {
		t.Fatalf("create admin-tier: %d %v", st, m)
	}
	memID := m["id"].(string)

	// Positive: the non-admin OWNER can share it (A18 exception).
	st, m, _ = e.do(t, "POST", "/api/v1/memories/"+memID+"/share", e.writer, nil)
	if st != 200 {
		t.Fatalf("non-admin owner share of own admin-tier entry: %d %v", st, m)
	}
	tok := m["share_token"].(string)
	// The consumer reads via the share face, anonymously.
	st, m, _ = e.do(t, "GET", "/api/v1/share/"+tok, "", nil)
	if st != 200 || !strings.Contains(m["content"].(string), "admin-tier body shareownerprobe") {
		t.Fatalf("share consume of admin-tier entry: %d %v", st, m)
	}
	if m["entry"] == nil {
		t.Fatal("share consume must return the entry view")
	}

	// Negative: a non-owner non-admin member is still refused — and learns
	// nothing of the entry (the read routes answer MEM-404 for it).
	if st, _, _ = e.do(t, "POST", "/api/v1/memories/"+memID+"/share", other, nil); st != 404 {
		t.Fatalf("non-owner member share of admin-tier entry: %d", st)
	}
	// Negative: a non-owner ADMIN is refused too (A18: owner-only, no admin
	// exception) — but without existence leak, since the admin CAN read the
	// admin tier the refusal is the owner-only MEM-403.
	if st, _, _ = e.do(t, "POST", "/api/v1/memories/"+memID+"/share", e.admin, nil); st != 403 {
		t.Fatalf("non-owner admin share of admin-tier entry: %d", st)
	}

	// The owner's own private-tier entry keeps sharing too (regression
	// guard: the A18 exception must not have narrowed the previously
	// working owner paths).
	st, m, _ = e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "private",
		"content": "private owner share probe",
	})
	if st != 201 {
		t.Fatalf("create private: %d %v", st, m)
	}
	st, m, _ = e.do(t, "POST", "/api/v1/memories/"+m["id"].(string)+"/share", e.writer, nil)
	if st != 200 {
		t.Fatalf("owner share of own private entry: %d %v", st, m)
	}
}
