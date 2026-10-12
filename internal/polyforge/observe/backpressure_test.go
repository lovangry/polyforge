package observe

// backpressure_test.go: §九 — consecutive failed pushes (N=5) and the
// time-since-last-success bound (T) each write ONE degradation event and
// NEVER kill the agent; the monitor keeps running and the next success
// resets the episode.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

func TestBackpressureFiveFailedPushesWriteDegradationEvent(t *testing.T) {
	h := newFakeHandle("att_1")
	var failures atomic.Int64
	var fail atomic.Bool
	fail.Store(true)
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, gitops.PushTimings, error) {
			if fail.Load() {
				failures.Add(1)
				return "sha_local", false, gitops.PushTimings{}, errors.New("GIT-500: upstream refused")
			}
			return "sha_ok", true, gitops.PushTimings{}, nil
		}
	})

	m.writeExec(t, 1, "code")
	// The retry loop drives failed pushes: at the 5th consecutive
	// failure the steps-degradation event fires, exactly once.
	soon(t, 3*time.Second, "5 failed pushes", func() bool {
		return failures.Load() >= 5
	})
	soon(t, time.Second, "steps-reason degradation event", func() bool {
		return len(backpressureByReason(m, "steps")) >= 1
	})
	// The agent is NOT killed and the monitor still consumes facts.
	m.link.Fact(supervise.StepStarted{Step: "code", SpawnN: 1, StartedAt: time.Now()})
	soon(t, 300*time.Millisecond, "monitor alive during degradation", func() bool {
		return m.activity().InDispatch
	})

	// Recovery of the push path resets the episode: no further
	// steps-reason degradation events fire even as pushes continue. (The
	// same-error cap may add its own reason="same_error" event at the
	// streak bound — a different signal, counted separately below.)
	before := len(backpressureByReason(m, "steps"))
	fail.Store(false)
	m.writeExec(t, 2, "code")
	soon(t, time.Second, "successful push after degradation", func() bool {
		return len(eventsOf(m, EvPublished)) >= 1
	})
	time.Sleep(100 * time.Millisecond)
	if got := len(backpressureByReason(m, "steps")); got != before {
		t.Fatalf("steps-reason degradation events after recovery: got %d, want %d", got, before)
	}
}

func TestBackpressureTimeBoundWritesDegradationEvent(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.BackpressureSteps = 1000 // keep the steps bound out of this test
		c.BackpressureAfter = 40 * time.Millisecond
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, gitops.PushTimings, error) {
			return "sha_local", false, gitops.PushTimings{}, errors.New("GIT-500: upstream refused")
		}
	})

	m.writeExec(t, 1, "code")
	soon(t, 3*time.Second, "time-bound degradation event", func() bool {
		for _, e := range eventsOf(m, EvPushBackpressure) {
			if reasonOf(e.Body) == "time" {
				return true
			}
		}
		return false
	})
}

func reasonOf(body []byte) string {
	var b struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(body, &b)
	return b.Reason
}

// backpressureByReason reads the monitor's push_backpressure events
// filtered by reason (steps / time / same_error are different signals
// over the same event kind).
func backpressureByReason(m *Monitor, reason string) []protocol.EventEntry {
	var out []protocol.EventEntry
	for _, e := range eventsOf(m, EvPushBackpressure) {
		if reasonOf(e.Body) == reason {
			out = append(out, e)
		}
	}
	return out
}
