package observe

// memory_test.go: the F6-B UDS memory channel — the two routes behind the
// flush-mode attempt fence: the happy paths, the STALE_ATTEMPT refusals
// (a zombie survivor, an absent id), the closed field-set validation,
// the 404 for an unmonitored work item, and the not-configured refusal
// when no MemoryFace is bound.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// fakeMemFace records every call and answers canned results.
type fakeMemFace struct {
	recalls []struct {
		wi    string
		query string
		topK  int
	}
	remembers []MemoryRemember
	fail      error // when non-nil, every call fails with this
}

func (f *fakeMemFace) Recall(wi, query string, topK int) ([]MemoryHit, error) {
	f.recalls = append(f.recalls, struct {
		wi    string
		query string
		topK  int
	}{wi, query, topK})
	if f.fail != nil {
		return nil, f.fail
	}
	return []MemoryHit{
		{ID: "mem_1", Type: "experience.debug", Title: "l1", Excerpt: "lesson one", Tags: []string{"x"}, CreatedAt: "t1"},
	}, nil
}

func (f *fakeMemFace) Remember(wi string, req MemoryRemember) (MemoryStored, error) {
	f.remembers = append(f.remembers, req)
	if f.fail != nil {
		return MemoryStored{}, f.fail
	}
	return MemoryStored{ID: "mem_new", Status: "created"}, nil
}

// newMemHarness is newHarness with a MemoryFace bound.
func newMemHarness(t *testing.T, face MemoryFace) *harness {
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
			return "sha_mem", true, nil
		},
		CallTimeout: 500 * time.Millisecond,
		MemoryFace:  face,
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

