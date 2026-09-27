// Package owner implements owner-generation arbitration: claim with idempotency
// hardening, generation lifecycle transitions and the generation-close flush
// evidence protocol. Design authority: docs/design/polyforge-vnext/02-aihub-core.md
// §2.3/§4.3/§6.1/§6.2/§6.4/§6.6.
package owner

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/workflow/digest"
)

// APIError is the typed Core error carried to the HTTP face.
type APIError struct {
	Code   string // CORE-409 etc.
	Reason string // machine-readable sub-reason ("" if none)
	Detail string
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s %s: %s", e.Code, e.Reason, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func errf(code, reason, format string, args ...any) *APIError {
	return &APIError{Code: code, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Manager owns generation transitions.
type Manager struct {
	db              *store.DB
	svc             *service.Service
	freshnessWindow time.Duration // flush-evidence scanned_at freshness (02 §6.4, default 60s)
}

func NewManager(db *store.DB, svc *service.Service) *Manager {
	return &Manager{db: db, svc: svc, freshnessWindow: 60 * time.Second}
}

// ClaimRequest is the POST /work-items/{id}/claim body.
type ClaimRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	MachineID      string `json:"machine_id"`
}

// ClaimResponse is the hardened claim response cached in claim_idempotency.
type ClaimResponse struct {
	Generation      int64           `json:"generation"`
	GenerationToken string          `json:"generation_token"`
	SecretEpoch     int             `json:"secret_epoch"`
	Owner           string          `json:"owner"`
	CapsuleBootstrap json.RawMessage `json:"capsule_bootstrap,omitempty"`
}

// ActiveGeneration returns the WI's active generation row, or ok=false.
func (m *Manager) ActiveGeneration(wiID string) (gen int64, ownerID, machine string, ok bool, err error) {
	var machineNull sql.NullString
	err = m.db.QueryRow(`SELECT generation, owner, machine_id FROM owner_generations
		WHERE work_item_id=?1 AND ended_at IS NULL`, wiID).Scan(&gen, &ownerID, &machineNull)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", false, nil
	}
	if err != nil {
		return 0, "", "", false, err
	}
	return gen, ownerID, machineNull.String, true, nil
}

// Claim executes §6.1 (with the §6.6 replay/reauthorize semantics on cache
// hits). callerIdentity is the api-key's user id (INV-2d triple binding).
// bootstrap is the capsule_bootstrap payload (bundle reference + budget
// baseline + repo inventory, 02 §8) assembled by the caller.
func (m *Manager) Claim(wiID string, req ClaimRequest, callerIdentity string, bootstrap json.RawMessage) (*ClaimResponse, error) {
	if req.IdempotencyKey == "" {
		return nil, errf("CORE-400", "", "idempotency_key required")
	}

	// 1. Idempotency check first — before any WI state validation (§6.1/§6.6).
	var cachedResp, cachedIdentity, cachedWI string
	err := m.db.QueryRow(`SELECT response_json, caller_identity, work_item_id FROM claim_idempotency
		WHERE idempotency_key=?1`, req.IdempotencyKey).Scan(&cachedResp, &cachedIdentity, &cachedWI)
	if err == nil {
		if cachedIdentity != callerIdentity || cachedWI != wiID {
			return nil, errf("CORE-409", "IDEMPOTENCY_KEY_REUSED", "key is bound to a different caller/work item")
		}
		return m.replayOrReclassify(wiID, req.IdempotencyKey, cachedResp)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// 2. State validation.
	var status string
	var bundleName sql.NullString
	err = m.db.QueryRow(`SELECT status, bundle_name FROM work_items WHERE id=?1`, wiID).
		Scan(&status, &bundleName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf("CORE-404", "", "work item %s", wiID)
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case "wrapped", "failed", "cancelled":
		return nil, errf("CORE-410", "", "work item is in terminal state %s", status)
	case "queued", "paused", "blocked":
		// claim / resume / unblock — all one claim (§2.3/§6.1).
	default:
		return nil, errf("CORE-409", "ALREADY_CLAIMED", "claim from status %s not legal", status)
	}
	if !bundleName.Valid || bundleName.String == "" {
		return nil, errf("CORE-409", "NOT_CLAIMABLE", "registration-only work item (bundle_name IS NULL)")
	}
	if _, _, _, active, err := m.ActiveGeneration(wiID); err != nil {
		return nil, err
	} else if active {
		return nil, errf("CORE-409", "ALREADY_CLAIMED", "an active generation exists")
	}

	// 3-6. New generation + token + idempotency row + event, one transaction.
	return m.claimNew(wiID, req, callerIdentity, bootstrap)
}

// replayOrReclassify handles an idempotency-cache hit (02 §6.6 step 3/4):
// the bound generation must still be active; a cached token whose epoch went
// stale is re-signed under the current epoch (a replay never returns a dead
// token); a cache predating the rollback boundary R is refused as
// secret_rolled_back (回滚不可复活).
func (m *Manager) replayOrReclassify(wiID, idemKey, cachedResp string) (*ClaimResponse, error) {
	var resp ClaimResponse
	if err := json.Unmarshal([]byte(cachedResp), &resp); err != nil {
		return nil, err
	}
	R, err := m.svc.RollbackBoundary()
	if err != nil {
		return nil, err
	}
	if resp.SecretEpoch < R {
		return nil, errf("CORE-403", "secret_rolled_back", "cached credential predates rollback boundary %d", R)
	}
	gen, _, machine, active, err := m.ActiveGeneration(wiID)
	if err != nil {
		return nil, err
	}
	if gen != resp.Generation {
		return nil, errf("CORE-404", "", "bound generation no longer matches the active generation")
	}
	if !active {
		return nil, errf("CORE-403", "forbidden", "bound generation is no longer active")
	}
	current, err := m.svc.CurrentEpoch()
	if err != nil {
		return nil, err
	}
	if resp.SecretEpoch != current {
		token, epoch, err := m.svc.SignToken(wiID, resp.Generation, machine)
		if err != nil {
			return nil, err
		}
		resp.GenerationToken = token
		resp.SecretEpoch = epoch
		blob, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}
		if _, err := m.db.Exec(`UPDATE claim_idempotency SET response_json=?1
			WHERE idempotency_key=?2 AND work_item_id=?3`, string(blob), idemKey, wiID); err != nil {
			return nil, err
		}
	}
	return &resp, nil
}

