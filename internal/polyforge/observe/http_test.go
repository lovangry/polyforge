package observe

// http_test.go: the three UDS routes end-to-end through the hub —
// observe (JobView), answer (relay) and flush — plus 404 semantics.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

type harness struct {
	o          *Observe
	rt         chan Attach
	ob         chan protocol.OBMsg
	srv        *httptest.Server
	capsuleDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	rt := make(chan Attach, 8)
	ob := make(chan protocol.OBMsg, 64)
	dataDir := t.TempDir()
	capsuleDir := filepath.Join(t.TempDir(), "capsule")
	if err := os.MkdirAll(filepath.Join(capsuleDir, "runtime-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	o, err := New(Config{
		RT:      rt,
		OB:      ob,
		DataDir: dataDir,
		PushFunc: func(ctx context.Context, o gitops.Opts, message string) (string, bool, error) {
			return "sha_http", true, nil
		},
		CallTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	srv := httptest.NewServer(o.Handler())
	t.Cleanup(func() {
		cancel()
		srv.Close()
	})
	return &harness{o: o, rt: rt, ob: ob, srv: srv, capsuleDir: capsuleDir}
}

func (hs *harness) start(t *testing.T, h Handle) {
	t.Helper()
	hs.rt <- Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: "wi_http", AttemptID: h.AttemptID()},
		Handle: h,
		WI: WIInfo{
			WorkItem: "wi_http", Project: "proj", CapsuleDir: hs.capsuleDir,
			BundleName: "bundle-x",
		},
	}
}

func (hs *harness) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(hs.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func (hs *harness) post(t *testing.T, path string, body any, hdr map[string]string) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, hs.srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func TestHTTPRoutesEndToEnd(t *testing.T) {
	h := newFakeHandle("att_http")
	hs := newHarness(t)
	hs.start(t, h)

	// GET observe: the monitor must be registered and serving.
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})
	code, body := hs.get(t, "/jobs/wi_http/observe")
	if code != http.StatusOK {
		t.Fatalf("observe: %d %s", code, body)
	}
	var view protocol.JobView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("JobView decode: %v (%s)", err, body)
	}
	if view.WorkItem != "wi_http" || view.BundleName != "bundle-x" {
		t.Fatalf("view: %+v", view)
	}

	// flush(needs_input): the agent UDS face entry.
	flushBody := protocol.FlushRequest{
		WorkItem: "wi_http", AttemptID: "att_http",
		Reason: protocol.FlushNeedsInput, QuestionID: "q_http", Question: "proceed?",
	}
	code, body = hs.post(t, "/jobs/wi_http/flush", flushBody, nil)
	if code != http.StatusOK {
		t.Fatalf("flush: %d %s", code, body)
	}
	var ack protocol.FlushAck
	if err := json.Unmarshal(body, &ack); err != nil || !ack.OK {
		t.Fatalf("flush ack: %v %s", err, body)
	}

	// The observe view now shows needs_input.
	_, body = hs.get(t, "/jobs/wi_http/observe")
	_ = json.Unmarshal(body, &view)
	if view.Status != protocol.JobNeedsInput || view.Pending == nil {
		t.Fatalf("view after needs_input: %+v", view)
	}

	// answer: correct caller → consumed.
	relay := AnswerRelay{
		QuestionID: "q_http", AnswerID: "a_http", AnswerText: "yes",
		AnsweredAt: "2026-01-01T00:00:00Z", AnsweredBy: "alice",
	}
	code, body = hs.post(t, "/jobs/wi_http/answer", relay, map[string]string{"X-PF-User": "alice"})
	if code != http.StatusOK {
		t.Fatalf("answer: %d %s", code, body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if out["outcome"] != "consumed" {
		t.Fatalf("outcome: %s", body)
	}

	// answer: wrong question binding → DAEMON-409.
	bad := relay
	bad.QuestionID = "q_other"
	bad.AnswerID = "a_bad"
	code, body = hs.post(t, "/jobs/wi_http/answer", bad, map[string]string{"X-PF-User": "alice"})
	if code != http.StatusConflict {
		t.Fatalf("binding refusal: %d %s", code, body)
	}

	// answer: identity mismatch → DAEMON-403.
	id := relay
	id.AnswerID = "a_id"
	code, _ = hs.post(t, "/jobs/wi_http/answer", id, map[string]string{"X-PF-User": "mallory"})
	if code != http.StatusForbidden {
		t.Fatalf("identity refusal: %d", code)
	}

	// flush(wrap): the ack distinguishes local commit and push.
	code, body = hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", AttemptID: "att_http",
		Reason: protocol.FlushWrap, FinalStatus: protocol.FinishWrapped,
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("wrap flush: %d %s", code, body)
	}
	_ = json.Unmarshal(body, &ack)
	if !ack.OK || ack.LocalCommit != "sha_http" || !ack.Pushed {
		t.Fatalf("wrap ack: %+v (%s)", ack, body)
	}
	msg := waitOB(t, hs.ob, "WICompleted via HTTP path")
	if msg.Kind != protocol.OBCompleted {
		t.Fatalf("OB message: %+v", msg)
	}
}

