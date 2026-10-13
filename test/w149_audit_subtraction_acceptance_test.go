package test

// w149_audit_subtraction_acceptance_test.go: the polyforge#149
// acceptance suite (audit subtraction C — aihub and its call chains:
// C1 delivery ledger, C9 terminate-blocked, C12 session_owner, B9 the
// bundle digest chain, the dead schema/code list, the waiting_on
// retention rewind fix, D3 rune-counted goal limit, D5 owner =
// reporter). Authored against the capsule's artifacts/spec.md
// (Requirements / #### Scenario / Acceptance-criteria blocks, mirrored
// scenario-for-scenario in artifacts/e2e-scenarios.md) BEFORE the
// implementation lands, per the e2e-author step.
//
// It drives the spec'd behavior through the project's real e2e
// surfaces only: the real aihub binary on a loopback port, its HTTP API
// (through the real aihubclient and through raw requests when a header
// or a route's existence is the subject), the real `pf` CLI binary, the
// aihub sqlite data file, and the real docs / skills trees. Every
// deleted mechanism is touched as a WIRE SHAPE (route path, JSON key,
// header name, table/column name, error-code string, doc token) and
// NEVER as a Go symbol — so this file compiles against the
// pre-implementation tree (e2e-baseline runs it before `code`) and
// keeps compiling after the deletion lands (e2e-run reruns it).
//
// Suite posture (full mapping in artifacts/e2e-scenarios.md):
//   - New-behavior bars are RED on the pre-implementation tree:
//     assertion-level reds only (HTTP status, JSON key presence, typed
//     error code, sqlite schema rows, doc/code token scans), never
//     build reds. They go green when the change lands the spec.
//   - Kept-face guards (bundle_name create/PATCH incl. the null-unbind,
//     recovery of a blocked WI via continue/abandon, the server-side
//     blocked transition writing waiting_on='owner', the value-agnostic
//     ready-queue filter, the reporter gate on continue/abandon, the
//     admin/maintainer-only ForceEnd gate) are green BOTH before and
//     after the change.
//
// Scan scoping (deliberate, see artifacts/e2e-scenarios.md §Notes):
//   - the code-face scan covers non-test .go files under internal/,
//     cmd/ and tools/. Test files are governed behaviorally: the bar
//     (`go test ./...`) forces the rewrite of every test that pinned a
//     deleted mechanism.
//   - CORE-508 / MEM-502 / MEM-503 are policed as CONSTANTS in the
//     error registry (internal/common/api) only: the spec deletes the
//     constants, while the CORE-508 refusal literal in cmd/aihub and
//     the MEM-50x literals of the live memory search face are not part
//     of this subtraction.
//   - the doc-face scan covers docs/design/polyforge-vnext and
//     skills/polyforge exactly as the spec's acceptance criterion names
//     them (historical docs/release-notes are not rewritten).

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// ---------------------------------------------------------------------------
// Helpers (all w149-prefixed; the shared v1 harness — newV1Install,
// sessionClaimReq, strPtr, hermeticConfigPath, v1Root, stopAihub/
// restartAihub — is reused as-is).
// ---------------------------------------------------------------------------

// w149Bundle is an arbitrary bundle name: the aihub side stores the
// name as a scalar, so any name binds.
const w149Bundle = "quick-fix"

// w149Setup boots the real aihub and one fresh scratch project.
func w149Setup(t *testing.T, project string) *v1Install {
	t.Helper()
	inst := newV1Install(t)
	if _, err := inst.client.CreateProject(aihubclient.ProjectCreateRequest{
		Name: project, Description: "w149 acceptance scratch",
	}); err != nil {
		t.Fatalf("create project %s: %v", project, err)
	}
	return inst
}

// w149CreateAs files one work item through the given client face (the
// authenticated key's user becomes the WI's reporter) and returns its id.
func w149CreateAs(t *testing.T, c *aihubclient.Client, project, goal string, bundle *string) string {
	t.Helper()
	raw, err := c.CreateWorkItem(aihubclient.CreateWorkItemRequest{
		Project: project, Goal: goal, BundleName: bundle,
	})
	if err != nil {
		t.Fatalf("create work item (%s): %v", goal, err)
	}
	var wi struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &wi); err != nil || wi.ID == "" {
		t.Fatalf("create work item answer: %v (%s)", err, raw)
	}
	return wi.ID
}

// w149Create files one work item through the install's admin face.
func w149Create(t *testing.T, inst *v1Install, project, goal string, bundle *string) string {
	t.Helper()
	return w149CreateAs(t, inst.client, project, goal, bundle)
}

// w149View fetches one WI detail record as a generic map (raw wire: the
// assertions are about which KEYS the server serves).
func w149View(t *testing.T, inst *v1Install, id string) map[string]any {
	t.Helper()
	raw, err := inst.client.GetWorkItemRaw(id)
	if err != nil {
		t.Fatalf("get work item %s: %v", id, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("get work item %s answer: %v (%s)", id, err, raw)
	}
	return m
}

// w149Has reports whether the served record carries the key at all.
func w149Has(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// w149Str reads a string field ("" when absent or null).
func w149Str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// w149Code is the typed wire error code ("" when the error is not a
// typed envelope; "HTTP-404" & co. are synthesized by the client).
func w149Code(err error) string {
	if ae, ok := err.(*api.Error); ok {
		return ae.Code
	}
	return ""
}

// w149Patch attempts a PATCH and returns the wire error (nil = accepted).
func w149Patch(inst *v1Install, id string, fields map[string]any) error {
	_, err := inst.client.PatchWorkItem(id, fields)
	return err
}

// w149Raw performs one RAW HTTP request against the install: bearer ""
// sends no Authorization header (the header-contract scenarios), hdr
// adds/overrides request headers. Returns (status, body).
func w149Raw(t *testing.T, inst *v1Install, method, path, bearer string, body any, hdr map[string]string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, inst.url+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, data
}

// w149Block parks a work item blocked through the REAL session lane:
// claim with a fresh session, then the server-side blocked verdict
// (session-end status=blocked — the frozen-ticket transition).
func w149Block(t *testing.T, inst *v1Install, id string) {
	t.Helper()
	req := sessionClaimReq(id)
	if _, err := inst.client.SessionClaim(req); err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	if err := inst.client.SessionEnd(id, api.SessionEndRequest{
		SessionID: req.SessionID, Status: api.StatusBlocked,
	}); err != nil {
		t.Fatalf("session-end(blocked) %s: %v", id, err)
	}
	if st := inst.wiStatus(t, id); st != api.StatusBlocked {
		t.Fatalf("work item %s is %s after the blocked verdict, want blocked", id, st)
	}
}

// w149Claim claims the WI with a fresh session and returns the session
// secret (the WI is left RUNNING, its session held).
func w149Claim(t *testing.T, inst *v1Install, id string) string {
	t.Helper()
	req := sessionClaimReq(id)
	if _, err := inst.client.SessionClaim(req); err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	return req.SessionID
}

// w149MintKey creates one human writer user plus an API key and returns
// (user id, key plaintext).
func w149MintKey(t *testing.T, inst *v1Install, name string) (string, string) {
	t.Helper()
	raw, err := inst.client.CreateUser(aihubclient.UserCreateRequest{
		DisplayName: name,
		Email:       name + "@w149.invalid",
		UserType:    "human",
		Role:        "writer",
	})
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	var user struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &user); err != nil || user.ID == "" {
		t.Fatalf("user answer for %s: %v (%s)", name, err, raw)
	}
	raw, err = inst.client.CreateApiKey(aihubclient.ApiKeyCreateRequest{UserID: user.ID})
	if err != nil {
		t.Fatalf("mint key for %s: %v", name, err)
	}
	var key struct {
		Plain string `json:"plain"`
	}
	if err := json.Unmarshal(raw, &key); err != nil || key.Plain == "" {
		t.Fatalf("key answer for %s: %v (%s)", name, err, raw)
	}
	return user.ID, key.Plain
}

// w149Pf runs the REAL pf CLI against this install and returns
// (combined output, exit code) with NO assertion on the code: the
// scenarios assert the HUB-side effect, not the CLI's wording.
func w149Pf(t *testing.T, inst *v1Install, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(inst.pfPath, append([]string{"--config", hermeticConfigPath(t)}, args...)...)
	cmd.Env = append(os.Environ(),
		"PF_SERVER="+inst.url,
		"PF_API_KEY="+inst.adminKey,
		"PF_USER_ID=usr_root",
	)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("pf %v: %v (output %q)", args, err, out.String())
		}
		code = ee.ExitCode()
	}
	return out.String(), code
}

