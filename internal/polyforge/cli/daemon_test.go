package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// daemonFixture serves the daemon UDS surface with a recording mux plus
// a per-route answer function.
type daemonFixture struct {
	rec     *reqRecorder
	sock    string
	answers map[string]func(w http.ResponseWriter, r *http.Request)
}

// newDaemonFixture starts a fake daemon on a REAL unix socket. The mux
// answers /jobs/... and /status routes from d.answers (keyed by
// "<METHOD> <path>"); unmatched routes answer 404 DAEMON-404.
func newDaemonFixture(t *testing.T) *daemonFixture {
	t.Helper()
	d := &daemonFixture{rec: &reqRecorder{}, answers: map[string]func(w http.ResponseWriter, r *http.Request){}}
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs/", func(w http.ResponseWriter, r *http.Request) {
		d.rec.record(r)
		key := r.Method + " " + r.URL.Path
		if h, ok := d.answers[key]; ok {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"code":"DAEMON-404","detail":"no local capsule for this work item"}`)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		d.rec.record(r)
		key := r.Method + " " + r.URL.Path
		if h, ok := d.answers[key]; ok {
			h(w, r)
			return
		}
		io.WriteString(w, `{"machine_id":"m1","mode":"interactive","shutting_down":false,"jobs":[],"queue":{"pending":0,"running":0,"max_concurrent":2}}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.rec.record(r)
		key := r.Method + " " + r.URL.Path
		if h, ok := d.answers[key]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"code":"DAEMON-404","detail":"no such route"}`)
	})
	d.sock = newFakeDaemon(t, mux)
	return d
}

// roundtrip: one canned route answer.
func (d *daemonFixture) answer(method, path string, body string) {
	d.answers[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

func TestDaemonClientObserveRoundtrip(t *testing.T) {
	d := newDaemonFixture(t)
	jv := protocol.JobView{
		WorkItem:    "wi_abc",
		Project:     "polyforge",
		Status:      protocol.JobRunning,
		Generation:  2,
		AttemptID:   "ra_1",
		BundleName:  "bundle-x",
		CurrentStep: "implement",
		Steps: map[string]*protocol.StepView{
			"spec":      {Status: "completed", Attempts: 1, Model: "m"},
			"implement": {Status: "in_progress", Attempts: 1, Model: "m"},
		},
		Budget:  json.RawMessage(`{"requests":{"used":3,"ceiling":50}}`),
		Pending: &protocol.PendingView{ID: "q_1", Step: "implement", Question: "Ship now?", AskedAt: "2026-10-01T00:00:00Z"},
		Events: []protocol.EventEntry{
			{At: "2026-10-01T00:00:01Z", Kind: "step_started"},
			{At: "2026-10-01T00:00:02Z", Kind: "needs_input", Body: json.RawMessage(`{"question_id":"q_1"}`)},
		},
	}
	d.answer("GET", "/jobs/wi_abc/observe", jsonBody(jv))

	c := NewDaemonClient(d.sock)
	raw, err := c.Observe("wi_abc", 5)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	got := d.rec.last()
	if got.Method != "GET" || got.Path != "/jobs/wi_abc/observe?after=5" {
		t.Errorf("request = %s %s, want GET /jobs/wi_abc/observe?after=5", got.Method, got.Path)
	}
	var back protocol.JobView
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.WorkItem != "wi_abc" || back.CurrentStep != "implement" ||
		back.Pending == nil || back.Pending.ID != "q_1" || len(back.Events) != 2 {
		t.Errorf("JobView roundtrip mismatch: %+v", back)
	}
}

func TestDaemonClientSubmitFamilyRoundtrip(t *testing.T) {
	d := newDaemonFixture(t)
	for _, intent := range []string{"claim", "resume", "takeover"} {
		d.answer("POST", "/jobs/wi_abc/"+intent, `{"accepted":true,"work_item_id":"wi_abc"}`)
	}
	c := NewDaemonClient(d.sock)

	cases := []struct {
		call func() (json.RawMessage, error)
		want string
	}{
		{func() (json.RawMessage, error) { return c.Claim("wi_abc", "high") }, "/jobs/wi_abc/claim"},
		{func() (json.RawMessage, error) { return c.Resume("wi_abc", "") }, "/jobs/wi_abc/resume"},
		{func() (json.RawMessage, error) { return c.Takeover("wi_abc", "") }, "/jobs/wi_abc/takeover"},
	}
	for i, tc := range cases {
		raw, err := tc.call()
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		got := d.rec.all()[i]
		if got.Method != "POST" || got.Path != tc.want {
			t.Errorf("request = %s %s, want POST %s", got.Method, got.Path, tc.want)
		}
		wantPriority := "normal"
		if i == 0 {
			wantPriority = "high"
		}
		if !strings.Contains(got.Body, `"priority":"`+wantPriority+`"`) {
			t.Errorf("body = %s, want priority %q", got.Body, wantPriority)
		}
		var sr SubmitResponse
		if err := json.Unmarshal(raw, &sr); err != nil || !sr.Accepted || sr.WorkItemID != "wi_abc" {
			t.Errorf("SubmitResponse mismatch: %s err=%v", raw, err)
		}
	}
}

func TestDaemonClientAnswerRoundtrip(t *testing.T) {
	d := newDaemonFixture(t)
	d.answer("POST", "/jobs/wi_abc/answer", `{"applied":true,"delivered":true}`)
	c := NewDaemonClient(d.sock)

	payload := AnswerPayload{
		QuestionID: "q_1",
		AnswerID:   "a_abc123",
		AnswerText: "yes, proceed",
		AnsweredAt: "2026-10-01T00:00:00Z",
		AnsweredBy: "usr_9",
	}
	if _, err := c.Answer("wi_abc", payload); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	got := d.rec.last()
	if got.Method != "POST" || got.Path != "/jobs/wi_abc/answer" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	// The relay message IS the body (five fields, verbatim), and the
	// caller identity travels in X-PF-User.
	if got.Header.Get("X-PF-User") != "usr_9" {
		t.Errorf("X-PF-User = %q, want usr_9", got.Header.Get("X-PF-User"))
	}
	var body AnswerPayload
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if body != payload {
		t.Errorf("body = %+v, want %+v", body, payload)
	}
}

func TestDaemonClientPauseWrapResumeEndToEnd(t *testing.T) {
	// End-to-end through Run: pause, resume (submit precheck answered by
	// the fake aihub) and wrap hit the right UDS routes.
	var aihubRec reqRecorder
	aihub := newFakeAihub(t, &aihubRec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"wi_abc","slug":"pf-1","status":"paused","project":"polyforge","goal":"g","created_at":"t"}`)
	})
	d := newDaemonFixture(t)
	d.answer("POST", "/jobs/wi_abc/pause", `{}`)
	d.answer("POST", "/jobs/wi_abc/wrap", `{"sealed":true}`)
	d.answer("POST", "/jobs/wi_abc/resume", `{"accepted":true,"work_item_id":"wi_abc"}`)

	env := map[string]string{"PF_SERVER": aihub.URL, "PF_DAEMON_SOCKET": d.sock}
	if code, _, stderr := runPf(t, env, "pause", "wi_abc"); code != 0 {
		t.Fatalf("pause: code=%d stderr=%s", code, stderr)
	}
	if got := d.rec.last(); got.Method != "POST" || got.Path != "/jobs/wi_abc/pause" || got.Body != "{}" {
		t.Errorf("pause request = %s %s %q", got.Method, got.Path, got.Body)
	}
	if code, _, stderr := runPf(t, env, "wrap", "wi_abc", "--fail"); code != 0 {
		t.Fatalf("wrap --fail: code=%d stderr=%s", code, stderr)
	}
	if got := d.rec.last(); got.Path != "/jobs/wi_abc/wrap" || !strings.Contains(got.Body, `"reason":"fail"`) {
		t.Errorf("wrap request = %s %s %q", got.Method, got.Path, got.Body)
	}
	if code, _, stderr := runPf(t, env, "resume", "wi_abc"); code != 0 {
		t.Fatalf("resume: code=%d stderr=%s", code, stderr)
	}
	// resume runs the aihub precheck first, then the daemon route.
	if got := d.rec.last(); got.Path != "/jobs/wi_abc/resume" {
		t.Errorf("resume request path = %s", got.Path)
	}
	if last := aihubRec.last(); last.Path != "/api/v1/work-items/wi_abc" {
		t.Errorf("precheck path = %s", last.Path)
	}
}

