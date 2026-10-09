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
// 32-byte session secret, POSTs force-takeover with it (plus the LOCAL
// DAEMON's machine id (#109), owner, reason, idempotency key), hands
// the secret to the local daemon for durable custody (#109), and
// renders the response's PUBLIC fingerprint — never the secret.
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
	d := newDaemonFixture(t)
	// The daemon's own machine identity (GET /status) is the mint's
	// machine (#109) — pinned to m-b to match the canned claim answer.
	d.answer("GET", "/status", `{"machine_id":"m-b","mode":"interactive","workspace":"/srv/pf-ws","shutting_down":false,"jobs":[],"queue":{"pending":0,"running":0,"max_concurrent":2}}`)
	d.answer("POST", "/jobs/wi_abc/adopt-session", `{"adopted":true,"work_item_id":"wi_abc","session_fingerprint":"ab12cd34ef56aa00"}`)

	code, stdout, stderr := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_API_KEY": "key-admin",
		"PF_USER_ID": "usr_1", "PF_DAEMON_SOCKET": d.sock,
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
	if m, _ := gotBody["machine_id"].(string); m != "m-b" {
		t.Errorf("machine_id = %v, want the local daemon's own id (GET /status, #109)", m)
	}
	// The custody handoff (#109): the minted secret went to the daemon's
	// adopt-session route — the SAME secret the hub swap presented — and
	// the operator-facing output names the custody, not an orphan.
	var adoptBody map[string]any
	var adoptSeen bool
	for _, r := range d.rec.all() {
		if r.Method == "POST" && r.Path == "/jobs/wi_abc/adopt-session" {
			adoptSeen = true
			if err := json.Unmarshal([]byte(r.Body), &adoptBody); err != nil {
				t.Fatalf("adopt body must decode: %v (%s)", err, r.Body)
			}
		}
	}
	if !adoptSeen {
		t.Fatalf("takeover must hand the minted secret to the daemon (POST /jobs/wi_abc/adopt-session); daemon saw: %v", d.rec.all())
	}
	if adoptBody["session_id"] != sid {
		t.Errorf("adopt session_id = %v, want the minted secret", adoptBody["session_id"])
	}
	if !strings.Contains(stdout, "custody") || strings.Contains(stdout, "orphan session after this") {
		t.Errorf("stdout must name the daemon custody (#109), not the old orphan note: %s", stdout)
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
	d := newDaemonFixture(t)
	d.answer("POST", "/jobs/wi_abc/adopt-session", `{"adopted":true,"work_item_id":"wi_abc","session_fingerprint":"ab12cd34ef56aa00"}`)
	code, stdout, _ := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_DAEMON_SOCKET": d.sock,
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
// exit-code mapping (4xx → 1, 5xx → 2). The local daemon is reachable
// (fixture) — the refusal rides the hub face.
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
		d := newDaemonFixture(t)
		code, _, stderr := runPf(t, map[string]string{
			"PF_SERVER": aihub.URL, "PF_DAEMON_SOCKET": d.sock,
		}, "takeover", "wi_abc", "--reason", "drill")
		if code != cls.want {
			t.Errorf("%s: exit = %d, want %d", cls.name, code, cls.want)
		}
		if !strings.Contains(stderr, "CORE-") {
			t.Errorf("%s: stderr must carry the typed code: %s", cls.name, stderr)
		}
	}
}

// TestTakeoverRequiresLocalDaemon (#109): with no local daemon
// reachable the verb refuses BEFORE minting or touching the hub — a
// takeover whose session no daemon holds is exactly the #97 orphan
// wedge. Same for a --machine that does not match the daemon's own id:
// a session labeled for a machine that will never hold the secret is
// the same unusable mint.
func TestTakeoverRequiresLocalDaemon(t *testing.T) {
	var rec reqRecorder
	aihub := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedSessionClaim)
	})
	// No daemon at the socket (the fixture-free dead path).
	code, _, stderr := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_DAEMON_SOCKET": "/nonexistent/polyforge.sock",
	}, "takeover", "wi_abc", "--reason", "drill")
	if code != 1 {
		t.Errorf("no-daemon takeover: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no local daemon reachable") {
		t.Errorf("stderr must name the refusal: %s", stderr)
	}
	if n := len(rec.all()); n != 0 {
		t.Errorf("the refusal must contact no hub (no mint, no swap); got %d calls", n)
	}

	// --machine mismatch: refused the same way, before the hub.
	d := newDaemonFixture(t) // default /status: machine m1
	code, _, stderr = runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_DAEMON_SOCKET": d.sock,
	}, "takeover", "wi_abc", "--reason", "drill", "--machine", "m-elsewhere")
	if code != 1 {
		t.Errorf("machine-mismatch takeover: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "does not match this box's daemon") {
		t.Errorf("stderr must name the mismatch refusal: %s", stderr)
	}
	if n := len(rec.all()); n != 0 {
		t.Errorf("the mismatch refusal must contact no hub; got %d calls", n)
	}
}

