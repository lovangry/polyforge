package observe

// fake_test.go: the test doubles. The agent record fixtures are the
// PINNED REAL RECORD SHAPES (frozen v0 capture; the v0 frozen v0 capture test
// double emits these exact twins) — see stream.go's header for the
// full burst sequence.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// --- pinned record fixtures (the frozen real-stream shapes) ---

const (
	fxResponse    = `{"id":"pf-1","type":"response","command":"prompt","success":true}`
	fxAgentStart  = `{"type":"agent_start"}`
	fxTurnStart   = `{"type":"turn_start"}`
	fxMsgStart    = `{"type":"message_start","message":{"role":"assistant","stopReason":"pending","content":[]}}`
	fxTextDelta   = `{"type":"message_update","usage":{"input":0,"output":2,"totalTokens":2},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"ok"}}`
	fxMsgEndStop  = `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"ok"}]}}`
	fxTurnEndStop = `{"type":"turn_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"ok"}]},"toolResults":[]}`
	fxAgentEnd    = `{"type":"agent_end","messages":[],"willRetry":false}`
	fxSettled     = `{"type":"agent_settled"}`
)

// A provider failure rides the ORDINARY records: stopReason=="error" +
// errorMessage, byte-shaped like the real capture minus vendor detail.
func fxMsgEndErr(detail string) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":%q,"content":[]}}`, detail)
}

func fxTurnEndErr(detail string) string {
	return fmt.Sprintf(`{"type":"turn_end","message":{"role":"assistant","stopReason":"error","errorMessage":%q,"content":[]},"toolResults":[]}`, detail)
}

// feed emits one record on the handle's stream.
func feed(h *fakeHandle, rec string) {
	h.records <- []byte(rec)
}

// --- fakeHandle ---

type fakeHandle struct {
	records chan []byte
	exit    chan struct{}

	mu     sync.Mutex
	sends  []protocol.ControlMsg
	sendFn func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error)
}

// newFakeHandle builds one fake agent handle. The label argument is
// documentation only (which derivation the handle plays in the test) —
// the v1.5 fence key is the spawn counter carried on the RT messages,
// not anything the handle owns.
func newFakeHandle(label string) *fakeHandle {
	return &fakeHandle{
		records: make(chan []byte, 256),
		exit:    make(chan struct{}),
	}
}

func (h *fakeHandle) Exit() <-chan struct{}  { return h.exit }
func (h *fakeHandle) Records() <-chan []byte { return h.records }

func (h *fakeHandle) SendControl(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error) {
	h.mu.Lock()
	fn := h.sendFn
	h.sends = append(h.sends, msg)
	h.mu.Unlock()
	if fn == nil {
		return protocol.ControlAck{Method: msg.Method, Seq: msg.Seq, OK: true}, nil
	}
	return fn(ctx, msg)
}

func (h *fakeHandle) sentCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sends)
}

func (h *fakeHandle) setSend(fn func(ctx context.Context, msg protocol.ControlMsg) (protocol.ControlAck, error)) {
	h.mu.Lock()
	h.sendFn = fn
	h.mu.Unlock()
}

// dying marks the process as exited (confirmed death signal).
func (h *fakeHandle) die() { close(h.exit) }

// --- monitor construction ---

func testCfg() monitorCfg {
	return monitorCfg{
		PollInterval: 5 * time.Millisecond,
		HangWindow:   40 * time.Millisecond,
		// IdleWindow: inert for every test that does not override it —
		// the idle watchdog tests below shrink it explicitly.
		IdleWindow:        time.Hour,
		ProbeTimeout:      80 * time.Millisecond,
		AnswerAckWindow:   50 * time.Millisecond,
		PushRetryBackoff:  2 * time.Millisecond,
		BackpressureSteps: 5,
		BackpressureAfter: 30 * time.Minute,
		FlushBound:        time.Second,
		push: func(ctx context.Context, o gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
			return "sha_default", true, gitops.PushTimings{}, nil
		},
	}
}

