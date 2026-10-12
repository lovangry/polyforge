package observe

// http_test.go: the UDS routes end-to-end through the hub — observe
// (JobView) and answer (the no-relay refusal) — plus 404 semantics.

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
		PushFunc: func(ctx context.Context, o gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
			return "sha_http", true, gitops.PushTimings{}, nil
		},
		PollInterval: 10 * time.Millisecond,
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
		Msg:    protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: "wi_http", SpawnN: 1},
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

	// A pending question persisted in execution.json (the driver's
	// write) surfaces through the monitor's poll.
	m := hs.o.lookup("wi_http")
	if m == nil {
		t.Fatal("monitor not registered")
	}
	writePendingQuestion(t, m, "q_http", "proceed?", "code")

	// The observe view now shows needs_input.
	_, body = hs.get(t, "/jobs/wi_http/observe")
	_ = json.Unmarshal(body, &view)
	if view.Status != protocol.JobNeedsInput || view.Pending == nil {
		t.Fatalf("view after needs_input: %+v", view)
	}

	// answer: the relay lane is deleted (polyforge#106) — the monitored
	// work item's answer is refused with the typed no-bridge-handle 409
	// (the fail-fast posture; the driver branch answers on the daemon
	// mux before this face is ever reached).
	code, body = hs.post(t, "/jobs/wi_http/answer", map[string]string{"question_id": "q_http"}, nil)
	if code != http.StatusConflict {
		t.Fatalf("answer on the relayless face: %d %s", code, body)
	}
	if !bytes.Contains(body, []byte("bridge")) {
		t.Fatalf("the answer refusal must name the missing bridge handle: %s", body)
	}

}

func TestHTTPUnknownWorkItemIs404(t *testing.T) {
	hs := newHarness(t)
	code, _ := hs.get(t, "/jobs/wi_nope/observe")
	if code != http.StatusNotFound {
		t.Fatalf("observe unknown: %d", code)
	}
	code, _ = hs.post(t, "/jobs/wi_nope/answer", map[string]string{}, nil)
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
	hs.rt <- Attach{Msg: protocol.RTMsg{Kind: protocol.RTFinished, WorkItem: "wi_http"}}
	soon(t, time.Second, "monitor removed", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusNotFound
	})
}
