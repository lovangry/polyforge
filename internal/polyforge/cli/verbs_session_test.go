package cli

// verbs_session_test.go: the v1.5 session-model verb goldens —
// takeover (force-takeover §1.4), the pause clean_pause flag ordering,
// and the resume flag-clear semantics.

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

const cannedSessionClaim = `{"claim":{"work_item_id":"wi_abc",` +
	`"session_fingerprint":"ab12cd34ef56aa00","machine_id":"m-b","owner":"op",` +
	`"bundle_name":"quick-fix","bundle_digest":"d0","claimed_at":"t0"},` +
	`"work_item":` + cannedWI + `}`

// TestTakeoverHappyPath: `pf takeover <id> --reason ...` mints a fresh
// 32-byte session secret, POSTs force-takeover with it (plus machine,
// owner, reason, idempotency key), and renders the response's PUBLIC
// fingerprint — never the secret.
func TestTakeoverHappyPath(t *testing.T) {
	var rec reqRecorder
	aihub := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/force-takeover") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"code":"CORE-404","detail":"wrong route"}`)
			return
		}
		io.WriteString(w, cannedSessionClaim)
	})

	code, stdout, stderr := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_API_KEY": "key-admin",
		"PF_USER_ID": "usr_1",
	}, "takeover", "wi_abc", "--reason", "holder machine died mid-drill")
	if code != 0 {
		t.Fatalf("takeover: code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"took over wi_abc",
		"session: ab12cd34ef56aa00 (machine m-b, owner op)",
		"fenced at its next push",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "sess") && strings.Count(stdout, "ab12cd34ef56aa00") == 0 {
		t.Errorf("stdout must carry the public fingerprint: %s", stdout)
	}

	// The request shape: session_id is 64 hex chars (32 bytes) and is
	// NOT echoed anywhere in the CLI output. (The recorder captured the
	// body before the handler ran — read it from there.)
	var gotBody map[string]any
	if err := json.Unmarshal([]byte(rec.last().Body), &gotBody); err != nil {
		t.Fatalf("takeover request body must decode: %v (%s)", err, rec.last().Body)
	}
	sid, _ := gotBody["session_id"].(string)
	if len(sid) != 64 {
		t.Fatalf("session_id must be 32 bytes hex (64 chars), got %q", sid)
	}
	if _, err := hex.DecodeString(sid); err != nil {
		t.Fatalf("session_id must be hex: %v", err)
	}
	if strings.Contains(stdout, sid) {
		t.Fatal("the session SECRET must never be printed")
	}
	if gotBody["reason"] != "holder machine died mid-drill" {
		t.Errorf("reason = %v", gotBody["reason"])
	}
	if gotBody["work_item_id"] != "wi_abc" {
		t.Errorf("work_item_id = %v", gotBody["work_item_id"])
	}
	if gotBody["owner"] != "usr_1" {
		t.Errorf("owner = %v, want the configured user id", gotBody["owner"])
	}
	if ik, _ := gotBody["idempotency_key"].(string); ik == "" || !strings.HasPrefix(ik, "tk_") {
		t.Errorf("idempotency_key = %v, want a fresh tk_ key", gotBody["idempotency_key"])
	}
	if m, _ := gotBody["machine_id"].(string); m == "" {
		t.Errorf("machine_id must be reported, got %q", m)
	}
	// The call rode the caller's api key.
	if last := rec.last(); last.Header.Get("Authorization") != "Bearer key-admin" {
		t.Errorf("Authorization = %q", last.Header.Get("Authorization"))
	}
}

// TestTakeoverJSONMode: --json emits the response envelope verbatim
// (machine consumers get one JSON document).
func TestTakeoverJSONMode(t *testing.T) {
	aihub := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedSessionClaim)
	})
	code, stdout, _ := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL,
	}, "--json", "takeover", "wi_abc", "--reason", "drill")
	if code != 0 {
		t.Fatalf("takeover --json: code=%d", code)
	}
	var resp api.SessionClaimResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("json output must be the SessionClaimResponse: %v (%s)", err, stdout)
	}
	if resp.Claim.WorkItemID != "wi_abc" || resp.Claim.SessionFingerprint != "ab12cd34ef56aa00" {
		t.Fatalf("claim = %+v", resp.Claim)
	}
}

