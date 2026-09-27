// Package publish implements the single full-scan publish path
// (05-sync-revision-recovery.md §6 six steps, §2.7 freeze/mutex/staging,
// §2.8 pending records + GET-HEAD disambiguation, §6.1 final flush and
// flush-evidence generation). The Daemon is the only party that may execute
// it (INV-11 / INV-D3); the WI Agent only requests.
package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/sync/freeze"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

// Remote is the AIHub publish face the flow talks to (05 §5.1 rows 14–16).
// Implemented by the HTTP client in production and by fakes in tests.
type Remote interface {
	PutBlock(ctx context.Context, wiID string, seq int64, shaHex string, block []byte, token string) error
	PostManifest(ctx context.Context, wiID string, m revision.Manifest, manifestSha, token string) (headSeq int64, err error)
	GetHead(ctx context.Context, wiID string) (*revision.HeadInfo, bool, error)
}

// Quiescer aliases the freeze face for callers of this package.
type Quiescer = freeze.Quiescer

// ErrNothingToDisambiguate is internal bookkeeping noise.
var ErrNothingToDisambiguate = errors.New("publish: no pending record")

// Request is one full-scan publish request (05 §6 step 1: the trigger).
type Request struct {
	WorkItemID string
	CapsuleDir string
	Generation int64
	Token      string
	// Milestone marks the optional trigger class: on freeze timeout the scan
	// is abandoned instead of escalated (05 §2.7).
	Milestone bool
	// Conditions carry the freeze acquisition faces (quiescer, process-group
	// death, replacement-write detection) resolved by the caller per WI.
	Conditions freeze.Conditions
}

// Result is the settled outcome of one publish flow.
type Result struct {
	// Published: HEAD advanced by THIS flow.
	Published bool
	// Idempotent: the server had already accepted an identical manifest
	// (idempotency check hit — §2.8); no second revision row exists.
	Idempotent bool
	// Noop: the flow legitimately did not publish (milestone abandoned).
	Noop            bool
	HeadSeq         int64
	HeadManifestSha string
}

// ---------------------------------------------------------------------------
// Persistence: pending publish records (SYNC-INV-8) + per-WI publish state.

// PendingRecord is the pre-upload persisted record (05 §2.8 fix B10):
// everything needed to (a) resend or (b) disambiguate this flow after a
// crash between upload start and confirmation. Deleted only after a DEFINITE
// server outcome (any explicit response, or GET-HEAD disambiguation says
// accepted).
type PendingRecord struct {
	WorkItemID    string                       `json:"work_item_id"`
	Manifest      revision.Manifest            `json:"manifest"`
	ManifestSha   string                       `json:"manifest_sha"`
	Seq           int64                        `json:"seq"`
	Parent        *int64                       `json:"parent"`
	Generation    int64                        `json:"generation"`
	Token         string                       `json:"token"`
	StagingSeq    int64                        `json:"staging_seq"`
	LiveFiles     map[string]revision.FileInfo `json:"live_files"`
	BytesDigest   string                       `json:"bytes_digest"`
	WorktreeState string                       `json:"worktree_state"`
	CreatedAt     string                       `json:"created_at"`
}

// PublishState is the Daemon's per-WI memory of the last publish outcome:
// HEAD position, the live file set (the incremental-diff parent), the
// frozen-scan digests for final-flush "no change" comparison (05 §6.1
// branch 2), and the cumulative data bytes driving the checkpoint cadence.
// Cross-machine recovery seeds this via SeedFromRecovery.
type PublishState struct {
	WorkItemID      string                       `json:"work_item_id"`
	Seq             int64                        `json:"seq"`
	ManifestSha     string                       `json:"manifest_sha"`
	Generation      int64                        `json:"generation"`
	LiveFiles       map[string]revision.FileInfo `json:"live_files"`
	BytesDigest     string                       `json:"bytes_digest"`
	WorktreeState   string                       `json:"worktree_state"`
	CumulativeBytes int64                        `json:"cumulative_bytes"`
}

// Store persists records and states under <root>/{pending,publishstate}.
type Store struct {
	Root string
}

// NewStore creates the persistence root.
func NewStore(daemonLocal string) *Store {
	return &Store{Root: daemonLocal}
}

func (s *Store) pendingPath(wi string) string {
	return filepath.Join(s.Root, "pending", wi+".json")
}

func (s *Store) statePath(wi string) string {
	return filepath.Join(s.Root, "publishstate", wi+".json")
}

// SavePending atomically persists the pending record (tmp+fsync+rename).
func (s *Store) SavePending(rec *PendingRecord) error {
	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicJSON(s.pendingPath(rec.WorkItemID), blob)
}

