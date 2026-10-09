package server

// session_test.go — the v1.5 session endpoints over real HTTP: the five
// contract §1 routes, their typed refusals, the paused verdict on
// session-end (the M3 pause transition), and the m4 read surface (public
// session block on every WorkItem JSON; never a hash, never the secret).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/common/api"
)

// sessClaim performs a session claim over HTTP and returns the decoded
// envelope plus the raw body.
func sessClaim(t *testing.T, e *srvEnv, wiID, secret, key string) (int, []byte, api.SessionClaimResponse) {
	t.Helper()
	body := map[string]string{}
	if secret != "" {
		body["session_id"] = secret
	}
	if key != "" {
		body["idempotency_key"] = key
	}
	body["machine_id"] = "m1"
	var out api.SessionClaimResponse
	code, raw := e.do(t, "POST", "/api/v1/work-items/"+wiID+"/claim", e.key, body, &out)
	return code, raw, out
}

// errEnvelope decodes the typed error body.
func errEnvelope(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	var envl struct {
		Code  string          `json:"code"`
		Extra json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(raw, &envl); err != nil {
		t.Fatalf("bad error envelope: %s", raw)
	}
	var extra struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(envl.Extra, &extra)
	return envl.Code, extra.Reason
}

// createExecWIOverHTTP creates an execution ticket and returns its id.
func createExecWIOverHTTP(t *testing.T, e *srvEnv, goal string) string {
	t.Helper()
	var wi struct {
		ID string `json:"id"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]any{"project": "alpha", "goal": goal, "bundle_name": "demo-bundle"}, &wi); code != 201 || wi.ID == "" {
		t.Fatalf("create: %d %s", code, raw)
	}
	return wi.ID
}

// eventsOf reads the WI timeline over HTTP.
func eventsOf(t *testing.T, e *srvEnv, wiID string) []map[string]any {
	t.Helper()
	var wi struct {
		Attrs struct {
			Events []map[string]any `json:"events"`
		} `json:"attrs"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+wiID+"?include=events", e.key, nil, &wi); code != 200 {
		t.Fatalf("get events: %d %s", code, raw)
	}
	return wi.Attrs.Events
}

func TestSessionVerifyOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	id := createExecWIOverHTTP(t, e, "verify over http")
	secret := "21212121212121212121212121212121"

	// Unheld: matches=false, no current fingerprint, clean_pause=false.
	var resp api.SessionVerifyResponse
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-verify", e.key,
		map[string]string{"session_id": secret}, &resp); code != 200 {
		t.Fatalf("verify unheld: %d %s", code, raw)
	} else if resp.Matches || resp.CurrentFingerprint != "" || resp.Status != "queued" || resp.CleanPause {
		t.Fatalf("unheld verify wrong: %+v", resp)
	} else if strings.Contains(string(raw), secret) {
		t.Fatalf("verify echoes the secret: %s", raw)
	}
	// Claim, then verify both ways.
	if code, raw, _ := sessClaim(t, e, id, secret, "sv-1"); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-verify", e.key,
		map[string]string{"session_id": secret}, &resp); code != 200 || !resp.Matches ||
		resp.Status != "running" || resp.CurrentFingerprint != api.SessionFingerprint(secret) {
		t.Fatalf("verify match wrong: %d %+v", code, resp)
	} else if strings.Contains(string(raw), secret) {
		t.Fatalf("verify echoes the secret on match: %s", raw)
	}
	if code, _ := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-verify", e.key,
		map[string]string{"session_id": "33333333333333333333333333333333"}, &resp); code != 200 || resp.Matches ||
		resp.CurrentFingerprint != api.SessionFingerprint(secret) {
		t.Fatalf("verify mismatch wrong: %+v", resp)
	}
	// Unknown WI: 404; empty body: 400.
	if code, _ := e.do(t, "POST", "/api/v1/work-items/wi_nosuchthing/session-verify", e.key,
		map[string]string{"session_id": secret}, nil); code != 404 {
		t.Fatalf("unknown WI verify: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-verify", e.key,
		map[string]string{}, nil); code != 400 {
		t.Fatalf("empty verify body: %d", code)
	}
}

func TestSessionAttachOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	id := createExecWIOverHTTP(t, e, "attach over http")
	secret := "41414141414141414141414141414141"
	if code, raw, _ := sessClaim(t, e, id, secret, "sa-1"); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	// Pause via session-end's paused verdict (the merge amend; daemon
	// shutdown shape: clean_pause stays false).
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]any{"status": "paused", "session_id": secret, "clean_pause": false}, nil); code != 200 {
		t.Fatalf("session pause: %d %s", code, raw)
	}
	// Attach: back to running under the same session.
	var resp api.SessionClaimResponse
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-attach", e.key,
		map[string]string{"session_id": secret, "machine_id": "m1"}, &resp); code != 200 {
		t.Fatalf("attach: %d %s", code, raw)
	}
	if resp.Claim.SessionFingerprint != api.SessionFingerprint(secret) || resp.WorkItem.Status != "running" {
		t.Fatalf("attach response wrong: %+v", resp)
	}
	// Pause again with clean_pause=true (operator pause): attach refuses.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]any{"status": "paused", "session_id": secret, "clean_pause": true}, nil); code != 200 {
		t.Fatalf("operator pause: %d %s", code, raw)
	}
	var wi struct {
		CleanPause bool `json:"clean_pause"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+id, e.key, nil, &wi); code != 200 || !wi.CleanPause {
		t.Fatalf("clean_pause not visible on the WI: %d %s %+v", code, raw, wi)
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-attach", e.key,
		map[string]string{"session_id": secret}, nil); code != 409 {
		t.Fatalf("attach of clean-paused WI: %d %s", code, raw)
	} else if c, r := errEnvelope(t, raw); c != "CORE-409" || r != "NOT_ATTACHABLE" {
		t.Fatalf("wrong refusal: %s %s", c, r)
	}
	// A stale session cannot attach either.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-attach", e.key,
		map[string]string{"session_id": "42424242424242424242424242424242"}, nil); code != 403 {
		t.Fatalf("stale attach: %d %s", code, raw)
	} else if c, r := errEnvelope(t, raw); c != "CORE-403" || r != "STALE_SESSION" {
		t.Fatalf("wrong refusal: %s %s", c, r)
	}
}

func TestForceTakeoverOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	id := createExecWIOverHTTP(t, e, "takeover over http")
	oldSecret := "51515151515151515151515151515151"
	if code, raw, _ := sessClaim(t, e, id, oldSecret, "ft-1"); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	// A plain writer (non-member) cannot force-takeover.
	outsider, _ := e.admin.CreateUser("plainw", "pw@example.com", "writer", "human")
	okey, _ := e.admin.CreateAPIKey(outsider.ID, "pw", "")
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/force-takeover", okey.Plain,
		map[string]string{"session_id": "52525252525252525252525252525252", "reason": "x"}, nil); code != 403 {
		t.Fatalf("non-maintainer takeover: %d %s", code, raw)
	}
	// Empty reason: 400.
	if code, _ := e.do(t, "POST", "/api/v1/work-items/"+id+"/force-takeover", e.key,
		map[string]string{"session_id": "52525252525252525252525252525252"}, nil); code != 400 {
		t.Fatalf("empty reason accepted: %d", code)
	}
	// Admin takeover: the session swaps atomically.
	newSecret := "53535353535353535353535353535353"
	var resp api.SessionClaimResponse
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/force-takeover", e.key,
		map[string]string{"session_id": newSecret, "machine_id": "m2", "owner": "op2",
			"reason": "machine lost", "idempotency_key": "ft-2"}, &resp); code != 200 {
		t.Fatalf("takeover: %d %s", code, raw)
	}
	if resp.Claim.SessionFingerprint != api.SessionFingerprint(newSecret) || resp.WorkItem.Status != "running" {
		t.Fatalf("takeover response wrong: %+v", resp)
	}
	// The old secret no longer verifies; the new one does.
	var v api.SessionVerifyResponse
	e.do(t, "POST", "/api/v1/work-items/"+id+"/session-verify", e.key, map[string]string{"session_id": oldSecret}, &v)
	if v.Matches {
		t.Fatalf("old secret still matches after takeover")
	}
	e.do(t, "POST", "/api/v1/work-items/"+id+"/session-verify", e.key, map[string]string{"session_id": newSecret}, &v)
	if !v.Matches {
		t.Fatalf("new secret does not match after takeover")
	}
	// The session_superseded event carries the OLD fingerprint (m4).
	found := false
	for _, ev := range eventsOf(t, e, id) {
		if ev["type"] != "session_superseded" {
			continue
		}
		found = true
		payload, _ := ev["payload"].(map[string]any)
		if payload["old_session_fingerprint"] != api.SessionFingerprint(oldSecret) {
			t.Fatalf("superseded event lost the old fingerprint: %v", ev)
		}
	}
	if !found {
		t.Fatalf("session_superseded event missing")
	}
	// Idempotent takeover replay: same key, same session.
	var replay api.SessionClaimResponse
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/force-takeover", e.key,
		map[string]string{"session_id": "54545454545454545454545454545454",
			"reason": "retry", "idempotency_key": "ft-2"}, &replay); code != 200 ||
		replay.Claim.SessionFingerprint != resp.Claim.SessionFingerprint {
		t.Fatalf("takeover replay minted a new session: %d %s %+v", code, raw, replay.Claim)
	}
}

// TestForceTakeoverMachineSelfTakeover (#109): a MACHINE-credential
// caller — the heartbeat-registration shape every production daemon
// holds — with project membership (role writer) may force-takeover
// ONLY onto its own machine identity (the executor's self-service
// recovery). Onto another machine it is refused, and a human writer
// member is refused exactly as before the widening.
func TestForceTakeoverMachineSelfTakeover(t *testing.T) {
	e := newSrvEnv(t)
	id := createExecWIOverHTTP(t, e, "machine self-takeover over http")
	oldSecret := "57151515151515151515151515151515"
	if code, raw, _ := sessClaim(t, e, id, oldSecret, "mft-1"); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	// Register a machine through the real heartbeat face (creates the
	// kind='machine' user + key) and make it a project writer.
	var hb struct {
		UserID string `json:"user_id"`
		APIKey struct {
			Plain string `json:"plain"`
		} `json:"api_key"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/machines/heartbeat", e.key,
		map[string]any{"machine_id": "m-self", "live_wi_ids": []string{}}, &hb); code != 200 || hb.UserID == "" || hb.APIKey.Plain == "" {
		t.Fatalf("heartbeat registration: %d %s", code, raw)
	}
	if code, raw := e.do(t, "PATCH", "/api/v1/projects/alpha", e.key,
		map[string]any{"members": []map[string]string{{"user_id": hb.UserID, "role": "writer"}}}, nil); code != 200 {
		t.Fatalf("membership: %d %s", code, raw)
	}
	// Machine self-takeover onto its own machine: admitted (the #109 gate).
	selfSecret := "58151515151515151515151515151515"
	var resp api.SessionClaimResponse
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/force-takeover", hb.APIKey.Plain,
		map[string]string{"session_id": selfSecret, "machine_id": "m-self",
			"owner": "m-self", "reason": "holder machine lost", "idempotency_key": "mft-2"}, &resp); code != 200 {
		t.Fatalf("machine self-takeover: %d %s", code, raw)
	}
	if resp.Claim.SessionFingerprint != api.SessionFingerprint(selfSecret) || resp.Claim.MachineID != "m-self" {
		t.Fatalf("self-takeover response wrong: %+v", resp.Claim)
	}
	// A machine claiming onto ANOTHER machine: refused.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/force-takeover", hb.APIKey.Plain,
		map[string]string{"session_id": "59151515151515151515151515151515",
			"machine_id": "m-other", "reason": "x"}, nil); code != 403 {
		t.Fatalf("machine foreign-machine takeover: %d %s", code, raw)
	} else if c, _ := errEnvelope(t, raw); c != "AUTH-403" {
		t.Fatalf("wrong refusal class: %s", raw)
	}
	// A HUMAN writer member stays refused (the v1 human gate is intact).
	human, _ := e.admin.CreateUser("humanw", "hw@example.com", "writer", "human")
	hkey, _ := e.admin.CreateAPIKey(human.ID, "hw", "")
	if code, raw := e.do(t, "PATCH", "/api/v1/projects/alpha", e.key,
		map[string]any{"members": []map[string]string{{"user_id": human.ID, "role": "writer"}}}, nil); code != 200 {
		t.Fatalf("human membership: %d %s", code, raw)
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/force-takeover", hkey.Plain,
		map[string]string{"session_id": "5a151515151515151515151515151515",
			"machine_id": "m-self", "reason": "x"}, nil); code != 403 {
		t.Fatalf("human writer takeover: %d %s", code, raw)
	}
	// The self-takeover actually holds: the new secret verifies.
	var v api.SessionVerifyResponse
	e.do(t, "POST", "/api/v1/work-items/"+id+"/session-verify", e.key, map[string]string{"session_id": selfSecret}, &v)
	if !v.Matches {
		t.Fatalf("self-takeover secret does not verify")
	}
}

func TestSessionEndOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	id := createExecWIOverHTTP(t, e, "end over http")
	secret := "61616161616161616161616161616161"
	if code, raw, _ := sessClaim(t, e, id, secret, "se-1"); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	// Fence: a foreign secret answers CORE-403 STALE_SESSION.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]string{"session_id": "62626262626262626262626262626262", "status": "wrapped"}, nil); code != 403 {
		t.Fatalf("unfenced session-end: %d %s", code, raw)
	} else if c, r := errEnvelope(t, raw); c != "CORE-403" || r != "STALE_SESSION" {
		t.Fatalf("wrong refusal: %s %s", c, r)
	}
	// Bad status: 400 (paused is legal since the merge amend — use an
	// illegal one).
	if code, _ := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]string{"session_id": secret, "status": "queued"}, nil); code != 400 {
		t.Fatalf("bad status accepted: %d", code)
	}
	// Real end: wrapped, session fields cleared on the read surface.
	var ended struct {
		Status             string `json:"status"`
		SessionFingerprint string `json:"session_fingerprint"`
		ClosedAt           string `json:"closed_at"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]string{"session_id": secret, "status": "wrapped", "note": "done"}, &ended); code != 200 ||
		ended.Status != "wrapped" || ended.SessionFingerprint != "" || ended.ClosedAt == "" {
		t.Fatalf("session-end: %d %s %+v", code, raw, ended)
	}
}

func TestSessionClaimGatesOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	// Terminal: CORE-410.
	id := createExecWIOverHTTP(t, e, "gates terminal")
	if code, raw, _ := sessClaim(t, e, id, "71717171717171717171717171717171", "g1"); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]string{"session_id": "71717171717171717171717171717171", "status": "wrapped"}, nil)
	if code, raw, _ := sessClaim(t, e, id, "72727272727272727272727272727272", "g2"); code != 410 {
		t.Fatalf("terminal claim: %d %s", code, raw)
	} else if c, r := errEnvelope(t, raw); c != "CORE-410" || r != "TERMINAL_STATE" {
		t.Fatalf("wrong refusal: %s %s", c, r)
	}
	// Dependency-blocked: CORE-409 DEPENDENCY_BLOCKED.
	blocker := createExecWIOverHTTP(t, e, "gates blocker")
	blocked := createExecWIOverHTTP(t, e, "gates blocked")
	e.do(t, "POST", "/api/v1/work-items/"+blocked+"/dependencies", e.key,
		map[string]string{"blocking_wi_id": blocker, "kind": "blocks"}, nil)
	if code, raw, _ := sessClaim(t, e, blocked, "73737373737373737373737373737373", "g3"); code != 409 {
		t.Fatalf("dependency-blocked claim: %d %s", code, raw)
	} else if c, r := errEnvelope(t, raw); c != "CORE-409" || r != "DEPENDENCY_BLOCKED" {
		t.Fatalf("wrong refusal: %s %s", c, r)
	}
	// No bundle: CORE-409 NOT_CLAIMABLE.
	var reg struct {
		ID string `json:"id"`
	}
	e.do(t, "POST", "/api/v1/work-items", e.key, map[string]any{"project": "alpha", "goal": "gates registry"}, &reg)
	if code, raw, _ := sessClaim(t, e, reg.ID, "74747474747474747474747474747474", "g4"); code != 409 {
		t.Fatalf("registration claim: %d %s", code, raw)
	} else if c, r := errEnvelope(t, raw); c != "CORE-409" || r != "NOT_CLAIMABLE" {
		t.Fatalf("wrong refusal: %s %s", c, r)
	}
	// Missing session_id: CORE-400.
	if code, _ := e.do(t, "POST", "/api/v1/work-items/"+blocker+"/claim", e.key,
		map[string]string{"machine_id": "m1"}, nil); code != 400 {
		t.Fatalf("secretless claim accepted: %d", code)
	}
}

func TestReadyQueueCarriesSessionBlock(t *testing.T) {
	e := newSrvEnv(t)
	id := createExecWIOverHTTP(t, e, "queue session block")
	secret := "81818181818181818181818181818181"
	if code, raw, _ := sessClaim(t, e, id, secret, "q-1"); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	var q struct {
		Running []struct {
			ID                 string `json:"id"`
			SessionFingerprint string `json:"session_fingerprint"`
			SessionMachine     string `json:"session_machine"`
			CleanPause         bool   `json:"clean_pause"`
		} `json:"running"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/projects/alpha/ready-queue", e.key, nil, &q); code != 200 {
		t.Fatalf("ready queue: %d %s", code, raw)
	}
	if len(q.Running) != 1 || q.Running[0].ID != id ||
		q.Running[0].SessionFingerprint != api.SessionFingerprint(secret) ||
		q.Running[0].SessionMachine != "m1" || q.Running[0].CleanPause {
		t.Fatalf("running segment lost the session block: %+v", q.Running)
	}
}

