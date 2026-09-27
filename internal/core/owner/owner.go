// Package owner implements owner-generation arbitration: claim with idempotency
// hardening, generation lifecycle transitions and the generation-close flush
// evidence protocol. Design authority: docs/design/polyforge-vnext/02-aihub-core.md
// §2.3/§4.3/§6.1/§6.2/§6.4/§6.6.
package owner

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	tokenIssuesErr  error         // issuance-evidence table install failure (R2C-F5)
}

func NewManager(db *store.DB, svc *service.Service) *Manager {
	m := &Manager{db: db, svc: svc, freshnessWindow: 60 * time.Second}
	m.ensureTokenIssues()
	return m
}

// generation_token_issues is the R2C-F5 issuance-evidence table: one row
// per token ever issued for a (work_item, generation), carrying the sha256
// digest of the token (never the raw token — the same hash-only discipline
// as the server's share_tokens table) and the secret epoch it was signed
// under. 02 §6.6 step 4 mandates that SweepRetired DELETES a retired grace
// epoch's secret file, after which the HMAC can no longer be recomputed and
// a swept-epoch token is indistinguishable from a fenced one at the service
// layer; §6.6 also pins the reason taxonomy this classification feeds:
// secret_rotated_expired is the ONLY reason that makes the Daemon replay
// the claim (reauthorize), while stale_generation means the generation was
// genuinely taken over — no reauthorization, only local seal-and-exit — and
// epoch ≤ R (rollback boundary) is secret_rolled_back, permanent, shared by
// the token check and the claim replay (同一判定). That classification must
// therefore not depend on the claim_idempotency cache: §4.3's retention
// constraint keeps rows of active generations, but response_json is
// REWRITTEN on every §6.6 re-sign, so the previously-swept token drops out
// of the cache exactly when it matters, and takeover-issued tokens never
// enter it at all. This table is the immutable record: every issuance point
// (claimNew, reauthorize re-sign, ForceTakeover) appends to it in the SAME
// transaction that mints the token. It is owner-owned and installed here
// (CREATE TABLE IF NOT EXISTS) rather than in core/schema because the
// schema package is frozen v1 (02 §11: 一期无存量迁移); the server's
// share_tokens table is the established precedent for a face-owned table.
const tokenIssuesDDL = `CREATE TABLE IF NOT EXISTS generation_token_issues (
  work_item_id TEXT NOT NULL,
  generation   INTEGER NOT NULL,
  secret_epoch INTEGER NOT NULL,
  token_sha    TEXT NOT NULL,
  issued_at    TEXT NOT NULL,
  issued_via   TEXT NOT NULL CHECK (issued_via IN ('claim','reauthorize','takeover')),
  PRIMARY KEY (work_item_id, generation, token_sha)
)`

// ensureTokenIssues installs the issuance-evidence table. NewManager cannot
// fail, so an install error is captured and surfaced by the first recording
// call (a claim/takeover fails loudly) while classification degrades to the
// pre-F5 cache fallback.
func (m *Manager) ensureTokenIssues() {
	_, m.tokenIssuesErr = m.db.Exec(tokenIssuesDDL)
}

// tokenDigest is the stored form of an issued token: sha256 hex of the hex
// token string. The raw token never reaches this table.
func tokenDigest(tokenHex string) string {
	sum := sha256.Sum256([]byte(tokenHex))
	return hex.EncodeToString(sum[:])
}

// execer is satisfied by both *store.DB (embedding *sql.DB) and *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// recordTokenIssue appends one issuance-evidence row through the caller's
// handle (inside the transaction that mints the token). issued_via ∈
// claim|reauthorize|takeover, mirroring the three issuance points of
// §6.1/§6.6/§6.2.
func (m *Manager) recordTokenIssue(q execer, wiID string, generation int64, epoch int, tokenHex, via string) error {
	if m.tokenIssuesErr != nil {
		return m.tokenIssuesErr
	}
	_, err := q.Exec(`INSERT OR IGNORE INTO generation_token_issues
		(work_item_id, generation, secret_epoch, token_sha, issued_at, issued_via)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6)`,
		wiID, generation, epoch, tokenDigest(tokenHex), store.Now(), via)
	return err
}

