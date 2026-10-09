package test

import (
	"strings"
	"testing"
	"time"
)

// v1_s14: two work items run concurrently on one driver-pinned daemon.
// Each keeps its own capsule, its own execution state and its own
// server-side session and chain; neither driver sees the other's
// identity, and both complete and close cleanly. The isolation pins:
// execution.json names its own work item and its own step table, the
// archived envelopes carry their own wi_id, the sealed chains never
// cross, the spawn journals carry distinct session handles (T8), and the
// hub's public session audit blocks name two DIFFERENT fingerprints
// while both run (T8 — session isolation is per-WI, m4).
func TestV1S14ConcurrentTwoWI(t *testing.T) {
	inst := newV1Install(t)
	wiA := inst.newWI(t, "s14: concurrent member A", envelopeQuickFix(), nil)
	wiB := inst.newWI(t, "s14: concurrent member B", envelopeQuickFix(), nil)

	d := newV1Daemon(t, inst, optDriverEngine())
	d.start()
	d.pf(t, "submit", wiA)
	d.pf(t, "submit", wiB)

	// Both drivers are live at the same time (the concurrency cap is 4):
	// two running jobs, each with a spawned step process (the spawn
	// journal is the step-process ledger — the driver lane carries no
	// agent pid on the status face).
	waitUntil(t, 30*time.Second, "the two drivers never ran concurrently", func() bool {
		jobs := d.status(t).Jobs
		var a, b bool
		for _, j := range jobs {
			if j.WorkItemID == wiA && j.Status == "running" {
				a = true
			}
			if j.WorkItemID == wiB && j.Status == "running" {
				b = true
			}
		}
		return a && b
	})
	waitSpawnCount(t, d, wiA, 1, 30*time.Second)
	waitSpawnCount(t, d, wiB, 1, 30*time.Second)

	// Each observe projection shows its own step table, never the peer's.
	waitUntil(t, 30*time.Second, "A's steps never showed", func() bool {
		jv := d.observe(t, wiA)
		return jv.Steps["grill"] != nil && jv.Steps["fix"] != nil
	})
	waitUntil(t, 30*time.Second, "B's steps never showed", func() bool {
		jv := d.observe(t, wiB)
		return jv.Steps["grill"] != nil && jv.Steps["fix"] != nil
	})
	if jv := d.observe(t, wiA); jv.WorkItem != wiA {
		t.Fatalf("observe projection of A names %q", jv.WorkItem)
	}
	if jv := d.observe(t, wiB); len(jv.Steps) != 4 {
		t.Fatalf("observe projection of B crossed streams: %+v", jv.Steps)
	}

	// T8: while both run, the hub names two DISTINCT session
	// fingerprints — per-WI session isolation on the public audit block.
	sessA := inst.sessionOf(t, wiA)
	sessB := inst.sessionOf(t, wiB)
	if sessA.Fingerprint == "" || sessB.Fingerprint == "" {
		t.Fatalf("a concurrent member carries no session: %+v %+v", sessA, sessB)
	}
	if sessA.Fingerprint == sessB.Fingerprint {
		t.Fatalf("the two concurrent work items share one session: %q", sessA.Fingerprint)
	}

	// Both complete and close cleanly (the driver's completed exit is the
	// wrap signal).
	inst.waitWIStatus(t, wiA, "wrapped", 90*time.Second)
	inst.waitWIStatus(t, wiB, "wrapped", 90*time.Second)

	// The execution faces are independent: each workflow completed with
	// its own step table and its own archived envelopes, never the
	// peer's identity anywhere.
	for _, wi := range []string{wiA, wiB} {
		waitWorkflowStatus(t, d, wi, "completed", 30*time.Second)
		x := readExecution(t, d.capsuleDir(wi))
		if len(x.Steps) != 4 {
			t.Fatalf("%s: step table crossed streams: %v", wi, x.Steps)
		}
		env := archivedEnvelope(t, d.capsuleDir(wi), "grill", 1)
		if env["wi_id"] != wi {
			t.Fatalf("%s: archived envelope carries %v", wi, env["wi_id"])
		}
		exec := inst.bareFileAtMain(t, wi, "runtime-state/execution.json")
		if !strings.Contains(exec, wi) {
			t.Fatalf("chain of %s carries foreign state:\n%s", wi, exec)
		}
		if strings.Contains(exec, wiA) && wi != wiA {
			t.Fatalf("chain of %s carries A's state:\n%s", wi, exec)
		}
		seqs := inst.bareSeqTrailers(t, wi)
		if len(seqs) == 0 {
			t.Fatalf("%s sealed an empty chain", wi)
		}
		for i, s := range seqs {
			if s != int64(i+1) {
				t.Fatalf("%s seq trailer %d is %d", wi, i, s)
			}
		}
	}

	// The two capsules' spawn journals carry distinct PUBLIC session
	// handles (v1.5: the v1 synthetic "session_id" value is deleted from
	// the journal — B3, the secret-class name must never land in the
	// capsule; the handle is the fingerprint).
	sa := spawnJournal(t, d.capsuleDir(wiA))
	sb := spawnJournal(t, d.capsuleDir(wiB))
	if len(sa) == 0 || len(sb) == 0 {
		t.Fatal("spawn journals missing")
	}
	if sa[0]["session_handle"] == sb[0]["session_handle"] {
		t.Fatalf("the two capsules share a session handle: %v", sa[0]["session_handle"])
	}
	if sa[0]["session_id"] != nil || sb[0]["session_id"] != nil {
		t.Fatal("the spawn journal still carries a session_id value (B3: the secret-class name must never ride the capsule)")
	}
}