// TestSessionHolderOverHTTP (§1.7): the public holder read — the
// tri-state is three ordinary 200 answers (unheld / held-paused /
// held-running), the response carries only the public audit fields
// (never the hash, never the secret), and the auth is the WI read gate
// (viewer member reads, non-member stays hidden).
func TestSessionHolderOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	id := createExecWIOverHTTP(t, e, "holder over http")
	secret := "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"

	// Unheld (queued): an ordinary 200 with the empty block (flat omitted
	// fields) — never 404/409 for an existing work item.
	var resp api.SessionHolderResponse
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+id+"/session", e.key, nil, &resp); code != 200 {
		t.Fatalf("unheld holder: %d %s", code, raw)
	} else if resp.WorkItemID != id || resp.Status != "queued" || resp.CleanPause ||
		resp.SessionFingerprint != "" || resp.MachineID != "" ||
		resp.SessionOwner != "" || resp.SessionStartedAt != "" {
		t.Fatalf("unheld holder wrong: %+v", resp)
	} else if strings.Contains(string(raw), "session_fingerprint") ||
		strings.Contains(string(raw), "machine_id") ||
		strings.Contains(string(raw), "session_owner") {
		t.Fatalf("unheld holder not flat-omitted: %s", raw)
	}

	// Held + running: claim with machine and owner, then read.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/claim", e.key,
		map[string]string{"session_id": secret, "machine_id": "m9", "owner": "hop",
			"idempotency_key": "h-1"}, nil); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+id+"/session", e.key, nil, &resp); code != 200 {
		t.Fatalf("held holder: %d %s", code, raw)
	} else if resp.WorkItemID != id || resp.Status != "running" ||
		resp.SessionFingerprint != api.SessionFingerprint(secret) || resp.MachineID != "m9" ||
		resp.SessionOwner != "hop" || resp.SessionStartedAt == "" || resp.CleanPause {
		t.Fatalf("held holder wrong: %+v", resp)
	}
	// The never-hash pin: the raw body carries neither the secret, nor
	// the stored hash, nor a session_hash field.
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+id+"/session", e.key, nil, nil); code != 200 {
		t.Fatalf("held holder raw: %d", code)
	} else if strings.Contains(string(raw), secret) ||
		strings.Contains(string(raw), api.SessionHashHex(secret)) ||
		strings.Contains(string(raw), "session_hash") {
		t.Fatalf("holder response leaks hash or secret: %s", raw)
	}

	// Held + paused (session-end "paused" keeps the row per §1.5 amend):
	// 200 with the retained holder fields plus clean_pause.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]any{"status": "paused", "session_id": secret, "clean_pause": true}, nil); code != 200 {
		t.Fatalf("operator pause: %d %s", code, raw)
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+id+"/session", e.key, nil, &resp); code != 200 {
		t.Fatalf("paused holder: %d %s", code, raw)
	} else if resp.Status != "paused" || !resp.CleanPause ||
		resp.SessionFingerprint != api.SessionFingerprint(secret) ||
		resp.MachineID != "m9" || resp.SessionOwner != "hop" || resp.SessionStartedAt == "" {
		t.Fatalf("paused holder wrong: %+v", resp)
	} else if strings.Contains(string(raw), secret) ||
		strings.Contains(string(raw), api.SessionHashHex(secret)) {
		t.Fatalf("paused holder leaks: %s", raw)
	}

	// Terminal after session-clear: session-end "wrapped" clears the
	// row — still an ordinary 200 with the empty block.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]string{"session_id": secret, "status": "wrapped"}, nil); code != 200 {
		t.Fatalf("session-end(wrapped): %d %s", code, raw)
	}
	var terminal api.SessionHolderResponse
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+id+"/session", e.key, nil, &terminal); code != 200 {
		t.Fatalf("terminal holder: %d %s", code, raw)
	} else if terminal.WorkItemID != id || terminal.Status != "wrapped" || terminal.CleanPause ||
		terminal.SessionFingerprint != "" || terminal.MachineID != "" ||
		terminal.SessionOwner != "" || terminal.SessionStartedAt != "" {
		t.Fatalf("terminal holder wrong: %+v", terminal)
	} else if strings.Contains(string(raw), secret) {
		t.Fatalf("terminal holder leaks the cleared secret: %s", raw)
	}

	// Auth matrix — the same read gate as GET /work-items/{id}:
	// a project VIEWER member reads; a non-member is hidden behind the
	// requireRead CORE-404; an unknown WI is CORE-404.
	viewer, _ := e.admin.CreateUser("holderview", "hv@example.com", "writer", "human")
	vkey, _ := e.admin.CreateAPIKey(viewer.ID, "hv", "")
	if _, err := e.admin.PatchProject("alpha", core.ProjectPatchRequest{
		Members: &[]api.ProjectMember{{UserID: viewer.ID, Role: "viewer"}},
	}); err != nil {
		t.Fatalf("add viewer member: %v", err)
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+id+"/session", vkey.Plain, nil, &resp); code != 200 {
		t.Fatalf("viewer member holder: %d %s", code, raw)
	}
	outsider, _ := e.admin.CreateUser("holderout", "ho@example.com", "writer", "human")
	okey, _ := e.admin.CreateAPIKey(outsider.ID, "ho", "")
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+id+"/session", okey.Plain, nil, nil); code != 404 {
		t.Fatalf("non-member holder: %d %s", code, raw)
	} else if c, _ := errEnvelope(t, raw); c != api.ErrCoreNotFound {
		t.Fatalf("non-member envelope: %s", raw)
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items/wi_nosuchthing/session", e.key, nil, nil); code != 404 {
		t.Fatalf("unknown WI holder: %d %s", code, raw)
	}

	// §1.2 sanity — the compare-only session-verify is untouched: it
	// still answers matches against the secret and never echoes it.
	held := createExecWIOverHTTP(t, e, "holder 1.2 sanity")
	if code, raw, _ := sessClaim(t, e, held, secret, "h-2"); code != 200 {
		t.Fatalf("sanity claim: %d %s", code, raw)
	}
	var v api.SessionVerifyResponse
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+held+"/session-verify", e.key,
		map[string]string{"session_id": secret}, &v); code != 200 || !v.Matches ||
		v.CurrentFingerprint != api.SessionFingerprint(secret) {
		t.Fatalf("sanity verify match: %d %+v", code, v)
	} else if strings.Contains(string(raw), secret) {
		t.Fatalf("sanity verify echoes the secret: %s", raw)
	}
	if code, _ := e.do(t, "POST", "/api/v1/work-items/"+held+"/session-verify", e.key,
		map[string]string{"session_id": "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"}, &v); code != 200 || v.Matches {
		t.Fatalf("sanity verify mismatch: %d %+v", code, v)
	}
}

