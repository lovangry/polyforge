package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v1_s03: operator pause → unpause under the v1.5 session model.
//
//   - pf pause = the daemon close path carrying session-end(paused,
//     clean_pause=true): the hub keeps the session row (M3 — paused is
//     not terminal), marks the operator-pause flag, the capsule is
//     sealed read-only and the daemon drops the local job. There is no
//     end-generation and no fresh claim: the SAME session keeps owning
//     the paused work item.
//   - pf resume = the whitelisted WI PATCH clean_pause=false — and
//     nothing else: the paused work item stays with its holder session.
//     v1.5 ASSERTION CHANGE vs the v1 scenario: v1's "resume opens a
//     fresh generation 2 claim on the same daemon" is gone — a paused
//     work item answers the plain CAS claim NOT_CLAIMABLE (session
//     non-empty), and since the pause dropped the holder daemon's local
//     record, the continuation the code actually offers is a takeover
//     (session swap, contract §1.4) on the same daemon, which re-clones
//     the sealed scene and continues ABOVE the paused tip. The seq
//     trailer stays monotonic across the boundary.
//   - the restart-attach half of M3 (paused + clean_pause=false → the
//     holder's next start attaches) is exercised by s09's
//     graceful-shutdown shape, where the record DOES survive.
func TestV1S03PauseResume(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s03: pause then resume",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "park"},
		},
		// The takeover derivation continues ABOVE the paused tip.
		[]map[string]any{{"do": "step", "id": "polish"}, {"do": "wrap"}})
	d := newV1Daemon(t, inst)
	d.start()
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})
	sessBefore := inst.sessionOf(t, wi)
	if sessBefore.Fingerprint == "" {
		t.Fatal("running work item carries no session")
	}

	d.pf(t, "pause", wi)
	inst.waitWIStatus(t, wi, "paused", 30*time.Second)

	// The pause is the operator shape: clean_pause rides the same
	// atomic fenced session-end, and the session row is KEPT (M3).
	sess := inst.sessionOf(t, wi)
	if sess.Fingerprint != sessBefore.Fingerprint {
		t.Fatalf("pause changed the session: %q -> %q", sessBefore.Fingerprint, sess.Fingerprint)
	}
	if !sess.CleanPause {
		t.Fatalf("operator pause did not set clean_pause: %+v", sess)
	}
	// The paused job is closed locally: the daemon holds no job.
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

	// A plain claim of the paused work item is refused typed ALREADY_HELD
	// on the wire (the session still holds it — pause is not a release;
	// M1's joint CAS gate names the holder). The CLI submit only enqueues,
	// so the typed refusal is asserted at the claim endpoint itself.
	refuseClaim(t, inst, wi, "ALREADY_HELD")

	// The operator unpause: the whitelisted PATCH clears clean_pause and
	// the work item stays with its holder session.
	d.pf(t, "resume", wi)
	sess = inst.sessionOf(t, wi)
	if sess.CleanPause {
		t.Fatalf("resume did not clear clean_pause: %+v", sess)
	}
	if sess.Fingerprint != sessBefore.Fingerprint {
		t.Fatalf("unpause changed the session: %+v", sess)
	}
	if st := inst.wiStatus(t, wi); st != "paused" {
		t.Fatalf("unpause must not flip the status itself: %q", st)
	}

	// The continuation: a takeover on the same daemon swaps the session
	// (the only legal resume path once the holder's local record is
	// gone) and the takeover derivation (resume_actions, scripted above)
	// continues ABOVE the sealed tip and wraps.
	inst.setPlanFull(t, wi,
		[]map[string]any{{"do": "step", "id": "polish"}, {"do": "wrap"}},
		[]map[string]any{{"do": "step", "id": "polish"}, {"do": "wrap"}})
	d.pf(t, "submit", wi, "--takeover")

	inst.waitWIStatus(t, wi, "running", 15*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	// The swap: a NEW public fingerprint took over from the paused
	// session, and the wrap cleared it.
	sess = inst.sessionOf(t, wi)
	if sess.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sess.Fingerprint)
	}

	// The chain continued ABOVE the sealed tip: seq trailers strictly
	// increasing across the pause/takeover boundary.
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