// TestTakeoverMissingReason: --reason is required; a missing/blank one
// is a local validation refusal — exit 1, no wire contact.
func TestTakeoverMissingReason(t *testing.T) {
	var rec reqRecorder
	aihub := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedSessionClaim)
	})
	for _, args := range [][]string{
		{"takeover", "wi_abc"},
		{"takeover", "wi_abc", "--reason", "   "},
		{"takeover"},
	} {
		code, _, stderr := runPf(t, map[string]string{"PF_SERVER": aihub.URL}, args...)
		if code != 1 {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
		if !strings.Contains(stderr, "--reason") && !strings.Contains(stderr, "exactly one work item") {
			t.Errorf("%v: stderr must name the local validation refusal: %s", args, stderr)
		}
	}
	if n := len(rec.all()); n != 0 {
		t.Errorf("local validation must not contact the server; got %d requests", n)
	}
}

// TestTakeoverWireErrorClasses: the typed envelope keeps the standard
// exit-code mapping (4xx → 1, 5xx → 2).
func TestTakeoverWireErrorClasses(t *testing.T) {
	for _, cls := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"typed-4xx", http.StatusConflict, `{"code":"CORE-409","detail":"ALREADY_HELD"}`, 1},
		{"typed-5xx", http.StatusInternalServerError, `{"code":"CORE-500","detail":"boom"}`, 2},
	} {
		aihub := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(cls.status)
			io.WriteString(w, cls.body)
		})
		code, _, stderr := runPf(t, map[string]string{"PF_SERVER": aihub.URL},
			"takeover", "wi_abc", "--reason", "drill")
		if code != cls.want {
			t.Errorf("%s: exit = %d, want %d", cls.name, code, cls.want)
		}
		if !strings.Contains(stderr, "CORE-") {
			t.Errorf("%s: stderr must carry the typed code: %s", cls.name, stderr)
		}
	}
}

// TestResumeFlagClearSemantics: v1.5 resume clears clean_pause on the
// hub (PATCH, flag=false) and claims nothing locally — no daemon route
// is contacted. The old v1 behavior (fresh local claim via
// POST /jobs/{id}/resume) is deliberately gone.
func TestResumeFlagClearSemantics(t *testing.T) {
	var aihubRec reqRecorder
	aihub := newFakeAihub(t, &aihubRec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedWI)
	})
	d := newDaemonFixture(t) // NO resume answer: any daemon call fails loudly

	code, stdout, stderr := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_DAEMON_SOCKET": d.sock,
	}, "resume", "wi_abc")
	if code != 0 {
		t.Fatalf("resume: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "clean_pause cleared") {
		t.Errorf("stdout must say the flag was cleared: %s", stdout)
	}
	// Precheck (GET) then the flag PATCH; both against aihub only.
	last := aihubRec.last()
	if last.Method != "PATCH" || !strings.Contains(last.Body, `"clean_pause":false`) {
		t.Errorf("resume must PATCH clean_pause=false; last aihub call = %s %s %q",
			last.Method, last.Path, last.Body)
	}
	for _, r := range aihubRec.all() {
		if strings.HasPrefix(r.Path, "/jobs/") {
			t.Errorf("v1.5 resume must not hit daemon routes; saw %s", r.Path)
		}
	}
}

