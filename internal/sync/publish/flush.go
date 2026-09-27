package publish

// flush.go: the generation-end final flush and the Daemon-side flush
// evidence generation (05 §6.1, D6 ruling; server validation authority
// 02 §6.4). The evidence's factual fields ALL come from a REAL frozen scan
// executed by the Daemon — the caller never self-reports. No-HEAD empty
// chains accept exactly one sentinel shape: published_seq=0 with
// bytes_digest == EMPTY_SYNC_BYTES_DIGEST, and only when this generation
// never produced sync-surface writes.

import (
	"context"
	"errors"
	"time"

	"github.com/silgrid/polyforge/internal/sync/aihub"
	"github.com/silgrid/polyforge/internal/sync/freeze"
	"github.com/silgrid/polyforge/internal/sync/revision"
	"github.com/silgrid/polyforge/internal/workflow/digest"
)

// FlushEvidence mirrors aihub.FlushEvidence (one wire shape, defined once).
type FlushEvidence = aihub.FlushEvidence

// FrozenCheckpoint mirrors aihub.FrozenCheckpoint.
type FrozenCheckpoint = aihub.FrozenCheckpoint

// FlushRequest parameterizes one end-generation final flush.
type FlushRequest struct {
	WorkItemID string
	CapsuleDir string
	Generation int64
	Token      string
	// Archive runs BEFORE the frozen scan (05 §6.1 step 1): the session
	// transcript archive must ride this last revision. May be nil.
	Archive func() error
	// Conditions carry the freeze acquisition faces (quiescer /
	// process-group-dead shortcut).
	Conditions freeze.Conditions
}

