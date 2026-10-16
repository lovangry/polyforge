package daemon

// close_refused_test.go: an operator close stops the agent before the
// final memory flush; when the runtime then refuses the close after
// all, the expected-exit suppression is withdrawn and the stopped agent
// goes to the normal recovery (it never stays dead and silent).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
)

func TestCloseRefusedAfterTheKillRecovers(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_cr")
	td.waitMonitored(t, "wi_cr")
	td.stopFn = func(context.Context, string, string) error {
		return &api.Error{Code: api.ErrDaemonConflict, Detail: "wi_cr: a recovery of this work item is in progress; retry once it settles"}
	}
	_, err := td.client.Wrap("wi_cr", "wrap")
	if err == nil || !strings.Contains(err.Error(), "DAEMON-409") {
		t.Fatalf("refused close answered %v", err)
	}
	if a := td.spawner.agentOf(0); a == nil || !a.isDone() {
		t.Fatal("the close did not stop the agent before the runtime's refusal")
	}
	deadline := time.Now().Add(10 * time.Second)
	for td.spawner.callCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the agent stopped by a refused close was never recovered (spawns=%d, events %v)",
				td.spawner.callCount(), td.obs.Events().SinceAfter("wi_cr", 0))
		}
		time.Sleep(10 * time.Millisecond)
	}
	var saw bool
	for _, e := range td.obs.Events().SinceAfter("wi_cr", 0) {
		if e.Kind == observe.EvNeedsRecovery {
			saw = true
		}
	}
	if !saw {
		t.Fatal("no needs_recovery event for the stopped agent")
	}
	// the close is admitted next time: the run seals normally
	td.stopFn = nil
	deadline = time.Now().Add(10 * time.Second)
	for {
		if _, err := td.client.Wrap("wi_cr", "wrap"); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("wrap after the recovery: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitGone(t, td, "wi_cr")
}

// TestCloseAdmittedSealsAfterStoppingTheAgent: the normal path — the
// agent is stopped, the close seals, no recovery is drawn.
func TestCloseAdmittedSealsAfterStoppingTheAgent(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_ca")
	td.waitMonitored(t, "wi_ca")
	if _, err := td.client.Wrap("wi_ca", "wrap"); err != nil {
		t.Fatalf("wrap: %v", err)
	}
	waitGone(t, td, "wi_ca")
	if a := td.spawner.agentOf(0); a == nil || !a.isDone() {
		t.Fatal("the agent was not stopped")
	}
	time.Sleep(100 * time.Millisecond)
	for _, e := range td.obs.Events().SinceAfter("wi_ca", 0) {
		if e.Kind == observe.EvNeedsRecovery {
			t.Fatalf("an admitted close drew a recovery: %v", e)
		}
	}
	if td.spawner.callCount() != 1 {
		t.Fatalf("spawns = %d, want 1", td.spawner.callCount())
	}
}