func TestDaemonClientTypedErrorDecoding(t *testing.T) {
	d := newDaemonFixture(t)
	// No answer registered for observe → the fixture answers DAEMON-404.
	c := NewDaemonClient(d.sock)
	_, err := c.Observe("wi_missing", 0)
	if err == nil {
		t.Fatal("expected typed error")
	}
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != "DAEMON-404" {
		t.Fatalf("want DAEMON-404, got %v", err)
	}
	if ae.HTTPStatus() != 404 {
		t.Errorf("HTTPStatus = %d, want 404", ae.HTTPStatus())
	}
	if code, _, _ := runPf(t, map[string]string{"PF_DAEMON_SOCKET": d.sock}, "observe", "wi_missing"); code != 1 {
		t.Errorf("DAEMON-404 must exit 1, got %d", code)
	}
}

// TestDaemonClientFlushRoute pins the flush route (ruling C2:
// publish_request is replaced by flush on the UDS proxy face).
func TestDaemonClientFlushRoute(t *testing.T) {
	d := newDaemonFixture(t)
	d.answer("POST", "/jobs/wi_abc/flush", `{"ok":true,"local_commit":"sha1","pushed":true}`)
	c := NewDaemonClient(d.sock)
	req := protocol.FlushRequest{
		WorkItem:  "wi_abc",
		AttemptID: "ra_1",
		Reason:    protocol.FlushWrap,
	}
	if _, err := c.Flush("wi_abc", req); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got := d.rec.last()
	if got.Method != "POST" || got.Path != "/jobs/wi_abc/flush" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	if !strings.Contains(got.Body, `"reason":"wrap"`) {
		t.Errorf("body = %s", got.Body)
	}
}