// FinalFlush executes the 05 §6.1 generation-end sequence and returns the
// evidence the end-generation call must carry (02 §6.4):
//
//  1. archive the session transcript (caller hook, before the scan);
//  2. resolve any open pending record (evidence is untrustworthy while one
//     is undecided — 04 §6.5 step 3's sync boundary);
//  3. frozen scan — ALWAYS actually executed, even when nothing changed
//     (02 §6.4 explicitly forbids inferring from the old HEAD);
//  4. branch:
//     - no HEAD, never wrote, empty surface → the empty-chain sentinel
//     evidence (published_seq=0, bytes_digest=EMPTY_SYNC_BYTES_DIGEST);
//     - no HEAD but writes exist → bootstrap publish (real seq=0) first —
//     unpublished local changes are never silently dropped;
//     - HEAD present and the frozen scan equals the last published digest →
//     no-change evidence anchored at HEAD (frozen.manifest_sha = HEAD row
//     sha: an ALIGNMENT declaration, not a fresh candidate digest);
//     - HEAD present and the scan differs → real publish; evidence anchors
//     at the new revision with matches_head=true (UNPUBLISHED_CHANGES is
//     not a legal submit shape).
func (p *Publisher) FinalFlush(ctx context.Context, req FlushRequest) (*FlushEvidence, error) {
	release := p.Mutex.LockWI(req.WorkItemID)
	defer release()

	// Step 1: transcript archive rides the last revision.
	if req.Archive != nil {
		if err := req.Archive(); err != nil {
			return nil, err
		}
	}
	// Step 2: no undecided publish may coexist with evidence generation.
	if rec, err := p.Store.LoadPending(req.WorkItemID); err != nil {
		return nil, err
	} else if rec != nil {
		if _, err := p.ResolvePending(ctx, req.WorkItemID, rec.Token); err != nil {
			return nil, err
		}
	}

	if err := p.Mutex.Acquire(ctx, req.WorkItemID, req.Conditions); err != nil {
		if errors.Is(err, freeze.ErrMilestoneAbandoned) {
			// 05 §2.7: an abandoned milestone scan releases the freeze
			// request too — the agent may have already paused writes.
			_ = freeze.Release(ctx, req.Conditions)
		}
		return nil, err
	}
	// R2N-4: the window is open — every exit path from here (scan/digest
	// failure, HEAD/state failure, the sentinel and no-change branches
	// that skip commitScan, and commitScan's own pre-pin failures) must
	// release the freeze, or the agent stays quiesced forever.
	guard := newFreezeGuard(ctx, req.Conditions)
	defer guard.release()
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
	scannedAt := time.Now().UTC().Format(time.RFC3339Nano)

	head, headFound, err := p.Remote.GetHead(ctx, req.WorkItemID)
	if err != nil {
		return nil, err
	}
	state, err := p.Store.LoadState(req.WorkItemID)
	if err != nil {
		return nil, err
	}

	if !headFound {
		// Empty-chain branch (05 §6.1 step 3 / SYNC-INV-9): allowed ONLY
		// when this generation never produced ANY sync-surface write.
		// Judgment basis: local publish history (state), pending records
		// (settled above) and the scan itself — an empty surface with no
		// history is the never-wrote shape; anything else must publish.
		if state == nil && bytesDigest == digest.EmptySyncBytesDigest {
			return &FlushEvidence{
				PublishedGeneration: req.Generation,
				PublishedSeq:        0, // "no revision exists" sentinel, NOT a real seq 0
				ManifestSha:         "",
				FrozenCheckpoint: &FrozenCheckpoint{
					ScannedAt: scannedAt, BytesDigest: digest.EmptySyncBytesDigest,
					WorktreeState: worktreeDigest, ManifestSha: "", MatchesHead: false,
				},
			}, nil
		}
		// Bootstrap publish first (05 §2.6), then normal evidence below.
		res, perr := p.commitScan(ctx, req.WorkItemID, req.CapsuleDir, req.Generation, req.Token,
			guard, scan, bytesDigest, worktreeDigest)
		if perr != nil {
			return nil, perr
		}
		return evidenceForPublish(req.Generation, res, scannedAt, bytesDigest, worktreeDigest), nil
	}

	// HEAD exists: compare the frozen scan with the last PUBLISHED state.
	// Same-digest basis (05 §6.1 branch 2) is the Daemon's own recorded
	// digest; a missing or unaligned local state (cross-machine recovery
	// without a seed) forces the publish path — never a fabricated
	// "no change".
	aligned := state != nil && state.Seq == head.Seq && state.ManifestSha == head.ManifestSha
	if aligned && state.BytesDigest == bytesDigest {
		return &FlushEvidence{
			PublishedGeneration: req.Generation,
			PublishedSeq:        head.Seq,
			ManifestSha:         head.ManifestSha,
			FrozenCheckpoint: &FrozenCheckpoint{
				ScannedAt: scannedAt, BytesDigest: bytesDigest, WorktreeState: worktreeDigest,
				// Alignment declaration: equals the outer manifest_sha
				// (= current HEAD row sha), never a fresh scan digest.
				ManifestSha: head.ManifestSha, MatchesHead: true,
			},
		}, nil
	}
	res, perr := p.commitScan(ctx, req.WorkItemID, req.CapsuleDir, req.Generation, req.Token,
		guard, scan, bytesDigest, worktreeDigest)
	if perr != nil {
		return nil, perr
	}
	return evidenceForPublish(req.Generation, res, scannedAt, bytesDigest, worktreeDigest), nil
}

// evidenceForPublish builds the evidence for the "scan found changes and the
// publish completed" shape: frozen.manifest_sha IS the new revision's
// idempotent-view digest — the same scan's product.
func evidenceForPublish(generation int64, res *Result, scannedAt, bytesDigest, worktreeDigest string) *FlushEvidence {
	return &FlushEvidence{
		PublishedGeneration: generation,
		PublishedSeq:        res.HeadSeq,
		ManifestSha:         res.HeadManifestSha,
		FrozenCheckpoint: &FrozenCheckpoint{
			ScannedAt: scannedAt, BytesDigest: bytesDigest, WorktreeState: worktreeDigest,
			ManifestSha: res.HeadManifestSha, MatchesHead: true,
		},
	}
}
