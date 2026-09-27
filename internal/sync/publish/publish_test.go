package publish

// Unit tests over a fake Remote: the six-step flow, pending-record
// lifecycle, three-branch disambiguation, final-flush evidence branches.
// Wire-level round-trips against the REAL Core server live in
// integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/sync/freeze"
	"github.com/silgrid/polyforge/internal/sync/revision"
	"github.com/silgrid/polyforge/internal/workflow/digest"
)

// fakeRemote is an in-memory AIHub implementing just enough of the accept
// chain for unit-level assertions (idempotency-first, HEAD parent checks).
type fakeRemote struct {
	mu       sync.Mutex
	blocks   map[string]map[string][]byte // wi → sha → content (original bytes)
	revs     map[string]map[int64]string
	head     map[string]*revision.HeadInfo
	tokenOK  func(wi, token string) error
	putCalls int
	// onPutBlock, when set, runs at every PutBlock entry (ordering probes).
	onPutBlock func()
	// transportFailAll makes every block PUT die on the wire (plain net
	// error, never an APIError) — the persistent-transport-failure drill.
	transportFailAll bool
	// loseNextResponse simulates a lost manifest confirmation: the server
	// accepts the manifest but the client never sees the response.
	loseNextResponse bool
	// conflictAll makes every manifest POST answer SYNC-409 (racing HEAD).
	conflictAll bool
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{
		blocks: map[string]map[string][]byte{}, revs: map[string]map[int64]string{}, head: map[string]*revision.HeadInfo{},
	}
}

func (f *fakeRemote) PutBlock(ctx context.Context, wi string, seq int64, shaHex string, block []byte, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onPutBlock != nil {
		f.onPutBlock()
	}
	f.putCalls++ // every attempt counts, transport deaths included
	if f.transportFailAll {
		return fmt.Errorf("connection reset by peer") // transport loss, not a verdict
	}
	if f.tokenOK != nil {
		if err := f.tokenOK(wi, token); err != nil {
			return err
		}
	}
	content, err := revision.DecodeBlock(block)
	if err != nil {
		return fmt.Errorf("zstd decode: %w", err)
	}
	if revision.DigestHex(content) != shaHex {
		return &fakeAPIError{code: "SYNC-400", reason: "DIGEST_MISMATCH"}
	}
	if _, ok := f.blocks[wi]; !ok {
		f.blocks[wi] = map[string][]byte{}
	}
	f.blocks[wi][shaHex] = content
	return nil
}

func (f *fakeRemote) PostManifest(ctx context.Context, wi string, m revision.Manifest, sha, token string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenOK != nil {
		if err := f.tokenOK(wi, token); err != nil {
			return 0, err
		}
	}
	if f.conflictAll {
		return 0, &fakeAPIError{code: "SYNC-409", reason: "SEQ_CONFLICT"}
	}
	// Idempotency FIRST (05 §2.8): (wi, seq, manifest_sha) hit → success
	// with the CURRENT head, no parent check.
	if existing, ok := f.revs[wi]; ok {
		if prev, ok2 := existing[m.Seq]; ok2 && prev == sha {
			return f.head[wi].Seq, nil
		}
	}
	// HEAD bootstrap / parent validation (05 §2.6).
	h, hasHead := f.head[wi]
	if !hasHead {
		if m.Seq != 0 || m.Parent != nil {
			return 0, &fakeAPIError{code: "SYNC-409", reason: "SEQ_CONFLICT"}
		}
	} else {
		if m.Parent == nil || *m.Parent != h.Seq || m.Seq != h.Seq+1 {
			return 0, &fakeAPIError{code: "SYNC-409", reason: "SEQ_CONFLICT"}
		}
	}
	// Block integrity.
	for _, op := range m.Ops {
		if op.Op == revision.OpPut {
			if _, ok := f.blocks[wi][op.Sha256]; !ok {
				return 0, &fakeAPIError{code: "SYNC-400", reason: "BLOCK_MISSING"}
			}
		}
	}
	if f.revs[wi] == nil {
		f.revs[wi] = map[int64]string{}
	}
	f.revs[wi][m.Seq] = sha
	f.head[wi] = &revision.HeadInfo{Seq: m.Seq, Generation: m.Generation, ManifestSha: sha, UpdatedAt: "now"}
	if f.loseNextResponse {
		f.loseNextResponse = false
		return 0, fmt.Errorf("connection reset by peer") // transport loss AFTER acceptance
	}
	return m.Seq, nil
}

