package core

// session_test.go — the v1.5 session model's core-level red lines (Lane A
// verification list): CAS race, idempotent replay, compare-only verify,
// attach gates, takeover swap + audit event, session-end fence + full
// field clear, claim gates, the pause/clean_pause distinction, and the
// git credential's session path.

import (
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

func sessionEnv(t *testing.T) (*testEnv, *SessionManager) {
	t.Helper()
	env := newEnv(t)
	return env, NewSessionManager(env.DB)
}

// mustSessionClaim claims wiID with a fresh secret and a unique key.
func mustSessionClaim(t *testing.T, m *SessionManager, env *testEnv, wiID, secret, key string) *api.SessionClaim {
	t.Helper()
	claim, err := m.Claim(wiID, api.SessionClaimRequest{
		SessionID: secret, MachineID: "m1", Owner: "op", IdempotencyKey: key,
	}, env.UserID)
	if err != nil {
		t.Fatalf("session claim: %v", err)
	}
	return claim
}

// rowSession reads the raw session columns for direct assertions
// (nil pointers = NULL columns).
func rowSession(t *testing.T, env *testEnv, wiID string) (status string, hash, fp, machine, owner, started *string, cleanPause bool) {
	t.Helper()
	var h, f, m, o, st sql.NullString
	err := env.DB.QueryRow(`SELECT status, session_hash, session_fingerprint, session_machine,
		session_owner, session_started_at, clean_pause FROM work_items WHERE id=?1`, wiID).
		Scan(&status, &h, &f, &m, &o, &st, &cleanPause)
	if err != nil {
		t.Fatalf("read session row: %v", err)
	}
	if h.Valid {
		hash = &h.String
	}
	if f.Valid {
		fp = &f.String
	}
	if m.Valid {
		machine = &m.String
	}
	if o.Valid {
		owner = &o.String
	}
	if st.Valid {
		started = &st.String
	}
	return
}

// TestSessionClaimCASRace: two concurrent claims of one queued WI —
// exactly one wins the CAS; the loser answers CORE-409 ALREADY_HELD and
// the stored session is the winner's.
func TestSessionClaimCASRace(t *testing.T) {
	env, m := sessionEnv(t)
	id := mustCreateExecWI(t, env, "race goal", nil, "")

	const racers = 2
	var wg sync.WaitGroup
	results := make([]*api.SessionClaim, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = m.Claim(id, api.SessionClaimRequest{
				SessionID: api.SessionFingerprint("racer") + strings.Repeat(string(rune('a'+i)), 60),
				MachineID: "m", Owner: "op", IdempotencyKey: "race-" + string(rune('a'+i)),
			}, env.UserID)
		}(i)
	}
	wg.Wait()

	wins := 0
	var winner *api.SessionClaim
	for i := 0; i < racers; i++ {
		switch {
		case errs[i] == nil:
			wins++
			if winner == nil {
				winner = results[i]
			}
		case IsCode(errs[i], api.ErrCoreConflict) && ErrorReason(errs[i]) == "ALREADY_HELD":
		default:
			t.Fatalf("racer %d: wrong error %v", i, errs[i])
		}
	}
	if wins != 1 || winner == nil {
		t.Fatalf("CAS race: %d winners, want exactly 1 (errs: %v)", wins, errs)
	}
	// The winner's write landed: running, hash = sha256 of the winner's
	// secret (the claim itself never exposes the secret, so recompute via
	// the known racer-secret scheme is impossible here — assert shape).
	status, hash, fp, _, _, _, _ := rowSession(t, env, id)
	if status != api.StatusRunning || hash == nil || *hash == "" || fp == nil || *fp != winner.SessionFingerprint {
		t.Fatalf("winner's write did not land: status=%s hash=%v fp=%v", status, *hash, *fp)
	}
}

