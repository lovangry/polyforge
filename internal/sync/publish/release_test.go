package publish

// release_test.go: R2N-4 regressions — the write freeze acquired by
// Publish/FinalFlush must be RELEASED on every exit path, not only on the
// happy path. Before the freezeGuard, any post-acquire failure (scan,
// digest, HEAD fetch, state load, manifest build, staging pin) returned
// without releasing: the WI Agent stayed quiesced FOREVER — a permanent
// freeze indistinguishable from a hung daemon. The sentinel and no-change
// final-flush branches that skip commitScan leaked the freeze the same way.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/sync/freeze"
)

func quiescingReq(e *tenv, q *countingQuiescer) Request {
	return Request{
		WorkItemID: "wi_test01", CapsuleDir: e.capsule, Generation: 1, Token: "tok",
		Conditions: freeze.Conditions{Quiescer: q, Timeout: 5 * time.Second},
	}
}

func quiescingFlushReq(e *tenv, q *countingQuiescer) FlushRequest {
	return FlushRequest{
		WorkItemID: "wi_test01", CapsuleDir: e.capsule, Generation: 1, Token: "tok",
		Conditions: freeze.Conditions{Quiescer: q, Timeout: 5 * time.Second},
	}
}

// TestScanFailureMidPublishReleasesFreeze: a failure AFTER the freeze was
// acquired (here: a corrupt per-WI publish state, the first post-acquire
// read inside commitScan) must leave the agent UN-QUIESCED — the release
// fires exactly once on the error path.
func TestScanFailureMidPublishReleasesFreeze(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.writeCapsule("artifacts/a.md", "hello")
	// Corrupt the per-WI publish state: commitScan's LoadState fails after
	// the freeze window opened.
	os.MkdirAll(filepath.Join(e.daemonLocal, "publishstate"), 0o700)
	os.WriteFile(filepath.Join(e.daemonLocal, "publishstate", "wi_test01.json"), []byte("not json"), 0o600)

	q := &countingQuiescer{}
	_, err := e.pub.Publish(context.Background(), quiescingReq(e, q))
	if err == nil {
		t.Fatal("the corrupt state must fail the publish")
	}
	quiesced, released := q.counts()
	if quiesced != 1 {
		t.Fatalf("the freeze must have been acquired through the quiescer, quiesced=%d", quiesced)
	}
	if released != 1 {
		t.Fatalf("R2N-4: a post-acquire failure must release the freeze exactly once, released=%d", released)
	}
}

// TestFinalFlushFailureReleasesFreeze: same drill on the final flush.
func TestFinalFlushFailureReleasesFreeze(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.writeCapsule("artifacts/a.md", "hello")
	os.MkdirAll(filepath.Join(e.daemonLocal, "publishstate"), 0o700)
	os.WriteFile(filepath.Join(e.daemonLocal, "publishstate", "wi_test01.json"), []byte("not json"), 0o600)

	q := &countingQuiescer{}
	_, err := e.pub.FinalFlush(context.Background(), quiescingFlushReq(e, q))
	if err == nil {
		t.Fatal("the corrupt state must fail the flush")
	}
	if _, released := q.counts(); released != 1 {
		t.Fatalf("R2N-4: a failed final flush must release the freeze, released=%d", released)
	}
}

// TestFinalFlushSentinelBranchReleasesFreeze: the empty-chain sentinel
// branch (no HEAD, never wrote, empty surface) returns evidence WITHOUT
// running commitScan — it must still release the freeze on its way out.
func TestFinalFlushSentinelBranchReleasesFreeze(t *testing.T) {
	e := newEnv(t)
	// Empty capsule, no HEAD, no publish state: the sentinel shape.
	q := &countingQuiescer{}
	fe, err := e.pub.FinalFlush(context.Background(), quiescingFlushReq(e, q))
	if err != nil {
		t.Fatal(err)
	}
	if fe.PublishedSeq != 0 || fe.FrozenCheckpoint == nil {
		t.Fatalf("expected the empty-chain sentinel evidence, got %+v", fe)
	}
	quiesced, released := q.counts()
	if quiesced != 1 || released != 1 {
		t.Fatalf("the sentinel branch must release the acquired freeze: quiesced=%d released=%d", quiesced, released)
	}
}

// TestFinalFlushNoChangeBranchReleasesFreeze: the HEAD-aligned no-change
// branch skips commitScan as well — the freeze it acquired must be
// released on return.
func TestFinalFlushNoChangeBranchReleasesFreeze(t *testing.T) {
	e := newEnv(t)
	// Publish once so a HEAD and an aligned local state exist.
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	q0 := &countingQuiescer{}
	e.pub.Publish(context.Background(), quiescingReq(e, q0))

	q := &countingQuiescer{}
	fe, err := e.pub.FinalFlush(context.Background(), quiescingFlushReq(e, q))
	if err != nil {
		t.Fatal(err)
	}
	if !fe.FrozenCheckpoint.MatchesHead {
		t.Fatalf("expected the no-change alignment evidence, got %+v", fe)
	}
	quiesced, released := q.counts()
	if quiesced != 1 || released != 1 {
		t.Fatalf("the no-change branch must release the acquired freeze: quiesced=%d released=%d", quiesced, released)
	}
}

// TestPublishHappyPathReleasesOnce: the pinned release in commitScan and
// the guard's deferred release must together fire EXACTLY once on the
// happy path (no double resume).
func TestPublishHappyPathReleasesOnce(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.writeCapsule("artifacts/a.md", "hello")
	q := &countingQuiescer{}
	e.mustPublish(e.pub.Publish(context.Background(), quiescingReq(e, q)))
	quiesced, released := q.counts()
	if quiesced != 1 || released != 1 {
		t.Fatalf("happy path: quiesced=%d released=%d (want 1/1)", quiesced, released)
	}
}