// ClaimRequest is the POST /work-items/{id}/claim body.
type ClaimRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	MachineID      string `json:"machine_id"`
}

// ClaimResponse is the hardened claim response cached in claim_idempotency.
type ClaimResponse struct {
	Generation       int64           `json:"generation"`
	GenerationToken  string          `json:"generation_token"`
	SecretEpoch      int             `json:"secret_epoch"`
	Owner            string          `json:"owner"`
	CapsuleBootstrap json.RawMessage `json:"capsule_bootstrap,omitempty"`
}

// ActiveGeneration returns the WI's active generation row, or ok=false.
func (m *Manager) ActiveGeneration(wiID string) (gen int64, ownerID, machine string, ok bool, err error) {
	return activeGeneration(m.db, wiID)
}

// queryRower is satisfied by both *store.DB (embedding *sql.DB) and *sql.Tx
// (same shape as service's): the in-transaction halves re-read state
// through the caller's transaction instead of asking the single-connection
// pool for a second connection (INV-2a self-deadlock).
type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// activeGeneration is ActiveGeneration over any handle satisfying
// queryRower, so the transactional halves (R3C-F1 replayReauthorizeTx) can
// re-read the bound generation inside their own transaction.
func activeGeneration(q queryRower, wiID string) (gen int64, ownerID, machine string, ok bool, err error) {
	var machineNull sql.NullString
	err = q.QueryRow(`SELECT generation, owner, machine_id FROM owner_generations
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
	// fix #9: a WI with an unfinished `blocks` dependency is excluded from the
	// executable ready-queue (02 §5.1b); claim hard-rejects it too (human
	// ruling), rather than silently opening a generation the queue would never
	// have offered.
	if blocked, err := m.hasUnfinishedBlockers(wiID); err != nil {
		return nil, err
	} else if blocked {
		return nil, errf("CORE-409", "DEPENDENCY_BLOCKED", "work item has an unfinished blocking dependency")
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
// secret_rolled_back (a rolled-back credential can never be revived).
//
// R3C-F1 (02 §6.6): the whole decision now runs inside ONE transaction.
// The previous shape read the rollback boundary, the active generation and
// the current epoch and signed the re-issue token on the shared connection
// BEFORE opening the write transaction: a mode=rollback rotation committing
// in that window was invisible to the stale boundary check (it still read
// the pre-rollback R), the cached pre-rollback claim was then re-signed with
// the NEW live secret, and the cache overwrite + issuance evidence
// committed — reviving the pre-rollback generation with a working credential
// and bypassing the admin force-takeover §6.6 makes the only revival path.
// Holding the store's single connection (INV-2a) across the boundary read,
// the generation check, the re-sign and both writes closes the window: a
// rollback that begins after this transaction opens cannot commit until it
// ends, and by then the re-signed token is itself a pre-rollback credential
// of the NEW rollback — retired with every other one, refused on its next
// check.
func (m *Manager) replayOrReclassify(wiID, idemKey, cachedResp string) (*ClaimResponse, error) {
	var resp ClaimResponse
	if err := json.Unmarshal([]byte(cachedResp), &resp); err != nil {
		return nil, err
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	resp, err = m.replayReauthorizeTx(tx, wiID, idemKey, resp)
	if err != nil {
		return nil, err
	}
	return &resp, tx.Commit()
}

// replayReauthorizeTx is the transactional half of the §6.6 idempotent
// replay (mirroring claimNew/endGenerationTx so the R3C-F1 rollback
// interleave is white-box testable): the boundary check, the generation
// binding, the epoch comparison, the re-sign and both writes — the cache
// overwrite and the issuance evidence — all run on the caller's open
// transaction, whose snapshot a concurrent rotation cannot land inside.
func (m *Manager) replayReauthorizeTx(tx *sql.Tx, wiID, idemKey string, resp ClaimResponse) (ClaimResponse, error) {
	R, err := m.svc.RollbackBoundaryTx(tx)
	if err != nil {
		return resp, err
	}
	if resp.SecretEpoch < R {
		return resp, errf("CORE-403", "secret_rolled_back", "cached credential predates rollback boundary %d", R)
	}
	gen, _, machine, active, err := activeGeneration(tx, wiID)
	if err != nil {
		return resp, err
	}
	if gen != resp.Generation {
		return resp, errf("CORE-404", "", "bound generation no longer matches the active generation")
	}
	if !active {
		return resp, errf("CORE-403", "forbidden", "bound generation is no longer active")
	}
	current, err := m.svc.CurrentEpochTx(tx)
	if err != nil {
		return resp, err
	}
	if resp.SecretEpoch == current {
		return resp, nil
	}
	token, epoch, err := m.svc.SignTokenTx(tx, wiID, resp.Generation, machine)
	if err != nil {
		return resp, err
	}
	resp.GenerationToken = token
	resp.SecretEpoch = epoch
	blob, err := json.Marshal(resp)
	if err != nil {
		return resp, err
	}
	if _, err := tx.Exec(`UPDATE claim_idempotency SET response_json=?1
		WHERE idempotency_key=?2 AND work_item_id=?3`, string(blob), idemKey, wiID); err != nil {
		return resp, err
	}
	// R2C-F5: the re-signed token's evidence is recorded in the SAME
	// transaction as the cache overwrite. Ordering matters: the cache
	// rewrite is exactly what ERASES the previously-swept token from the
	// mutable fallback, so the immutable evidence must land atomically
	// with it — never after — or a crash between the two would leave the
	// re-signed generation without an evidence row for its live token.
	if err := m.recordTokenIssue(tx, wiID, resp.Generation, epoch, token, "reauthorize"); err != nil {
		return resp, err
	}
	return resp, nil
}

// claimNew opens the new generation, all inside ONE transaction. R2C-F4
// (02 §2.3/§6.1/§6.5, the same TOCTOU discipline cancelTx already has): the
// Claim prechecks above run on the shared connection BEFORE this
// transaction opens, so a Cancel can commit `cancelled` in the window
// between them and the INSERT below — resurrecting a cancelled WI with a
// live generation, exactly what §2.5 forbids (cancel terminalizes a WI
// that by §6.5 never had an active generation). The WI status, the
// active-generation absence, the execution-ticket (bundle) property and
// the unfinished-blockers state are therefore re-read INSIDE the
// transaction — which holds the store's single connection (INV-2a), so
// nothing can interleave past this point — and the status UPDATE is
// guarded: 0 rows affected means the race was lost and a typed error is
// returned with nothing written.
func (m *Manager) claimNew(wiID string, req ClaimRequest, callerIdentity string, bootstrap json.RawMessage) (*ClaimResponse, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// In-transaction state re-validation (R2C-F4): mirrors the prechecks
	// in Claim, but on the transaction's snapshot.
	var status string
	var bundleName sql.NullString
	if err := tx.QueryRow(`SELECT status, bundle_name FROM work_items WHERE id=?1`, wiID).
		Scan(&status, &bundleName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errf("CORE-404", "", "work item %s", wiID)
		}
		return nil, err
	}
	switch status {
	case "wrapped", "failed", "cancelled":
		// The reverse claim/cancel race: the outer prechecks saw a claimable
		// status, a Cancel committed in the window, and this transaction
		// now observes the terminal state. Refuse — never resurrect.
		return nil, errf("CORE-410", "", "work item is in terminal state %s", status)
	case "queued", "paused", "blocked":
		// claim / resume / unblock — all one claim (§2.3/§6.1).
	default:
		return nil, errf("CORE-409", "ALREADY_CLAIMED", "claim from status %s not legal", status)
	}
	if !bundleName.Valid || bundleName.String == "" {
		return nil, errf("CORE-409", "NOT_CLAIMABLE", "registration-only work item (bundle_name IS NULL)")
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1 AND ended_at IS NULL`, wiID).Scan(&active); err != nil {
		return nil, err
	}
	if active > 0 {
		return nil, errf("CORE-409", "ALREADY_CLAIMED", "an active generation exists")
	}
	if blocked, err := unfinishedBlockers(tx, wiID); err != nil {
		return nil, err
	} else if blocked {
		return nil, errf("CORE-409", "DEPENDENCY_BLOCKED", "work item has an unfinished blocking dependency")
	}

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
		// A concurrent claimant that read "no active generation" at the same
		// moment (TOCTOU window between Claim's ActiveGeneration check and this
		// transaction) loses the race here: the uq_owner_gen_active partial
		// unique index refuses the second INSERT. Surface that as the same
		// typed CORE-409 ALREADY_CLAIMED the pre-check would have returned had
		// it run a moment later, rather than a raw driver error.
		if isUniqueViolation(err) {
			return nil, errf("CORE-409", "ALREADY_CLAIMED", "an active generation exists")
		}
		return nil, err
	}
	// Guarded transition (same discipline as cancelTx): the WHERE clause
	// re-asserts the claimable status, and 0 rows affected means a concurrent
	// terminalization won — refuse with nothing written rather than
	// flipping a non-claimable row to running.
	res, err := tx.Exec(`UPDATE work_items SET assignee=?1, status='running', waiting_on=NULL, updated_at=?2
		WHERE id=?3 AND status IN ('queued','paused','blocked')`,
		callerIdentity, store.Now(), wiID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errf("CORE-410", "", "work item %s is no longer claimable (state changed concurrently)", wiID)
	}

	// SignTokenTx (not SignToken): this call runs INSIDE the open tx above, and
	// with the store's single-connection pool (INV-2a) a plain SignToken here
	// would ask for a second connection while this one is held — a
	// self-deadlock. SignTokenTx reads the epoch through the same tx handle.
	token, epoch, err := m.svc.SignTokenTx(tx, wiID, gen, req.MachineID)
	if err != nil {
		return nil, err
	}
	// R2C-F5: issuance evidence lands in the SAME transaction that mints
	// the token, so a committed generation always carries its evidence row.
	if err := m.recordTokenIssue(tx, wiID, gen, epoch, token, "claim"); err != nil {
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
	Reason        string         `json:"reason"` // pause|block|wrap|fail
	FlushEvidence *FlushEvidence `json:"flush_evidence"`
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
	// fix #11: 02 §2.4 invariant — status='blocked' implies a non-empty
	// waiting_on, and waiting_on's value domain is closed (§2.4). A block
	// transition with a missing or out-of-domain waiting_on would otherwise
	// commit a blocked row that violates that invariant.
	if req.Reason == "block" {
		if strings.TrimSpace(req.WaitingOn) == "" {
			return errf("CORE-400", "", "waiting_on is required when reason=block")
		}
		if err := validateWaitingOn(req.WaitingOn); err != nil {
			return err
		}
	}
	gen, ownerID, machine, active, err := m.ActiveGeneration(wiID)
	if err != nil {
		return err
	}
	if !active {
		return errf("CORE-409", "", "no active generation to end")
	}
	res := m.classifyToken(tokenHex, wiID, gen, machine)
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
	if err := m.endGenerationTx(tx, wiID, gen, req, ownerID); err != nil {
		return err
	}
	return tx.Commit()
}

