package publish_test

// outputsignal_integration_test.go: the 03-execution-capsule.md §5.1a
// round trip against the REAL Core accept chain, using the existing
// publish integration harness (pubEnv):
//
//   - conforming done step -> HEAD advances, NO signal;
//   - violating done step  -> HEAD advances anyway (signal, not veto),
//     the response carries step id + JSON pointer + failing keyword;
//   - the executor-side consumption path (01 §6.3 falsified-done, as
//     pf-execute performs it) appends the infra_failure archive, rewrites
//     done -> in_progress and advances the candidate pointer bounded by
//     the candidate list length;
//   - the mismatched bytes stay on the Revision chain as auditable
//     history (every publish advanced HEAD).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/sync/freeze"
	"github.com/silgrid/polyforge/internal/sync/publish"
	"github.com/silgrid/polyforge/internal/workflow/drive"
)

// strictCodeSkill: a step skill whose pinned output schema rejects the
// fixture outputs below (summary minLength 5, verdict enum).
const strictCodeSkill = `---
name: code
description: writes the fix
tools: bash
output:
  type: object
  required: [summary, verdict]
  properties:
    summary: { type: string, minLength: 5 }
    verdict: { type: string, enum: [pass, fail] }
---

# code

Do the work.
`

// materializeSnapshot writes the capsule's read-only materialized
// workflow snapshot members the §5.1a check reads its pinned schemas
// from (.pi/agents — the pi adapter's target, 03 §4.5). workflow.md is
// written for realism; both are outside the sync surface (SYNC-INV-10).
func (e *pubEnv) materializeSnapshot(t *testing.T) {
	e.t.Helper()
	agents := filepath.Join(e.capsule.Dir, ".pi", "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "code.md"), []byte(strictCodeSkill), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.capsule.Dir, "workflow.md"), []byte("---\nname: demo\n---\nbody"), 0o444); err != nil {
		t.Fatal(err)
	}
}

// writeExecution performs the WI Agent's controlled write of
// runtime-state/execution.json (03 §4.2): MakeWritable, tmp+fsync+rename
// via SaveExecution, version strictly advancing. Like the real
// executor's write it MUTATES the live state (the prior attempts[]
// archive survives); when no state exists yet a fresh one is seeded.
func (e *pubEnv) writeExecution(t *testing.T, version int64, mutate func(exec *capsule.Execution)) {
	e.t.Helper()
	if err := e.capsule.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	exec, err := e.capsule.LoadExecution()
	if err != nil {
		if err != capsule.ErrNoExecutionState {
			t.Fatal(err)
		}
		exec = &capsule.Execution{
			CapsuleID: e.capsule.ID(), WorkItemID: e.wi, Generation: e.gen,
			BundleDigest: "demo-digest", CurrentStep: "code",
			WorkflowStatus: capsule.StatusRunning, Steps: map[string]*capsule.Step{},
		}
	}
	exec.Version = version
	if mutate != nil {
		mutate(exec)
	}
	if err := e.capsule.SaveExecution(exec); err != nil {
		t.Fatal(err)
	}
}