// TestSessionClaimIdempotentReplay (D4): the same idempotency key answers
// the same claim; a key bound to another caller/WI is refused; a cached
// session that is no longer live answers CORE-403 STALE_SESSION.
func TestSessionClaimIdempotentReplay(t *testing.T) {
	env, m := sessionEnv(t)
	id := mustCreateExecWI(t, env, "idem goal", nil, "")
	secret := "0123456789abcdef0123456789abcdef"
	c1 := mustSessionClaim(t, m, env, id, secret, "key-1")

	c2 := mustSessionClaim(t, m, env, id, "ffffffffffffffffffffffffffffffff", "key-1")
	if c2.SessionFingerprint != c1.SessionFingerprint {
		t.Fatalf("replay minted a different session: %s vs %s", c2.SessionFingerprint, c1.SessionFingerprint)
	}
	// Key reuse across callers: refused.
	if _, err := m.Claim(id, api.SessionClaimRequest{SessionID: secret, IdempotencyKey: "key-1"}, env.UserIDB); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("key reuse across callers not refused: %v", err)
	}
	// Takeover swaps the session: the cached response is now stale.
	if _, err := m.Takeover(id, api.SessionClaimRequest{
		SessionID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Reason: "machine lost",
	}, env.UserID); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	_, err := m.Claim(id, api.SessionClaimRequest{SessionID: secret, IdempotencyKey: "key-1"}, env.UserID)
	if !IsCode(err, api.ErrCoreForbidden) || ErrorReason(err) != "STALE_SESSION" {
		t.Fatalf("stale cached replay not fenced: %v", err)
	}
}

// TestSessionVerifyCompareOnly (B3): match, mismatch and unheld answers;
// verify never mutates anything and never echoes the secret.
func TestSessionVerifyCompareOnly(t *testing.T) {
	env, m := sessionEnv(t)
	secret := "feedfacefeedfacefeedfacefeedface"
	// Unheld: matches=false, no current fingerprint, status served.
	id := mustCreateExecWI(t, env, "verify goal", nil, "")
	resp, err := m.Verify(id, secret)
	if err != nil {
		t.Fatalf("verify unheld: %v", err)
	}
	if resp.Matches || resp.CurrentFingerprint != "" || resp.Status != api.StatusQueued || resp.CleanPause {
		t.Fatalf("unheld verify wrong: %+v", resp)
	}
	// Held + matching secret.
	mustSessionClaim(t, m, env, id, secret, "v-1")
	resp, err = m.Verify(id, secret)
	if err != nil {
		t.Fatalf("verify held: %v", err)
	}
	if !resp.Matches || resp.Status != api.StatusRunning || resp.CurrentFingerprint != api.SessionFingerprint(secret) {
		t.Fatalf("held verify wrong: %+v", resp)
	}
	// Held + foreign secret: the answer names the current holder.
	resp, err = m.Verify(id, "deadbeefdeadbeefdeadbeefdeadbeef")
	if err != nil {
		t.Fatalf("verify mismatch: %v", err)
	}
	if resp.Matches || resp.CurrentFingerprint != api.SessionFingerprint(secret) {
		t.Fatalf("mismatch verify wrong: %+v", resp)
	}
	// Compare-only: no event was written and the status did not move.
	events, err := env.WIs.Events(id)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range events {
		if strings.HasPrefix(e.Type, "session_") && e.Type != "session_claimed" {
			t.Fatalf("verify wrote a session event: %+v", e)
		}
	}
	// Unknown WI: typed 404.
	if _, err := m.Verify("wi_nosuchthing", secret); !IsCode(err, api.ErrCoreNotFound) {
		t.Fatalf("unknown WI verify: %v", err)
	}
}