func (m *Manager) claimNew(wiID string, req ClaimRequest, callerIdentity string, bootstrap json.RawMessage) (*ClaimResponse, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var maxGen sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(generation) FROM owner_generations WHERE work_item_id=?1`, wiID).Scan(&maxGen); err != nil {
		return nil, err
	}
	gen := maxGen.Int64 + 1

	var machineAny any
	if req.MachineID != "" {
		machineAny = req.MachineID
	}
	if _, err := tx.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, machine_id, started_at)
		VALUES (?1, ?2, ?3, ?4, ?5)`, wiID, gen, callerIdentity, machineAny, store.Now()); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE work_items SET assignee=?1, status='running', waiting_on=NULL, updated_at=?2 WHERE id=?3`,
		callerIdentity, store.Now(), wiID); err != nil {
		return nil, err
	}

	token, epoch, err := m.svc.SignToken(wiID, gen, req.MachineID)
	if err != nil {
		return nil, err
	}
	resp := ClaimResponse{
		Generation:       gen,
		GenerationToken:  token,
		SecretEpoch:      epoch,
		Owner:            callerIdentity,
		CapsuleBootstrap: bootstrap,
	}
	blob, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO claim_idempotency(idempotency_key, work_item_id, caller_identity, response_json, created_at)
		VALUES (?1, ?2, ?3, ?4, ?5)`, req.IdempotencyKey, wiID, callerIdentity, string(blob), store.Now()); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO wi_events(work_item_id, created_at, actor, event_type, payload)
		VALUES (?1, ?2, ?3, 'claimed', ?4)`, wiID, store.Now(), callerIdentity,
		`{"generation":`+fmt.Sprint(gen)+`}`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &resp, nil
}

// EndGenerationRequest is the four-in-one end-generation body (02 §5.1 row 9).
type EndGenerationRequest struct {
	Reason        string          `json:"reason"` // pause|block|wrap|fail
	FlushEvidence *FlushEvidence  `json:"flush_evidence"`
	// WaitingOn accompanies reason=block (workflow failure mapping: §2.5
	// maps workflow terminal states to business signal blocked + waiting_on=owner).
	WaitingOn string `json:"waiting_on,omitempty"`
}

// FlushEvidence is 02 §6.4's evidence object.
type FlushEvidence struct {
	PublishedGeneration int64             `json:"published_generation"`
	PublishedSeq        int64             `json:"published_seq"`
	ManifestSha         string            `json:"manifest_sha"`
	FrozenCheckpoint    *FrozenCheckpoint `json:"frozen_checkpoint"`
}

// FrozenCheckpoint binds the evidence to the post-freeze final local checkpoint.
type FrozenCheckpoint struct {
	ScannedAt     string `json:"scanned_at"`
	BytesDigest   string `json:"bytes_digest"`
	WorktreeState string `json:"worktree_state"`
	ManifestSha   string `json:"manifest_sha"`
	MatchesHead   bool   `json:"matches_head"`
}

var endEvent = map[string]string{
	"pause": "paused", "block": "blocked", "wrap": "wrapped", "fail": "failed",
}

// EndGeneration closes the current active generation (pause/block/wrap/fail)
// after token fencing and full flush-evidence validation (§6.4 rules 1-8).
func (m *Manager) EndGeneration(wiID string, req EndGenerationRequest, tokenHex string) error {
	switch req.Reason {
	case "pause", "block", "wrap", "fail":
	default:
		return errf("CORE-400", "", "reason must be one of pause|block|wrap|fail")
	}
	gen, ownerID, machine, active, err := m.ActiveGeneration(wiID)
	if err != nil {
		return err
	}
	if !active {
		return errf("CORE-409", "", "no active generation to end")
	}
	res := m.svc.CheckToken(tokenHex, wiID, gen, machine)
	if !res.OK {
		return errf("CORE-403", res.Reason, "generation token rejected")
	}
	if err := m.validateFlushEvidence(wiID, gen, req.FlushEvidence); err != nil {
		return err
	}

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := store.Now()
	if _, err := tx.Exec(`UPDATE owner_generations SET ended_at=?1, end_reason=?2
		WHERE work_item_id=?3 AND generation=?4 AND ended_at IS NULL`, now, req.Reason, wiID, gen); err != nil {
		return err
	}
	nextStatus := map[string]string{
		"pause": "paused", "block": "blocked", "wrap": "wrapped", "fail": "failed",
	}[req.Reason]
	if _, err := tx.Exec(`UPDATE work_items SET status=?1, updated_at=?2,
		waiting_on=CASE WHEN ?1='blocked' AND ?3<>'' THEN ?3 ELSE waiting_on END,
		closed_at=CASE WHEN ?1 IN ('wrapped','failed') THEN ?2 ELSE closed_at END
		WHERE id=?4`, nextStatus, now, req.WaitingOn, wiID); err != nil {
		return err
	}
	// assignee 保留为最后一任 owner（§6.2），不改写。
	payload := map[string]any{"generation": gen}
	if nextStatus == "blocked" && req.WaitingOn != "" {
		payload["reason"] = req.WaitingOn
	}
	pblob, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO wi_events(work_item_id, created_at, actor, event_type, payload)
		VALUES (?1, ?2, ?3, ?4, ?5)`, wiID, now, ownerID, endEvent[req.Reason], string(pblob)); err != nil {
		return err
	}
	return tx.Commit()
}