// endGenerationTx performs the generation-close writes inside the caller's
// open transaction. FA-1 (takeover race): the guarded UPDATE on
// owner_generations is the atomic still-the-owner test. The pre-transaction
// checks (active generation + token + flush evidence) run on the shared
// connection BEFORE the transaction opens, so a concurrent force-takeover
// can end this very generation in that window; the UPDATE then matches 0
// rows and this MUST be refused as fencing (CORE-403 stale_generation, 02
// §2.3/§6.2) instead of terminalizing the WI under a dead generation's
// authority. Checking RowsAffected inside the transaction closes the race:
// with the store's single connection (INV-2a) no further write can land
// between the check and the commit.
func (m *Manager) endGenerationTx(tx *sql.Tx, wiID string, gen int64, req EndGenerationRequest, ownerID string) error {
	now := store.Now()
	res, err := tx.Exec(`UPDATE owner_generations SET ended_at=?1, end_reason=?2
		WHERE work_item_id=?3 AND generation=?4 AND ended_at IS NULL`, now, req.Reason, wiID, gen)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errf("CORE-403", "stale_generation",
			"generation %d was ended concurrently (takeover/duplicate close); refusing to transition the work item", gen)
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
	// assignee stays the last owner (§6.2): it is deliberately not rewritten.
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
	return nil
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

// hasUnfinishedBlockers reports whether wiID has any non-terminal `blocks`
// dependency (fix #9). Reimplemented here rather than imported from
// workitem: owner must not import workitem (would create a cycle), so this
// is a minimal, self-contained query over the same two tables. The query
// runs through queryRower so the in-transaction re-validation of claimNew
// (R2C-F4) can share the caller's transaction instead of asking the pool
// for a second connection (INV-2a self-deadlock).
func (m *Manager) hasUnfinishedBlockers(wiID string) (bool, error) {
	return unfinishedBlockers(m.db, wiID)
}

// unfinishedBlockers is hasUnfinishedBlockers over any handle satisfying
// QueryRow (both *store.DB and *sql.Tx).
func unfinishedBlockers(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, wiID string) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM wi_dependencies d
		JOIN work_items b ON b.id = d.blocking_wi_id
		WHERE d.blocked_wi_id=?1 AND d.kind='blocks'
		AND b.status NOT IN ('wrapped','failed','cancelled')`, wiID).Scan(&n)
	return n > 0, err
}

// validateWaitingOn enforces the closed waiting_on value domain of 02
// §2.4: "" | owner | runtime:<machine_id> | dependency:<wi_id> | gate.
// Kept local (like hasUnfinishedBlockers) because owner must not import
// workitem.
func validateWaitingOn(wo string) error {
	switch {
	case wo == "" || wo == "owner" || wo == "gate":
		return nil
	case strings.HasPrefix(wo, "runtime:"):
		if strings.TrimPrefix(wo, "runtime:") == "" {
			return errf("CORE-400", "", "waiting_on runtime: requires a machine_id (02 §2.4)")
		}
		return nil
	case strings.HasPrefix(wo, "dependency:"):
		if strings.TrimPrefix(wo, "dependency:") == "" {
			return errf("CORE-400", "", "waiting_on dependency: requires a wi_id (02 §2.4)")
		}
		return nil
	}
	return errf("CORE-400", "", "waiting_on %q is outside the §2.4 value domain (owner|runtime:<machine_id>|dependency:<wi_id>|gate|empty)", wo)
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
	res := m.classifyToken(tokenHex, wiID, gen, machine)
	return gen, res.OK, res.Reason, nil
}

// classifyToken is the service-level HMAC check refined for the §6.6
// reason taxonomy. service.CheckToken alone cannot distinguish the two
// "no HMAC match" outcomes once SweepRetired has deleted an expired grace
// epoch's secret file (§6.6 step 4 mandates the deletion): a token signed
// under that swept epoch reads the same as a fenced/garbage one, and the
// plain fallback would classify it stale_generation — the one reason that
// must NOT trigger the §6.6 claim-replay reauthorization (stale_generation
// pins a genuine takeover: seal locally and exit, never re-sign). The
// binding of a presented token to its signing secret_epoch is therefore
// read from RECORDED ISSUANCE EVIDENCE (R2C-F5), not from live HMACs:
//
//   - bound epoch < R (rollback boundary; the rollback epoch's own
//     freshly minted secret stays live, like every other token check)
//     ⇒ secret_rolled_back — no revival path, admin force-takeover only
//   - bound epoch retired (grace expired) ⇒ secret_rotated_expired — the
//     only reason that makes the Daemon replay the claim (§6.6)
//   - anything else (live-epoch token that fails the active generation's
//     triple, or an unknown token) ⇒ stale_generation (genuinely fenced)
//
// The evidence source is the generation_token_issues table (one immutable
// row per issuance: claim, §6.6 reauthorize re-sign, takeover), scoped to
// the ACTIVE generation being checked — a token minted for a dead
// generation stays stale_generation no matter its epoch's disposition.
// The claim_idempotency cache (§4.3) is kept only as a fallback for tokens
// issued before the evidence table existed: it is mutable (§6.6 re-sign
// overwrites response_json, dropping the previously-swept token) and
// incomplete (takeover-issued tokens never enter it), so it cannot be the
// classification authority.
func (m *Manager) classifyToken(tokenHex, wiID string, gen int64, machine string) service.TokenResult {
	res := m.svc.CheckToken(tokenHex, wiID, gen, machine)
	if res.OK || res.Reason != "stale_generation" {
		return res
	}
	epoch, found, err := m.issuedTokenEpoch(wiID, gen, tokenHex)
	if err != nil || !found {
		// Fallback for tokens minted before the evidence table: the mutable
		// idempotency cache. Degradation is the pre-F5 behavior (stale),
		// never a false reauthorize signal.
		epoch, found, err = m.cachedTokenEpoch(wiID, gen, tokenHex)
		if err != nil || !found {
			return res
		}
	}
	disp, err := m.svc.EpochDisposition(epoch)
	if err != nil {
		return res
	}
	switch disp {
	case "rolled_back":
		return service.TokenResult{Reason: "secret_rolled_back"}
	case "retired":
		return service.TokenResult{Reason: "secret_rotated_expired"}
	}
	return res
}

// issuedTokenEpoch looks the presented token's digest up in the immutable
// issuance-evidence rows of this WI's generation and returns the secret
// epoch it was signed under (found=false when no issuance recorded it).
func (m *Manager) issuedTokenEpoch(wiID string, gen int64, tokenHex string) (epoch int, found bool, err error) {
	if m.tokenIssuesErr != nil {
		return 0, false, m.tokenIssuesErr
	}
	err = m.db.QueryRow(`SELECT secret_epoch FROM generation_token_issues
		WHERE work_item_id=?1 AND generation=?2 AND token_sha=?3`,
		wiID, gen, tokenDigest(tokenHex)).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return epoch, true, nil
}

// cachedTokenEpoch looks the presented token up in the claim_idempotency
// cache of this WI and returns the secret epoch it was signed under
// (found=false when no cached response carries this token for THIS
// generation — a token minted for an older generation must not be
// refined, it is fenced by definition).
func (m *Manager) cachedTokenEpoch(wiID string, gen int64, tokenHex string) (epoch int, found bool, err error) {
	rows, err := m.db.Query(`SELECT response_json FROM claim_idempotency WHERE work_item_id=?1`, wiID)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return 0, false, err
		}
		var resp ClaimResponse
		if json.Unmarshal([]byte(blob), &resp) != nil {
			continue
		}
		if resp.Generation == gen && resp.GenerationToken == tokenHex {
			return resp.SecretEpoch, true, nil
		}
	}
	return 0, false, rows.Err()
}

func isHexLower(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// isUniqueViolation is a best-effort, driver-agnostic detection of a SQLite
// UNIQUE/PRIMARY KEY constraint failure (modernc.org/sqlite's error strings
// contain this phrase; there is no typed sentinel exported for it here).
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT")
}

// ForceTakeover ends the old active generation (end_reason=takeover) and
// opens a new one for the admin caller (§6.2). No flush evidence, no old token
// required. The returned response carries the new generation's token.
func (m *Manager) ForceTakeover(wiID, callerID, machineID string) (*ClaimResponse, error) {
	// fix #5: mirror Claim's state validation — an admin must not be able to
	// resurrect a terminal WI or open a generation on a registration ticket
	// (bundle_name IS NULL, which has no owner_generations/Capsule concept at
	// all) via force-takeover.
	var status string
	var bundleName sql.NullString
	err := m.db.QueryRow(`SELECT status, bundle_name FROM work_items WHERE id=?1`, wiID).Scan(&status, &bundleName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf("CORE-404", "", "work item %s", wiID)
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case "wrapped", "failed", "cancelled":
		return nil, errf("CORE-410", "", "work item is in terminal state %s", status)
	}
	if !bundleName.Valid || bundleName.String == "" {
		return nil, errf("CORE-409", "NOT_CLAIMABLE", "registration-only work item (bundle_name IS NULL)")
	}
	// FA-2: 02 §2.5/§6.2 restrict force-takeover to a RUNNING work item —
	// the state machine row is `running` (held by another machine), and the
	// §6.2 step-2 semantics (end the OLD ACTIVE generation, open a new one)
	// presuppose one. Taking over a queued/paused/blocked WI would bypass
	// the tier-2 claim path (§5.1) and resurrect execution without the
	// §2.5 transition that is supposed to open the generation.
	if status != "running" {
		return nil, errf("CORE-409", "NOT_RUNNING", "force-takeover requires a running work item (02 §2.5), got %s", status)
	}
	// Pre-transaction snapshot of the active generation, on the shared
	// connection (ActiveGeneration uses m.db, and with the store's single
	// connection (INV-2a) calling it while the tx already holds that one
	// connection would self-deadlock waiting for a second connection that
	// never comes). R2C-F9: this snapshot is only the reference the
	// in-transaction re-read inside forceTakeoverTx is compared against —
	// never the value the takeover writes are derived from.
	g, _, _, active, aerr := m.ActiveGeneration(wiID)
	if aerr != nil {
		return nil, aerr
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	resp, err := m.forceTakeoverTx(tx, wiID, callerID, machineID, g, active)
	if err != nil {
		return nil, err
	}
	return resp, tx.Commit()
}

// forceTakeoverTx performs the takeover writes inside the caller's open
// transaction (the transactional half, mirroring endGenerationTx/cancelTx
// so the race states are white-box testable). R2C-F9: the active generation
// is re-read INSIDE the transaction and compared against the caller's
// pre-transaction snapshot (preGen, preActive). The snapshot goes stale the
// moment another takeover commits in the window before this transaction
// opened: two concurrent takeovers that both read generation g pre-tx would
// otherwise both end g and both insert g+1 — the loser dying on
// uq_owner_gen_seq with a raw driver error. Serialized on the store's
// single connection (INV-2a), the loser's in-tx read observes the winner's
// committed generation and is refused with a typed, retryable CORE-409
// TAKEOVER_RACE before any write; generations stay strictly sequential.
func (m *Manager) forceTakeoverTx(tx *sql.Tx, wiID, callerID, machineID string, preGen int64, preActive bool) (*ClaimResponse, error) {
	// In-transaction status re-check (same TOCTOU class as FA-1): the
	// status was read on the shared connection BEFORE this transaction
	// opened, so the owner may have paused/blocked/wrapped in between; a
	// takeover must then be refused, not forced onto a non-running WI.
	var statusNow string
	if err := tx.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&statusNow); err != nil {
		return nil, err
	}
	if statusNow != "running" {
		return nil, errf("CORE-409", "NOT_RUNNING", "force-takeover requires a running work item (02 §2.5), got %s", statusNow)
	}
	var genNow int64
	err := tx.QueryRow(`SELECT generation FROM owner_generations
		WHERE work_item_id=?1 AND ended_at IS NULL`, wiID).Scan(&genNow)
	activeNow := !errors.Is(err, sql.ErrNoRows)
	if err != nil && activeNow {
		return nil, err
	}
	if activeNow != preActive || (activeNow && genNow != preGen) {
		return nil, errf("CORE-409", "TAKEOVER_RACE",
			"the active generation changed since validation (concurrent takeover or generation close); retry the force-takeover")
	}
	now := store.Now()
	var gen int64
	if activeNow {
		gen = genNow
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
	// R2C-F5: the takeover's token is signed and its issuance evidence
	// recorded INSIDE the transaction (SignTokenTx reads the epoch through
	// the tx handle — INV-2a). Previously the token was minted after the
	// commit with NO evidence at all, so once its epoch was swept (§6.6
	// step 4 deletes the secret file) the token was unclassifiable and read
	// stale_generation — the reason that forbids reauthorization — instead
	// of secret_rotated_expired.
	token, epoch, err := m.svc.SignTokenTx(tx, wiID, gen, machineID)
	if err != nil {
		return nil, err
	}
	if err := m.recordTokenIssue(tx, wiID, gen, epoch, token, "takeover"); err != nil {
		return nil, err
	}
	return &ClaimResponse{Generation: gen, GenerationToken: token, SecretEpoch: epoch, Owner: callerID}, nil
}