// w149Queue fetches the ready queue as a generic map.
func w149Queue(t *testing.T, inst *v1Install, project string) map[string]any {
	t.Helper()
	raw, err := inst.client.ReadyQueueRaw(project)
	if err != nil {
		t.Fatalf("ready queue %s: %v", project, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("ready queue %s answer: %v (%s)", project, err, raw)
	}
	return m
}

// w149InSegment reports whether the queue segment carries the WI id.
func w149InSegment(q map[string]any, segment, id string) bool {
	arr, _ := q[segment].([]any)
	for _, e := range arr {
		if obj, ok := e.(map[string]any); ok {
			if obj["id"] == id {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// sqlite data-file inspection (the DDL face: "a fresh DB created by the
// new Open").
// ---------------------------------------------------------------------------

func w149OpenDB(t *testing.T, inst *v1Install) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite",
		"file:"+filepath.Join(inst.dataDir, "aihub.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open aihub.db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func w149TableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?1`, table).Scan(&n); err != nil {
		t.Fatalf("sqlite_master lookup of %s: %v", table, err)
	}
	return n > 0
}

func w149ColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?1) WHERE name=?2`, table, column).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(%s) lookup of %s: %v", table, column, err)
	}
	return n > 0
}

// ---------------------------------------------------------------------------
// Token scans (code face / doc face).
// ---------------------------------------------------------------------------

// w149Hits lists "<relpath>:<line>" occurrences of token under root
// (absolute), visiting only files keep() accepts.
func w149Hits(t *testing.T, root, token string, keep func(path string) bool) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if keep != nil && !keep(path) {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if !strings.Contains(string(b), token) {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, token) {
				hits = append(hits, fmt.Sprintf("%s:%d", path, i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s for %q: %v", root, token, err)
	}
	return hits
}

// w149GoKeep accepts non-test Go sources (the code face).
func w149GoKeep(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

// w149MdKeep accepts markdown (the doc face).
func w149MdKeep(path string) bool { return strings.HasSuffix(path, ".md") }

// w149CodeHits scans the project's non-test Go code (internal/, cmd/,
// tools/) for token.
func w149CodeHits(t *testing.T, token string) []string {
	t.Helper()
	root := v1Root(t)
	var hits []string
	for _, dir := range []string{"internal", "cmd", "tools"} {
		p := filepath.Join(root, dir)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		hits = append(hits, w149Hits(t, p, token, w149GoKeep)...)
	}
	return hits
}

// w149AssertCodeClean fails (non-fatally: every token reports) when any
// token still appears in the non-test code face.
func w149AssertCodeClean(t *testing.T, item string, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if hits := w149CodeHits(t, tok); len(hits) > 0 {
			t.Errorf("%s: token %q still present in non-test code: %v", item, tok, hits)
		}
	}
}

// w149DocHits scans the two doc trees the spec names (the design doc set
// under docs/design/polyforge-vnext and the shipped skills) for token.
func w149DocHits(t *testing.T, token string) []string {
	t.Helper()
	root := v1Root(t)
	var hits []string
	for _, dir := range []string{"docs/design/polyforge-vnext", "skills/polyforge"} {
		p := filepath.Join(root, filepath.FromSlash(dir))
		if _, err := os.Stat(p); err != nil {
			continue
		}
		hits = append(hits, w149Hits(t, p, token, w149MdKeep)...)
	}
	return hits
}

// w149ReadFile reads one repo-relative file.
func w149ReadFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(v1Root(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// C1 — the delivery ledger is removed end-to-end.
// ---------------------------------------------------------------------------

// TestW149C1ShipLedgerRemoved covers spec scenarios "Ledger endpoint and
// client method gone", "Daemon ship path no longer records" (the write
// face: no table to write) and the fresh-DDL half of "Table dropped in
// DDL and upgrade SQL".
func TestW149C1ShipLedgerRemoved(t *testing.T) {
	const project = "w149-c1"
	inst := w149Setup(t, project)
	id := w149Create(t, inst, project, "C1 ledger ticket", strPtr(w149Bundle))

	status, body := w149Raw(t, inst, "POST", "/api/v1/work-items/"+id+"/ship-ledger",
		inst.adminKey, map[string]any{
			"branch": "polyforge/" + id,
			"repos": []map[string]any{
				{"repo": "demo-src", "branch_top_sha": strings.Repeat("a", 40)},
			},
		}, nil)
	if status != http.StatusNotFound {
		t.Errorf("POST .../ship-ledger answered %d (%.200s), want 404: the ledger route must be unregistered", status, body)
	}

	db := w149OpenDB(t, inst)
	if w149TableExists(t, db, "wi_ship_ledger") {
		t.Errorf("fresh DDL still creates wi_ship_ledger: delivery truth is the git branch")
	}

	w149AssertCodeClean(t, "C1",
		"wi_ship_ledger", "ship-ledger", "ShipLedger", "DAEMON-514", "ErrDaemonShipLedger")
}

// ---------------------------------------------------------------------------
// C9 — the terminate-blocked entry is removed whole-chain.
// ---------------------------------------------------------------------------

// TestW149C9TerminateBlockedRemoved covers "Aihub refuses no blocked
// terminations" plus the kept recovery doors (continue / abandon).
func TestW149C9TerminateBlockedRemoved(t *testing.T) {
	const project = "w149-c9"
	inst := w149Setup(t, project)

	a := w149Create(t, inst, project, "C9 parked ticket a", strPtr(w149Bundle))
	w149Block(t, inst, a)

	status, body := w149Raw(t, inst, "POST", "/api/v1/work-items/"+a+"/terminate-blocked",
		inst.adminKey, map[string]any{}, nil)
	if status != http.StatusNotFound {
		t.Errorf("POST .../terminate-blocked answered %d (%.200s), want 404: the route must be gone", status, body)
	}
	if st := inst.wiStatus(t, a); st != api.StatusBlocked {
		t.Errorf("the blocked work item is %s after the terminate-blocked attempt, want blocked", st)
	}

	// Kept face: continue (resume-blocked) recovers the parked WI.
	if _, err := inst.client.ResumeBlockedWorkItem(a); err != nil {
		t.Fatalf("continue (resume-blocked) of %s: %v", a, err)
	}
	if st := inst.wiStatus(t, a); st != api.StatusQueued {
		t.Errorf("after continue, %s is %s, want queued", a, st)
	}

	// Kept face: abandon (cancel) recovers a second parked WI.
	b := w149Create(t, inst, project, "C9 parked ticket b", strPtr(w149Bundle))
	w149Block(t, inst, b)
	if _, err := inst.client.CancelWorkItem(b); err != nil {
		t.Fatalf("abandon (cancel) of %s: %v", b, err)
	}
	if st := inst.wiStatus(t, b); st != api.StatusCanceled {
		t.Errorf("after abandon, %s is %s, want canceled", b, st)
	}

	w149AssertCodeClean(t, "C9", "terminate-blocked", "TerminateBlocked")
}

// TestW149C9CLIWrapFailBlockedBranchRemoved covers "CLI blocked branch
// gone": `pf wrap --fail` must no longer terminate a parked WI at the
// hub; the WI survives blocked and is recovered via cancel/resume only.
func TestW149C9CLIWrapFailBlockedBranchRemoved(t *testing.T) {
	const project = "w149-c9-cli"
	inst := w149Setup(t, project)

	id := w149Create(t, inst, project, "C9 CLI parked ticket", strPtr(w149Bundle))
	w149Block(t, inst, id)

	out, code := w149Pf(t, inst, "wrap", id, "--fail")
	if st := inst.wiStatus(t, id); st != api.StatusBlocked {
		t.Errorf("`pf wrap --fail` moved the blocked work item to %s (exit %d, output %q): the blocked-specific terminate branch must be deleted", st, code, out)
	}
	if code == 0 {
		t.Errorf("`pf wrap --fail` on a blocked work item exited 0 (output %q): with the blocked branch gone it has no hub door to land on (no daemon job either)", out)
	}

	// Kept face: abandon still works from the CLI.
	out, code = w149Pf(t, inst, "cancel", id)
	if code != 0 {
		t.Fatalf("`pf cancel` of the parked work item: exit %d (output %q)", code, out)
	}
	if st := inst.wiStatus(t, id); st != api.StatusCanceled {
		t.Errorf("after `pf cancel`, %s is %s, want canceled", id, st)
	}
}

// ---------------------------------------------------------------------------
// C12 — session_owner is removed.
// ---------------------------------------------------------------------------

// TestW149C12SessionOwnerRemoved covers "Sessions carry no owner field".
func TestW149C12SessionOwnerRemoved(t *testing.T) {
	const project = "w149-c12"
	inst := w149Setup(t, project)

	id := w149Create(t, inst, project, "C12 session ticket", strPtr(w149Bundle))
	w149Claim(t, inst, id)

	view := w149View(t, inst, id)
	if w149Has(view, "session_owner") {
		t.Errorf("the work item view still serves session_owner=%v", view["session_owner"])
	}
	// Kept face: the rest of the public session audit block rides on.
	if w149Str(view, "session_fingerprint") == "" {
		t.Errorf("session_fingerprint missing from the view of a held work item: only the owner field is deleted")
	}
	if got := w149Str(view, "session_machine"); got != "m-e2e-probe" {
		t.Errorf("session_machine = %q, want the claiming machine: only the owner field is deleted", got)
	}

	// The collection faces must not carry it either.
	rawQueue, err := inst.client.ReadyQueueRaw(project)
	if err != nil {
		t.Fatalf("ready queue: %v", err)
	}
	if strings.Contains(string(rawQueue), "session_owner") {
		t.Errorf("the ready-queue payload still carries session_owner: %.400s", rawQueue)
	}
	rawList, err := inst.client.ListWorkItemsRaw(project, "", 0)
	if err != nil {
		t.Fatalf("list work items: %v", err)
	}
	if strings.Contains(string(rawList), "session_owner") {
		t.Errorf("the work-item list payload still carries session_owner: %.400s", rawList)
	}

	db := w149OpenDB(t, inst)
	if w149ColumnExists(t, db, "work_items", "session_owner") {
		t.Errorf("fresh DDL still creates work_items.session_owner")
	}

	w149AssertCodeClean(t, "C12", "session_owner", "SessionOwner")
}

// ---------------------------------------------------------------------------
// B9 — the bundle digest chain is removed, bundle_name is retained.
// ---------------------------------------------------------------------------

// TestW149B9BundleDigestRemovedNameKept covers "Create binds a name with
// no digest", "PATCH carries only the bundle_name half", "PATCH with
// bundle_digest is rejected" and (through the code face) "Runtime
// materialize does no digest verification".
func TestW149B9BundleDigestRemovedNameKept(t *testing.T) {
	const project = "w149-b9"
	inst := w149Setup(t, project)

	// Create with a name only: no stand-in digest is ever derived.
	id := w149Create(t, inst, project, "B9 named ticket", strPtr(w149Bundle))
	view := w149View(t, inst, id)
	if w149Has(view, "bundle_digest") {
		t.Errorf("the create/read face still serves bundle_digest=%v (the sha256(\"bundle-name:\"+name) stand-in must be gone)", view["bundle_digest"])
	}
	if got := w149Str(view, "bundle_name"); got != w149Bundle {
		t.Errorf("bundle_name = %q, want %q: the name half is retained", got, w149Bundle)
	}

	// Create with an explicit digest in the payload: the field does not
	// exist, so it is simply not persisted and never served back.
	var raw json.RawMessage
	if err := inst.client.Do("POST", "/api/v1/work-items", map[string]any{
		"project":       project,
		"goal":          "B9 ticket with a digest field in the payload",
		"bundle_name":   w149Bundle,
		"bundle_digest": strings.Repeat("b", 64),
	}, &raw); err != nil {
		t.Fatalf("create with a digest field in the payload: %v", err)
	}
	var created map[string]any
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("create answer: %v (%s)", err, raw)
	}
	if w149Has(created, "bundle_digest") {
		t.Errorf("the create answer still carries bundle_digest=%v", created["bundle_digest"])
	}
	if got := w149Str(created, "bundle_name"); got != w149Bundle {
		t.Errorf("bundle_name = %q after a create carrying a digest, want %q", got, w149Bundle)
	}

	// PATCH carrying bundle_digest is refused CORE-400-class, and binds
	// nothing (the digest half of parseBundlePatch and its whitelist
	// entry are deleted; the key is simply unpatchable).
	err := w149Patch(inst, id, map[string]any{
		"bundle_name":   "other-bundle",
		"bundle_digest": strings.Repeat("c", 64),
	})
	if code := w149Code(err); code != api.ErrCoreBadInput {
		t.Errorf("PATCH with bundle_digest answered %v (code %q), want %s", err, code, api.ErrCoreBadInput)
	}
	if got := w149Str(w149View(t, inst, id), "bundle_name"); got != w149Bundle {
		t.Errorf("the refused PATCH changed bundle_name to %q, want the original %q", got, w149Bundle)
	}

	// Kept face: the R2 window's name half — bind, rename, null-unbind.
	reg := w149Create(t, inst, project, "B9 registration ticket", nil)
	if err := w149Patch(inst, reg, map[string]any{"bundle_name": w149Bundle}); err != nil {
		t.Fatalf("PATCH bind bundle_name: %v", err)
	}
	if got := w149Str(w149View(t, inst, reg), "bundle_name"); got != w149Bundle {
		t.Errorf("after the bind PATCH, bundle_name = %q, want %q", got, w149Bundle)
	}
	if err := w149Patch(inst, reg, map[string]any{"bundle_name": "renamed-bundle"}); err != nil {
		t.Fatalf("PATCH rename bundle_name: %v", err)
	}
	if got := w149Str(w149View(t, inst, reg), "bundle_name"); got != "renamed-bundle" {
		t.Errorf("after the rename PATCH, bundle_name = %q, want renamed-bundle", got)
	}
	if err := w149Patch(inst, reg, map[string]any{"bundle_name": nil}); err != nil {
		t.Fatalf("PATCH null-unbind bundle_name: %v", err)
	}
	if v := w149View(t, inst, reg)["bundle_name"]; v != nil {
		t.Errorf("after the null-unbind PATCH, bundle_name = %v, want null", v)
	}

	db := w149OpenDB(t, inst)
	if w149ColumnExists(t, db, "work_items", "bundle_digest") {
		t.Errorf("fresh DDL still creates work_items.bundle_digest")
	}
	if !w149ColumnExists(t, db, "work_items", "bundle_name") {
		t.Errorf("work_items.bundle_name is gone: only the digest half is deleted")
	}

	w149AssertCodeClean(t, "B9", "bundle_digest", "BundleDigest")
}

// ---------------------------------------------------------------------------
// Dead tables, columns, constants, interfaces.
// ---------------------------------------------------------------------------

// TestW149DeadSchemaAbsentFromFreshDB covers "Fresh schema matches
// reality".
func TestW149DeadSchemaAbsentFromFreshDB(t *testing.T) {
	const project = "w149-ddl"
	inst := w149Setup(t, project)
	// A real row in every live table the scenario talks about.
	_ = w149Create(t, inst, project, "DDL probe ticket", strPtr(w149Bundle))

	db := w149OpenDB(t, inst)
	for _, table := range []string{"core_meta", "wi_ship_ledger"} {
		if w149TableExists(t, db, table) {
			t.Errorf("fresh DDL still creates the dead table %s", table)
		}
	}
	dead := []struct{ table, column string }{
		{"projects", "scenario"},
		{"projects", "identifier_hash"},
		{"work_items", "workflow_budget_baseline"},
		{"work_items", "bundle_digest"},
		{"work_items", "session_owner"},
	}
	for _, d := range dead {
		if w149ColumnExists(t, db, d.table, d.column) {
			t.Errorf("fresh DDL still creates the dead column %s.%s", d.table, d.column)
		}
	}
	// Kept face: the live columns of the same tables stay.
	for _, k := range []struct{ table, column string }{
		{"projects", "repos"},
		{"work_items", "bundle_name"},
		{"work_items", "waiting_on"},
		{"work_items", "session_fingerprint"},
	} {
		if !w149ColumnExists(t, db, k.table, k.column) {
			t.Errorf("live column %s.%s disappeared", k.table, k.column)
		}
	}

	w149AssertCodeClean(t, "dead schema",
		"core_meta", "identifier_hash", "workflow_budget_baseline")
}

// TestW149DeadEndpointsHeadersConstantsRemoved covers "Dead
// endpoints/headers/constants gone".
func TestW149DeadEndpointsHeadersConstantsRemoved(t *testing.T) {
	const project = "w149-dead"
	inst := w149Setup(t, project)

	// 1. The rebuild-index endpoint is unregistered.
	status, body := w149Raw(t, inst, "POST", "/api/v1/memories/rebuild-index",
		inst.adminKey, map[string]any{"project": project}, nil)
	if status != http.StatusNotFound {
		t.Errorf("POST /api/v1/memories/rebuild-index answered %d (%.200s), want 404", status, body)
	}

	// 2. X-API-Key no longer authenticates (header removed from parsing).
	status, body = w149Raw(t, inst, "GET", "/api/v1/projects/"+project, "", nil,
		map[string]string{"X-API-Key": inst.adminKey})
	if status != http.StatusUnauthorized {
		t.Errorf("a request authenticated ONLY by X-API-Key answered %d (%.200s), want 401", status, body)
	}
	// Kept face: the Bearer header is the one and only key face.
	status, body = w149Raw(t, inst, "GET", "/api/v1/projects/"+project, inst.adminKey, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("Bearer-authenticated GET project answered %d (%.200s), want 200", status, body)
	}

	// 3. The dead error-code constants are gone from the registry.
	registry := filepath.Join(v1Root(t), filepath.FromSlash("internal/common/api"))
	for _, tok := range []string{
		"ErrCoreLockHeld", "ErrMemUpstream", "ErrMemUnavailable", "ErrDaemonShipLedger",
		"CORE-508", "MEM-502", "MEM-503", "DAEMON-514",
	} {
		if hits := w149Hits(t, registry, tok, w149GoKeep); len(hits) > 0 {
			t.Errorf("the error registry still carries %q: %v", tok, hits)
		}
	}
	// And no non-test code references the deleted identifiers.
	w149AssertCodeClean(t, "dead constants",
		"ErrCoreLockHeld", "ErrMemUpstream", "ErrMemUnavailable", "ErrDaemonShipLedger")

	// 4. The ProjectMember comment no longer advertises a phantom "owner"
	//    role (the DDL's CHECK knows viewer|writer|maintainer only).
	types := w149ReadFile(t, "internal/common/api/types.go")
	if strings.Contains(types, "owner|maintainer") {
		t.Errorf("internal/common/api/types.go still documents the nonexistent \"owner\" ProjectMember role")
	}

	// 5. The X-API-Key header name is gone from the parsing code.
	w149AssertCodeClean(t, "dead header", "X-API-Key")
}

// ---------------------------------------------------------------------------
// waiting_on — two dead branches deleted, `owner` and `dependency:<slug>`
// retained (the rewind-fix Requirement).
// ---------------------------------------------------------------------------

// TestW149WaitingOnValueDomain covers "waiting_on drops only the dead
// branches" and "Valid values accepted, dead values rejected".
func TestW149WaitingOnValueDomain(t *testing.T) {
	const project = "w149-wo"
	inst := w149Setup(t, project)
	id := w149Create(t, inst, project, "waiting_on domain ticket", strPtr(w149Bundle))

	// Dead branches: rejected, and nothing persists.
	for _, dead := range []string{"gate", "runtime:spawn", "runtime:anything"} {
		err := w149Patch(inst, id, map[string]any{"waiting_on": dead})
		if code := w149Code(err); code != api.ErrCoreBadInput {
			t.Errorf("PATCH waiting_on=%q answered %v (code %q), want %s: the branch is deleted from validWaitingOn",
				dead, err, code, api.ErrCoreBadInput)
		}
		if got := w149Str(w149View(t, inst, id), "waiting_on"); got != "" {
			t.Errorf("the refused waiting_on=%q persisted as %q", dead, got)
		}
	}

	// Live values: accepted and read back unchanged.
	for _, live := range []string{"owner", "dependency:w149-upstream"} {
		if err := w149Patch(inst, id, map[string]any{"waiting_on": live}); err != nil {
			t.Fatalf("PATCH waiting_on=%q was refused (%v): the value is live", live, err)
		}
		if got := w149Str(w149View(t, inst, id), "waiting_on"); got != live {
			t.Errorf("waiting_on reads back %q, want %q", got, live)
		}
	}

	// Clearing is still the way back.
	if err := w149Patch(inst, id, map[string]any{"waiting_on": nil}); err != nil {
		t.Fatalf("PATCH waiting_on=null: %v", err)
	}
	if got := w149Str(w149View(t, inst, id), "waiting_on"); got != "" {
		t.Errorf("after the null PATCH, waiting_on = %q, want empty", got)
	}

	// The CLI face writes the dependency value too (pf file edit
	// --waiting-on), per the spec's dependency-waiting scenario.
	out, code := w149Pf(t, inst, "file", "edit", id, "--waiting-on", "dependency:w149-cli")
	if code != 0 {
		t.Fatalf("`pf file edit --waiting-on dependency:...`: exit %d (output %q)", code, out)
	}
	if got := w149Str(w149View(t, inst, id), "waiting_on"); got != "dependency:w149-cli" {
		t.Errorf("after the CLI write, waiting_on = %q, want dependency:w149-cli", got)
	}
}

// TestW149WaitingOnOwnerWrittenByBlockedTransition covers "Server still
// writes owner on the frozen-ticket path" (green before AND after).
func TestW149WaitingOnOwnerWrittenByBlockedTransition(t *testing.T) {
	const project = "w149-wo-frozen"
	inst := w149Setup(t, project)
	id := w149Create(t, inst, project, "frozen ticket", strPtr(w149Bundle))

	w149Block(t, inst, id)

	view := w149View(t, inst, id)
	if got := w149Str(view, "status"); got != api.StatusBlocked {
		t.Fatalf("status = %q, want blocked", got)
	}
	if got := w149Str(view, "waiting_on"); got != "owner" {
		t.Errorf("after the blocked verdict, waiting_on = %q, want \"owner\": the frozen-ticket write is untouched by this change", got)
	}
}

// TestW149WaitingOnReadyQueueExclusion covers "Ready-queue filtering
// excludes both retained values" across BOTH queue segments (green
// before AND after: the filter is value-agnostic).
func TestW149WaitingOnReadyQueueExclusion(t *testing.T) {
	const project = "w149-wo-queue"
	inst := w149Setup(t, project)

	exec1 := w149Create(t, inst, project, "executable ticket", strPtr(w149Bundle))
	pend1 := w149Create(t, inst, project, "registration ticket", nil)

	q := w149Queue(t, inst, project)
	if !w149InSegment(q, "items", exec1) {
		t.Fatalf("the bundle-bound queued ticket is not in the executable items segment: %v", q["items"])
	}
	if !w149InSegment(q, "pending_bundles", pend1) {
		t.Fatalf("the childless registration ticket is not in the pending_bundles segment: %v", q["pending_bundles"])
	}

	// Both retained values exclude from both segments — tested in both
	// assignments so neither value is only ever seen in one segment.
	for _, pair := range [][2]string{
		{"owner", "dependency:w149-dep"},
		{"dependency:w149-dep", "owner"},
	} {
		if err := w149Patch(inst, exec1, map[string]any{"waiting_on": pair[0]}); err != nil {
			t.Fatalf("PATCH waiting_on=%q on the executable ticket: %v", pair[0], err)
		}
		if err := w149Patch(inst, pend1, map[string]any{"waiting_on": pair[1]}); err != nil {
			t.Fatalf("PATCH waiting_on=%q on the registration ticket: %v", pair[1], err)
		}
		q = w149Queue(t, inst, project)
		if w149InSegment(q, "items", exec1) {
			t.Errorf("waiting_on=%q did not exclude the ticket from the executable items segment", pair[0])
		}
		if w149InSegment(q, "pending_bundles", pend1) {
			t.Errorf("waiting_on=%q did not exclude the ticket from the pending_bundles segment", pair[1])
		}
	}

	// Clearing re-includes both.
	for _, id := range []string{exec1, pend1} {
		if err := w149Patch(inst, id, map[string]any{"waiting_on": nil}); err != nil {
			t.Fatalf("clear waiting_on on %s: %v", id, err)
		}
	}
	q = w149Queue(t, inst, project)
	if !w149InSegment(q, "items", exec1) {
		t.Errorf("clearing waiting_on did not re-include the ticket in the executable items segment")
	}
	if !w149InSegment(q, "pending_bundles", pend1) {
		t.Errorf("clearing waiting_on did not re-include the ticket in the pending_bundles segment")
	}
}

// ---------------------------------------------------------------------------
// D3 — the goal length limit counts characters (runes).
// ---------------------------------------------------------------------------

// TestW149GoalLimitCountedInRunes covers "500-character goal accepted,
// over-limit rejected" on both the create and the PATCH face.
func TestW149GoalLimitCountedInRunes(t *testing.T) {
	const project = "w149-d3"
	inst := w149Setup(t, project)

	g500 := strings.Repeat("界", 500) // 1500 bytes, 500 runes
	g501 := strings.Repeat("界", 501)

	if _, err := inst.client.CreateWorkItem(aihubclient.CreateWorkItemRequest{
		Project: project, Goal: g500, BundleName: strPtr(w149Bundle),
	}); err != nil {
		t.Errorf("a 500-CHARACTER (1500-byte) goal was refused on create: %v — the limit counts runes", err)
	}
	_, err := inst.client.CreateWorkItem(aihubclient.CreateWorkItemRequest{
		Project: project, Goal: g501, BundleName: strPtr(w149Bundle),
	})
	if code := w149Code(err); code != api.ErrCoreBadInput {
		t.Errorf("a 501-character goal answered %v (code %q) on create, want %s", err, code, api.ErrCoreBadInput)
	}

	id := w149Create(t, inst, project, "D3 patch ticket", strPtr(w149Bundle))
	if err := w149Patch(inst, id, map[string]any{"goal": g500}); err != nil {
		t.Errorf("a 500-character goal was refused on PATCH: %v — the limit counts runes", err)
	}
	err = w149Patch(inst, id, map[string]any{"goal": g501})
	if code := w149Code(err); code != api.ErrCoreBadInput {
		t.Errorf("a 501-character goal answered %v (code %q) on PATCH, want %s", err, code, api.ErrCoreBadInput)
	}
}

// ---------------------------------------------------------------------------
// D5 — owner is the reporter; the gate covers continue + abandon only.
// ---------------------------------------------------------------------------

// TestW149ReporterGateContinueAbandonOnly covers "Continue/abandon gate
// allows reporter, maintainer, admin" and "ForceEnd gate unchanged"
// (behavior pins: green before AND after).
func TestW149ReporterGateContinueAbandonOnly(t *testing.T) {
	const project = "w149-d5"
	inst := w149Setup(t, project)

	uid, key := w149MintKey(t, inst, "w149-reporter")
	if _, err := inst.client.PatchProject(project, aihubclient.ProjectPatchRequest{
		Members: &[]aihubclient.ProjectMember{{UserID: uid, Role: "writer"}},
	}); err != nil {
		t.Fatalf("add the reporter as a plain project writer: %v", err)
	}
	reporter := aihubclient.New(inst.url, key)

	// Continue: the reporter (neither maintainer nor admin) may resume
	// their own parked WI.
	a := w149CreateAs(t, reporter, project, "reporter ticket a", strPtr(w149Bundle))
	w149Block(t, inst, a)
	if _, err := reporter.ResumeBlockedWorkItem(a); err != nil {
		t.Errorf("the reporter was refused continue on their own work item: %v", err)
	}
	if st := inst.wiStatus(t, a); st != api.StatusQueued {
		t.Errorf("after the reporter's continue, %s is %s, want queued", a, st)
	}

	// Abandon: the reporter may cancel their own WI.
	b := w149CreateAs(t, reporter, project, "reporter ticket b", strPtr(w149Bundle))
	if _, err := reporter.CancelWorkItem(b); err != nil {
		t.Errorf("the reporter was refused abandon on their own work item: %v", err)
	}
	if st := inst.wiStatus(t, b); st != api.StatusCanceled {
		t.Errorf("after the reporter's abandon, %s is %s, want canceled", b, st)
	}

	// Maintainer and admin keep both doors (admin here; the gate's third
	// arm, maintainer, rides the same ownerMaintainerOrAdmin predicate).
	c := w149CreateAs(t, reporter, project, "reporter ticket c", strPtr(w149Bundle))
	w149Block(t, inst, c)
	if _, err := inst.client.ResumeBlockedWorkItem(c); err != nil {
		t.Errorf("admin was refused continue: %v", err)
	}

	// ForceEnd is NOT widened: the reporter is still refused.
	d := w149CreateAs(t, reporter, project, "reporter ticket d", strPtr(w149Bundle))
	w149Claim(t, inst, d)
	err := reporter.ForceEndSession(d, "reporter attempts a force-end")
	if err == nil {
		t.Fatalf("the reporter was ALLOWED to force-end a session: D5 widens continue/abandon only")
	}
	if code := w149Code(err); code != api.ErrAuthForbidden {
		t.Errorf("the reporter's force-end answered %v (code %q), want %s", err, code, api.ErrAuthForbidden)
	}
	if fp := w149Str(w149View(t, inst, d), "session_fingerprint"); fp == "" {
		t.Errorf("the refused force-end still cleared the session holder")
	}
	// Kept face: admin may force-end.
	if err := inst.client.ForceEndSession(d, "admin recovery"); err != nil {
		t.Errorf("admin was refused force-end: %v", err)
	}
}

// TestW149DocsOwnerIsReporter covers "Documentation matches the code".
func TestW149DocsOwnerIsReporter(t *testing.T) {
	const docPath = "docs/design/polyforge-vnext/v2-converge.md"
	doc := w149ReadFile(t, docPath)

	start := strings.Index(doc, "\n## 4.")
	if start < 0 {
		t.Fatalf("%s: section \"## 4.\" (身份模型) not found", docPath)
	}
	rest := doc[start+1:]
	end := strings.Index(rest[1:], "\n## ")
	section := rest
	if end >= 0 {
		section = rest[:end+1]
	}

	for _, tok := range []string{"PF_USER_ID", "提交人", "提交者"} {
		if strings.Contains(section, tok) {
			t.Errorf("%s §4 still defines the owner by %q: the owner is the REPORTER (aihub has no trustworthy submitter identity)", docPath, tok)
		}
	}
	if !strings.Contains(section, "reporter") {
		t.Errorf("%s §4 does not name the reporter as the owner", docPath)
	}
}

// ---------------------------------------------------------------------------
// Docs / skills face: no references to the removed mechanisms.
// ---------------------------------------------------------------------------

// TestW149DocsAndSkillsDropRemovedMechanisms covers the docs half of
// "Docs and delivery notes updated".
func TestW149DocsAndSkillsDropRemovedMechanisms(t *testing.T) {
	for _, tok := range []string{
		"wi_ship_ledger", "ship-ledger", "DAEMON-514",
		"terminate-blocked",
		"session_owner",
		"bundle_digest",
		"core_meta", "identifier_hash", "workflow_budget_baseline",
		"CORE-508", "memories/rebuild-index", "X-API-Key",
	} {
		if hits := w149DocHits(t, tok); len(hits) > 0 {
			t.Errorf("docs/design/polyforge-vnext or skills/polyforge still reference the removed mechanism %q: %v", tok, hits)
		}
	}
}

// TestW149DeliveryNotesPerDeletion covers the delivery-notes half of
// "Docs and delivery notes updated": one document lists every audit
// item with its line counts.
func TestW149DeliveryNotesPerDeletion(t *testing.T) {
	root := v1Root(t)
	docsDir := filepath.Join(root, "docs")
	items := []string{"C1", "C9", "C12", "B9", "D3", "D5"}

	var found string
	err := filepath.WalkDir(docsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !w149MdKeep(path) || found != "" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		body := string(b)
		if !strings.Contains(body, "149") {
			return nil
		}
		for _, it := range items {
			if !strings.Contains(body, it) {
				return nil
			}
		}
		lower := strings.ToLower(body)
		if !strings.Contains(lower, "line") && !strings.Contains(body, "行") {
			return nil
		}
		found = path
		return nil
	})
	if err != nil {
		t.Fatalf("scan docs for the delivery notes: %v", err)
	}
	if found == "" {
		t.Errorf("no document under docs/ carries the per-deletion delivery notes (every audit item %v plus line counts)", items)
	}
}

// ---------------------------------------------------------------------------
// Upgrade path: documented manual SQL, applied to old-schema DBs.
// ---------------------------------------------------------------------------

// w149SQLBlocks returns every fenced ```sql block of a markdown body.
func w149SQLBlocks(body string) []string {
	var out []string
	rest := body
	for {
		i := strings.Index(rest, "```sql")
		if i < 0 {
			return out
		}
		rest = rest[i+len("```sql"):]
		j := strings.Index(rest, "```")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		rest = rest[j+3:]
	}
}

// w149UpgradeDoc finds the delivery/upgrade document by CONTENT (spec
// Assumption 4 leaves the exact file and SQL text to plan/code): the
// markdown under docs/ whose fenced ```sql blocks drop the delivery
// ledger. Returns (path, all sql blocks of that doc, doc body).
func w149UpgradeDoc(t *testing.T) (string, []string, string) {
	t.Helper()
	docsDir := filepath.Join(v1Root(t), "docs")
	var path, body string
	var blocks []string
	err := filepath.WalkDir(docsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !w149MdKeep(p) || path != "" {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		bl := w149SQLBlocks(string(b))
		for _, blk := range bl {
			up := strings.ToUpper(blk)
			if strings.Contains(up, "DROP") && strings.Contains(blk, "wi_ship_ledger") {
				path, blocks, body = p, bl, string(b)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan docs for the upgrade SQL: %v", err)
	}
	if path == "" {
		t.Fatalf("no document under docs/ carries the manual upgrade SQL (a fenced ```sql block dropping wi_ship_ledger): the upgrade path must be documented, backup first, no auto migration")
	}
	return path, blocks, body
}

// w149Statements splits a SQL block into executable statements,
// dropping `--` comments and blank statements.
func w149Statements(block string) []string {
	var clean []string
	for _, line := range strings.Split(block, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		clean = append(clean, line)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(clean, "\n"), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// w149AddColumn re-adds one old-schema column when the tree under test
// no longer creates it: the fixture must be an OLD-schema database in
// BOTH trees (pre-change it already is; post-change it is synthesized).
func w149AddColumn(t *testing.T, db *sql.DB, table, column, decl string) {
	t.Helper()
	if w149ColumnExists(t, db, table, column) {
		return
	}
	if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl)); err != nil {
		t.Fatalf("fixture: add old column %s.%s: %v", table, column, err)
	}
}

// w149OldSchema turns the install's database into an OLD-schema one:
// the dead tables and columns exactly as the pre-change DDL created
// them. populate fills every affected table/column with rows.
func w149OldSchema(t *testing.T, db *sql.DB, populate bool, wi string) {
	t.Helper()
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS core_meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS wi_ship_ledger (
			work_item_id   TEXT NOT NULL REFERENCES work_items(id),
			repo           TEXT NOT NULL,
			branch         TEXT NOT NULL DEFAULT '',
			branch_top_sha TEXT NOT NULL,
			pushed_at      TEXT NOT NULL,
			PRIMARY KEY (work_item_id, repo)
		)`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture: old table: %v", err)
		}
	}
	w149AddColumn(t, db, "projects", "scenario", "TEXT")
	w149AddColumn(t, db, "projects", "identifier_hash", "TEXT NOT NULL DEFAULT ''")
	w149AddColumn(t, db, "work_items", "bundle_digest", "TEXT")
	w149AddColumn(t, db, "work_items", "workflow_budget_baseline", "TEXT")
	w149AddColumn(t, db, "work_items", "session_owner", "TEXT")
	if !populate {
		return
	}
	rows := []struct {
		stmt string
		args []any
	}{
		{`INSERT OR REPLACE INTO core_meta(key,value) VALUES('schema_version','1')`, nil},
		{`UPDATE projects SET scenario='legacy', identifier_hash='deadbeef'`, nil},
		{`UPDATE work_items SET bundle_digest=?1, workflow_budget_baseline='{}', session_owner='m-old' WHERE id=?2`,
			[]any{strings.Repeat("d", 64), wi}},
		{`INSERT OR REPLACE INTO wi_ship_ledger(work_item_id,repo,branch,branch_top_sha,pushed_at)
			VALUES(?1,'demo-src','polyforge/old',?2,'2026-01-01T00:00:00Z')`,
			[]any{wi, strings.Repeat("e", 40)}},
	}
	for _, r := range rows {
		if _, err := db.Exec(r.stmt, r.args...); err != nil {
			t.Fatalf("fixture: populate old schema (%s): %v", r.stmt, err)
		}
	}
}

// w149ApplyUpgrade applies the mandatory (DROP-carrying) blocks of the
// upgrade doc to db; the optional waiting_on cleanup UPDATE is exercised
// by its own scenario.
func w149ApplyUpgrade(t *testing.T, db *sql.DB, blocks []string) {
	t.Helper()
	applied := 0
	for _, blk := range blocks {
		if !strings.Contains(strings.ToUpper(blk), "DROP") {
			continue
		}
		for _, stmt := range w149Statements(blk) {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("upgrade statement failed on the old-schema DB: %v\n%s", err, stmt)
			}
			applied++
		}
	}
	if applied == 0 {
		t.Fatalf("the upgrade doc carries no applicable DROP statements")
	}
}

// w149AssertDeadGone asserts every dead table/column is absent.
func w149AssertDeadGone(t *testing.T, db *sql.DB, when string) {
	t.Helper()
	for _, table := range []string{"core_meta", "wi_ship_ledger"} {
		if w149TableExists(t, db, table) {
			t.Errorf("%s: dead table %s still present", when, table)
		}
	}
	for _, d := range []struct{ table, column string }{
		{"projects", "scenario"},
		{"projects", "identifier_hash"},
		{"work_items", "bundle_digest"},
		{"work_items", "workflow_budget_baseline"},
		{"work_items", "session_owner"},
	} {
		if w149ColumnExists(t, db, d.table, d.column) {
			t.Errorf("%s: dead column %s.%s still present", when, d.table, d.column)
		}
	}
}

// w149UpgradeRun is the shared body of the two upgrade scenarios.
func w149UpgradeRun(t *testing.T, project string, populate bool) {
	t.Helper()
	inst := w149Setup(t, project)

	// The upgrade doc must exist, must carry the SQL, and must tell the
	// operator to back up first.
	docPath, blocks, body := w149UpgradeDoc(t)
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "backup") && !strings.Contains(body, "备份") {
		t.Errorf("%s documents the upgrade SQL but never tells the operator to back up first", docPath)
	}

	var wi string
	if populate {
		wi = w149Create(t, inst, project, "upgrade fixture ticket", strPtr(w149Bundle))
		w149Claim(t, inst, wi)
		second := w149Create(t, inst, project, "upgrade fixture ticket 2", nil)
		if err := w149Patch(inst, second, map[string]any{"waiting_on": "dependency:w149-keep"}); err != nil {
			t.Fatalf("seed a dependency-waiting row: %v", err)
		}
	}

	inst.stopAihub()
	db := w149OpenDB(t, inst)
	w149OldSchema(t, db, populate, wi)
	w149ApplyUpgrade(t, db, blocks)
	w149AssertDeadGone(t, db, "after the documented upgrade SQL")
	if populate {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&n); err != nil {
			t.Fatalf("count work items after the upgrade: %v", err)
		}
		if n < 2 {
			t.Errorf("the upgrade lost rows: %d work items left, want the 2 seeded", n)
		}
		var kept string
		if err := db.QueryRow(`SELECT COALESCE(waiting_on,'') FROM work_items WHERE waiting_on LIKE 'dependency:%'`).Scan(&kept); err != nil {
			t.Errorf("the dependency-waiting row did not survive the upgrade: %v", err)
		} else if kept != "dependency:w149-keep" {
			t.Errorf("the dependency-waiting row reads %q after the upgrade, want dependency:w149-keep", kept)
		}
	}
	db.Close()

	// The server starts on the upgraded database and serves normally...
	inst.restartAihub()
	if _, err := inst.client.ReadyQueueRaw(project); err != nil {
		t.Fatalf("ready queue after the upgrade: %v", err)
	}
	fresh := w149Create(t, inst, project, "post-upgrade ticket", strPtr(w149Bundle))
	if st := inst.wiStatus(t, fresh); st != api.StatusQueued {
		t.Errorf("post-upgrade work item is %s, want queued", st)
	}
	// ...and `Open` runs NO migration: nothing recreates the dead shapes.
	db2 := w149OpenDB(t, inst)
	w149AssertDeadGone(t, db2, "after restarting the server on the upgraded DB (no auto migration at Open)")
}

// TestW149UpgradeOldEmptyDB covers "Old empty DB upgraded".
func TestW149UpgradeOldEmptyDB(t *testing.T) { w149UpgradeRun(t, "w149-upgrade-empty", false) }

// TestW149UpgradeOldPopulatedDB covers "Old populated DB upgraded".
func TestW149UpgradeOldPopulatedDB(t *testing.T) { w149UpgradeRun(t, "w149-upgrade-full", true) }

// TestW149WaitingOnCleanupSQLBounded covers the bounded-cleanup half of
// the waiting_on Requirement (spec Assumption 2): the OPTIONAL cleanup
// UPDATE must match ONLY waiting_on='gate' / 'runtime:%' rows — rows
// holding `owner` or `dependency:*` are live data.
func TestW149WaitingOnCleanupSQLBounded(t *testing.T) {
	const project = "w149-wo-cleanup"
	inst := w149Setup(t, project)
	docPath, blocks, _ := w149UpgradeDoc(t)

	var cleanup []string
	for _, blk := range blocks {
		if strings.Contains(strings.ToUpper(blk), "UPDATE") && strings.Contains(blk, "waiting_on") {
			cleanup = append(cleanup, blk)
		}
	}
	if len(cleanup) == 0 {
		t.Logf("%s carries no optional waiting_on cleanup UPDATE: the bounded-ness requirement is vacuous", docPath)
		return
	}

	// Four rows, one per value class.
	gate := w149Create(t, inst, project, "stale gate row", strPtr(w149Bundle))
	runtimeRow := w149Create(t, inst, project, "stale runtime row", strPtr(w149Bundle))
	owner := w149Create(t, inst, project, "live owner row", strPtr(w149Bundle))
	dep := w149Create(t, inst, project, "live dependency row", strPtr(w149Bundle))
	if err := w149Patch(inst, owner, map[string]any{"waiting_on": "owner"}); err != nil {
		t.Fatalf("seed waiting_on=owner: %v", err)
	}
	if err := w149Patch(inst, dep, map[string]any{"waiting_on": "dependency:w149-live"}); err != nil {
		t.Fatalf("seed waiting_on=dependency:...: %v", err)
	}

	inst.stopAihub()
	db := w149OpenDB(t, inst)
	// The stale values are pre-change leftovers: the post-change server
	// refuses to write them, so the fixture writes them directly.
	for id, val := range map[string]string{gate: "gate", runtimeRow: "runtime:spawn"} {
		if _, err := db.Exec(`UPDATE work_items SET waiting_on=?1 WHERE id=?2`, val, id); err != nil {
			t.Fatalf("seed stale waiting_on=%s: %v", val, err)
		}
	}
	for _, blk := range cleanup {
		for _, stmt := range w149Statements(blk) {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("cleanup statement failed: %v\n%s", err, stmt)
			}
		}
	}
	live := map[string]string{owner: "owner", dep: "dependency:w149-live"}
	for id, want := range live {
		var got string
		if err := db.QueryRow(`SELECT COALESCE(waiting_on,'') FROM work_items WHERE id=?1`, id).Scan(&got); err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		if got != want {
			t.Errorf("the optional cleanup UPDATE touched a LIVE row: %s holds %q, want %q (the WHERE clause must match only 'gate' / 'runtime:%%')", id, got, want)
		}
	}
	for _, id := range []string{gate, runtimeRow} {
		var got string
		if err := db.QueryRow(`SELECT COALESCE(waiting_on,'') FROM work_items WHERE id=?1`, id).Scan(&got); err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		if got == "gate" || strings.HasPrefix(got, "runtime:") {
			t.Errorf("the cleanup UPDATE left the stale value %q on %s", got, id)
		}
	}
}