func TestHTTPUnknownWorkItemIs404(t *testing.T) {
	hs := newHarness(t)
	code, _ := hs.get(t, "/jobs/wi_nope/observe")
	if code != http.StatusNotFound {
		t.Fatalf("observe unknown: %d", code)
	}
	code, _ = hs.post(t, "/jobs/wi_nope/answer", AnswerRelay{}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("answer unknown: %d", code)
	}
}

func TestHTTPObserveAfterFinishIs404(t *testing.T) {
	h := newFakeHandle("att_fin")
	hs := newHarness(t)
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})

	// RTFinished tears the monitor down; the lane's routes answer 404.
	hs.rt <- Attach{Msg: protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: "wi_http", AttemptID: "att_fin"}}
	soon(t, time.Second, "monitor removed", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusNotFound
	})
}

func TestHTTPFlushRejectsBadReason(t *testing.T) {
	h := newFakeHandle("att_bad")
	hs := newHarness(t)
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})
	code, _ := hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{Reason: "publish_request"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown reason: %d", code)
	}
}

// TestHTTPFlushStaleAttemptRefused (#8): the flush face is attempt-fenced.
// A zombie survivor of a superseded instance — or any request not naming
// the monitor's CURRENT instance — answers DAEMON-409 STALE_ATTEMPT and
// never reaches the monitor: no seal, no OBCompleted, and the live
// attempt's own flush still works afterwards.
func TestHTTPFlushStaleAttemptRefused(t *testing.T) {
	h := newFakeHandle("att_cur")
	hs := newHarness(t)
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})

	// (1) flush(wrap) from a superseded instance's id: 409 STALE_ATTEMPT.
	code, body := hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", AttemptID: "att_old", // a zombie of the same claim
		Reason: protocol.FlushWrap, FinalStatus: protocol.FinishWrapped,
	}, nil)
	if code != http.StatusConflict {
		t.Fatalf("stale wrap flush: %d %s", code, body)
	}
	if !bytes.Contains(body, []byte("STALE_ATTEMPT")) {
		t.Fatalf("stale wrap flush detail must name STALE_ATTEMPT: %s", body)
	}

	// (2) a flush with NO attempt id is stale too (the fence requires it).
	code, body = hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", Reason: protocol.FlushNeedsInput,
		QuestionID: "q_stale", Question: "which?",
	}, nil)
	if code != http.StatusConflict {
		t.Fatalf("anonymous needs_input flush: %d %s", code, body)
	}

	// (3) the current attempt was untouched: no OB message, no seal, no
	// pending question from the refused flush, job still running.
	expectNoOB(t, hs.ob, 150*time.Millisecond, "OBCompleted from a stale flush")
	_, body = hs.get(t, "/jobs/wi_http/observe")
	var view protocol.JobView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("view decode: %v (%s)", err, body)
	}
	if view.Status != protocol.JobRunning || view.Pending != nil {
		t.Fatalf("stale flush leaked into the live attempt: %+v", view)
	}
	for _, e := range hs.o.Events().Since("wi_http") {
		if e.Kind == EvNeedsInput && bytes.Contains(e.Body, []byte("q_stale")) {
			t.Fatalf("refused flush still recorded its question: %+v", e)
		}
	}

	// (4) the LIVE attempt's flushes still work: needs_input lands, and
	// the wrap seals exactly once for the current instance.
	code, body = hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", AttemptID: "att_cur",
		Reason: protocol.FlushNeedsInput, QuestionID: "q_live", Question: "which?",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("live needs_input flush: %d %s", code, body)
	}
	code, body = hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", AttemptID: "att_cur",
		Reason: protocol.FlushWrap, FinalStatus: protocol.FinishWrapped,
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("live wrap flush: %d %s", code, body)
	}
	var ack protocol.FlushAck
	if err := json.Unmarshal(body, &ack); err != nil || !ack.OK {
		t.Fatalf("live wrap ack: %v (%s)", err, body)
	}
	msg := waitOB(t, hs.ob, "WICompleted from the live attempt")
	if msg.Kind != protocol.OBCompleted || msg.AttemptID != "att_cur" {
		t.Fatalf("WICompleted: %+v", msg)
	}
}