// TestResumeWireErrorClasses: with the daemon socket deliberately dead
// (resume never dials it), the exit code rides the aihub face alone.
func TestResumeWireErrorClasses(t *testing.T) {
	// Typed 4xx on the PATCH → 1.
	aihub := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"code":"CORE-403","detail":"no write access"}`)
			return
		}
		io.WriteString(w, cannedWI)
	})
	code, _, stderr := runPf(t, map[string]string{"PF_SERVER": aihub.URL}, "resume", "wi_abc")
	if code != 1 || !strings.Contains(stderr, "CORE-403") {
		t.Errorf("typed 4xx: exit=%d stderr=%s", code, stderr)
	}

	// Typed 5xx → 2.
	aihub5 := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"code":"CORE-500","detail":"boom"}`)
			return
		}
		io.WriteString(w, cannedWI)
	})
	code, _, _ = runPf(t, map[string]string{"PF_SERVER": aihub5.URL}, "resume", "wi_abc")
	if code != 2 {
		t.Errorf("typed 5xx: exit=%d, want 2", code)
	}

	// Terminal work item → the precheck refuses before any PATCH.
	aihubT := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"wi_abc","slug":"pf-1","status":"wrapped","project":"polyforge","goal":"g","created_at":"t"}`)
	})
	code, _, stderr = runPf(t, map[string]string{"PF_SERVER": aihubT.URL}, "resume", "wi_abc")
	if code != 1 || !strings.Contains(stderr, "terminal") {
		t.Errorf("terminal precheck: exit=%d stderr=%s", code, stderr)
	}
}

// TestPauseFlagLandFailureDegradesToWarning: when the hub-side flag
// write cannot land — a dead server OR the v1 server's typed
// IMMUTABLE refusal for the not-yet-implemented clean_pause field —
// the pause still proceeds (the daemon request body repeats the flag)
// with a stderr warning, never a refused pause.
func TestPauseFlagLandFailureDegradesToWarning(t *testing.T) {
	d := newDaemonFixture(t)
	d.answer("POST", "/jobs/wi_abc/pause", `{}`)
	code, _, stderr := runPf(t, map[string]string{
		// No PF_SERVER: the PATCH dials the default (dead) local address.
		"PF_DAEMON_SOCKET": d.sock,
	}, "pause", "wi_abc")
	if code != 0 {
		t.Fatalf("pause must degrade, not fail: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "could not set clean_pause") {
		t.Errorf("stderr must carry the flag warning: %s", stderr)
	}
	// The daemon pause still happened, and its body carries the flag.
	got := d.rec.last()
	if got.Path != "/jobs/wi_abc/pause" || !strings.Contains(got.Body, `"clean_pause":true`) {
		t.Errorf("pause request = %s %s %q", got.Method, got.Path, got.Body)
	}

	// The transition shape specifically: today's aihub answers the
	// clean_pause PATCH with CORE-400 IMMUTABLE (the field is not
	// patchable until Lane A lands). The pause still succeeds.
	d2 := newDaemonFixture(t)
	d2.answer("POST", "/jobs/wi_abc/pause", `{}`)
	aihub := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"code":"CORE-400","extra":"IMMUTABLE","detail":"field \"clean_pause\" is not patchable"}`)
			return
		}
		io.WriteString(w, cannedWI)
	})
	code, _, stderr = runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_DAEMON_SOCKET": d2.sock,
	}, "pause", "wi_abc")
	if code != 0 {
		t.Fatalf("pause must tolerate the pre-Lane-A IMMUTABLE refusal: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "could not set clean_pause") {
		t.Errorf("stderr must carry the flag warning: %s", stderr)
	}
}

// TestPauseRefusalUndoesTheFlag: when the daemon refuses the pause, the
// early flag write is undone (PATCH clean_pause=false) so a non-paused
// work item does not carry a stale marker.
func TestPauseRefusalUndoesTheFlag(t *testing.T) {
	var aihubRec reqRecorder
	aihub := newFakeAihub(t, &aihubRec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedWI)
	})
	d := newDaemonFixture(t)
	// No pause answer → the fixture answers DAEMON-404.
	code, _, stderr := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_DAEMON_SOCKET": d.sock,
	}, "pause", "wi_abc")
	if code != 1 || !strings.Contains(stderr, "DAEMON-404") {
		t.Fatalf("pause refusal: code=%d stderr=%s", code, stderr)
	}
	// The undo PATCH landed.
	sawUndo := false
	for _, r := range aihubRec.all() {
		if r.Method == "PATCH" && strings.Contains(r.Body, `"clean_pause":false`) {
			sawUndo = true
		}
	}
	if !sawUndo {
		t.Errorf("the refused pause must undo the early flag write; saw %v", aihubRec.all())
	}
}