func (f *fakeRemote) GetHead(ctx context.Context, wi string) (*revision.HeadInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.head[wi]
	if !ok {
		return nil, false, nil
	}
	cp := *h
	return &cp, true, nil
}

type fakeAPIError struct {
	code   string
	reason string
}

func (e *fakeAPIError) Error() string    { return e.code + " " + e.reason }
func (e *fakeAPIError) IsAPIError() bool { return true }

// env wires a capsule dir + publisher for tests.
type tenv struct {
	t           *testing.T
	capsule     string
	remote      *fakeRemote
	pub         *Publisher
	store       *Store
	stager      *freeze.Stager
	daemonLocal string
}

func newEnv(t *testing.T) *tenv {
	t.Helper()
	dir := t.TempDir()
	capsuleDir := filepath.Join(dir, "capsules", "wi_test01")
	for _, d := range []string{"runtime-state", "artifacts", "logs", "sync"} {
		os.MkdirAll(filepath.Join(capsuleDir, d), 0o755)
	}
	remote := newFakeRemote()
	daemonLocal := filepath.Join(dir, "daemon")
	store := NewStore(daemonLocal)
	stager := freeze.NewStager(daemonLocal)
	mutex := freeze.NewManager()
	pub := New(remote, mutex, stager, store)
	return &tenv{t: t, capsule: capsuleDir, remote: remote, pub: pub, store: store, stager: stager, daemonLocal: daemonLocal}
}

func (e *tenv) writeCapsule(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.capsule, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(content), 0o644)
}

func (e *tenv) req() Request {
	return Request{
		WorkItemID: "wi_test01", CapsuleDir: e.capsule, Generation: 1, Token: "tok",
		Conditions: freeze.Conditions{ProcessGroupDead: func() bool { return true }},
	}
}

func (e *tenv) flushReq() FlushRequest {
	return FlushRequest{
		WorkItemID: "wi_test01", CapsuleDir: e.capsule, Generation: 1, Token: "tok",
		Conditions: freeze.Conditions{ProcessGroupDead: func() bool { return true }},
	}
}

func (e *tenv) mustPublish(res *Result, err error) *Result {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
	if res == nil {
		e.t.Fatal("nil result")
	}
	return res
}

func TestPublishBootstrapAndIncrement(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.writeCapsule("artifacts/a.md", "hello")

	// Bootstrap: seq 0, parent null.
	res := e.mustPublish(e.pub.Publish(context.Background(), e.req()))
	if !res.Published || res.HeadSeq != 0 {
		t.Fatalf("bootstrap: %+v", res)
	}
	// No pending record left behind; staging cleaned.
	if rec, _ := e.store.LoadPending("wi_test01"); rec != nil {
		t.Fatal("pending record must be deleted after a definite success")
	}
	if _, err := os.Stat(e.stager.SeqDir("wi_test01", 0)); !os.IsNotExist(err) {
		t.Fatal("staging must be cleaned after success")
	}

	// Change a file → incremental seq 1 with parent 0.
	e.writeCapsule("artifacts/a.md", "changed!")
	e.writeCapsule("runtime-state/execution.json", `{"version":2}`)
	res = e.mustPublish(e.pub.Publish(context.Background(), e.req()))
	if res.HeadSeq != 1 {
		t.Fatalf("incremental: %+v", res)
	}
	// Manifest recorded the parent correctly.
	var st *PublishState
	st, _ = e.store.LoadState("wi_test01")
	if st.Seq != 1 || len(st.LiveFiles) != 2 {
		t.Fatalf("state: %+v", st)
	}
}

