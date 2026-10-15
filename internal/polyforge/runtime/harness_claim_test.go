package runtime

// harness_claim_test.go: polyforge#159 on the claim path — the per-step
// harness compose rides the job and its daemon-private state record, and
// a model/harness mismatch refuses BEFORE the claim (DAEMON-414).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func TestClaimComposeHarnessRidesTheJob(t *testing.T) {
	hub := newFakeHub()
	r, ws, _ := newWorkspaceTestRuntime(t, hub, "proj")
	r.cfg.WorkflowsDir = frontmatterBundle(t, w079Frontmatter)
	writeWorkspaceYaml(t, ws, "steps:\n  proj:\n    fix: { models: [prov/flash, claude-opus-5-5], harness: claude }\n")
	if err := r.Start(context.Background(), "wi_h159comp"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := r.jobOf("wi_h159comp")
	if j == nil {
		t.Fatal("the claim must run to a live agent")
	}
	j.mu.Lock()
	got := j.stepHarness
	j.mu.Unlock()
	if got["fix"] != "claude" || got["grill"] != "pi" || got["release"] != "pi" {
		t.Fatalf("step harness %v, want fix=claude, others pi", got)
	}
	// the snapshot is persisted daemon-side and rehydrates verbatim
	rec := j.record()
	blob, _ := json.Marshal(rec)
	var back jobRecord
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if jobFromRecord(&back).stepHarness["fix"] != "claude" {
		t.Fatalf("state record lost the harness snapshot: %s", blob)
	}
	if err := r.Stop(context.Background(), "wi_h159comp", protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A pre-#159 state record carries no step_harness: every step is pi.
func TestStateRecordWithoutHarnessIsPi(t *testing.T) {
	var rec jobRecord
	if err := json.Unmarshal([]byte(`{"work_item_id":"wi_old","step_candidates":{"fix":["a/b"]}}`), &rec); err != nil {
		t.Fatal(err)
	}
	if h := jobFromRecord(&rec).stepHarness; h != nil {
		t.Fatalf("old record harness %v, want nil (pi)", h)
	}
}

func TestHarnessMismatchRefusesBeforeClaim(t *testing.T) {
	cases := map[string]string{
		// fix falls through to the bundle's pi list
		"claude step with only pi candidates": "steps:\n  proj:\n    fix: { harness: claude }\n",
		// every pi step given only a claude id
		"pi steps with only claude ids": "steps:\n  proj:\n    default: { models: [claude-opus-5-5] }\n    fix: { models: [claude-sonnet-5] }\n    release: { models: [claude-sonnet-5] }\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			hub := newFakeHub()
			r, ws, sp := newWorkspaceTestRuntime(t, hub, "proj")
			r.cfg.WorkflowsDir = frontmatterBundle(t, w079Frontmatter)
			writeWorkspaceYaml(t, ws, yaml)
			err := r.Start(context.Background(), "wi_h159mis")
			ae, ok := err.(*api.Error)
			if !ok || ae.Code != api.ErrDaemonWorkspaceYaml {
				t.Fatalf("expected DAEMON-414, got %v", err)
			}
			if n := hub.claimCount(); n != 0 {
				t.Fatalf("refused AFTER a claim (%d claim calls)", n)
			}
			if n := sp.callCount(); n != 0 {
				t.Fatalf("the refusal derived %d agents", n)
			}
		})
	}
}
