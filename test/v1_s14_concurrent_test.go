package test

import (
	"strings"
	"testing"
	"time"
)

// v1_s14: two work items run concurrently on one daemon. Each keeps its
// own capsule, its own execution state and its own server-side session
// and chain; neither agent sees the other's identity, and both close
// cleanly. The isolation pins: execution.json names its own work item,
// the sealed chains never cross, the spawn journals carry distinct
// session handles (T8), and the hub's public session audit blocks name
// two DIFFERENT fingerprints while both run (T8 — session isolation is
// per-WI, m4).
func TestV1S14ConcurrentTwoWI(t *testing.T) {
	inst := newV1Install(t)
	wiA := inst.newWI(t, "s14: concurrent member A", []map[string]any{
		{"do": "step", "id": "code"}, {"do": "step", "id": "review"}, {"do": "wrap"},
	}, nil)
	wiB := inst.newWI(t, "s14: concurrent member B", []map[string]any{
		{"do": "step", "id": "design"}, {"do": "step", "id": "code"}, {"do": "wrap"},
	}, nil)

	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wiA)
	d.pf(t, "submit", wiB)

	// Both agents are live at the same time (the concurrency cap is 4).
	waitUntil(t, 30*time.Second, "the two agents never ran concurrently", func() bool {
		return d.agentPIDOf(t, wiA) > 0 && d.agentPIDOf(t, wiB) > 0
	})

	// Each observe projection shows its own step table, never the peer's.
	waitUntil(t, 30*time.Second, "A's steps never showed", func() bool {
		jv := d.observe(t, wiA)
		return jv.Steps["code"] != nil && jv.Steps["review"] != nil
	})
	waitUntil(t, 30*time.Second, "B's steps never showed", func() bool {
		jv := d.observe(t, wiB)
		return jv.Steps["design"] != nil && jv.Steps["code"] != nil
	})
	if jv := d.observe(t, wiA); jv.WorkItem != wiA {
		t.Fatalf("observe projection of A names %q", jv.WorkItem)
	}
	if jv := d.observe(t, wiB); len(jv.Steps) != 2 || jv.Steps["design"] == nil {
		t.Fatalf("observe projection of B crossed streams: %+v", jv)
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

	// Both close cleanly.
	inst.waitWIStatus(t, wiA, "wrapped", 60*time.Second)
	inst.waitWIStatus(t, wiB, "wrapped", 60*time.Second)

	// The sealed chains are independent: each carries exactly its own
	// execution state, and each bare repo holds only its own commits.
	for _, wi := range []string{wiA, wiB} {
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
