package observe

// monitor_v4v8_test.go: the V4/V8 gate-repair regressions at the monitor
// level —
//   - V4: the wrap flush's fail-closed evidence gate (an executed
//     verification step without structured command evidence refuses the
//     seal with the typed DAEMON-400, non-terminal; evidence appended →
//     the re-flush seals; an unexecuted verification step is exempt);
//   - V8: the local-gitlink precheck refusal classifies as
//     push_poisoned_local (never push_failed), and the same-error retry
//     cap halts an async retry round after 5 identical failures until
//     the next commit boundary.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// writeExecVerification writes execution.json carrying a verification
// step with the given status and output (the capsule write shape).
func writeExecVerification(t *testing.T, m *Monitor, version int64, status string, output map[string]any) {
	t.Helper()
	step := map[string]any{
		"status":          status,
		"attempts":        []any{},
		"answers":         []any{},
		"budget_consumed": map[string]any{"usd": 0.1},
	}
	if output != nil {
		step["output"] = output
	}
	exec := map[string]any{
		"capsule_id":      "cap_wi_test",
		"work_item_id":    "wi_test",
		"generation":      int64(1),
		"current_step":    "verification",
		"workflow_status": "running",
		"steps":           map[string]any{"verification": step},
		"review_rounds":   map[string]int64{},
		"updated_at":      time.Now().UTC().Format(time.RFC3339Nano),
		"version":         version,
	}
	raw, err := json.Marshal(exec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.wi.CapsuleDir, "runtime-state", "execution.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

var goodEvidence = map[string]any{
	"verdict": "pass",
	"commands": []any{map[string]any{
		"argv":        []any{"go", "test", "./..."},
		"exit_code":   0,
		"duration_ms": 1234,
		"started_at":  "2026-10-01T00:00:00Z",
	}},
}

func TestWrapRefusedWithoutVerificationEvidence(t *testing.T) {
	h := newFakeHandle("att_1")
	var wrapPushes atomic.Int64
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, gitops.PushTimings, error) {
			if strings.HasPrefix(msg, "wrap: ") {
				wrapPushes.Add(1)
			}
			return "sha_ok", true, gitops.PushTimings{}, nil
		}
	})

	// Executed verification step, old-schema output (no commands field):
	// the seal is refused, typed, and names the work item and the step.
	writeExecVerification(t, m, 3, "done", map[string]any{"verdict": "pass"})
	ack := doFlush(m, flushReq(protocol.FlushWrap))
	if ack.OK {
		t.Fatalf("evidence-less wrap must be refused: %+v", ack)
	}
	if !containsAll(ack.Error, "DAEMON-400", "VERIFICATION_EVIDENCE_MISSING", "wi_test", "verification") {
		t.Fatalf("refusal must be the typed DAEMON-400 naming the work item and step: %q", ack.Error)
	}
	if wrapPushes.Load() != 0 {
		t.Fatalf("a refused wrap must attempt no seal push (got %d)", wrapPushes.Load())
	}
	if len(eventsOf(m, EvWrapEvidenceRefused)) != 1 {
		t.Fatalf("one wrap_evidence_refused event, got %d", len(eventsOf(m, EvWrapEvidenceRefused)))
	}
	// NOT terminal: no blocked transition, no OBCompleted, seal guard
	// released (a second wrap is not "already in flight").
	m.stateMu.Lock()
	blocked, sealing := m.blocked, m.sealing
	m.stateMu.Unlock()
	if blocked || sealing {
		t.Fatalf("refused wrap must leave the attempt running (blocked=%v sealing=%v)", blocked, sealing)
	}
	expectNoOB(t, ob, 250*time.Millisecond, "no OBCompleted after a refused wrap")

	// Evidence appended (the agent's fix): the re-flush seals.
	writeExecVerification(t, m, 4, "done", goodEvidence)
	ack2 := doFlush(m, flushReq(protocol.FlushWrap))
	if !ack2.OK || !ack2.Pushed || ack2.LocalCommit != "sha_ok" {
		t.Fatalf("wrap after the evidence fix must seal: %+v", ack2)
	}
	if got := wrapPushes.Load(); got != 1 {
		t.Fatalf("exactly one seal push after the fix, got %d", got)
	}
	msg := waitOB(t, ob, "WICompleted(wrapped)")
	if msg.Kind != protocol.OBCompleted || msg.Reason != protocol.FinishWrapped {
		t.Fatalf("WICompleted: %+v", msg)
	}
}

