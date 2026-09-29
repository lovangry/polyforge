package cli

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// hasSub is a substring check (the production `contains` helper is a list
// membership test).
func hasSub(s, sub string) bool { return strings.Contains(s, sub) }

// newUnixDaemon spins an httptest server bound to a real unix domain
// socket (the daemon face is HTTP-over-UDS, the shape pinned by
// internal/daemon/api.go) and returns a client pointed at it.
func newUnixDaemon(t *testing.T, status int, response string, cap *captured) *DaemonClient {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "polyforge.sock")
	return newUnixDaemonAt(t, sock, status, response, cap)
}

// startUnixDaemonAtPath starts the stub at an explicit socket path.
func startUnixDaemonAtPath(t *testing.T, sock string, status int, response string, cap *captured) {
	t.Helper()
	newUnixDaemonAt(t, sock, status, response, cap)
}

// newUnixDaemonAt starts the stub at sock and returns a client for it.
func newUnixDaemonAt(t *testing.T, sock string, status int, response string, cap *captured) *DaemonClient {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*cap = captured{
			Method: r.Method,
			Path:   r.URL.RequestURI(),
			Header: r.Header.Clone(),
		}
		if r.Body != nil {
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			cap.Body = string(buf[:n])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(response))
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return NewDaemonClient(sock)
}

func TestDaemonSubmitFamilyFraming(t *testing.T) {
	// claim/resume/takeover are one POST /jobs/{id}/{intent} route family
	// with a {"priority"} body (internal/daemon/api.go hSubmit).
	var cap captured
	d := newUnixDaemon(t, 200, `{"accepted":true,"work_item_id":"wi_abc"}`, &cap)

	raw, err := d.Claim("wi_abc", "")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !hasSub(string(raw), "accepted") {
		t.Error("raw passthrough broken")
	}
	if cap.Method != "POST" || cap.Path != "/jobs/wi_abc/claim" {
		t.Errorf("claim: got %s %s", cap.Method, cap.Path)
	}
	if cap.Body != `{"priority":"normal"}` {
		t.Errorf("claim body = %s", cap.Body)
	}

	if _, err := d.Resume("wi_abc", "high"); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/jobs/wi_abc/resume" || cap.Body != `{"priority":"high"}` {
		t.Errorf("resume: %s %s", cap.Path, cap.Body)
	}

	if _, err := d.Takeover("wi_abc", ""); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/jobs/wi_abc/takeover" {
		t.Errorf("takeover path = %s", cap.Path)
	}
}

