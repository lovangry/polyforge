package observe

// monitor_v4v8_test.go: the V8 gate-repair regressions at the monitor
// level (the V4 wrap-flush evidence gate was retired with the flush lane) —
//   - V8: the local-gitlink precheck refusal classifies as
//     push_poisoned_local (never push_failed), and the same-error retry
//     cap halts an async retry round after 5 identical failures until
//     the next commit boundary.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

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
