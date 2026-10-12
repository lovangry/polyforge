package test

// v1_s23_verify_session_test.go: the v1.5 §1.7 public holder read face —
// GET /api/v1/work-items/{id}/session — end to end over the REAL aihub binary and the REAL pf
// CLI. The spec's contract under test:
//
//   - the tri-state (unheld / held-paused / held-running) is three
//     ORDINARY HTTP 200 answers — an existing-but-unheld work item is
//     never a 404/409, so a recovery-decision script needs no error
//     handling to tell "free" from "taken";
//   - the response carries only the public audit fields (the m4
//     SessionPublic set: fingerprint, machine, owner, started_at,
//     clean_pause) — never the stored session_hash, never the secret,
//     in ANY state;
//   - authorization is the same read gate as GET /work-items/{id}
//     (requireRead after wiProject): a project VIEWER member reads, a
//     non-member stays hidden behind the ordinary WI-read refusal;
//   - the frozen §1.2 compare-only session-verify answers exactly as
//     before, and the contract doc carries the new §1.7.
//
// No daemon is involved: this is the direct-to-aihub operator surface,
// so the scenarios drive the aihub's own v1.5 session API (claim /
// session-end) to stage each holder state and then read the new face.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// s23HTTP is the raw wire helper for the holder face: one request with a
// Bearer API key, returning the status and the UNDECODED body — the
// never-hash invariant is pinned on the raw bytes, not a typed decode.
func s23HTTP(t *testing.T, method, url, key string, body map[string]any) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(raw)
}

// s23Holder is the decoded §1.7 holder answer. The spec pins the FIELD
// SET (work_item_id, status, the m4 public session fields, clean_pause)
// but leaves two wire spellings open: the fingerprint key ("fingerprint"
// vs the m4 block's "session_fingerprint") and the empty-case shape
// (flat-omitted fields vs a `session: null` nested block — the §1.2
// fingerprint-omission precedent). The decoder accepts both spellings
// and both shapes; Held is false exactly when no fingerprint is carried.
type s23Holder struct {
	WorkItemID  string
	Status      string
	Fingerprint string
	MachineID   string
	Owner       string
	StartedAt   string
	CleanPause  bool
	Held        bool
}

// s23Str returns the first present string among the keys.
func s23Str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

// s23DecodeHolder decodes a §1.7 answer (endpoint body or the verb's
// --json output — the same JSON) tolerantly of the two open spellings.
func s23DecodeHolder(t *testing.T, raw string) s23Holder {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("holder answer undecodable: %v (%s)", err, raw)
	}
	h := s23Holder{
		WorkItemID:  s23Str(m, "work_item_id"),
		Status:      s23Str(m, "status"),
		Fingerprint: s23Str(m, "fingerprint", "session_fingerprint"),
		MachineID:   s23Str(m, "machine_id", "session_machine"),
		Owner:       s23Str(m, "session_owner", "owner"),
		StartedAt:   s23Str(m, "session_started_at", "started_at"),
	}
	h.CleanPause, _ = m["clean_pause"].(bool)
	// The nested-block spelling: the holder fields may ride under a
	// `session` object instead of flat (the empty case may be
	// `session: null`; the fields then stay empty below).
	if sub, ok := m["session"].(map[string]any); ok {
		if h.Fingerprint == "" {
			h.Fingerprint = s23Str(sub, "fingerprint", "session_fingerprint")
		}
		if h.MachineID == "" {
			h.MachineID = s23Str(sub, "machine_id", "session_machine")
		}
		if h.Owner == "" {
			h.Owner = s23Str(sub, "session_owner", "owner")
		}
		if h.StartedAt == "" {
			h.StartedAt = s23Str(sub, "session_started_at", "started_at")
		}
		if cp, ok := sub["clean_pause"].(bool); ok && !h.CleanPause {
			h.CleanPause = cp
		}
	}
	h.Held = h.Fingerprint != ""
	return h
}

// s23NeverHash pins the never-hash invariant on the RAW body: no
// session_hash field, no stored hash value, no secret — in every state.
func s23NeverHash(t *testing.T, raw, secret, where string) {
	t.Helper()
	if strings.Contains(raw, secret) {
		t.Fatalf("%s: body leaks the session secret: %s", where, raw)
	}
	if strings.Contains(raw, api.SessionHashHex(secret)) {
		t.Fatalf("%s: body leaks the stored session hash: %s", where, raw)
	}
	if strings.Contains(raw, "session_hash") {
		t.Fatalf("%s: body carries a session_hash field: %s", where, raw)
	}
}

