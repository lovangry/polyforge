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

	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

func TestBackpressureFiveFailedPushesWriteDegradationEvent(t *testing.T) {
	h := newFakeHandle("att_1")
	var failures atomic.Int64
	var fail atomic.Bool
	fail.Store(true)
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, error) {
			if fail.Load() {
				failures.Add(1)
				return "sha_local", false, errors.New("GIT-500: upstream refused")
			}
			return "sha_ok", true, nil
		}
	})

	m.writeExec(t, 1, "code")
	// The retry loop drives failed pushes: at the 5th consecutive
	// failure the steps-degradation event fires, exactly once.
	soon(t, 3*time.Second, "5 failed pushes", func() bool {
		return failures.Load() >= 5
	})
	soon(t, time.Second, "backpressure degradation event", func() bool {
		return len(eventsOf(m, EvPushBackpressure)) >= 1
	})
	// The agent is NOT killed and the monitor still classifies records.
	feed(h, fxTextDelta)
	soon(t, 300*time.Millisecond, "monitor alive during degradation", func() bool {
		return m.stream.heartbeat().Records >= 1
	})

	// Recovery of the push path resets the episode: no further
	// degradation events fire even as pushes continue.
	before := len(eventsOf(m, EvPushBackpressure))
	fail.Store(false)
	m.writeExec(t, 2, "code")
	soon(t, time.Second, "successful push after degradation", func() bool {
		return len(eventsOf(m, EvPublished)) >= 1
	})
	time.Sleep(100 * time.Millisecond)
	if got := len(eventsOf(m, EvPushBackpressure)); got != before {
		t.Fatalf("degradation events after recovery: got %d, want %d", got, before)
	}
}

func TestBackpressureTimeBoundWritesDegradationEvent(t *testing.T) {
	h := newFakeHandle("att_1")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.BackpressureSteps = 1000 // keep the steps bound out of this test
		c.BackpressureAfter = 40 * time.Millisecond
		c.push = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, error) {
			return "sha_local", false, errors.New("GIT-500: upstream refused")
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
