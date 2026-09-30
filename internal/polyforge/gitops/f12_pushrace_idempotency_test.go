package gitops

// f12_pushrace_idempotency_test.go — the corrected-behavior counterpart of
// f12_pushrace_repro_test.go (work item polyforge#46, the shutdown-window
// commit-dirty push race).
//
// The repro file pins the WIRE mechanism (git-receive-pack's implicit
// compare-and-swap against the advertised ref value rejects an overlapping
// same-WI push) — that mechanism stays true regardless of the fix. These
// tests pin the CHOKE-POINT behavior the fix installs: a push issued
// through gitops (the commit-dirty/seal/observe entry points) that loses
// the advertisement race to an overlapping lane of the same work item must
// not surface the CAS rejection — the bounded retry re-advertises, the
// server already holding the standing HEAD answers "Everything up-to-date",
// and the publish lands as the idempotent no-op it semantically is.
//
// The overlapping lane is driven as the raw `git push` exec (identical
// argv/env to pushLocked) exactly as in the repro file, because gitops'
// own entry points serialize in-process per work item; the raw lane is
// what the second daemon process looked like on the wire.

import (
	"os"
	"strings"
	"testing"
	"time"
)

// racePushOverlapping runs the overlapping-lane scenario: lane A (raw
// push, started first) and lane B (the gitops Push entry point, started
// half a second later) both publish o's standing HEAD against the real
// aihub proxy while the pre-receive hook's sleep keeps both ref
// transactions from committing. It returns lane B's outcome.
func racePushOverlapping(t *testing.T, e *reproEnv, wi string, o Opts) (bool, error) {
	t.Helper()
	// Deterministic window widening (test scaffolding, same technique
	// as the repro file).
	e.slowHook(t, wi, 3*time.Second)

	type pushOut struct {
		pushed bool
		err    error
	}
	chA := make(chan pushOut, 1)
	go func() {
		// Lane A first: it wins the ref transaction, so the gitops lane
		// below is the one that must recover from the CAS rejection.
		_ = rawPush(t, o)
		chA <- pushOut{}
	}()
	time.Sleep(500 * time.Millisecond)
	pushed, err := Push(o)
	<-chA
	return pushed, err
}

// TestOverlappingSameWIPushIsIdempotent: the "is at X but expected Y"
// shape. The WI has a published baseline; a new commit-dirty commit
// stands locally; an overlapping lane delivers it first. The gitops
// push (the runtime seal / retry entry point) must NOT surface the CAS
// rejection: it retries, the server already holds the pushed commit,
// and the publish ends with the server ref and the capsule in
// agreement — the state the graceful-stop log demanded.
func TestOverlappingSameWIPushIsIdempotent(t *testing.T) {
	setIdentity(t)
	e := newReproEnv(t)
	const wi = "wi_racefix1"
	if err := e.mgr.EnsureBare(wi); err != nil {
		t.Fatalf("ensure bare: %v", err)
	}
	o := e.reproOpts(t, wi)
	if err := os.MkdirAll(o.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkCapsule(t, o)

	// Baseline published through the production choke point.
	writeDirty(t, o, "base.txt", "base\n")
	if sha, pushed, _, err := CommitPush(o, "baseline commit"); err != nil || !pushed {
		t.Fatalf("baseline CommitPush = (%q, %v, %v)", sha, pushed, err)
	}

	// The commit-dirty state both lanes publish.
	raced := localCommit(t, o, "dirty.txt", 2)

	pushed, err := racePushOverlapping(t, e, wi, o)
	if err != nil {
		t.Fatalf("gitops push lost the overlap and surfaced the rejection (want idempotent success): %v", err)
	}
	if !pushed {
		t.Fatal("gitops push did not reach the server")
	}
	if got := headSHA(t, o); got != raced {
		t.Fatalf("local commit did not stand: HEAD=%s want %s", got, raced)
	}
	if got := bareMainSHA(t, e, wi); got != raced {
		t.Fatalf("server main=%s, want the raced commit %s", got, raced)
	}
	if !cleanTree(t, o) {
		t.Fatal("capsule worktree unexpectedly dirty")
	}
}

// TestOverlappingFirstPublishIsIdempotent: the "reference already
// exists" shape. The WI's ref does not exist yet; both lanes advertise
// it as nonexistent and push the standing first commit; the raw lane
// creates the ref first. The gitops push must retry past the
// create-race rejection and answer up-to-date.
func TestOverlappingFirstPublishIsIdempotent(t *testing.T) {
	setIdentity(t)
	e := newReproEnv(t)
	const wi = "wi_racefix2"
	if err := e.mgr.EnsureBare(wi); err != nil {
		t.Fatalf("ensure bare: %v", err)
	}
	o := e.reproOpts(t, wi)
	if err := os.MkdirAll(o.CapsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkCapsule(t, o)
	first := localCommit(t, o, "one.txt", 1)

	pushed, err := racePushOverlapping(t, e, wi, o)
	if err != nil {
		t.Fatalf("gitops first publish lost the overlap and surfaced the rejection (want idempotent success): %v", err)
	}
	if !pushed {
		t.Fatal("gitops push did not reach the server")
	}
	if got := bareMainSHA(t, e, wi); got != first {
		t.Fatalf("server main=%s, want the first commit %s", got, first)
	}
	if got := headSHA(t, o); got != first {
		t.Fatalf("local commit did not stand: HEAD=%s want %s", got, first)
	}
}

// TestOverlappingDivergentPushStillSurfacesError: the retry boundary.
// Idempotency covers a duplicate push of state the server already
// holds; it must NOT mask a genuinely divergent publish. Two capsules
// of one work item hold unrelated first commits; the raw lane lands
// capsule A's commit, and the gitops push of capsule B's commit —
// rejected by the create race, retried, still unable to land — must
// surface its error (the operator's conflict signal), with both local
// commits standing.
func TestOverlappingDivergentPushStillSurfacesError(t *testing.T) {
	setIdentity(t)
	e := newReproEnv(t)
	const wi = "wi_racefix3"
	if err := e.mgr.EnsureBare(wi); err != nil {
		t.Fatalf("ensure bare: %v", err)
	}
	o1 := e.reproOpts(t, wi)
	o1.CapsuleDir += "-a"
	o1.GitDir += "-a"
	o2 := e.reproOpts(t, wi)
	o2.CapsuleDir += "-b"
	o2.GitDir += "-b"
	for _, o := range []Opts{o1, o2} {
		if err := os.MkdirAll(o.CapsuleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		mkCapsule(t, o)
	}
	sha1 := localCommit(t, o1, "one.txt", 1)
	sha2 := localCommit(t, o2, "two.txt", 1)

	e.slowHook(t, wi, 3*time.Second)
	type done struct{}
	chA := make(chan done, 1)
	go func() {
		_ = rawPush(t, o1) // capsule A's lane wins the create
		chA <- done{}
	}()
	time.Sleep(500 * time.Millisecond)
	_, err := Push(o2)
	<-chA
	if err == nil {
		t.Fatal("divergent publish answered success; the honest conflict must surface")
	}
	if got := bareMainSHA(t, e, wi); got != sha1 {
		t.Fatalf("server main=%s, want the winning capsule A commit %s", got, sha1)
	}
	if got := headSHA(t, o1); got != sha1 {
		t.Fatalf("capsule A commit did not stand: %s", got)
	}
	if got := headSHA(t, o2); got != sha2 {
		t.Fatalf("capsule B commit did not stand: %s", got)
	}
	if !strings.Contains(err.Error(), "git push") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}
