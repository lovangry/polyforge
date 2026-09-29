package daemon

// outputschema_signal_test.go: the 03 §5.1a signal at the Daemon's
// publish_request face — the handler path WI Agents actually call. The
// demo bundle's pinned schema (output: {type: object}) is replaced in the
// capsule's materialized snapshot with a strict one (the check's schema
// source is the capsule's own materialized snapshot, 03 §4.5), and the
// response must carry the mismatch list while HEAD still advanced.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/capsule"
)

const strictDemoCodeSkill = `---
name: code
description: writes the fix
tools: bash
output:
  type: object
  required: [summary]
  properties:
    summary: { type: string, minLength: 5 }
---

# code

Do the work.
`

// TestPublishRequestResponseCarriesSchemaMismatchSignal: a done step with
// a schema-violating output publishes (HEAD advances — signal, not veto)
// and PublishOutcome.SchemaMismatches carries step id + pointer +
// failing keyword on the response the WI Agent consumes.
func TestPublishRequestResponseCarriesSchemaMismatchSignal(t *testing.T) {
	e := newDaemonEnv(t)
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim", Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	capc, _, err := capsule.Open(filepath.Join(e.dataDir, "home", "ws"), e.wi)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the demo bundle's pinned schema inside the capsule's
	// materialized snapshot with a strict one (chmod: the snapshot is
	// read-only by contract, the test writes it as the materializer).
	agentPath := filepath.Join(capc.Dir, ".pi", "agents", "code.md")
	if err := os.Chmod(agentPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentPath, []byte(strictDemoCodeSkill), 0o444); err != nil {
		t.Fatal(err)
	}

	if err := capc.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	exec := &capsule.Execution{
		CapsuleID: capc.ID(), WorkItemID: e.wi, Generation: 1,
		BundleDigest: "demo", CurrentStep: "code",
		WorkflowStatus: capsule.StatusRunning, Version: 1,
		Steps: map[string]*capsule.Step{
			"code": {Status: capsule.StepDone, StepStartedAt: "t0",
				Output: map[string]any{"summary": "hi"}},
		},
	}
	if err := capc.SaveExecution(exec); err != nil {
		t.Fatal(err)
	}

	out, err := e.d.PublishRequest(ctx, e.wi, "step_completed")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Published || out.HeadSeq != 0 {
		t.Fatalf("HEAD must advance despite the mismatch (no veto): %+v", out)
	}
	if len(out.SchemaMismatches) != 1 {
		t.Fatalf("want exactly the minLength unit, got %+v", out.SchemaMismatches)
	}
	m := out.SchemaMismatches[0]
	if m.StepID != "code" || m.Pointer != "/summary" || m.Keyword != "minLength" || m.Detail == "" {
		t.Fatalf("signal unit must carry step/pointer/keyword/detail: %+v", m)
	}
}
