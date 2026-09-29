package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v1_s03: pause → resume. A CLI pause closes the generation with the
// paused status (terminal FOR THE GENERATION, not the work item), the
// capsule is sealed, and a resume opens a FRESH generation (generation 2,
// a new claim) that continues the work item; the capsule chain continues
// above the sealed tip (the new clone starts from the server history, so
// the seq trailer stays monotonic across the pause).
func TestV1S03PauseResume(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s03: pause then resume",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "park"},
		}, nil)
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})

	d.pf(t, "pause", wi)
	inst.waitWIStatus(t, wi, "paused", 30*time.Second)

	// The paused generation is closed locally: the daemon holds no job.
	if st := d.status(t); len(st.Jobs) != 0 {
		t.Fatalf("daemon holds jobs after pause: %+v", st.Jobs)
	}
	// The paused scene is sealed read-only (files 0444).
	st, err := os.Stat(filepath.Join(d.capsuleDir(wi), "capsule.json"))
	if err != nil {
		t.Fatalf("sealed capsule.json: %v", err)
	}
	if st.Mode().Perm() != 0o444 {
		t.Fatalf("capsule.json not sealed read-only: %v", st.Mode())
	}
	pausedCommits := inst.bareCommitCount(t, wi)
	if pausedCommits == 0 {
		t.Fatal("paused chain never reached the server")
	}

	// Script the continuation BEFORE the resume (the fresh derivation
	// reads the plan at spawn): finish and wrap.
	inst.setPlan(t, wi, []map[string]any{{"do": "step", "id": "polish"}, {"do": "wrap"}})
	d.pf(t, "resume", wi)

	// A fresh generation: the work item runs again and wraps.
	inst.waitWIStatus(t, wi, "running", 15*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	// The new capsule is generation 2 of the same work item.
	rawID, err := os.ReadFile(filepath.Join(d.capsuleDir(wi), "capsule.json"))
	if err != nil {
		t.Fatalf("capsule.json (gen 2): %v", err)
	}
	var idn struct {
		Generation int64 `json:"generation"`
	}
	_ = json.Unmarshal(rawID, &idn)
	if idn.Generation != 2 {
		t.Fatalf("resume did not open generation 2: %+v", idn)
	}

	// The chain continued ABOVE the sealed tip: seq trailers strictly
	// increasing across the pause/resume boundary.
	seqs := inst.bareSeqTrailers(t, wi)
	if len(seqs) <= pausedCommits {
		t.Fatalf("chain did not grow past the pause (%d <= %d)", len(seqs), pausedCommits)
	}
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("seq trailer %d is %d, want %d (chain: %v)", i, s, i+1, seqs)
		}
	}

	// The continuation's step is in the sealed chain.
	exec := inst.bareFileAtMain(t, wi, "runtime-state/execution.json")
	if !strings.Contains(exec, `"polish"`) {
		t.Fatalf("resumed work missing from the chain:\n%s", exec)
	}
}
