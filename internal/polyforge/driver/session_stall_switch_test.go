package driver

// session_stall_switch_test.go: polyforge#42's switch half — a dispatch
// judged session_stalled by the supervisor (rule 0 in package supervise,
// fed by the observe monitor's watch) reaches the driver's contract-4
// failure table as a MODEL-CLASS cause: the candidate is judged unusable
// and the chain moves to the next one at once — a fresh conversation, no
// backoff, the counter reset. Same shape the existing
// TestModelErrorSwitchesAtOnce pins for the other model-class causes.
//
// carries: candidates.go (IsModelClass), supervision.go (the
// FailSessionStalled cause and the SessionStall threshold riding the
// step_started fact), supervision_test.go (the env), transition.go (the
// switch disposition).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// stalledVerdict is the supervisor's rule-0 verdict for a dispatch whose
// live session file stayed frozen — the exact one the observe monitor's
// watch produces.
func stalledVerdict(step string, n int64) supervise.StepDead {
	return supervise.StepDead{
		Step: step, SpawnN: n, Cause: supervise.CauseSessionStalled,
		Evidence: "stalled: no session output within 5m0s of the last write (the live session file stayed at 58875 bytes; 5m10s since the dispatch)",
	}
}

// TestSessionStalledSwitchesCandidate: a session_stalled verdict tears
// the dispatch down and the chain advances at once — the next candidate
// runs FRESH (a hung candidate's half-written conversation is not
// resumed), the failure is archived with the cause, and the AC1 line rides
// the step_dead event verbatim. No backoff is booked.
func TestSessionStalledSwitchesCandidate(t *testing.T) {
	rec := &waitRecorder{}
	env := newChainEnv(t, func(c *Config) { c.Sleep = rec.sleep }, "s", "a", "b")
	env.sp.script = []fakeEntry{{hold: true}, done(`{"ok":true}`)}
	go func() {
		waitForSpawns(env, 1)
		sp := env.sp.spawnAt(0)
		env.link.Verdict(stalledVerdict(sp.StepID, sp.SpawnN))
	}()
	if err := env.d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if models := env.models(); len(models) != 2 || models[0] != "a" || models[1] != "b" {
		t.Fatalf("dispatched %v, want [a b]: a session_stalled candidate is unusable, the chain moves on", models)
	}
	if sp := env.sp.spawnAt(1); !sp.Fresh || sp.Resume {
		t.Fatalf("the switched-to candidate must start fresh, got %+v", sp)
	}
	fails := failedBodies(env)
	if len(fails) != 1 || fails[0]["cause"] != FailSessionStalled || fails[0]["disposition"] != "switch_model" ||
		fails[0]["from_model"] != "pi:a" || fails[0]["to_model"] != "pi:b" || fails[0]["failure_count"] != 0 {
		t.Fatalf("step_failed %v, want session_stalled / switch_model pi:a -> pi:b, count 0", fails)
	}
	if _, ok := fails[0]["backoff_ms"]; ok {
		t.Fatalf("a switch books no backoff: %v", fails[0])
	}
	if sw := env.log.byKind("candidate_switched"); len(sw) != 1 || sw[0].body["from"] != "pi:a" || sw[0].body["to"] != "pi:b" {
		t.Fatalf("candidate_switched events %+v, want one pi:a -> pi:b", sw)
	}
	// the AC1 log line: the step_dead event carries the verdict's evidence
	de := env.log.byKind("step_dead")
	if len(de) != 1 || !strings.HasPrefix(de[0].body["evidence"].(string), "stalled: no session output within") {
		t.Fatalf("step_dead %+v must carry the stalled: no session output evidence line", de)
	}
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("waits %v, want none", got)
	}
}

// TestSessionStallThresholdRidesStepStarted: the window the daemon
// configures (via the limits) reaches the dispatch's announced
// thresholds — the supervisor's rule 0 cannot judge what it is never
// told. A LimitsFunc answer of 7m must arrive verbatim.
func TestSessionStallThresholdRidesStepStarted(t *testing.T) {
	env := newChainEnv(t, func(c *Config) {
		c.LimitsFunc = func() supervise.Defaults {
			return supervise.Defaults{SessionStall: 7 * time.Minute}
		}
	}, "s", "a")
	env.sp.script = []fakeEntry{{hold: true}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = env.d.Run(ctx)
	}()
	var th supervise.Thresholds
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		for _, f := range env.link.Facts.Drain() {
			if st, ok := f.(supervise.StepStarted); ok && st.Step == "s" {
				th = st.Thresholds
			}
		}
		if th.SessionStall != 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if th.SessionStall != 7*time.Minute {
		t.Fatalf("step_started thresholds = %+v, want SessionStall 7m (the configured window must reach the dispatch)", th)
	}
}
