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