func TestPublishIdempotentReplay(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.mustPublish(e.pub.Publish(context.Background(), e.req()))
	// Replay an identical manifest via a manually crafted pending record
	// (05 §2.8 scenario two: a re-scan regenerating identical content).
	state, _ := e.store.LoadState("wi_test01")
	scan := &revision.ScanResult{Files: state.LiveFiles}
	m, err := revision.BuildManifest(&revision.ScanResult{Files: state.LiveFiles}, scan, revision.BuildOptions{
		WorkItemID: "wi_test01", Seq: 1, Parent: &state.Seq, Generation: 1,
		Kind: revision.KindIncremental,
	})
	if err != nil {
		t.Fatal(err)
	}
	sha, _ := revision.ComputeManifestSha(*m)
	if _, err := e.remote.PostManifest(context.Background(), "wi_test01", *m, sha, "tok"); err != nil {
		t.Fatalf("idempotent replay must hit: %v", err)
	}
	// Different content at the same seq: conflict.
	e.writeCapsule("artifacts/other.md", "x")
	scan2, _ := revision.ScanCapsule(e.capsule)
	m2, _ := revision.BuildManifest(&revision.ScanResult{Files: state.LiveFiles}, scan2, revision.BuildOptions{
		WorkItemID: "wi_test01", Seq: 1, Parent: &state.Seq, Generation: 1,
		Kind: revision.KindIncremental,
	})
	sha2, _ := revision.ComputeManifestSha(*m2)
	if _, err := e.remote.PostManifest(context.Background(), "wi_test01", *m2, sha2, "tok"); err == nil {
		t.Fatal("same seq with different manifest_sha must conflict")
	}
}

func TestPublishWrongParentSettlesAsDefiniteFailure(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	// A racing publisher moved HEAD between our GetHead and our submit: the
	// manifest POST answers SYNC-409 SEQ_CONFLICT — a DEFINITE refusal. The
	// flow must settle (record + staging dropped) and surface the error.
	e.remote.conflictAll = true
	_, err := e.pub.Publish(context.Background(), e.req())
	if err == nil {
		t.Fatal("stale parent must fail")
	}
	// The pending record was settled (definite refusal) and removed.
	if rec, _ := e.store.LoadPending("wi_test01"); rec != nil {
		t.Fatal("definite SYNC-409 must settle the pending record")
	}
	if _, serr := os.Stat(e.stager.SeqDir("wi_test01", 0)); !os.IsNotExist(serr) {
		t.Fatal("staging of a settled flow must be cleaned")
	}
}

func TestDisambiguationBranch1Accepted(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	// Simulate: manifest accepted but confirmation lost. Build the record by
	// running a publish whose PostManifest response is dropped after
	// acceptance.
	e.remote.loseNextResponse = true
	res, err := e.pub.Publish(context.Background(), e.req())
	// The publisher falls back to GET-HEAD disambiguation immediately: the
	// branch-1 path must settle it as accepted WITHOUT a second revision.
	if err != nil {
		t.Fatalf("disambiguation should have settled the flow: %v", err)
	}
	if res == nil || !res.Published || res.HeadSeq != 0 {
		t.Fatalf("branch 1 (accepted): %+v", res)
	}
	if rec, _ := e.store.LoadPending("wi_test01"); rec != nil {
		t.Fatal("record must be settled after branch 1")
	}
	if len(e.remote.revs["wi_test01"]) != 1 {
		t.Fatal("no second revision row may exist")
	}
}