// TestTakeoverAdoptFailureIsLoud (#109): if the hub swap landed but the
// custody handoff to the daemon failed, the verb exits non-zero and
// says the session is an orphan until a daemon-side takeover fences it
// — never a silent exit-0 "success" over a lost secret.
func TestTakeoverAdoptFailureIsLoud(t *testing.T) {
	aihub := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedSessionClaim)
	})
	d := newDaemonFixture(t) // no adopt-session answer → 404
	code, _, stderr := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_DAEMON_SOCKET": d.sock,
	}, "takeover", "wi_abc", "--reason", "drill")
	if code != 1 {
		t.Fatalf("failed custody: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "orphan until a daemon-side takeover fences it") {
		t.Errorf("stderr must carry the orphan warning and the recovery path: %s", stderr)
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

// TestPauseNeedsNoHub: the atomic pause (merge amend 724c5e4) touches
// ONLY the daemon route — the clean_pause flag rides the daemon's own
// fenced SessionEnd, so a CLI with no hub configured pauses fine, and
// nothing is written anywhere before the daemon accepts.
func TestPauseNeedsNoHub(t *testing.T) {
	d := newDaemonFixture(t)
	d.answer("POST", "/jobs/wi_abc/pause", `{}`)
	code, _, stderr := runPf(t, map[string]string{
		// No PF_SERVER: nothing outside the daemon route is dialed.
		"PF_DAEMON_SOCKET": d.sock,
	}, "pause", "wi_abc")
	if code != 0 {
		t.Fatalf("pause: code=%d stderr=%s", code, stderr)
	}
	got := d.rec.last()
	if got.Method != "POST" || got.Path != "/jobs/wi_abc/pause" {
		t.Errorf("pause request = %s %s %q", got.Method, got.Path, got.Body)
	}
}

// TestPauseRefusalWritesNothing: a refused pause makes NO hub write at
// all — the flag lands atomically with the pause transition, so there
// is no early write to undo and no stale marker can survive a refusal.
func TestPauseRefusalWritesNothing(t *testing.T) {
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
	if n := len(aihubRec.all()); n != 0 {
		t.Errorf("the refused pause must write nothing to the hub; saw %d calls: %v", n, aihubRec.all())
	}
}

// TestUnblockHappyPath (Finding 3, v1.5 review): `pf unblock <id>` POSTs
// the exit-door route directly to aihub and renders the returned work
// item's new queued status.
func TestUnblockHappyPath(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Replace(cannedWI, `"queued"`, `"queued"`, 1))
	})
	code, stdout, stderr := runPf(t, map[string]string{
		"PF_SERVER": srv.URL, "PF_API_KEY": "key-admin",
	}, "unblock", "wi_abc")
	if code != 0 {
		t.Fatalf("unblock: code=%d stderr=%s", code, stderr)
	}
	if got := rec.last(); got.Method != "POST" || got.Path != "/api/v1/work-items/wi_abc/unblock" {
		t.Errorf("request = %s %s, want POST /api/v1/work-items/wi_abc/unblock", got.Method, got.Path)
	}
	if !strings.Contains(stdout, "unblocked pf-42 (status queued; claimable again)") {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestUnblockRefusalClasses: a typed wire refusal (NOT_BLOCKED /
// AUTH-403) surfaces as exit 1 with the typed envelope on stderr.
func TestUnblockRefusalClasses(t *testing.T) {
	srv := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"code":"CORE-409","detail":"unblock requires a blocked work item (got queued)","extra":{"reason":"NOT_BLOCKED"}}`)
	})
	code, _, stderr := runPf(t, map[string]string{
		"PF_SERVER": srv.URL, "PF_API_KEY": "key-admin",
	}, "unblock", "wi_abc")
	if code != 1 {
		t.Fatalf("refused unblock: exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "CORE-409") || !strings.Contains(stderr, "blocked work item") {
		t.Errorf("stderr must carry the typed refusal: %s", stderr)
	}
}

// §1.7 holder-read goldens: `pf verify-session <wi>` renders the held
// tri-state's public fields, hands the unheld answer through as an
// ordinary exit-0 answer, and rides GET /work-items/{id}/session with
// the caller's bearer key. No secret ever appears — the verb only ever
// sees the public audit fields.
const cannedHeldHolder = `{"work_item_id":"wi_abc","status":"running",` +
	`"session_fingerprint":"ab12cd34ef56aa00","machine_id":"m-b",` +
	`"session_owner":"op","session_started_at":"t0","clean_pause":false}`

const cannedUnheldHolder = `{"work_item_id":"wi_abc","status":"queued","clean_pause":false}`

// TestVerifySessionHeldText: a held-running WI renders the holder's
// public fields and exits 0; the request is the §1.7 GET on the
// caller's bearer key.
func TestVerifySessionHeldText(t *testing.T) {
	var rec reqRecorder
	aihub := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedHeldHolder)
	})
	code, stdout, stderr := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL, "PF_API_KEY": "key-admin",
	}, "verify-session", "wi_abc")
	if code != 0 {
		t.Fatalf("verify-session: code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"verify-session wi_abc: held (status running)",
		"session: ab12cd34ef56aa00 (machine m-b, owner op)",
		"started: t0",
		"clean_pause: false",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	// The request shape: GET /api/v1/work-items/{id}/session, bearer key.
	got := rec.last()
	if got.Method != "GET" || got.Path != "/api/v1/work-items/wi_abc/session" {
		t.Errorf("request = %s %s, want GET /api/v1/work-items/wi_abc/session", got.Method, got.Path)
	}
	if got.Header.Get("Authorization") != "Bearer key-admin" {
		t.Errorf("Authorization = %q", got.Header.Get("Authorization"))
	}
	if got.Body != "" {
		t.Errorf("the holder read must send no body, got %q", got.Body)
	}
}

// TestVerifySessionHeldJSON: --json emits the endpoint response
// verbatim (decodes as the §1.7 wire type).
func TestVerifySessionHeldJSON(t *testing.T) {
	aihub := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedHeldHolder)
	})
	code, stdout, _ := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL,
	}, "--json", "verify-session", "wi_abc")
	if code != 0 {
		t.Fatalf("verify-session --json: code=%d", code)
	}
	var resp api.SessionHolderResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("json output must be the SessionHolderResponse: %v (%s)", err, stdout)
	}
	if resp.WorkItemID != "wi_abc" || resp.Status != "running" ||
		resp.SessionFingerprint != "ab12cd34ef56aa00" || resp.MachineID != "m-b" ||
		resp.SessionOwner != "op" || resp.SessionStartedAt != "t0" || resp.CleanPause {
		t.Fatalf("holder = %+v", resp)
	}
	if strings.Contains(stdout, "session_id") {
		t.Fatalf("--json must never carry a session secret: %s", stdout)
	}
}

// TestVerifySessionUnheldIsAnAnswer: the unheld state is an ordinary
// exit-0 answer in BOTH renderings — never an error (the takeover-
// decision script needs no error handling to tell free from taken).
func TestVerifySessionUnheldIsAnAnswer(t *testing.T) {
	// Text mode.
	aihub := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedUnheldHolder)
	})
	code, stdout, stderr := runPf(t, map[string]string{
		"PF_SERVER": aihub.URL,
	}, "verify-session", "wi_abc")
	if code != 0 {
		t.Fatalf("unheld text: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "nobody holds it (status queued)") {
		t.Errorf("unheld text = %q", stdout)
	}

	// --json mode: the unheld endpoint JSON, still exit 0.
	aihub2 := newFakeAihub(t, &reqRecorder{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedUnheldHolder)
	})
	code, stdout, _ = runPf(t, map[string]string{
		"PF_SERVER": aihub2.URL,
	}, "--json", "verify-session", "wi_abc")
	if code != 0 {
		t.Fatalf("unheld --json: code=%d", code)
	}
	var resp api.SessionHolderResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("unheld --json must decode: %v (%s)", err, stdout)
	}
	if resp.WorkItemID != "wi_abc" || resp.Status != "queued" || resp.CleanPause ||
		resp.SessionFingerprint != "" || resp.MachineID != "" ||
		resp.SessionOwner != "" || resp.SessionStartedAt != "" {
		t.Fatalf("unheld holder = %+v", resp)
	}
}