// TestUnblockOverHTTP (Finding 3, v1.5 review): the blocked state's exit
// door. session-end(blocked) is the STANDARD compensation (abandonClaim's
// D5 rule, the bounded fail), so a human must be able to return the
// verdict to the queued pool: POST /unblock flips blocked→queued, clears
// waiting_on and leaves a wi_unblocked event. Authorization mirrors
// force-takeover (admin or project maintainer); a non-blocked work item
// answers the typed NOT_BLOCKED refusal.
func TestUnblockOverHTTP(t *testing.T) {
	e := newSrvEnv(t)
	id := createExecWIOverHTTP(t, e, "unblock over http")
	secret := "91919191919191919191919191919191"
	if code, raw, _ := sessClaim(t, e, id, secret, "ub-1"); code != 200 {
		t.Fatalf("claim: %d %s", code, raw)
	}
	// The bounded-fail shape: session-end(blocked) clears the session row
	// (M2) and sets waiting_on=owner.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]string{"session_id": secret, "status": "blocked", "note": "recovery budget exhausted"}, nil); code != 200 {
		t.Fatalf("session-end(blocked): %d %s", code, raw)
	}

	// Authorization: a plain writer (non-member) is refused.
	outsider, _ := e.admin.CreateUser("plainub", "pub@example.com", "writer", "human")
	okey, _ := e.admin.CreateAPIKey(outsider.ID, "pub", "")
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/unblock", okey.Plain,
		map[string]any{}, nil); code != 403 {
		t.Fatalf("non-maintainer unblock: %d %s", code, raw)
	}

	// The gate: unblocking a QUEUED work item answers NOT_BLOCKED.
	other := createExecWIOverHTTP(t, e, "unblock not blocked")
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+other+"/unblock", e.key,
		map[string]any{}, nil); code != 409 {
		t.Fatalf("queued unblock: %d %s", code, raw)
	} else if c, r := errEnvelope(t, raw); c != "CORE-409" || r != "NOT_BLOCKED" {
		t.Fatalf("wrong refusal: %s %s", c, r)
	}

	// A project maintainer may unblock (the takeover gate's twin).
	member, _ := e.admin.CreateUser("maintub", "mub@example.com", "writer", "human")
	mkey, _ := e.admin.CreateAPIKey(member.ID, "mub", "")
	e.do(t, "PATCH", "/api/v1/projects/"+e.adminProjectID(t, "alpha"), e.key,
		map[string]any{"members": []map[string]string{{"user_id": member.ID, "role": "maintainer"}}}, nil)

	var unblocked struct {
		ID        string  `json:"id"`
		Status    string  `json:"status"`
		WaitingOn *string `json:"waiting_on"`
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/unblock", mkey.Plain,
		map[string]any{}, &unblocked); code != 200 || unblocked.Status != "queued" || unblocked.WaitingOn != nil {
		t.Fatalf("maintainer unblock: %d %s %+v", code, raw, unblocked)
	}

	// The wi_unblocked event landed on the timeline.
	found := false
	for _, ev := range eventsOf(t, e, id) {
		if ev["type"] == "wi_unblocked" {
			found = true
		}
	}
	if !found {
		t.Fatalf("wi_unblocked event missing: %v", eventsOf(t, e, id))
	}

	// The unblocked work item is claimable again through the ordinary CAS
	// gate (the runtime needs no change — queued is queued).
	if code, raw, _ := sessClaim(t, e, id, "92929292929292929292929292929292", "ub-2"); code != 200 {
		t.Fatalf("claim after unblock: %d %s", code, raw)
	}

	// Terminal states stay read-only: a wrapped work item answers 410.
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/session-end", e.key,
		map[string]string{"session_id": "92929292929292929292929292929292", "status": "wrapped"}, nil); code != 200 {
		t.Fatalf("session-end(wrapped): %d %s", code, raw)
	}
	if code, raw := e.do(t, "POST", "/api/v1/work-items/"+id+"/unblock", e.key,
		map[string]any{}, nil); code != 410 {
		t.Fatalf("terminal unblock: %d %s", code, raw)
	}
}