// TestDaemonClientStatusRoundtrip pins GET /status decoding.
func TestDaemonClientStatusRoundtrip(t *testing.T) {
	d := newDaemonFixture(t)
	c := NewDaemonClient(d.sock)
	raw, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var s StatusResponse
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.MachineID != "m1" || s.Mode != "interactive" || s.Queue.MaxConcurrent != 2 {
		t.Errorf("StatusResponse = %+v", s)
	}
}

// TestAnswerUniquePendingResolution: without --question, the answer verb
// resolves the WI's single pending question from the observe projection;
// with no pending question it refuses.
func TestAnswerUniquePendingResolution(t *testing.T) {
	jv := protocol.JobView{
		WorkItem: "wi_abc", Project: "p", Status: protocol.JobNeedsInput,
		Pending: &protocol.PendingView{ID: "q_9", Step: "implement", Question: "Ship?", AskedAt: "t"},
	}
	d := newDaemonFixture(t)
	d.answer("GET", "/jobs/wi_abc/observe", jsonBody(jv))
	d.answer("POST", "/jobs/wi_abc/answer", `{"applied":true,"delivered":true}`)

	code, _, stderr := runPf(t, map[string]string{"PF_DAEMON_SOCKET": d.sock, "PF_USER_ID": "usr_9"},
		"answer", "wi_abc", "--text", "yes")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	got := d.rec.last()
	if got.Path != "/jobs/wi_abc/answer" {
		t.Fatalf("request path = %s", got.Path)
	}
	if !strings.Contains(got.Body, `"question_id":"q_9"`) || !strings.Contains(got.Body, `"answered_by":"usr_9"`) {
		t.Errorf("answer body = %s", got.Body)
	}

	// No pending question → refusal, no answer call.
	d2 := newDaemonFixture(t)
	empty := jv
	empty.Pending = nil
	d2.answer("GET", "/jobs/wi_abc/observe", jsonBody(empty))
	code, _, stderr = runPf(t, map[string]string{"PF_DAEMON_SOCKET": d2.sock, "PF_USER_ID": "usr_9"},
		"answer", "wi_abc", "--text", "yes")
	if code != 1 || !strings.Contains(stderr, "no pending question") {
		t.Errorf("no-pending: code=%d stderr=%s", code, stderr)
	}
}

// TestObserveRenderedOutput drives the full observe verb and checks the
// human table reached stdout.
func TestObserveRenderedOutput(t *testing.T) {
	jv := protocol.JobView{
		WorkItem: "wi_abc", Project: "polyforge", Status: protocol.JobRunning,
		Generation: 1, AttemptID: "ra_1", CurrentStep: "implement",
		Steps: map[string]*protocol.StepView{"implement": {Status: "in_progress", Attempts: 1}},
	}
	d := newDaemonFixture(t)
	d.answer("GET", "/jobs/wi_abc/observe", jsonBody(jv))
	code, stdout, stderr := runPf(t, map[string]string{"PF_DAEMON_SOCKET": d.sock}, "observe", "wi_abc")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"wi_abc (polyforge) — running", "current step: implement", "implement  in_progress"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}