func TestDaemonObserveFraming(t *testing.T) {
	var cap captured
	d := newUnixDaemon(t, 200,
		`{"events":[{"seq":4,"type":"step_started"}],"state_snapshot":{},"next_cursor":4}`, &cap)
	raw, err := d.Observe("wi_abc", 3)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// Decode the response through the client-side view types.
	var resp ObserveResponse
	if err := decodeJSON(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 1 || resp.Events[0].Seq != 4 || resp.Events[0].Type != "step_started" {
		t.Errorf("events = %+v", resp.Events)
	}
	if resp.NextCursor != 4 {
		t.Errorf("NextCursor = %d", resp.NextCursor)
	}
	if cap.Method != "GET" {
		t.Errorf("Method = %s", cap.Method)
	}
	if cap.Path != "/jobs/wi_abc/observe?after=3" {
		t.Errorf("Path = %s", cap.Path)
	}
	if cap.Body != "" {
		t.Errorf("observe must be a bodyless GET, got %q", cap.Body)
	}
}

func TestDaemonObserveCompactedShape(t *testing.T) {
	var cap captured
	d := newUnixDaemon(t, 200, `{"state_snapshot":{"current":1},"next_cursor":9,"compacted":true}`, &cap)
	raw, err := d.Observe("wi_abc", 2)
	if err != nil {
		t.Fatal(err)
	}
	var resp ObserveResponse
	if err := decodeJSON(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Compacted || len(resp.Events) != 0 || resp.NextCursor != 9 {
		t.Errorf("compacted response decoded wrong: %+v", resp)
	}
}

func TestDaemonAnswerRelayFiveFields(t *testing.T) {
	// The relay message IS the request body (hAnswer decodes AnswerRelay
	// directly); the caller identity rides the X-PF-User header.
	var cap captured
	d := newUnixDaemon(t, 200, `{"applied":true,"answer_id":"a_7k9m2p"}`, &cap)
	payload := AnswerPayload{
		QuestionID: "q_7f3a1c",
		AnswerID:   "a_7k9m2p",
		AnswerText: "yes, proceed",
		AnsweredAt: "2026-09-26T10:00:00Z",
		AnsweredBy: "u_5dFjeaMZ",
	}
	if _, err := d.Answer("wi_abc", payload); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/jobs/wi_abc/answer" {
		t.Errorf("answer: %s %s", cap.Method, cap.Path)
	}
	for _, want := range []string{
		`"question_id":"q_7f3a1c"`,
		`"answer_id":"a_7k9m2p"`,
		`"answer_text":"yes, proceed"`,
		`"answered_at":"2026-09-26T10:00:00Z"`,
		`"answered_by":"u_5dFjeaMZ"`,
	} {
		if !hasSub(cap.Body, want) {
			t.Errorf("answer body missing %s: %s", want, cap.Body)
		}
	}
	if hasSub(cap.Body, `"wi_id"`) {
		t.Errorf("answer body must not wrap the relay: %s", cap.Body)
	}
	if got := cap.Header.Get("X-PF-User"); got != "u_5dFjeaMZ" {
		t.Errorf("X-PF-User = %q", got)
	}
}

func TestDaemonModeAndShutdown(t *testing.T) {
	var cap captured
	d := newUnixDaemon(t, 200, `{"mode":"drain"}`, &cap)

	n := 8
	if _, err := d.Mode(ModeRequest{Action: "set", MaxConcurrentJobs: &n}); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/mode" || cap.Body != `{"action":"set","max_concurrent_jobs":8}` {
		t.Errorf("mode set: %s %s", cap.Path, cap.Body)
	}

	if _, err := d.Mode(ModeRequest{Action: "drain"}); err != nil {
		t.Fatal(err)
	}
	if cap.Body != `{"action":"drain"}` {
		t.Errorf("drain body = %s", cap.Body)
	}

	if _, err := d.Mode(ModeRequest{Action: "get_recommendation"}); err != nil {
		t.Fatal(err)
	}
	if cap.Body != `{"action":"get_recommendation"}` {
		t.Errorf("recommendation body = %s", cap.Body)
	}

	if _, err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/shutdown" {
		t.Errorf("shutdown: %s %s", cap.Method, cap.Path)
	}
}

func TestDaemonBatchSubmitFraming(t *testing.T) {
	// The route is /batch_submit (underscore), body {"wi_ids":[...]}
	// (hBatchSubmit); the daemon never widens or narrows the set.
	var cap captured
	d := newUnixDaemon(t, 200, `{"accepted":2}`, &cap)
	if _, err := d.BatchSubmit([]string{"wi_a", "wi_b"}); err != nil {
		t.Fatal(err)
	}
	if cap.Method != "POST" || cap.Path != "/batch_submit" {
		t.Errorf("batch: %s %s", cap.Method, cap.Path)
	}
	if cap.Body != `{"wi_ids":["wi_a","wi_b"]}` {
		t.Errorf("batch body = %s", cap.Body)
	}
}

func TestDaemonPauseWrapPublishRequest(t *testing.T) {
	// pause/wrap routes are client-side projections into the /jobs/{id}
	// family (the daemon face on main exposes no pause/wrap route yet —
	// recorded as a spec gap in the README note).
	var cap captured
	d := newUnixDaemon(t, 200, `{"ok":true,"flush_evidence":{}}`, &cap)

	if _, err := d.Pause("wi_a"); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/jobs/wi_a/pause" || cap.Method != "POST" {
		t.Errorf("pause: %s %s", cap.Method, cap.Path)
	}

	if _, err := d.Wrap("wi_a", "wrap"); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/jobs/wi_a/wrap" || !hasSub(cap.Body, `"reason":"wrap"`) {
		t.Errorf("wrap: %s %s", cap.Path, cap.Body)
	}

	if _, err := d.PublishRequest("wi_a", "pause"); err != nil {
		t.Fatal(err)
	}
	if cap.Path != "/jobs/wi_a/publish_request" || !hasSub(cap.Body, `"reason":"pause"`) {
		t.Errorf("publish: %s %s", cap.Path, cap.Body)
	}
}

