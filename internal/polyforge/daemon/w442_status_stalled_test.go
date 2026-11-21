package daemon

// w442_status_stalled_test.go: `pf ops status` reads `stalled`, not
// `running`, for a running row that holds no agent pid and whose dispatch
// has outlived session_stall_window (polyforge#42, AC3).

import (
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

func TestStalledRowCondition(t *testing.T) {
	const win = 5 * time.Minute
	cases := []struct {
		name       string
		status     string
		pid        int
		inDispatch bool
		age, win   time.Duration
		want       bool
	}{
		{"no pid, past the window", "running", 0, true, 10 * time.Minute, win, true},
		{"exactly at the window", "running", 0, true, win, win, true},
		{"agent pid present", "running", 4242, true, 10 * time.Minute, win, false},
		{"inside the window", "running", 0, true, 4 * time.Minute, win, false},
		{"window disabled", "running", 0, true, time.Hour, -1, false},
		{"no dispatch in flight", "running", 0, false, time.Hour, win, false},
		{"pending question wins", protocol.JobNeedsInput, 0, true, time.Hour, win, false},
		{"closing keeps its name", "sealing", 0, true, time.Hour, win, false},
	}
	for _, c := range cases {
		if got := stalledRow(c.status, c.pid, c.inDispatch, c.age, c.win); got != c.want {
			t.Errorf("%s: stalledRow = %v, want %v", c.name, got, c.want)
		}
	}
}

// the incident: a claimed job, no agent pid, dispatch started 10 minutes ago
// with nothing since — jobs[] must say stalled, and say running again once
// the window is longer than the idle time.
func TestStatusJobsStalledProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
		want   string
	}{
		{"default window passed", 0, protocol.JobStalled},
		{"window longer than the idle time", time.Hour, "running"},
		{"window disabled", -time.Minute, "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			td := newTestDaemon(t, func(c *Config) { c.Supervision.SessionStallWindow = tc.window })
			td.submitClaim(t, "wi_stall")
			td.waitMonitored(t, "wi_stall")
			td.sup.Link("wi_stall").Fact(supervise.StepStarted{
				Step: "build", SpawnN: 1, Model: "gpt-6.1-sol", Harness: "pi",
				StartedAt:  time.Now().Add(-10 * time.Minute),
				Thresholds: supervise.Thresholds{Silence: time.Hour, Tool: time.Hour, StepTimeout: time.Hour},
			})
			deadline := time.Now().Add(5 * time.Second)
			for {
				st := td.statusOf(t)
				j := statusJobOf(&st, "wi_stall")
				if j != nil && j.Candidate != "" && j.StepAgeSec != nil {
					if j.Status != tc.want {
						t.Fatalf("status %q, want %q (row %+v)", j.Status, tc.want, j)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("the dispatch never surfaced on /status: %+v", st.Jobs)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