// TestSessionAttachGates: attach requires hash match AND status==paused
// AND clean_pause==0; each violation gets its typed refusal.
func TestSessionAttachGates(t *testing.T) {
	env, m := sessionEnv(t)
	secret := "12341234123412341234123412341234"
	id := mustCreateExecWI(t, env, "attach goal", nil, "")
	mustSessionClaim(t, m, env, id, secret, "a-1")

	// Running (not paused): NOT_ATTACHABLE.
	if _, err := m.Attach(id, api.SessionAttachRequest{SessionID: secret}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "NOT_ATTACHABLE" {
		t.Fatalf("attach of running WI allowed: %v", err)
	}
	// Wrong secret on a paused WI: STALE_SESSION.
	if err := m.Pause(id, secret, false, "", env.UserID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := m.Attach(id, api.SessionAttachRequest{SessionID: "wrong"}, env.UserID); !IsCode(err, api.ErrCoreForbidden) || ErrorReason(err) != "STALE_SESSION" {
		t.Fatalf("attach with wrong secret allowed: %v", err)
	}
	// Happy path: same session resumes.
	claim, err := m.Attach(id, api.SessionAttachRequest{SessionID: secret, MachineID: "m1"}, env.UserID)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if claim.SessionFingerprint != api.SessionFingerprint(secret) {
		t.Fatalf("attach answered a foreign fingerprint: %+v", claim)
	}
	// The session_attached event is on the timeline.
	found := false
	events, _ := env.WIs.Events(id)
	for _, e := range events {
		if e.Type == "session_attached" {
			found = true
		}
	}
	if !found {
		t.Fatalf("session_attached event missing")
	}
	// Operator pause (clean_pause): attach refuses, only takeover (or a
	// CLI unpause) may resume.
	if err := m.Pause(id, secret, true, "operator pause", env.UserID); err != nil {
		t.Fatalf("clean pause: %v", err)
	}
	st, _, _, _, _, _, cp := rowSession(t, env, id)
	if st != api.StatusPaused || !cp {
		t.Fatalf("clean pause did not land: status=%s clean_pause=%v", st, cp)
	}
	if _, err := m.Attach(id, api.SessionAttachRequest{SessionID: secret}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "NOT_ATTACHABLE" {
		t.Fatalf("attach of clean-paused WI allowed: %v", err)
	}
}

// TestSessionTakeover: the swap is atomic, the OLD fingerprint rides the
// session_superseded event (m4), the old secret dies at the git face, the
// new one works, and takeover is idempotent on its key.
func TestSessionTakeover(t *testing.T) {
	env, m := sessionEnv(t)
	id := mustCreateExecWI(t, env, "takeover goal", nil, "")
	oldSecret := "01010101010101010101010101010101"
	mustSessionClaim(t, m, env, id, oldSecret, "t-1")

	newSecret := "02020202020202020202020202020202"
	claim, err := m.Takeover(id, api.SessionClaimRequest{
		SessionID: newSecret, MachineID: "m2", Owner: "op2", Reason: "machine lost", IdempotencyKey: "tk-1",
	}, env.UserID)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if claim.SessionFingerprint != api.SessionFingerprint(newSecret) {
		t.Fatalf("takeover answered the wrong fingerprint: %+v", claim)
	}
	if fp, err := env.Owners.CheckSessionCredential(id, id, oldSecret); err == nil {
		t.Fatalf("old secret still authenticates (fp=%s)", fp)
	}
	if fp, err := env.Owners.CheckSessionCredential(id, id, newSecret); err != nil || fp != api.SessionFingerprint(newSecret) {
		t.Fatalf("new secret rejected: %v %q", err, fp)
	}
	// The audit event carries the OLD fingerprint.
	found := false
	events, _ := env.WIs.Events(id)
	for _, e := range events {
		if e.Type != "session_superseded" {
			continue
		}
		found = true
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("superseded payload: %v", err)
		}
		if p["old_session_fingerprint"] != api.SessionFingerprint(oldSecret) {
			t.Fatalf("superseded event lost the old fingerprint: %s", e.Payload)
		}
	}
	if !found {
		t.Fatalf("session_superseded event missing")
	}
	// Idempotency: the same key replays the same new session.
	again, err := m.Takeover(id, api.SessionClaimRequest{
		SessionID: "03030303030303030303030303030303", Reason: "retry", IdempotencyKey: "tk-1",
	}, env.UserID)
	if err != nil {
		t.Fatalf("takeover replay: %v", err)
	}
	if again.SessionFingerprint != claim.SessionFingerprint {
		t.Fatalf("takeover replay minted another session: %+v", again)
	}
	// Takeover of a queued WI refuses (NOT_TAKEOVERABLE); reason required.
	qid := mustCreateExecWI(t, env, "queued takeover", nil, "")
	if _, err := m.Takeover(qid, api.SessionClaimRequest{SessionID: "17171717171717171717171717171717", Reason: "r"}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "NOT_TAKEOVERABLE" {
		t.Fatalf("takeover of queued WI allowed: %v", err)
	}
	if _, err := m.Takeover(id, api.SessionClaimRequest{SessionID: "0404", Reason: ""}, env.UserID); !IsCode(err, api.ErrCoreBadInput) {
		t.Fatalf("empty reason accepted: %v", err)
	}
}

// TestSessionEndFenceAndClear: the end is fenced on the live session
// (a foreign secret answers CORE-403 STALE_SESSION), a successful end
// clears EVERY session field, blocked is excluded from the claim gate
// (M2), and the session_ended event is recorded.
func TestSessionEndFenceAndClear(t *testing.T) {
	env, m := sessionEnv(t)
	secret := "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"
	id := mustCreateExecWI(t, env, "end goal", nil, "")
	mustSessionClaim(t, m, env, id, secret, "e-1")

	// Fence: a foreign secret cannot end the WI.
	if err := m.End(id, api.SessionEndRequest{SessionID: "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b", Status: api.StatusWrapped}, env.UserID); !IsCode(err, api.ErrCoreForbidden) || ErrorReason(err) != "STALE_SESSION" {
		t.Fatalf("session-end fence broken: %v", err)
	}
	// Bad status: 400 (paused is LEGAL now — merge amend: it routes to the
	// M3 pause transition; use a genuinely illegal status).
	if err := m.End(id, api.SessionEndRequest{SessionID: secret, Status: "queued"}, env.UserID); !IsCode(err, api.ErrCoreBadInput) {
		t.Fatalf("bad status accepted: %v", err)
	}
	// Blocked (M2): session cleared + status blocked.
	if err := m.End(id, api.SessionEndRequest{SessionID: secret, Status: api.StatusBlocked, Note: "bounded fail"}, env.UserID); err != nil {
		t.Fatalf("session-end blocked: %v", err)
	}
	status, hash, fp, machine, owner, started, clean := rowSession(t, env, id)
	if status != api.StatusBlocked || hash != nil || fp != nil || machine != nil || owner != nil || started != nil || clean {
		t.Fatalf("blocked did not clear the session row: status=%s hash=%v fp=%v machine=%v owner=%v started=%v clean=%v",
			status, hash, fp, machine, owner, started, clean)
	}
	// Blocked is NOT claimable (M2): excluded until a human unblocks.
	if _, err := m.Claim(id, api.SessionClaimRequest{SessionID: "0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c"}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "NOT_CLAIMABLE" {
		t.Fatalf("blocked WI is claimable: %v", err)
	}
	// The session_ended event carries the ended fingerprint + status.
	found := false
	events, _ := env.WIs.Events(id)
	for _, e := range events {
		if e.Type == "session_ended" {
			found = true
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p)
			if p["status"] != api.StatusBlocked || p["session_fingerprint"] != api.SessionFingerprint(secret) {
				t.Fatalf("session_ended payload wrong: %s", e.Payload)
			}
		}
	}
	if !found {
		t.Fatalf("session_ended event missing")
	}
}

// TestSessionClaimGates: the M1 joint gate's typed refusals — terminal
// (CORE-410), no bundle / not queued (CORE-409 NOT_CLAIMABLE),
// dependency-blocked (CORE-409 DEPENDENCY_BLOCKED).
func TestSessionClaimGates(t *testing.T) {
	env, m := sessionEnv(t)
	// Terminal: a wrapped WI answers CORE-410 whatever else is true.
	tid := mustCreateExecWI(t, env, "terminal gate", nil, "")
	mustSessionClaim(t, m, env, tid, "12121212121212121212121212121212", "g-1")
	if err := m.End(tid, api.SessionEndRequest{SessionID: "12121212121212121212121212121212", Status: api.StatusWrapped}, env.UserID); err != nil {
		t.Fatalf("end: %v", err)
	}
	if _, err := m.Claim(tid, api.SessionClaimRequest{SessionID: "13131313131313131313131313131313"}, env.UserID); !IsCode(err, api.ErrCoreGone) || ErrorReason(err) != "TERMINAL_STATE" {
		t.Fatalf("terminal claim not refused: %v", err)
	}
	// No bundle: NOT_CLAIMABLE.
	rid := mustCreateWI(t, env, "registry gate", nil, "")
	if _, err := m.Claim(rid, api.SessionClaimRequest{SessionID: "14141414141414141414141414141414"}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "NOT_CLAIMABLE" {
		t.Fatalf("registration claim allowed: %v", err)
	}
	// Dependency-blocked: DEPENDENCY_BLOCKED wins over NOT_CLAIMABLE for
	// a WI with both problems (blockers first, then bundle).
	blocker := mustCreateExecWI(t, env, "blocker exec", nil, "")
	blockedReg := mustCreateWI(t, env, "blocked registry", nil, "")
	if err := env.WIs.AddDependency(blocker, blockedReg, "blocks"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Claim(blockedReg, api.SessionClaimRequest{SessionID: "15151515151515151515151515151515"}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "DEPENDENCY_BLOCKED" {
		t.Fatalf("dependency diagnosis lost: %v", err)
	}
	// A blocked dependency on an execution ticket: DEPENDENCY_BLOCKED.
	blockedExec := mustCreateExecWI(t, env, "blocked exec", nil, "")
	if err := env.WIs.AddDependency(blocker, blockedExec, "blocks"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Claim(blockedExec, api.SessionClaimRequest{SessionID: "16161616161616161616161616161616"}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "DEPENDENCY_BLOCKED" {
		t.Fatalf("dependency-blocked exec claim allowed: %v", err)
	}
}

// TestCheckSessionCredentialSessionPath: the git credential check validates
// hex(sha256(password)) == session_hash AND status==running (M3); the
// mismatch names the current holder fingerprint.
func TestCheckSessionCredentialSessionPath(t *testing.T) {
	env, m := sessionEnv(t)
	secret := "1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a"
	id := mustCreateExecWI(t, env, "push auth", nil, "")
	mustSessionClaim(t, m, env, id, secret, "p-1")

	// Match: returns the PUBLIC fingerprint (the hook backup fence's
	// expected value).
	fp, err := env.Owners.CheckSessionCredential(id, id, secret)
	if err != nil || fp != api.SessionFingerprint(secret) {
		t.Fatalf("valid session secret rejected: %v %q", err, fp)
	}
	// Mismatch: GIT-401, detail names the current holder.
	_, err = env.Owners.CheckSessionCredential(id, id, "1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b")
	if !IsCode(err, api.ErrGitUnauthorized) || ErrorReason(err) != "BAD_CREDENTIAL" {
		t.Fatalf("wrong secret accepted: %v", err)
	}
	if !strings.Contains(err.Error(), api.SessionFingerprint(secret)) {
		t.Fatalf("mismatch does not name the holder: %v", err)
	}
	// Paused (M3): GIT-403, no push while paused.
	if err := m.Pause(id, secret, false, "", env.UserID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, err = env.Owners.CheckSessionCredential(id, id, secret)
	if !IsCode(err, api.ErrGitForbidden) || ErrorReason(err) != "PAUSED" {
		t.Fatalf("paused push not refused: %v", err)
	}
	// After takeover the old secret is dead even while running.
	if _, err := m.Attach(id, api.SessionAttachRequest{SessionID: secret}, env.UserID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := m.Takeover(id, api.SessionClaimRequest{
		SessionID: "1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c", Reason: "swap",
	}, env.UserID); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if _, err := env.Owners.CheckSessionCredential(id, id, secret); !IsCode(err, api.ErrGitUnauthorized) {
		t.Fatalf("superseded secret still valid: %v", err)
	}
	// Wrong username never authenticates.
	if _, err := env.Owners.CheckSessionCredential(id, "otheruser", "1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c"); !IsCode(err, api.ErrGitUnauthorized) {
		t.Fatalf("wrong username accepted: %v", err)
	}
}

// TestSessionPauseCleanFlagDistinction (M3): operator pause sets
// clean_pause=1 (restart must not auto-resume), daemon shutdown keeps 0;
// the session survives the pause either way.
func TestSessionPauseCleanFlagDistinction(t *testing.T) {
	env, m := sessionEnv(t)
	secret := "1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d"
	id := mustCreateExecWI(t, env, "pause flags", nil, "")
	mustSessionClaim(t, m, env, id, secret, "pf-1")

	// Daemon shutdown pause: clean_pause stays 0, session kept.
	if err := m.Pause(id, secret, false, "", env.UserID); err != nil {
		t.Fatalf("shutdown pause: %v", err)
	}
	st, hash, _, _, _, _, cp := rowSession(t, env, id)
	if st != api.StatusPaused || cp || hash == nil || *hash != api.SessionHashHex(secret) {
		t.Fatalf("shutdown pause wrong: status=%s clean=%v hash=%v", st, cp, hash)
	}
	// Operator pause: attach back first (a paused WI pauses no further),
	// then clean_pause=1, session kept.
	if _, err := m.Attach(id, api.SessionAttachRequest{SessionID: secret}, env.UserID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := m.Pause(id, secret, true, "operator", env.UserID); err != nil {
		t.Fatalf("operator pause: %v", err)
	}
	st, hash, _, _, _, _, cp = rowSession(t, env, id)
	if st != api.StatusPaused || !cp || hash == nil {
		t.Fatalf("operator pause wrong: status=%s clean=%v", st, cp)
	}
	// Both pauses write their session_paused event with the flag.
	n := 0
	events, _ := env.WIs.Events(id)
	for _, e := range events {
		if e.Type == "session_paused" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("session_paused events = %d, want 2", n)
	}
	// A foreign session cannot pause (fenced).
	if err := m.Pause(id, "1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e", true, "", env.UserID); !IsCode(err, api.ErrCoreForbidden) || ErrorReason(err) != "STALE_SESSION" {
		t.Fatalf("foreign pause allowed: %v", err)
	}
}

// TestWorkItemCarriesPublicSessionBlockNeverHash: the m4 read surface —
// View carries the public audit fields, and no view anywhere contains
// session_hash.
func TestWorkItemCarriesPublicSessionBlockNeverHash(t *testing.T) {
	env, m := sessionEnv(t)
	secret := "1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f"
	id := mustCreateExecWI(t, env, "view goal", nil, "")
	mustSessionClaim(t, m, env, id, secret, "vw-1")

	wi, err := env.WIs.Get(id, GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	view, err := env.WIs.View(wi)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.SessionFingerprint != api.SessionFingerprint(secret) ||
		view.SessionMachine != "m1" || view.SessionOwner != "op" ||
		view.SessionStartedAt == "" || view.CleanPause {
		t.Fatalf("view session block wrong: %+v", view.SessionPublic)
	}
	blob, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "session_hash") || strings.Contains(string(blob), secret) {
		t.Fatalf("view leaks the hash or the secret: %s", blob)
	}
	// A second WI keeps a zero block.
	id2 := mustCreateExecWI(t, env, "unheld view", nil, "")
	wi2, _ := env.WIs.Get(id2, GetOptions{})
	view2, err := env.WIs.View(wi2)
	if err != nil {
		t.Fatal(err)
	}
	if view2.SessionFingerprint != "" || view2.CleanPause {
		t.Fatalf("unheld view block not empty: %+v", view2.SessionPublic)
	}
}

// TestSessionHolderPublicRead (§1.7): the public holder read maps the
// held, paused, and unheld (zero-block) rows into the additive wire
// type — and never carries the hash or the secret.
func TestSessionHolderPublicRead(t *testing.T) {
	env, m := sessionEnv(t)
	secret := "20202020202020202020202020202020"

	// Unheld (queued): an ordinary answer with the empty block.
	id := mustCreateExecWI(t, env, "holder goal", nil, "")
	resp, err := m.Holder(id)
	if err != nil {
		t.Fatalf("holder unheld: %v", err)
	}
	if resp.WorkItemID != id || resp.Status != api.StatusQueued || resp.CleanPause ||
		resp.SessionFingerprint != "" || resp.MachineID != "" ||
		resp.SessionOwner != "" || resp.SessionStartedAt != "" {
		t.Fatalf("unheld holder wrong: %+v", resp)
	}
	blob, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "session_hash") || strings.Contains(string(blob), secret) {
		t.Fatalf("unheld holder leaks: %s", blob)
	}

	// Held + running: the public audit fields map across.
	claim := mustSessionClaim(t, m, env, id, secret, "h-1")
	resp, err = m.Holder(id)
	if err != nil {
		t.Fatalf("holder held: %v", err)
	}
	if resp.Status != api.StatusRunning || resp.SessionFingerprint != api.SessionFingerprint(secret) ||
		resp.SessionFingerprint != claim.SessionFingerprint ||
		resp.MachineID != "m1" || resp.SessionOwner != "op" ||
		resp.SessionStartedAt == "" || resp.CleanPause {
		t.Fatalf("held holder wrong: %+v", resp)
	}
	blob, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "session_hash") || strings.Contains(string(blob), secret) ||
		strings.Contains(string(blob), api.SessionHashHex(secret)) {
		t.Fatalf("held holder leaks the hash or secret: %s", blob)
	}

	// Held + paused (the M3 transition keeps the row; §1.5 amend): the
	// holder fields are retained and clean_pause rides along.
	if err := m.Pause(id, secret, true, "operator", env.UserID); err != nil {
		t.Fatalf("operator pause: %v", err)
	}
	resp, err = m.Holder(id)
	if err != nil {
		t.Fatalf("holder paused: %v", err)
	}
	if resp.Status != api.StatusPaused || !resp.CleanPause ||
		resp.SessionFingerprint != api.SessionFingerprint(secret) ||
		resp.MachineID != "m1" || resp.SessionOwner != "op" ||
		resp.SessionStartedAt == "" {
		t.Fatalf("paused holder wrong: %+v", resp)
	}

	// Unknown WI: the typed CORE-404.
	if _, err := m.Holder("wi_nosuchthing"); !IsCode(err, api.ErrCoreNotFound) {
		t.Fatalf("unknown WI holder: %v", err)
	}
}