// TestHTTPFlushRecoveryWindowNotStale (the #8 fence's recovery window): a
// flush whose attempt id names the hub's LATEST KNOWN instance — the id
// RTRecovered announced, which the monitor's adoption lags — must NOT be
// refused as STALE_ATTEMPT: it is the just-resumed agent's own lifeline.
// The call is queued by the monitor until the adoption completes (or
// answered once the id is current), while every OLDER generation's id
// stays fenced exactly as before.
func TestHTTPFlushRecoveryWindowNotStale(t *testing.T) {
	h := newFakeHandle("att_cur")
	hs := newHarness(t)
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})

	// (1) the recovery window, adoption NOT yet delivered: the hub knows
	// the announced id (emission-time registration), the monitor does
	// not. The flush is queued, not refused — it must not answer the
	// instant STALE_ATTEMPT 409; it settles only after adoption, so the
	// bounded caller sees the settle-timeout instead (CallTimeout here).
	hs.o.NoteInstance("wi_http", "att_next")
	start := time.Now()
	code, body := hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", AttemptID: "att_next",
		Reason: protocol.FlushWrap, FinalStatus: protocol.FinishWrapped,
	}, nil)
	if code == http.StatusConflict && bytes.Contains(body, []byte("STALE_ATTEMPT")) {
		t.Fatalf("recovery-window flush was refused as stale: %d %s", code, body)
	}
	if code != http.StatusInternalServerError || !bytes.Contains(body, []byte("settle")) {
		t.Fatalf("queued flush before adoption: %d %s, want the settle-timeout shape", code, body)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatalf("queued flush answered too fast (%v): it must wait out the adoption window", time.Since(start))
	}

	// (2) the adoption lands: from here the announced id is current, and
	// its flushes take the ordinary path.
	h2 := newFakeHandle("att_next")
	hs.rt <- Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_http", AttemptID: "att_next"},
		Handle: h2,
	}
	code, body = hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
		WorkItem: "wi_http", AttemptID: "att_next",
		Reason: protocol.FlushNeedsInput, QuestionID: "q_next", Question: "which?",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("post-adoption needs_input flush: %d %s", code, body)
	}

	// (3) the fence did not widen: every OLDER generation — the
	// pre-recovery current id included — is still refused.
	for _, stale := range []string{"att_old", "att_cur"} {
		code, body = hs.post(t, "/jobs/wi_http/flush", protocol.FlushRequest{
			WorkItem: "wi_http", AttemptID: stale,
			Reason: protocol.FlushNeedsInput, QuestionID: "q_z_" + stale, Question: "which?",
		}, nil)
		if code != http.StatusConflict || !bytes.Contains(body, []byte("STALE_ATTEMPT")) {
			t.Fatalf("older id %q after recovery: %d %s, want 409 STALE_ATTEMPT", stale, code, body)
		}
	}
}