// validateFlushEvidence implements the server-side rules of 02 §6.4 (rules
// 2-8; rule 1 = token check done by the caller of this function's caller).
func (m *Manager) validateFlushEvidence(wiID string, activeGen int64, fe *FlushEvidence) error {
	if fe == nil || fe.FrozenCheckpoint == nil {
		return errf("CORE-425", "MISSING", "flush_evidence (or frozen_checkpoint) missing")
	}
	if fe.PublishedGeneration != activeGen {
		return errf("CORE-425", "GENERATION_MISMATCH", "published_generation %d ≠ active generation %d", fe.PublishedGeneration, activeGen)
	}
	var headSeq, headGen sql.NullInt64
	var headSha sql.NullString
	err := m.db.QueryRow(`SELECT h.seq, h.generation, r.manifest_sha
		FROM capsule_heads h JOIN capsule_revisions r ON r.work_item_id=h.work_item_id AND r.seq=h.seq
		WHERE h.work_item_id=?1`, wiID).Scan(&headSeq, &headGen, &headSha)
	hasHead := true
	if errors.Is(err, sql.ErrNoRows) {
		hasHead = false
	} else if err != nil {
		return err
	}

	if !hasHead {
		// Rule 8: empty-chain branch (§6.4 / 05 §6.1 SYNC-INV-9). Only
		// matches_head and the manifest_sha equality are waived; the frozen
		// checkpoint's own field checks still apply.
		if fe.PublishedSeq != 0 {
			return errf("CORE-425", "NOT_HEAD", "no HEAD exists; published_seq must be the 0 sentinel")
		}
		if fe.ManifestSha != "" {
			return errf("CORE-425", "NOT_HEAD", "no HEAD exists; manifest_sha must be empty")
		}
		if fe.FrozenCheckpoint.BytesDigest != digest.EmptySyncBytesDigest {
			return errf("CORE-425", "MISSING", "empty chain requires bytes_digest == EMPTY_SYNC_BYTES_DIGEST")
		}
		return m.checkFrozenCheckpointFields(fe.FrozenCheckpoint)
	}

	if fe.PublishedSeq != headSeq.Int64 {
		return errf("CORE-425", "NOT_HEAD", "published_seq %d ≠ current HEAD %d", fe.PublishedSeq, headSeq.Int64)
	}
	if !headSha.Valid || fe.ManifestSha != headSha.String {
		return errf("CORE-425", "MANIFEST_MISMATCH", "manifest_sha does not match the HEAD row")
	}

	// Rule 6: frozen checkpoint binding.
	fc := fe.FrozenCheckpoint
	if fc.ManifestSha != fe.ManifestSha {
		return errf("CORE-425", "FROZEN_CHECKPOINT_MISMATCH", "frozen_checkpoint.manifest_sha must equal the outer manifest_sha")
	}
	if !fc.MatchesHead {
		return errf("CORE-425", "UNPUBLISHED_CHANGES", "frozen scan found unpublished changes; publish before ending the generation")
	}
	return m.checkFrozenCheckpointFields(fc)
}