// s23Claim holds wi under secret via the §1.1 claim — the pre-existing
// v1.5 surface the holder read observes.
func s23Claim(t *testing.T, inst *v1Install, wi, secret, machine, owner string) {
	t.Helper()
	if _, err := inst.client.SessionClaim(api.SessionClaimRequest{
		WorkItemID:     wi,
		SessionID:      secret,
		MachineID:      machine,
		Owner:          owner,
		IdempotencyKey: "s23-" + secret[:8],
	}); err != nil {
		t.Fatalf("claim %s: %v", wi, err)
	}
}

// s23MintKey creates one user plus an API key for the auth matrix and
// returns (user id, key plaintext).
func s23MintKey(t *testing.T, inst *v1Install, name string) (string, string) {
	t.Helper()
	raw, err := inst.client.CreateUser(aihubclient.UserCreateRequest{
		DisplayName: name,
		Email:       name + "@s23.invalid", // core requires email for human users
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

// TestV1S23SessionHolderRead walks the §1.7 tri-state over the real wire
// — unheld/queued, held/running, held/paused, blocked, terminal after
// session-clear — asserting the ordinary-200 rule, the public field set,
// the never-hash invariant on the raw body, the read-gate auth matrix,
// and the untouched §1.2 compare-only verify.
func TestV1S23SessionHolderRead(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s23: public holder read over the wire", nil, nil)
	holder := inst.url + "/api/v1/work-items/" + wi + "/session"
	secret := "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"

	// Unheld — queued: an ordinary 200 (never 404/409 for an existing
	// work item) carrying work_item_id + status and NO holder fields.
	code, raw := s23HTTP(t, "GET", holder, inst.adminKey, nil)
	if code != 200 {
		t.Fatalf("unheld/queued holder: HTTP %d (%s)", code, raw)
	}
	h := s23DecodeHolder(t, raw)
	if h.WorkItemID != wi || h.Status != "queued" || h.Held || h.CleanPause ||
		h.MachineID != "" || h.Owner != "" || h.StartedAt != "" {
		t.Fatalf("unheld/queued holder wrong: %+v (%s)", h, raw)
	}

	// Held + running: claim, then the answer names the holder's public
	// fields — fingerprint, machine, owner, started_at, clean_pause.
	s23Claim(t, inst, wi, secret, "m-e2e-hold", "s23-op")
	code, raw = s23HTTP(t, "GET", holder, inst.adminKey, nil)
	if code != 200 {
		t.Fatalf("held/running holder: HTTP %d (%s)", code, raw)
	}
	h = s23DecodeHolder(t, raw)
	if h.WorkItemID != wi || h.Status != "running" || !h.Held ||
		h.Fingerprint != api.SessionFingerprint(secret) ||
		h.MachineID != "m-e2e-hold" || h.Owner != "s23-op" ||
		h.StartedAt == "" || h.CleanPause {
		t.Fatalf("held/running holder wrong: %+v (%s)", h, raw)
	}
	s23NeverHash(t, raw, secret, "held/running holder")

	// Held + paused: session-end's paused verdict KEEPS the session row
	// (§1.5 amend) — the retained holder fields plus the clean_pause
	// value, still a plain 200.
	if err := inst.client.SessionEnd(wi, api.SessionEndRequest{
		SessionID: secret, Status: "paused", CleanPause: true,
	}); err != nil {
		t.Fatalf("pause %s: %v", wi, err)
	}
	code, raw = s23HTTP(t, "GET", holder, inst.adminKey, nil)
	if code != 200 {
		t.Fatalf("held/paused holder: HTTP %d (%s)", code, raw)
	}
	h = s23DecodeHolder(t, raw)
	if h.Status != "paused" || !h.Held || !h.CleanPause ||
		h.Fingerprint != api.SessionFingerprint(secret) ||
		h.MachineID != "m-e2e-hold" || h.Owner != "s23-op" || h.StartedAt == "" {
		t.Fatalf("held/paused holder wrong: %+v (%s)", h, raw)
	}
	s23NeverHash(t, raw, secret, "held/paused holder")

	// Terminal after session-clear: session-end(wrapped) clears the row
	// per §1.5 — still an ordinary 200 with work_item_id + status and an
	// empty session block.
	if err := inst.client.SessionEnd(wi, api.SessionEndRequest{
		SessionID: secret, Status: "wrapped",
	}); err != nil {
		t.Fatalf("wrap %s: %v", wi, err)
	}
	code, raw = s23HTTP(t, "GET", holder, inst.adminKey, nil)
	if code != 200 {
		t.Fatalf("terminal holder: HTTP %d (%s)", code, raw)
	}
	h = s23DecodeHolder(t, raw)
	if h.WorkItemID != wi || h.Status != "wrapped" || h.Held || h.CleanPause ||
		h.MachineID != "" || h.Owner != "" || h.StartedAt != "" {
		t.Fatalf("terminal holder wrong: %+v (%s)", h, raw)
	}

	// Blocked is an unheld state too: session-end(blocked) clears the
	// session row (the bounded-fail shape), so the answer is the
	// ordinary 200 with status blocked and an empty block.
	wiBlocked := inst.newWI(t, "s23: blocked is an unheld answer", nil, nil)
	secretB := "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	s23Claim(t, inst, wiBlocked, secretB, "m-e2e-hold", "s23-op")
	if err := inst.client.SessionEnd(wiBlocked, api.SessionEndRequest{
		SessionID: secretB, Status: "blocked", Note: "s23 bounded fail",
	}); err != nil {
		t.Fatalf("block %s: %v", wiBlocked, err)
	}
	code, raw = s23HTTP(t, "GET",
		inst.url+"/api/v1/work-items/"+wiBlocked+"/session", inst.adminKey, nil)
	if code != 200 {
		t.Fatalf("blocked holder: HTTP %d (%s)", code, raw)
	}
	h = s23DecodeHolder(t, raw)
	if h.WorkItemID != wiBlocked || h.Status != "blocked" || h.Held || h.CleanPause ||
		h.MachineID != "" || h.Owner != "" || h.StartedAt != "" {
		t.Fatalf("blocked holder wrong: %+v (%s)", h, raw)
	}

	// Auth matrix — the same read gate as GET /work-items/{id}: a
	// project VIEWER member reads the holder block; a non-member is
	// refused exactly the way the ordinary WI read refuses them.
	wiView := inst.newWI(t, "s23: viewer reads the holder block", nil, nil)
	secretV := "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	s23Claim(t, inst, wiView, secretV, "m-e2e-hold", "s23-op")
	viewerID, viewerKey := s23MintKey(t, inst, "s23-viewer")
	if _, err := inst.client.PatchProject(v1Project, aihubclient.ProjectPatchRequest{
		Members: &[]aihubclient.ProjectMember{{UserID: viewerID, Role: "viewer"}},
	}); err != nil {
		t.Fatalf("add viewer member: %v", err)
	}
	code, raw = s23HTTP(t, "GET",
		inst.url+"/api/v1/work-items/"+wiView+"/session", viewerKey, nil)
	if code != 200 {
		t.Fatalf("viewer holder: HTTP %d (%s)", code, raw)
	}
	h = s23DecodeHolder(t, raw)
	if h.WorkItemID != wiView || h.Status != "running" || !h.Held ||
		h.Fingerprint != api.SessionFingerprint(secretV) ||
		h.MachineID != "m-e2e-hold" || h.Owner != "s23-op" {
		t.Fatalf("viewer holder wrong: %+v (%s)", h, raw)
	}
	s23NeverHash(t, raw, secretV, "viewer holder")

	// The non-member: whatever the ordinary WI read answers them (the
	// requireRead not-found/forbidden family), the holder face answers
	// identically — and never more.
	_, outsiderKey := s23MintKey(t, inst, "s23-outsider")
	wiCode, _ := s23HTTP(t, "GET", inst.url+"/api/v1/work-items/"+wiView, outsiderKey, nil)
	code, raw = s23HTTP(t, "GET",
		inst.url+"/api/v1/work-items/"+wiView+"/session", outsiderKey, nil)
	if code != wiCode || (code != 403 && code != 404) {
		t.Fatalf("non-member holder: session face answered %d, WI read %d (%s)",
			code, wiCode, raw)
	}
	// An unknown work item is a plain 404, like any read.
	if code, raw = s23HTTP(t, "GET",
		inst.url+"/api/v1/work-items/wi_nosuchthing/session", inst.adminKey, nil); code != 404 {
		t.Fatalf("unknown WI holder: HTTP %d (%s)", code, raw)
	}

	// §1.2 sanity — the frozen compare-only session-verify is untouched
	// by the new face: it still answers matches against the secret,
	// carries the current public fingerprint, and never echoes the
	// secret.
	verify := inst.url + "/api/v1/work-items/" + wiView + "/session-verify"
	code, raw = s23HTTP(t, "POST", verify, inst.adminKey, map[string]any{"session_id": secretV})
	if code != 200 {
		t.Fatalf("1.2 verify match: HTTP %d (%s)", code, raw)
	}
	var match api.SessionVerifyResponse
	if err := json.Unmarshal([]byte(raw), &match); err != nil {
		t.Fatalf("1.2 verify answer undecodable: %v (%s)", err, raw)
	}
	if !match.Matches || match.WorkItemID != wiView || match.Status != "running" ||
		match.CurrentFingerprint != api.SessionFingerprint(secretV) || match.CleanPause {
		t.Fatalf("1.2 verify match wrong: %+v (%s)", match, raw)
	}
	if strings.Contains(raw, secretV) {
		t.Fatalf("1.2 verify echoes the secret: %s", raw)
	}
	code, raw = s23HTTP(t, "POST", verify, inst.adminKey,
		map[string]any{"session_id": "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"})
	if code != 200 {
		t.Fatalf("1.2 verify mismatch: HTTP %d (%s)", code, raw)
	}
	var mismatch api.SessionVerifyResponse
	if err := json.Unmarshal([]byte(raw), &mismatch); err != nil {
		t.Fatalf("1.2 mismatch answer undecodable: %v (%s)", err, raw)
	}
	if mismatch.Matches || mismatch.CurrentFingerprint != api.SessionFingerprint(secretV) {
		t.Fatalf("1.2 verify mismatch wrong: %+v (%s)", mismatch, raw)
	}
}

// TestV1S23ContractSection pins the contract doc: §1.7 documents the new
// face — the endpoint, its public-only field set, its read authorization
// and the unheld-is-200 rule — while the frozen §1.2 session-verify
// section stays in place.
func TestV1S23ContractSection(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(v1Root(t), "docs", "design",
		"polyforge-vnext", "v1.5-api-contract.md"))
	if err != nil {
		t.Fatalf("contract doc: %v", err)
	}
	doc := string(raw)

	// §1.2 stays: the compare-only session-verify section is intact.
	if !strings.Contains(doc, "1.2") || !strings.Contains(doc, "session-verify") {
		t.Fatalf("contract doc lost the frozen §1.2 session-verify section")
	}

	// §1.7 exists; slice its section (heading to the next heading) and
	// check it carries the spec-mandated content.
	lines := strings.Split(doc, "\n")
	start, end := -1, len(lines)
	for i, ln := range lines {
		if strings.HasPrefix(ln, "#") {
			if start < 0 && strings.Contains(ln, "1.7") {
				start = i
			} else if start >= 0 {
				end = i
				break
			}
		}
	}
	if start < 0 {
		t.Fatalf("contract doc has no §1.7 section")
	}
	sec := strings.ToLower(strings.Join(lines[start:end], "\n"))
	for _, want := range []string{
		"/session",   // the endpoint path
		"machine_id", // the public-only field set
		"clean_pause",
		"200", // the tri-state-as-200 rule
	} {
		if !strings.Contains(sec, want) {
			t.Fatalf("contract §1.7 lost %q:\n%s", want, sec)
		}
	}
	// The never-hash rule and the read authorization are both stated.
	if !strings.Contains(sec, "hash") && !strings.Contains(sec, "secret") {
		t.Fatalf("contract §1.7 does not state the never-hash rule:\n%s", sec)
	}
	if !strings.Contains(sec, "viewer") && !strings.Contains(sec, "read") {
		t.Fatalf("contract §1.7 does not state the read authorization:\n%s", sec)
	}
}