func (e *pubEnv) loadExecution(t *testing.T) *capsule.Execution {
	e.t.Helper()
	exec, err := e.capsule.LoadExecution()
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

// TestPublishOutputSchemaConformingDoneStep: a done step whose output
// conforms to the pinned schema publishes normally — HEAD advances and
// the response carries NO mismatch signal.
func TestPublishOutputSchemaConformingDoneStep(t *testing.T) {
	e := newPubEnv(t)
	e.materializeSnapshot(t)
	e.writeExecution(t, 1, func(exec *capsule.Execution) {
		exec.Steps["code"] = &capsule.Step{
			Status: capsule.StepDone, StepStartedAt: "t0",
			Output: map[string]any{"summary": "fixed the off-by-one", "verdict": "pass"},
		}
	})
	res, err := e.pub.Publish(context.Background(), e.publishReq())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Published || res.HeadSeq != 0 {
		t.Fatalf("HEAD must advance on the conforming publish: %+v", res)
	}
	if len(res.SchemaMismatches) != 0 {
		t.Fatalf("conforming done step must produce no signal: %+v", res.SchemaMismatches)
	}
}

// TestPublishOutputSchemaViolatingDoneStep: a done step whose output
// violates the pinned schema STILL advances HEAD (the check is a
// detection signal, never a publish veto — 03 §5.1a point 4) and the
// response carries the mismatch list with the failing pointer and
// keyword.
func TestPublishOutputSchemaViolatingDoneStep(t *testing.T) {
	e := newPubEnv(t)
	e.materializeSnapshot(t)
	e.writeExecution(t, 1, func(exec *capsule.Execution) {
		exec.Steps["code"] = &capsule.Step{
			Status: capsule.StepDone, StepStartedAt: "t0",
			Output: map[string]any{"summary": "hi", "verdict": "maybe"},
		}
	})
	res, err := e.pub.Publish(context.Background(), e.publishReq())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Published || res.HeadSeq != 0 {
		t.Fatalf("HEAD must advance DESPITE the mismatch (no veto): %+v", res)
	}
	if len(res.SchemaMismatches) != 2 {
		t.Fatalf("want 2 mismatch units (minLength + enum), got %+v", res.SchemaMismatches)
	}
	seen := map[string]bool{}
	for _, m := range res.SchemaMismatches {
		if m.StepID != "code" {
			t.Fatalf("mismatch must name the done step: %+v", m)
		}
		seen[m.Pointer+"/"+m.Keyword] = true
	}
	if !seen["/summary/minLength"] || !seen["/verdict/enum"] {
		t.Fatalf("signal must carry the failing pointers/keywords: %+v", res.SchemaMismatches)
	}
}

// TestPublishOutputSchemaSignalConsumptionWalksCandidates: the full
// falsified-done loop of 01 §6.3 step 5, driven through repeated
// publishes. Each round: the step completes done with a violating output,
// the publish advances HEAD and returns the signal, and the executor-side
// consumption (drive.FalsifiedDoneRewrite — what pf-execute performs in
// its single atomic write) appends the infra_failure archive, rewrites
// done -> in_progress and advances the effective candidate ordinal —
// bounded by the candidate list length (2 here: ordinals walk 2, then
// stay capped at 2).
func TestPublishOutputSchemaSignalConsumptionWalksCandidates(t *testing.T) {
	e := newPubEnv(t)
	e.materializeSnapshot(t)
	// The step's effective candidate list (workflow.md frontmatter,
	// defaults or step override) has TWO entries in this fixture.
	const listLen = 2

	wantOrdinals := []int{2, 2} // bounded walk: 2, then capped at list length
	version := int64(0)
	for round, wantOrd := range wantOrdinals {
		version++ // the executor's done-write (one controlled write per completion)
		v := version
		e.writeExecution(t, v, func(exec *capsule.Execution) {
			// The re-dispatch completes done again with the SAME violating
			// output shape — attempts[] from the earlier rounds survives
			// the executor's write.
			if exec.Steps["code"] == nil {
				exec.Steps["code"] = &capsule.Step{}
			}
			exec.Steps["code"].Status = capsule.StepDone
			exec.Steps["code"].StepStartedAt = "t0"
			exec.Steps["code"].Output = map[string]any{"summary": "hi", "verdict": "maybe"}
		})
		res, err := e.pub.Publish(context.Background(), e.publishReq())
		if err != nil {
			t.Fatal(err)
		}
		if !res.Published {
			t.Fatalf("round %d: HEAD must advance (no veto): %+v", round, res)
		}
		if len(res.SchemaMismatches) == 0 {
			t.Fatalf("round %d: the signal must be present", round)
		}

		// Executor-side consumption (01 §6.3): ONE atomic write appending
		// the infra_failure archive AND rewriting done -> in_progress,
		// then the candidate recomputation from the archive alone.
		exec := e.loadExecution(t)
		if err := e.capsule.MakeWritable(); err != nil {
			t.Fatal(err)
		}
		ord, rerr := drive.FalsifiedDoneRewrite(exec, "code", "publish-time output-schema violation", listLen, time.Now())
		if rerr != nil {
			t.Fatalf("round %d: consumption rewrite: %v", round, rerr)
		}
		version++ // the consumption write advances the executor-state version too
		exec.Version = version
		if err := e.capsule.SaveExecution(exec); err != nil {
			t.Fatal(err)
		}
		if ord != wantOrd {
			t.Fatalf("round %d: candidate ordinal = %d, want %d", round, ord, wantOrd)
		}

		exec = e.loadExecution(t)
		st := exec.Steps["code"]
		if st.Status != capsule.StepInProgress {
			t.Fatalf("round %d: status = %q, want in_progress", round, st.Status)
		}
		infra := 0
		for _, a := range st.Attempts {
			if a.Cause == drive.CauseInfraFailure {
				infra++
			}
		}
		if infra != round+1 {
			t.Fatalf("round %d: infra_failure archive count = %d", round, infra)
		}
		// The ordinal is recomputable from execution.json alone (crash
		// recovery invariant): a fresh read derives the same value.
		if got := drive.CandidateOrdinal(st, listLen); got != wantOrd {
			t.Fatalf("round %d: recomputed ordinal = %d, want %d", round, got, wantOrd)
		}
	}

	// Every round advanced HEAD: the mismatched bytes are auditable
	// history on the Revision chain, nothing was vetoed.
	h, found, err := e.client.GetHead(e.wi)
	if err != nil || !found || h.Seq != int64(len(wantOrdinals))-1 {
		t.Fatalf("HEAD after %d publishes: %+v found=%v err=%v", len(wantOrdinals), h, found, err)
	}
}

// TestPublishOutputSchemaSignalRidesFlushPath: the end-generation final
// flush surfaces the same signal through the FlushRequest.Signal hook
// when it performs a real publish (the pause/block boundary keeps the
// signal consumable for the resumed generation).
func TestPublishOutputSchemaSignalRidesFlushPath(t *testing.T) {
	e := newPubEnv(t)
	e.materializeSnapshot(t)
	e.writeExecution(t, 1, func(exec *capsule.Execution) {
		exec.Steps["code"] = &capsule.Step{
			Status: capsule.StepDone, StepStartedAt: "t0",
			Output: map[string]any{"summary": "hi", "verdict": "pass"},
		}
	})
	var signal []publish.SchemaMismatch
	req := e.flushReq()
	req.Signal = func(ms []publish.SchemaMismatch) { signal = ms }
	fe, err := e.pub.FinalFlush(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if fe.PublishedSeq != 0 || !fe.FrozenCheckpoint.MatchesHead {
		t.Fatalf("flush evidence: %+v", fe)
	}
	if len(signal) != 1 || signal[0].Pointer != "/summary" || signal[0].Keyword != "minLength" {
		t.Fatalf("flush-path signal: %+v", signal)
	}
}

// TestPublishOutputSchemaNoSnapshotNoSignal: a capsule without a
// materialized workflow snapshot has nothing pinned to validate against —
// publishes are unaffected and the signal is empty (the check can never
// veto or fail the flow).
func TestPublishOutputSchemaNoSnapshotNoSignal(t *testing.T) {
	e := newPubEnv(t)
	e.writeExecution(t, 1, func(exec *capsule.Execution) {
		exec.Steps["code"] = &capsule.Step{
			Status: capsule.StepDone, StepStartedAt: "t0",
			Output: map[string]any{"anything": "goes"},
		}
	})
	res, err := e.pub.Publish(context.Background(), e.publishReq())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Published || len(res.SchemaMismatches) != 0 {
		t.Fatalf("no snapshot: %+v", res)
	}
}

// Compile-time assertion that the flush request keeps the freeze faces
// the daemon builds (quiescer path) usable in these fixtures.
var _ freeze.Conditions