func TestDisambiguationBranch2SafeResend(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.mustPublish(e.pub.Publish(context.Background(), e.req())) // HEAD at 0

	// Simulate the crash window: staging pinned + pending record persisted,
	// but the manifest never reached the server (HEAD still at parent 0).
	e.writeCapsule("artifacts/new.md", "second wave")
	scan, err := revision.ScanCapsule(e.capsule)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := e.store.LoadState("wi_test01")
	seq := state.Seq + 1
	m, err := revision.BuildManifest(&revision.ScanResult{Files: state.LiveFiles}, scan, revision.BuildOptions{
		WorkItemID: "wi_test01", Seq: seq, Parent: &state.Seq, Generation: 1, Kind: revision.KindIncremental,
	})
	if err != nil {
		t.Fatal(err)
	}
	sha, _ := revision.ComputeManifestSha(*m)
	var entries []freeze.StageEntry
	for _, op := range m.Ops {
		if op.Op == revision.OpPut {
			entries = append(entries, freeze.StageEntry{
				AbsPath: filepath.Join(e.capsule, filepath.FromSlash(op.Path)), Sha256: op.Sha256,
			})
		}
	}
	if _, err := e.stager.Stage("wi_test01", seq, entries); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SavePending(&PendingRecord{
		WorkItemID: "wi_test01", Manifest: *m, ManifestSha: sha, Seq: seq,
		Parent: &state.Seq, Generation: 1, Token: "tok", StagingSeq: seq,
		LiveFiles: scan.Files,
	}); err != nil {
		t.Fatal(err)
	}

	// Branch 2: HEAD == record.parent → safe resend from staging.
	res, err := e.pub.ResolvePending(context.Background(), "wi_test01", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.Published || res.HeadSeq != 1 {
		t.Fatalf("branch 2 resend: %+v", res)
	}
	if rec, _ := e.store.LoadPending("wi_test01"); rec != nil {
		t.Fatal("record must be settled after the resend")
	}
}

func TestDisambiguationBranch3StaleRecordDropped(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	// The record claims seq 1 parent 0, but remote HEAD has moved to 5
	// (a takeover published on top): branch 3 — drop, no blind resend.
	parent := int64(0)
	if err := e.store.SavePending(&PendingRecord{
		WorkItemID: "wi_test01", Seq: 1, Parent: &parent, Generation: 1,
		Token: "tok", StagingSeq: 1, ManifestSha: "sha256:old",
	}); err != nil {
		t.Fatal(err)
	}
	e.remote.mu.Lock()
	e.remote.head["wi_test01"] = &revision.HeadInfo{Seq: 5, ManifestSha: "sha256:new"}
	e.remote.mu.Unlock()
	res, err := e.pub.ResolvePending(context.Background(), "wi_test01", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Fatalf("stale record must be dropped without a result: %+v", res)
	}
	if rec, _ := e.store.LoadPending("wi_test01"); rec != nil {
		t.Fatal("stale record must be deleted")
	}
}

func TestPublishResolvesOpenRecordBeforeScanning(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	// Bootstrap so branch 2 of a later record can succeed.
	e.mustPublish(e.pub.Publish(context.Background(), e.req()))
	// Forge an UNDECIDED record (HEAD at parent) and request a new publish:
	// the new Publish must resolve it first, not stack a second flow.
	scan, _ := revision.ScanCapsule(e.capsule)
	state, _ := e.store.LoadState("wi_test01")
	seq := state.Seq + 1
	m, _ := revision.BuildManifest(&revision.ScanResult{Files: state.LiveFiles}, scan, revision.BuildOptions{
		WorkItemID: "wi_test01", Seq: seq, Parent: &state.Seq, Generation: 1, Kind: revision.KindIncremental,
	})
	sha, _ := revision.ComputeManifestSha(*m)
	var entries []freeze.StageEntry
	for _, op := range m.Ops {
		if op.Op == revision.OpPut {
			entries = append(entries, freeze.StageEntry{
				AbsPath: filepath.Join(e.capsule, filepath.FromSlash(op.Path)), Sha256: op.Sha256,
			})
		}
	}
	e.stager.Stage("wi_test01", seq, entries)
	e.store.SavePending(&PendingRecord{
		WorkItemID: "wi_test01", Manifest: *m, ManifestSha: sha, Seq: seq,
		Parent: &state.Seq, Generation: 1, Token: "tok", StagingSeq: seq, LiveFiles: scan.Files,
	})
	e.mustPublish(e.pub.Publish(context.Background(), e.req()))
	// HEAD advanced by the resent flow (seq 1) and then by the fresh scan's
	// own single-op revision (seq 2 — SYNC-INV-3 pins an execution.json put
	// into every manifest, so a no-change scan still publishes).
	h, _, _ := e.remote.GetHead(context.Background(), "wi_test01")
	if h.Seq != 2 {
		t.Fatalf("expected HEAD at 2, got %d", h.Seq)
	}
	if rec, _ := e.store.LoadPending("wi_test01"); rec != nil {
		t.Fatal("no pending record may survive a settled flow")
	}
}

func TestFinalFlushEmptyChainEvidence(t *testing.T) {
	e := newEnv(t)
	// First claim, immediately pause: no sync-surface writes ever. The
	// capsule is EMPTY (no execution.json yet — the WI Agent never ran).
	fe, err := e.pub.FinalFlush(context.Background(), e.flushReq())
	if err != nil {
		t.Fatal(err)
	}
	if fe.PublishedSeq != 0 || fe.ManifestSha != "" {
		t.Fatalf("empty chain: %+v", fe)
	}
	if fe.FrozenCheckpoint.BytesDigest != digest.EmptySyncBytesDigest {
		t.Fatalf("empty chain bytes_digest: %s", fe.FrozenCheckpoint.BytesDigest)
	}
	// Round trip through the wire shape.
	blob, _ := json.Marshal(fe)
	var back FlushEvidence
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if back.PublishedSeq != 0 {
		t.Fatal("wire round trip broke the sentinel")
	}
}

func TestFinalFlushEmptyChainRequiresPublishWhenWritesExist(t *testing.T) {
	e := newEnv(t)
	// No HEAD, but the sync surface holds writes (e.g. the WI Agent wrote
	// execution.json before a crash, publish never happened).
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	fe, err := e.pub.FinalFlush(context.Background(), e.flushReq())
	if err != nil {
		t.Fatal(err)
	}
	if fe.PublishedSeq == 0 && fe.ManifestSha == "" {
		t.Fatal("unpublished writes must NOT take the empty-chain shape")
	}
	if fe.FrozenCheckpoint.MatchesHead != true {
		t.Fatal("post-publish evidence must carry matches_head=true")
	}
	// A real seq-0 revision now exists.
	if h, ok, _ := e.remote.GetHead(context.Background(), "wi_test01"); !ok || h.Seq != 0 {
		t.Fatal("bootstrap publish must have created the chain")
	}
}

func TestFinalFlushNoChangeEvidence(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.mustPublish(e.pub.Publish(context.Background(), e.req()))
	headBefore, _, _ := e.remote.GetHead(context.Background(), "wi_test01")
	fe, err := e.pub.FinalFlush(context.Background(), e.flushReq())
	if err != nil {
		t.Fatal(err)
	}
	if fe.PublishedSeq != headBefore.Seq || fe.ManifestSha != headBefore.ManifestSha {
		t.Fatalf("no-change evidence: %+v vs head %+v", fe, headBefore)
	}
	if fe.FrozenCheckpoint.ManifestSha != headBefore.ManifestSha || !fe.FrozenCheckpoint.MatchesHead {
		t.Fatal("frozen.manifest_sha must be the HEAD alignment declaration")
	}
	// No new revision was produced.
	if h, _, _ := e.remote.GetHead(context.Background(), "wi_test01"); h.Seq != headBefore.Seq {
		t.Fatal("no-change flush must not publish")
	}
}

func TestFinalFlushChangedPublishes(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.mustPublish(e.pub.Publish(context.Background(), e.req()))
	e.writeCapsule("artifacts/late.md", "written after last publish")
	fe, err := e.pub.FinalFlush(context.Background(), e.flushReq())
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := e.remote.GetHead(context.Background(), "wi_test01")
	if fe.PublishedSeq != h.Seq || h.Seq != 1 {
		t.Fatalf("changed flush: %+v head %+v", fe, h)
	}
	if !fe.FrozenCheckpoint.MatchesHead {
		t.Fatal("matches_head=false is not a legal submit shape")
	}
	// The late write made it into the published revision.
	if blk := e.remote.blocks["wi_test01"][revision.DigestHex([]byte("written after last publish"))]; blk == nil {
		t.Fatal("late change must ride the flush revision")
	}
}

func TestFencingStopsBlockUpload(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.remote.tokenOK = func(wi, token string) error {
		if token != "fresh" {
			return &fakeAPIError{code: "SYNC-403", reason: "stale_generation"}
		}
		return nil
	}
	req := e.req()
	req.Token = "stale"
	_, err := e.pub.Publish(context.Background(), req)
	if err == nil {
		t.Fatal("fenced-out owner must fail")
	}
	if e.remote.putCalls != 0 && e.remote.putCalls > 1 {
		t.Fatalf("fencing must bite at the FIRST block PUT (calls=%d)", e.remote.putCalls)
	}
}

// countingQuiescer records both ends of the freeze window (05 §2.7).
type countingQuiescer struct {
	mu       sync.Mutex
	quiesced int
	released int
}

func (q *countingQuiescer) RequestQuiesce(ctx context.Context) error {
	q.mu.Lock()
	q.quiesced++
	q.mu.Unlock()
	return nil
}

func (q *countingQuiescer) ReleaseQuiesce(ctx context.Context) error {
	q.mu.Lock()
	q.released++
	q.mu.Unlock()
	return nil
}

func (q *countingQuiescer) counts() (int, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.quiesced, q.released
}

// TestPublishRoundTripEndsUnquiesced is the FE-1 regression: the
// publisher acquires quiescence for the scan but MUST release it at the
// design-pinned point — immediately after the staging pin, BEFORE any
// network I/O (05 §2.7: “staging 固化完成后……立即释放 write freeze（通知 WI
// Agent 恢复写入），不等待块上传、manifest 提交或 HEAD 推进完成”). A publish
// round trip therefore ends with the agent un-quiesced, and the release
// has already fired when the first block PUT leaves.
func TestPublishRoundTripEndsUnquiesced(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.writeCapsule("artifacts/a.md", "hello")
	q := &countingQuiescer{}
	releasedBeforeFirstPut := false
	e.remote.onPutBlock = func() {
		if _, rel := q.counts(); rel > 0 {
			releasedBeforeFirstPut = true
		}
	}
	req := e.req()
	req.Conditions = freeze.Conditions{Quiescer: q, Timeout: 5 * time.Second}
	res := e.mustPublish(e.pub.Publish(context.Background(), req))
	if res.HeadSeq != 0 {
		t.Fatalf("bootstrap: %+v", res)
	}
	quiesced, released := q.counts()
	if quiesced != 1 {
		t.Fatalf("the scan window must quiesce the agent exactly once, got %d", quiesced)
	}
	if released != 1 {
		t.Fatalf("the round trip must END with the agent un-quiesced: released %d times, want 1", released)
	}
	if !releasedBeforeFirstPut {
		t.Fatal("the release must fire BEFORE the first block PUT (staging pin ends the window, not the upload)")
	}
	// The flow settled normally despite the release having happened mid-flow.
	if rec, _ := e.store.LoadPending("wi_test01"); rec != nil {
		t.Fatal("pending record must be settled after the round trip")
	}

	// Milestone abandonment (05 §2.7): an abandoned optional scan must ALSO
	// release the freeze request — the agent may have already paused.
	q2 := &milestoneQuiescer{delay: 500 * time.Millisecond}
	mreq := e.req()
	mreq.Conditions = freeze.Conditions{Quiescer: q2, Timeout: 20 * time.Millisecond, Milestone: true}
	mres, err := e.pub.Publish(context.Background(), mreq)
	if err != nil || mres == nil || !mres.Noop {
		t.Fatalf("milestone timeout must abandon the scan: %v %+v", err, mres)
	}
	if q2.releases != 1 {
		t.Fatalf("milestone abandonment must release the freeze request, got %d releases", q2.releases)
	}
}

// milestoneQuiescer never confirms within the window (timeout → abandon)
// but records the release notification.
type milestoneQuiescer struct {
	delay    time.Duration
	releases int
}

func (q *milestoneQuiescer) RequestQuiesce(ctx context.Context) error {
	select {
	case <-time.After(q.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *milestoneQuiescer) ReleaseQuiesce(ctx context.Context) error {
	q.releases++
	return nil
}

// TestPublishPersistentTransportFailureParksPending is the FE-5
// regression: a transport failure followed by a HEAD-at-parent
// disambiguation used to recurse into the resend unboundedly. It must
// terminate typed after ONE bounded resend, with the pending record (and
// its staging) left intact for the next trigger's recovery (05 §2.8/§7.2:
// no independent retry scheduling — convergence happens at the next §2.4
// trigger's full scan).
func TestPublishPersistentTransportFailureParksPending(t *testing.T) {
	e := newEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.writeCapsule("artifacts/a.md", "hello")
	e.remote.transportFailAll = true
	_, err := e.pub.Publish(context.Background(), e.req())
	if !errors.Is(err, ErrTransportUnresolved) {
		t.Fatalf("persistent transport failure must terminate typed, got %v", err)
	}
	// Bounded: the original attempt plus exactly one resend, then it stops.
	if e.remote.putCalls != 2 {
		t.Fatalf("exactly one bounded resend is allowed, saw %d block PUTs", e.remote.putCalls)
	}
	// The pending record and its staging survive for later recovery.
	if rec, rerr := e.store.LoadPending("wi_test01"); rerr != nil || rec == nil {
		t.Fatalf("the pending record must survive an unresolved transport failure: %v %+v", rerr, rec)
	}
	if _, serr := os.Stat(e.stager.SeqDir("wi_test01", 0)); serr != nil {
		t.Fatal("the pending record's staging must survive (it is the resend source)")
	}
}