func TestWrapEvidenceGateExemptions(t *testing.T) {
	// An UNEXECUTED verification step (pending) is outside the M4
	// denominator and outside the gate; unreadable evidence entries do
	// not count as evidence.
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, gitops.PushTimings, error) {
			return "sha_ok", true, gitops.PushTimings{}, nil
		}
	})

	// Pending: exempt.
	writeExecVerification(t, m, 2, "pending", nil)
	if ack := doFlush(m, flushReq(protocol.FlushWrap)); !ack.OK {
		t.Fatalf("pending verification step must not gate the wrap: %+v", ack)
	}

	// Unreadable commands entries (argv empty / exit_code absent):
	// present-but-unreadable is exactly the M4 "no readable entry" shape.
	m2, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, gitops.PushTimings, error) {
			return "sha_ok", true, gitops.PushTimings{}, nil
		}
	})
	badOutput := map[string]any{
		"commands": []any{
			map[string]any{"argv": []any{}, "exit_code": 0, "duration_ms": 5},
			map[string]any{"argv": "go test ./...", "duration_ms": 5},
			map[string]any{"argv": []any{"go", "vet"}, "exit_code": nil, "duration_ms": 5},
		},
	}
	writeExecVerification(t, m2, 2, "done", badOutput)
	if ack := doFlush(m2, flushReq(protocol.FlushWrap)); ack.OK {
		t.Fatalf("unreadable evidence entries must refuse the wrap: %+v", ack)
	}
}

func TestPushPoisonedLocalClassifiesItsOwnEvent(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, gitops.PushTimings, error) {
			return "", false, gitops.PushTimings{}, &api.Error{
				Code:   api.ErrGitGitlink,
				Detail: "LOCAL_GITLINK_DETECTED: staged gitlink (mode 160000) at poison — cleanup omitted",
			}
		}
	})
	m.writeExec(t, 1, "code")
	soon(t, 3*time.Second, "a poisoned-local classification event", func() bool {
		return len(eventsOf(m, EvPushPoisonedLocal)) >= 1
	})
	if n := len(eventsOf(m, EvPushFailed)); n != 0 {
		t.Fatalf("the local precheck refusal must never surface as push_failed, got %d", n)
	}
	// The event carries the not-an-M8-failure rationale for the reader.
	e := eventsOf(m, EvPushPoisonedLocal)[0]
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(e.Body, &body); err != nil {
		t.Fatalf("body: %v (%s)", err, e.Body)
	}
	if !containsAll(body.Error, "LOCAL_GITLINK_DETECTED") || !containsAll(body.Detail, "M8") {
		t.Fatalf("event must carry the error and the exclusion rationale: %+v", body)
	}
}

func TestSameErrorCapHaltsRoundUntilNextBoundary(t *testing.T) {
	h := newFakeHandle("att_1")
	var failures atomic.Int64
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.BackpressureSteps = 1000 // keep the steps ladder out of this test
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, gitops.PushTimings, error) {
			failures.Add(1)
			return "", false, gitops.PushTimings{}, errors.New("GIT-500: pre-receive rejected the exact same commit again")
		}
	})

	m.writeExec(t, 1, "code")
	soon(t, 3*time.Second, "the round runs to its cap", func() bool {
		return failures.Load() >= sameErrorCap
	})
	// The halt: one degraded event with reason same_error, and the
	// attempt count freezes at the cap (no 6th push) even though
	// dirtyVersion > pushedVersion keeps the condition armed.
	soon(t, time.Second, "same_error degradation event", func() bool {
		for _, e := range eventsOf(m, EvPushBackpressure) {
			if reasonOf(e.Body) == "same_error" {
				return true
			}
		}
		return false
	})
	time.Sleep(150 * time.Millisecond)
	if got := failures.Load(); got != sameErrorCap {
		t.Fatalf("halted round must attempt exactly %d pushes, got %d", sameErrorCap, got)
	}

	// The next commit boundary (a fresh execution.json Version) re-arms
	// the round: attempts resume (and re-halt at their own cap).
	m.writeExec(t, 2, "code")
	soon(t, 3*time.Second, "round resumes at the next boundary", func() bool {
		return failures.Load() > sameErrorCap
	})
	// And the resumed round is itself bounded.
	time.Sleep(150 * time.Millisecond)
	if got := failures.Load(); got > 2*sameErrorCap {
		t.Fatalf("resumed round must re-halt at its own cap (%d), got %d", 2*sameErrorCap, got)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