// checkFrozenCheckpointFields validates the frozen checkpoint's own
// fact fields (presence, shapes, freshness) — shared by both branches.
func (m *Manager) checkFrozenCheckpointFields(fc *FrozenCheckpoint) error {
	if len(fc.BytesDigest) != 64 || !isHexLower(fc.BytesDigest) {
		return errf("CORE-425", "FROZEN_CHECKPOINT_MISSING", "bytes_digest missing or not 64-hex")
	}
	if len(fc.WorktreeState) != 64 || !isHexLower(fc.WorktreeState) {
		return errf("CORE-425", "FROZEN_CHECKPOINT_MISSING", "worktree_state missing or not 64-hex")
	}
	if fc.ScannedAt == "" {
		return errf("CORE-425", "FROZEN_CHECKPOINT_MISSING", "scanned_at missing")
	}
	var scanned time.Time
	var perr error
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		scanned, perr = time.Parse(layout, fc.ScannedAt)
		if perr == nil {
			break
		}
	}
	if perr != nil {
		return errf("CORE-425", "FROZEN_CHECKPOINT_MISSING", "scanned_at not RFC3339: %v", perr)
	}
	if time.Since(scanned) > m.freshnessWindow {
		return errf("CORE-425", "FROZEN_CHECKPOINT_STALE", "scanned_at %s older than the freshness window", fc.ScannedAt)
	}
	return nil
}


// CheckTokenForWI validates a presented token against the WI's active
// generation (used by capsule fencing and the sync endpoints).
func (m *Manager) CheckTokenForWI(wiID, tokenHex string) (gen int64, ok bool, reason string, err error) {
	gen, _, machine, active, err := m.ActiveGeneration(wiID)
	if err != nil {
		return 0, false, "", err
	}
	if !active {
		return gen, false, "stale_generation", nil
	}
	res := m.svc.CheckToken(tokenHex, wiID, gen, machine)
	return gen, res.OK, res.Reason, nil
}

func isHexLower(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ForceTakeover ends the old active generation (end_reason=takeover) and
// opens a new one for the admin caller (§6.2). No flush evidence, no old token
// required. The returned response carries the new generation's token.
func (m *Manager) ForceTakeover(wiID, callerID, machineID string) (*ClaimResponse, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := store.Now()
	var gen int64
	if g, _, _, active, err := m.ActiveGeneration(wiID); err != nil {
		return nil, err
	} else if active {
		gen = g
		if _, err := tx.Exec(`UPDATE owner_generations SET ended_at=?1, end_reason='takeover'
			WHERE work_item_id=?2 AND generation=?3 AND ended_at IS NULL`, now, wiID, gen); err != nil {
			return nil, err
		}
		gen++
	} else {
		var maxGen sql.NullInt64
		if err := tx.QueryRow(`SELECT MAX(generation) FROM owner_generations WHERE work_item_id=?1`, wiID).Scan(&maxGen); err != nil {
			return nil, err
		}
		gen = maxGen.Int64 + 1
	}
	var machineAny any
	if machineID != "" {
		machineAny = machineID
	}
	if _, err := tx.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, machine_id, started_at)
		VALUES (?1, ?2, ?3, ?4, ?5)`, wiID, gen, callerID, machineAny, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE work_items SET assignee=?1, status='running', waiting_on=NULL, updated_at=?2 WHERE id=?3`,
		callerID, now, wiID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO wi_events(work_item_id, created_at, actor, event_type, payload)
		VALUES (?1, ?2, ?3, 'claimed', ?4)`, wiID, now, callerID, `{"generation":`+fmt.Sprint(gen)+`}`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	token, epoch, err := m.svc.SignToken(wiID, gen, machineID)
	if err != nil {
		return nil, err
	}
	return &ClaimResponse{Generation: gen, GenerationToken: token, SecretEpoch: epoch, Owner: callerID}, nil
}