func TestMemoryRecallRoute(t *testing.T) {
	face := &fakeMemFace{}
	hs := newMemHarness(t, face)
	h := newFakeHandle("att_mem")
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})

	// A stale attempt id (a zombie survivor) is refused exactly like a
	// flush: DAEMON-409 STALE_ATTEMPT, and the face never sees the call.
	code, body := hs.post(t, "/jobs/wi_http/recall", map[string]any{
		"attempt_id": "att_old", "query": "lessons", "top_k": 3,
	}, nil)
	if code != http.StatusConflict || !jsonContains(body, "STALE_ATTEMPT") {
		t.Fatalf("stale recall: %d %s", code, body)
	}
	// An absent attempt id is stale too.
	code, body = hs.post(t, "/jobs/wi_http/recall", map[string]any{
		"query": "lessons",
	}, nil)
	if code != http.StatusConflict {
		t.Fatalf("anonymous recall: %d %s", code, body)
	}
	if len(face.recalls) != 0 {
		t.Fatalf("fenced recalls reached the face: %+v", face.recalls)
	}

	// The live attempt's recall answers the hits and defaults top_k.
	code, body = hs.post(t, "/jobs/wi_http/recall", map[string]any{
		"attempt_id": "att_mem", "query": "flaky retry ladder",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("live recall: %d %s", code, body)
	}
	var out struct {
		Items []MemoryHit `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("recall answer decode: %v (%s)", err, body)
	}
	if len(out.Items) != 1 || out.Items[0].ID != "mem_1" || out.Items[0].Excerpt != "lesson one" {
		t.Fatalf("recall items = %+v", out.Items)
	}
	if len(face.recalls) != 1 || face.recalls[0].wi != "wi_http" ||
		face.recalls[0].query != "flaky retry ladder" || face.recalls[0].topK != 10 {
		t.Fatalf("face saw %+v", face.recalls)
	}

	// An empty query is DAEMON-400, and an unmonitored work item is 404.
	code, _ = hs.post(t, "/jobs/wi_http/recall", map[string]any{
		"attempt_id": "att_mem", "query": "  ",
	}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("empty-query recall: %d", code)
	}
	code, _ = hs.post(t, "/jobs/wi_none/recall", map[string]any{
		"attempt_id": "att_mem", "query": "x",
	}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("unmonitored recall: %d", code)
	}

	// A face error is surfaced verbatim when typed (MEM-*/CORE-*
	// transparency) and the bookkeeping event landed either way.
	face.fail = &api.Error{Code: api.ErrMemForbidden, Detail: "no access"}
	code, body = hs.post(t, "/jobs/wi_http/recall", map[string]any{
		"attempt_id": "att_mem", "query": "again",
	}, nil)
	if code != http.StatusForbidden || !jsonContains(body, api.ErrMemForbidden) {
		t.Fatalf("typed face error must surface verbatim: %d %s", code, body)
	}
	var sawEvent bool
	for _, e := range hs.o.Events().Since("wi_http") {
		if e.Kind == EvMemoryRecall {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Fatal("no memory_recall event was appended")
	}
}

func TestMemoryRememberRoute(t *testing.T) {
	face := &fakeMemFace{}
	hs := newMemHarness(t, face)
	h := newFakeHandle("att_mem")
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})

	// The fence first: a stale id never reaches the face.
	code, body := hs.post(t, "/jobs/wi_http/remember", map[string]any{
		"attempt_id": "att_old", "type": "experience.debug", "content": "c",
	}, nil)
	if code != http.StatusConflict || !jsonContains(body, "STALE_ATTEMPT") {
		t.Fatalf("stale remember: %d %s", code, body)
	}

	// The closed field set: content and type are required, visibility is
	// validated against the closed set.
	for name, bad := range map[string]map[string]any{
		"no content":  {"attempt_id": "att_mem", "type": "experience.debug"},
		"no type":     {"attempt_id": "att_mem", "content": "c"},
		"bad visible": {"attempt_id": "att_mem", "type": "t", "content": "c", "visibility": "everyone"},
	} {
		code, _ := hs.post(t, "/jobs/wi_http/remember", bad, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d", name, code)
		}
	}
	if len(face.remembers) != 0 {
		t.Fatalf("invalid remembers reached the face: %+v", face.remembers)
	}

	// The live attempt's remember stores and answers the id; visibility
	// defaults to project.
	code, body = hs.post(t, "/jobs/wi_http/remember", map[string]any{
		"attempt_id": "att_mem", "type": "experience.debug",
		"content": "the retry ladder must be bounded", "tags": []string{"infra"},
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("live remember: %d %s", code, body)
	}
	var stored MemoryStored
	if err := json.Unmarshal(body, &stored); err != nil || stored.ID != "mem_new" || stored.Status != "created" {
		t.Fatalf("remember answer: %v (%s)", err, body)
	}
	if len(face.remembers) != 1 || face.remembers[0].Visibility != "project" ||
		face.remembers[0].Content != "the retry ladder must be bounded" {
		t.Fatalf("face saw %+v", face.remembers)
	}
	var sawEvent bool
	for _, e := range hs.o.Events().Since("wi_http") {
		if e.Kind == EvMemoryRemember {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Fatal("no memory_remember event was appended")
	}
}

// No bound MemoryFace disables both routes with the typed refusal — a
// daemon built without the memory lanes answers honestly instead of
// proxying into a nil seam.
func TestMemoryRoutesNotConfigured(t *testing.T) {
	hs := newMemHarness(t, nil)
	h := newFakeHandle("att_mem")
	hs.start(t, h)
	soon(t, time.Second, "job registered", func() bool {
		code, _ := hs.get(t, "/jobs/wi_http/observe")
		return code == http.StatusOK
	})
	code, _ := hs.post(t, "/jobs/wi_http/recall", map[string]any{
		"attempt_id": "att_mem", "query": "q",
	}, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("recall without a face: %d", code)
	}
	code, _ = hs.post(t, "/jobs/wi_http/remember", map[string]any{
		"attempt_id": "att_mem", "type": "t", "content": "c",
	}, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("remember without a face: %d", code)
	}
}

func jsonContains(raw []byte, needle string) bool {
	return strings.Contains(string(raw), needle)
}