// LoadPending returns the WI's open pending record, if any.
func (s *Store) LoadPending(wi string) (*PendingRecord, error) {
	raw, err := os.ReadFile(s.pendingPath(wi))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec PendingRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("publish: pending record for %s unparsable: %w", wi, err)
	}
	return &rec, nil
}

// DeletePending removes the record (after a definite outcome only).
func (s *Store) DeletePending(wi string) error {
	err := os.Remove(s.pendingPath(wi))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// SaveState atomically persists the per-WI publish state.
func (s *Store) SaveState(st *PublishState) error {
	blob, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicJSON(s.statePath(st.WorkItemID), blob)
}

// LoadState returns the WI's publish state, if any.
func (s *Store) LoadState(wi string) (*PublishState, error) {
	raw, err := os.ReadFile(s.statePath(wi))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st PublishState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("publish: state for %s unparsable: %w", wi, err)
	}
	return &st, nil
}

func atomicJSON(path string, blob []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(blob); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// Publisher

// Publisher executes the six-step flow. All state is Daemon-local; the only
// remote face is Remote.
type Publisher struct {
	Remote Remote
	Mutex  *freeze.Manager
	Stager *freeze.Stager
	Store  *Store
}

// New wires a publisher.
func New(remote Remote, mutex *freeze.Manager, stager *freeze.Stager, store *Store) *Publisher {
	return &Publisher{Remote: remote, Mutex: mutex, Stager: stager, Store: store}
}

// freezeGuard ends the write-freeze window EXACTLY ONCE, on every exit
// path between freeze.Acquire and the design-pinned release point (05
// §2.7: the staging pin completes, the Daemon releases immediately).
//
// R2N-4: before this guard, every post-acquire FAILURE path (scan, digest,
// HEAD fetch, state load, manifest build, staging pin) returned without
// releasing — the WI Agent stayed quiesced FOREVER, a permanent freeze
// indistinguishable from a hung daemon. The guard is armed right after a
// successful Acquire and released via the caller's defer, so an error
// anywhere in the window still notifies the agent it may resume;
// commitScan releases through the SAME once at the pinned point, making
// the deferred release a no-op there. Release stays best-effort per the
// design (the bytes are pinned before it matters).
type freezeGuard struct {
	ctx  context.Context
	cond freeze.Conditions
	once sync.Once
}

func newFreezeGuard(ctx context.Context, cond freeze.Conditions) *freezeGuard {
	return &freezeGuard{ctx: ctx, cond: cond}
}

// release ends the freeze window once: at the design-pinned point when
// commitScan calls it after the staging pin, or on ANY exit path via the
// caller's defer.
func (g *freezeGuard) release() {
	g.once.Do(func() { _ = freeze.Release(g.ctx, g.cond) })
}

// Publish runs one full-scan publish (05 §6 steps 2–6; step 1 is the caller
// deciding to request). Serialize per WI via the publish mutex; hold the
// write freeze only through staging.
func (p *Publisher) Publish(ctx context.Context, req Request) (*Result, error) {
	release := p.Mutex.LockWI(req.WorkItemID)
	defer release()

	// A still-open pending record from a crashed flow is disambiguated
	// FIRST (04 §6.5 step 3's sync boundary): its outcome must be final
	// before a new scan can be trusted.
	if rec, err := p.Store.LoadPending(req.WorkItemID); err != nil {
		return nil, err
	} else if rec != nil {
		if _, err := p.ResolvePending(ctx, req.WorkItemID, rec.Token); err != nil {
			return nil, err
		}
	}

	// Step 2: write freeze (05 §2.7).
	if err := p.Mutex.Acquire(ctx, req.WorkItemID, req.Conditions); err != nil {
		if errors.Is(err, freeze.ErrMilestoneAbandoned) {
			// 05 §2.7 timeout handling: an abandoned milestone scan must
			// also release the freeze request ("释放 write freeze 请求与
			// publish mutex") — the agent may have already paused writes.
			_ = freeze.Release(ctx, req.Conditions)
			return &Result{Noop: true}, nil
		}
		return nil, err // forced-trigger timeout: infra failure escalation
	}
	// R2N-4: the window is open — from here to the pinned release point
	// EVERY exit path must release the freeze (see freezeGuard).
	guard := newFreezeGuard(ctx, req.Conditions)
	defer guard.release()

	// Step 3: full scan inside the window.
	scan, err := revision.ScanCapsule(req.CapsuleDir)
	if err != nil {
		return nil, err
	}
	bytesDigest, err := scan.BytesDigest()
	if err != nil {
		return nil, err
	}
	worktreeDigest, err := revision.WorktreeDigest(req.CapsuleDir)
	if err != nil {
		return nil, err
	}
	return p.commitScan(ctx, req.WorkItemID, req.CapsuleDir, req.Generation, req.Token,
		guard, scan, bytesDigest, worktreeDigest)
}

// commitScan is steps 3c–6 shared by Publish and the final-flush publish
// branch: manifest build, staging pin (closing the freeze window), pending
// record, block upload from staging only, manifest submit, settlement.
// The per-WI publish mutex must already be held; guard carries the freeze
// faces and releases the window EXACTLY ONCE — at the pinned point below
// on the success path, or (already) via the caller's deferred release when
// any pre-pin step (state load, HEAD fetch, manifest build/validation,
// staging pin) fails. Without the guard a pre-pin failure left the agent
// quiesced forever (R2N-4).
func (p *Publisher) commitScan(ctx context.Context, wi, capsuleDir string, generation int64, token string,
	guard *freezeGuard, scan *revision.ScanResult, bytesDigest, worktreeDigest string) (*Result, error) {

	state, err := p.Store.LoadState(wi)
	if err != nil {
		return nil, err
	}
	head, headFound, err := p.Remote.GetHead(ctx, wi)
	if err != nil {
		return nil, err
	}
	var seq int64
	var parent *int64
	var parentScan *revision.ScanResult
	if state != nil && headFound && head.Seq == state.Seq && head.ManifestSha == state.ManifestSha {
		// Normal in-generation advance from our own last publish.
		seq = state.Seq + 1
		pv := state.Seq
		parent = &pv
		parentScan = &revision.ScanResult{Files: state.LiveFiles}
	} else if !headFound {
		// Bootstrap (05 §2.6): seq=0, parent=null.
		seq = 0
		parent = nil
	} else {
		// HEAD moved without aligned local state (cross-machine takeover
		// without a recovery seed): incremental ops are impossible without
		// the parent's live set — the safe move is a CHECKPOINT on remote
		// HEAD carrying the full inventory.
		seq = head.Seq + 1
		pv := head.Seq
		parent = &pv
		parentScan = nil
	}
	kind := revision.KindIncremental
	if revision.ShouldCheckpoint(seq, cumulative(state)) || (parentScan == nil && headFound) {
		kind = revision.KindCheckpoint
	}
	manifest, err := revision.BuildManifest(parentScan, scan, revision.BuildOptions{
		WorkItemID: wi, Seq: seq, Parent: parent,
		Generation: generation, Kind: kind, BuiltAt: time.Now(),
	})
	if err != nil {
		return nil, err
	}
	if err := revision.ValidateOps(manifest.Ops); err != nil {
		return nil, err
	}
	sha, err := revision.ComputeManifestSha(*manifest)
	if err != nil {
		return nil, err
	}

	// Window tail: pin every referenced byte into immutable staging; the
	// write freeze window ENDS when this returns (05 §2.7).
	entries := []freeze.StageEntry{}
	for _, op := range manifest.Ops {
		if op.Op == revision.OpPut {
			entries = append(entries, freeze.StageEntry{
				AbsPath: filepath.Join(capsuleDir, filepath.FromSlash(op.Path)),
				Sha256:  op.Sha256,
			})
		}
	}
	if _, err := p.Stager.Stage(wi, seq, entries); err != nil {
		return nil, err
	}

	// 05 §2.7 release, the design-pinned point: the staging pin is
	// complete, so the write freeze window ENDS HERE — "Daemon 立即释放
	// write freeze（通知 WI Agent 恢复写入）……不等待块上传、manifest 提交或
	// HEAD 推进完成". The pending record below and all network I/O happen
	// OUTSIDE the window, still under the publish mutex. The notify is
	// best-effort: the bytes are already pinned, so a failed notify cannot
	// corrupt this publish — it only delays the agent's own writes, which
	// the next trigger's quiesce round re-establishes (the design pins no
	// failure semantics for the release notification).
	guard.release()

	// SYNC-INV-8: the pending record exists BEFORE the first block PUT.
	rec := &PendingRecord{
		WorkItemID: wi, Manifest: *manifest, ManifestSha: sha,
		Seq: seq, Parent: parent, Generation: generation, Token: token,
		StagingSeq: seq, LiveFiles: scan.Files, BytesDigest: bytesDigest,
		WorktreeState: worktreeDigest, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := p.Store.SavePending(rec); err != nil {
		return nil, err
	}
	return p.uploadAndSubmit(ctx, wi, rec)
}

// ErrTransportUnresolved marks a publish whose transport failure survived
// its one bounded disambiguation-and-resend round (05 §2.8/§7.2): the flow
// terminates WITHOUT settling, the pending publish record and its staging
// directory stay on disk, and convergence is deferred to the next §2.4
// trigger's full scan ("失败后的收敛发生在下一次触发时刻的全扫描内，不存在
// 独立的重试调度", 05 §2.5).
var ErrTransportUnresolved = errors.New("publish: transport failure persists after the bounded disambiguation round; pending record kept for later recovery")

// uploadAndSubmit performs steps 4c–6: block upload from STAGING ONLY (never
// the live tree), manifest submit, outcome settlement, pending-record and
// staging cleanup. The publish mutex is expected to be held by the caller
// (Publish) or re-acquired by ResolvePending. Transport failures get ONE
// bounded disambiguation-and-resend round (resends); a failure that
// survives it terminates typed with the pending record intact (05 §2.5:
// no independent retry scheduling — the next trigger's full scan converges).
func (p *Publisher) uploadAndSubmit(ctx context.Context, wi string, rec *PendingRecord) (*Result, error) {
	return p.upload(ctx, wi, rec, 1)
}

// upload is uploadAndSubmit with an explicit resend budget.
func (p *Publisher) upload(ctx context.Context, wi string, rec *PendingRecord, resends int) (*Result, error) {
	for _, op := range rec.Manifest.Ops {
		if op.Op != revision.OpPut {
			continue
		}
		block, err := p.Stager.Block(wi, rec.StagingSeq, op.Sha256)
		if err != nil {
			return nil, fmt.Errorf("publish: staging block %s unreadable: %w", op.Sha256, err)
		}
		// Upload the zstd-compressed form; the digest addresses the original.
		encoded, err := revision.EncodeBlock(block)
		if err != nil {
			return nil, err
		}
		if err := p.Remote.PutBlock(ctx, wi, rec.Seq, op.Sha256, encoded, rec.Token); err != nil {
			// Definite server refusals settle the flow (no resends on 4xx);
			// transport loss keeps the pending record for disambiguation.
			if isDefiniteRefusal(err) {
				p.settleFailed(ctx, wi, rec)
				return nil, err
			}
			// Response lost mid-flight: disambiguate NOW via GET-HEAD
			// (05 §2.8); the record survives if the branches cannot decide.
			if res, derr := p.resolve(ctx, wi, rec.Token, resends); derr == nil && res != nil {
				return res, nil
			} else if derr != nil && errors.Is(derr, ErrTransportUnresolved) {
				return nil, derr
			}
			return nil, err
		}
	}
	headSeq, err := p.Remote.PostManifest(ctx, wi, rec.Manifest, rec.ManifestSha, rec.Token)
	if err != nil {
		if isDefiniteRefusal(err) {
			// A definite SYNC-4xx settles the flow: this publish did NOT
			// happen, HEAD did not move, the next trigger reconverges.
			p.settleFailed(ctx, wi, rec)
			return nil, err
		}
		// Confirmation lost: GET-HEAD disambiguation (three branches),
		// bounded by the same resend budget.
		res, derr := p.resolve(ctx, wi, rec.Token, resends)
		if derr != nil {
			if errors.Is(derr, ErrTransportUnresolved) {
				return nil, derr
			}
			return nil, err
		}
		return res, nil
	}
	idem := headSeq != rec.Seq // server returned the CURRENT head, not ours: idempotent replay of an already-accepted manifest
	p.settleSuccess(wi, rec, headSeq)
	return &Result{
		Published:       !idem,
		Idempotent:      idem,
		HeadSeq:         headSeq,
		HeadManifestSha: rec.ManifestSha,
	}, nil
}

// ResolvePending runs the GET-HEAD three-branch disambiguation (05 §2.8):
//  1. HEAD == record.seq AND sha matches   → ACCEPTED: settle as success.
//  2. HEAD == record.parent (or no HEAD row and record.parent == nil)
//     → NEVER ACCEPTED: safe resend from staging — ONE bounded round; a
//     transport failure that survives it terminates typed with the record
//     intact (ErrTransportUnresolved), never an unbounded retry loop.
//  3. anything else                       → STALE: drop the record; the
//     next full scan reconverges on the remote truth.
//
// Returns the settled Result, or nil when the record was dropped as stale.
func (p *Publisher) ResolvePending(ctx context.Context, wi, token string) (*Result, error) {
	return p.resolve(ctx, wi, token, 1)
}

func (p *Publisher) resolve(ctx context.Context, wi, token string, resends int) (*Result, error) {
	rec, err := p.Store.LoadPending(wi)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, ErrNothingToDisambiguate
	}
	head, found, err := p.Remote.GetHead(ctx, wi)
	if err != nil {
		return nil, err
	}
	if !found {
		if rec.Parent == nil {
			// Branch 2 (bootstrap never accepted): safe resend.
			return p.resendOrPark(ctx, wi, rec, resends)
		}
		// Branch 3: no HEAD but a non-bootstrap record — stale.
		p.dropRecord(wi, rec)
		return nil, nil
	}
	if head.Seq == rec.Seq && head.ManifestSha == rec.ManifestSha {
		// Branch 1: accepted; settle without resending anything.
		p.settleSuccess(wi, rec, head.Seq)
		return &Result{Published: true, HeadSeq: head.Seq, HeadManifestSha: rec.ManifestSha}, nil
	}
	if rec.Parent != nil && head.Seq == *rec.Parent {
		// Branch 2: server still at the parent — resend from staging.
		return p.resendOrPark(ctx, wi, rec, resends)
	}
	// Branch 3: HEAD beyond the record, or same seq different sha — stale.
	p.dropRecord(wi, rec)
	return nil, nil
}

// resendOrPark performs branch 2's safe resend within the budget: with
// resends left it retries the flow (each retry itself bounded — the retry
// passes a DECREMENTED budget so a persistently failing transport can
// never recurse unboundedly); with none left it parks the flow — the
// pending record and its staging stay on disk for the next trigger's
// recovery (05 §2.8/§7.2).
func (p *Publisher) resendOrPark(ctx context.Context, wi string, rec *PendingRecord, resends int) (*Result, error) {
	if resends > 0 {
		return p.upload(ctx, wi, rec, resends-1)
	}
	return nil, fmt.Errorf("%w (wi=%s seq=%d)", ErrTransportUnresolved, wi, rec.Seq)
}

func (p *Publisher) settleSuccess(wi string, rec *PendingRecord, headSeq int64) {
	st := &PublishState{
		WorkItemID: wi, Seq: rec.Seq, ManifestSha: rec.ManifestSha,
		Generation: rec.Generation, LiveFiles: rec.LiveFiles,
		BytesDigest: rec.BytesDigest, WorktreeState: rec.WorktreeState,
	}
	if prev, err := p.Store.LoadState(wi); err == nil && prev != nil {
		st.CumulativeBytes = prev.CumulativeBytes + rec.Manifest.DataBytes
	} else {
		st.CumulativeBytes = rec.Manifest.DataBytes
	}
	_ = p.Store.SaveState(st)
	_ = p.Store.DeletePending(wi)
	_ = p.Stager.Cleanup(wi, rec.StagingSeq)
}

func (p *Publisher) settleFailed(ctx context.Context, wi string, rec *PendingRecord) {
	_ = p.Store.DeletePending(wi)
	_ = p.Stager.Cleanup(wi, rec.StagingSeq)
}

func (p *Publisher) dropRecord(wi string, rec *PendingRecord) {
	_ = p.Store.DeletePending(wi)
	_ = p.Stager.Cleanup(wi, rec.StagingSeq)
}

func cumulative(st *PublishState) int64 {
	if st == nil {
		return 0
	}
	return st.CumulativeBytes
}

// isDefiniteRefusal reports whether the error is an explicit server verdict
// (any HTTP response, success or 4xx) rather than a lost/ambiguous one.
func isDefiniteRefusal(err error) bool {
	// The HTTP client returns *aihub.APIError for every answered request,
	// including 5xx; transport failures are plain net errors. We treat every
	// answered request as definite per 05 §2.8 ("任何明确响应…确认落定"),
	// 5xx included: a 507 means the server saw the request and refused.
	var typed interface{ IsAPIError() bool }
	if errors.As(err, &typed) {
		return true
	}
	return false
}

// SeedFromRecovery initializes the per-WI publish state after a remote-site
// reconstruction (05 §7.4): the recovered live set IS the parent for the
// first incremental of the new generation.
func (p *Publisher) SeedFromRecovery(wi string, gen int64, head *revision.HeadInfo, live map[string]revision.FileInfo, bytesDigest, worktreeDigest string) error {
	st := &PublishState{
		WorkItemID: wi, Generation: gen, LiveFiles: live,
		BytesDigest: bytesDigest, WorktreeState: worktreeDigest,
	}
	if head != nil {
		st.Seq = head.Seq
		st.ManifestSha = head.ManifestSha
	}
	return p.Store.SaveState(st)
}
