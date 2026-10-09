package daemon

// answer_failfast_test.go: pins the answer criterion's fail-fast posture
// on the daemon's answer route (polyforge#106) — the legacy no-bridge-
// handle relay fallback is deleted, so a request the driver branch
// cannot consume fails fast with a typed error and no relay executes.
// (The plan named this pin for the cli package; it lives here because
// the route and the in-process fakes it needs are daemon-side.)

import (
	"errors"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

func TestAnswerWithoutLiveBridgeHandleFailsFast(t *testing.T) {
	td := newTestDaemon(t, nil)
	payload := cli.AnswerPayload{
		QuestionID: "q_ff", AnswerID: "a_ff",
		AnswerText: "fail fast", AnsweredAt: "2026-01-01T00:00:00Z", AnsweredBy: "u_1",
	}

	// (1) Not monitored at all (an idle daemon): the observe fallthrough
	// shape — typed 404, never a success.
	if _, err := td.client.Answer("wi_ghost", payload); err == nil {
		t.Fatal("answer for an unmonitored work item was accepted")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonNotFound {
			t.Fatalf("unmonitored answer error = %v, want DAEMON-404", err)
		}
	}

	// (2) Monitored, but the scripted engine carries no bridge Handle
	// (the between-derivations shape): typed 409 naming the missing
	// bridge handle — and no relay path executes anywhere.
	td.submitClaim(t, "wi_ff")
	td.waitMonitored(t, "wi_ff")
	raw, err := td.client.Answer("wi_ff", payload)
	if err == nil {
		t.Fatalf("answer with no live bridge handle was accepted: %s", raw)
	}
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrDaemonConflict {
		t.Fatalf("no-bridge answer error = %v, want DAEMON-409", err)
	}
	if !strings.Contains(ae.Detail, "bridge") {
		t.Fatalf("the refusal must name the missing bridge handle: %q", ae.Detail)
	}
	// No relay executed: the fake engine received zero control methods.
	for i := 0; i < td.spawner.callCount(); i++ {
		if got := td.spawner.agentOf(i).sentMethods(); len(got) != 0 {
			t.Fatalf("a relay reached agent %d (%v) — no relay path may execute", i, got)
		}
	}
}