func TestDaemonStatusDecodes(t *testing.T) {
	// The status() answer shape (hStatus): machine_id, mode,
	// shutting_down, jobs[] identity fields, queue counters.
	var cap captured
	d := newUnixDaemon(t, 200,
		`{"machine_id":"m-01","mode":"interactive","shutting_down":false,`+
			`"jobs":[{"work_item_id":"wi_a","harness_adapter":"pi","harness_session_id":"s1","ring_latest":3}],`+
			`"queue":{"pending":1,"running":2,"max_concurrent":4}}`, &cap)
	raw, err := d.Status()
	if err != nil {
		t.Fatal(err)
	}
	var s StatusResponse
	if err := decodeJSON(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.MachineID != "m-01" || s.Mode != "interactive" || s.ShuttingDown {
		t.Fatalf("status decoded wrong: %+v", s)
	}
	if s.Queue.Pending != 1 || s.Queue.Running != 2 || s.Queue.MaxConcurrent != 4 {
		t.Errorf("queue decoded wrong: %+v", s.Queue)
	}
	if len(s.Jobs) != 1 {
		t.Fatalf("jobs = %+v", s.Jobs)
	}
	j := s.Jobs[0]
	if j.WorkItemID != "wi_a" || j.HarnessAdapter != "pi" || j.HarnessSessionID != "s1" || j.RingLatest != 3 {
		t.Errorf("job decoded wrong: %+v", j)
	}
}

func TestDaemonStatusDrainFetchDegraded(t *testing.T) {
	// The drain degraded signal must ride the status answer and the
	// human pf ops status rendering (monitoring signal, like the stale
	// heartbeat list).
	var cap captured
	d := newUnixDaemon(t, 200,
		`{"machine_id":"m-01","mode":"drain","shutting_down":false,`+
			`"jobs":[],"queue":{"pending":0,"running":0,"max_concurrent":4},`+
			`"drain_fetch_degraded":true}`, &cap)
	raw, err := d.Status()
	if err != nil {
		t.Fatal(err)
	}
	var s StatusResponse
	if err := decodeJSON(raw, &s); err != nil {
		t.Fatal(err)
	}
	if !s.DrainFetchDegraded {
		t.Fatalf("status must decode drain_fetch_degraded, got %+v", s)
	}
	var buf strings.Builder
	renderStatus(&buf, &s)
	if !strings.Contains(buf.String(), "drain fetch degraded") {
		t.Fatalf("pf ops status must render the degraded signal:\n%s", buf.String())
	}
}

func TestDaemonErrorEnvelopeDAEMONCode(t *testing.T) {
	var cap captured
	d := newUnixDaemon(t, 409, envelope("DAEMON-409", "", "question already consumed"), &cap)
	_, err := d.Answer("wi_a", AnswerPayload{QuestionID: "q_x", AnswerID: "a_x", AnswerText: "t", AnsweredAt: "now", AnsweredBy: "u"})
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Code != "DAEMON-409" {
		t.Errorf("Code = %q", ae.Code)
	}
	if ae.Detail != "question already consumed" {
		t.Errorf("Detail = %q", ae.Detail)
	}
}

func TestDaemonUnreachableSocket(t *testing.T) {
	// Socket path that does not exist: a dial error, NOT an APIError —
	// observe uses exactly this signal for its read-through fallback.
	d := NewDaemonClient(filepath.Join(t.TempDir(), "nope.sock"))
	_, err := d.Status()
	if err == nil {
		t.Fatal("expected a dial error")
	}
	var ae *APIError
	if asAPIError(err, &ae) {
		t.Fatalf("dial failure must not be an APIError, got %v", err)
	}
	if !isDialError(err) {
		t.Errorf("isDialError did not recognize: %v", err)
	}
}
