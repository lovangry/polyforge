package server_test

// Regression test for the FA-4 budget-token fix (02 §5.1 tier 3): row 12
// (GET budget) belongs to the active generation's execution plane. With an
// active generation the caller must present the X-PF-Generation-Token
// (CORE-403 + §6.6 reason on failure); with no active generation the read is
// tier 2 and the api_key alone suffices.

import (
	"testing"
)

func errReason(t *testing.T, m map[string]any) string {
	t.Helper()
	e, _ := m["error"].(map[string]any)
	r, _ := e["reason"].(string)
	return r
}

func TestBudgetTokenTiering(t *testing.T) {
	e := newEnv(t)
	wi := newExecTicket(t, e)

	// Tier 2 (no active generation): an in-scope key reads without a token.
	st, m, _ := e.do(t, "GET", "/api/v1/work-items/"+wi+"/budget", e.writer, nil)
	if st != 200 {
		t.Fatalf("queued WI budget read must be tier 2 (no token): %d %v", st, m)
	}

	// Claim -> active generation: the read is now tier 3.
	st, m, _ = e.do(t, "POST", "/api/v1/work-items/"+wi+"/claim", e.writer,
		map[string]any{"idempotency_key": "budget-tier3", "machine_id": "mac_a"})
	if st != 200 {
		t.Fatalf("claim: %d %v", st, m)
	}
	token, _ := m["generation_token"].(string)
	if token == "" {
		t.Fatalf("claim must return a generation_token: %v", m)
	}

	// Missing token -> typed CORE-403 (never a 500), for an unscoped key...
	st, m, _ = e.do(t, "GET", "/api/v1/work-items/"+wi+"/budget", e.writer, nil)
	if st != 403 || errCode(t, m) != "CORE-403" {
		t.Fatalf("running WI budget read without token must be CORE-403, got %d %v", st, m)
	}
	// ...and for an IN-SCOPE project-scoped key as well (the scope gate
	// passes; the token gate must answer with a typed 403, not a 500).
	st, m, _ = e.do(t, "GET", "/api/v1/work-items/"+wi+"/budget", e.scoped, nil)
	if st != 403 || errCode(t, m) != "CORE-403" {
		t.Fatalf("in-scope scoped key without token must be a typed CORE-403, got %d %v", st, m)
	}

	// A wrong token is CORE-403 with the §6.6 reason taxonomy.
	st, m, _ = e.doWithToken(t, "GET", "/api/v1/work-items/"+wi+"/budget", e.writer,
		"deadbeef", nil)
	if st != 403 || errCode(t, m) != "CORE-403" {
		t.Fatalf("wrong token must be CORE-403, got %d %v", st, m)
	}
	if r := errReason(t, m); r != "stale_generation" {
		t.Fatalf("unmatchable token must read stale_generation, got %v", m)
	}

	// The active generation's own token passes.
	st, m, _ = e.doWithToken(t, "GET", "/api/v1/work-items/"+wi+"/budget", e.writer, token, nil)
	if st != 200 || m["max_seq"] == nil {
		t.Fatalf("budget read with the active generation's token must succeed: %d %v", st, m)
	}
}