// newTestMonitor builds a monitor wired to a temp capsule + events log
// and runs it. The returned cleanup stops it.
func newTestMonitor(t *testing.T, h Handle, mod func(*monitorCfg)) (*Monitor, chan protocol.OBMsg) {
	t.Helper()
	dir := t.TempDir()
	log, err := NewEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	capsuleDir := filepath.Join(dir, "capsule")
	if err := os.MkdirAll(filepath.Join(capsuleDir, "runtime-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	ob := make(chan protocol.OBMsg, 64)
	cfg := testCfg()
	if mod != nil {
		mod(&cfg)
	}
	m := &Monitor{
		wi: WIInfo{
			WorkItem:   "wi_test",
			Project:    "proj",
			CapsuleDir: capsuleDir,
			BundleName: "bundle-a",
		},
		cfg:        cfg,
		log:        log,
		ob:         ob,
		ledger:     NewAnswerLedger(),
		rtCh:       make(chan Attach, 4),
		stopCh:     make(chan struct{}),
		flushCh:    make(chan FlushCall, 8),
		gate:       make(chan struct{}, 1),
		pushDone:   make(chan pushResult, 1),
		wrapDone:   make(chan wrapResult, 1),
		probeDone:  make(chan probeResult, 1),
		verifyDone: make(chan verifyResult, 1),
		hangAnchor: time.Now(),
		stream:     newStreamState(),
	}
	m.execPath = m.execJSONPath()
	m.attach(Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: "wi_test", SpawnN: 1},
		Handle: h,
		WI:     m.wi,
	})
	go m.run()
	t.Cleanup(func() {
		m.requestStop()
		_ = log.Close()
	})
	return m, ob
}

// writeExec writes capsule runtime-state/execution.json with the given
// version and current step (atomic tmp+rename, the capsule write shape).
func (m *Monitor) writeExec(t *testing.T, version int64, currentStep string) {
	t.Helper()
	exec := map[string]any{
		"capsule_id":      "cap_wi_test",
		"work_item_id":    "wi_test",
		"generation":      int64(1),
		"current_step":    currentStep,
		"workflow_status": "running",
		"steps": map[string]any{
			currentStep: map[string]any{
				"status":          "in_progress",
				"attempts":        []any{},
				"answers":         []any{},
				"budget_consumed": map[string]any{"usd": 0.1},
			},
		},
		"review_rounds": map[string]int64{},
		"updated_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"version":       version,
	}
	raw, err := json.Marshal(exec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.wi.CapsuleDir, "runtime-state", "execution.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// force a distinct mtime from any previous write
	os.Chtimes(tmp, time.Now(), time.Now())
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// waitOB reads one OB message or fails the test on timeout.
func waitOB(t *testing.T, ob chan protocol.OBMsg, what string) protocol.OBMsg {
	t.Helper()
	select {
	case msg := <-ob:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for OB message %s", what)
		return protocol.OBMsg{}
	}
}

// expectNoOB asserts no OB message arrives within d.
func expectNoOB(t *testing.T, ob chan protocol.OBMsg, d time.Duration, what string) {
	t.Helper()
	select {
	case msg := <-ob:
		t.Fatalf("unexpected OB message %s: %+v", what, msg)
	case <-time.After(d):
	}
}

// soon polls cond until it holds or fails after the bound.
func soon(t *testing.T, bound time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met in time: %s", what)
}

// eventsOf reads the monitor's ring, filtered by kind.
func eventsOf(m *Monitor, kind string) []protocol.EventEntry {
	var out []protocol.EventEntry
	for _, e := range m.log.Since(m.wi.WorkItem) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// eventBodyInt64 reads one integer field out of an event body, failing
// the test when the key is ABSENT: presence — not mere decodability — is
// what the timing fields (published's commit_ms/push_ms, remember's
// write_ms) must guarantee.
func eventBodyInt64(t *testing.T, e protocol.EventEntry, key string) int64 {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(e.Body, &body); err != nil {
		t.Fatalf("%s body must decode: %v", e.Kind, err)
	}
	raw, ok := body[key]
	if !ok {
		t.Fatalf("%s body must carry %q, got %s", e.Kind, key, e.Body)
	}
	n, ok := raw.(float64)
	if !ok {
		t.Fatalf("%s body's %q must be a number, got %T", e.Kind, key, raw)
	}
	return int64(n)
}

// incidentKinds reads the infra_incident payload kinds off the ring.
func incidentKinds(m *Monitor) []string {
	var out []string
	for _, e := range eventsOf(m, EvInfraIncident) {
		var body map[string]any
		if json.Unmarshal(e.Body, &body) == nil {
			if k, _ := body["kind"].(string); k != "" {
				out = append(out, k)
			}
		}
	}
	return out
}

// hasIncidentKind reports whether an infra_incident of that payload kind
// was written.
func hasIncidentKind(m *Monitor, kind string) bool {
	for _, k := range incidentKinds(m) {
		if k == kind {
			return true
		}
	}
	return false
}
