package core

// Claim/generation arbitration with attempt-scoped push credentials —
// the v1 Generation-fencing MAIN layer (v1-spec-full §六): every claim
// mints one credential (username = work-item id, random secret); takeover
// ends the previous generation, whose credential then fails git auth at
// the proxy. Idempotency: the same key + caller + WI replays the same
// attempt (no new generation).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// OwnerManager arbitrates generations.
type OwnerManager struct{ db *DB }

func NewOwnerManager(db *DB) *OwnerManager { return &OwnerManager{db: db} }

// Claim intents.
const (
	IntentClaim    = "claim"
	IntentResume   = "resume"
	IntentTakeover = "takeover"
)

// ClaimRequest is the claim body (wire-compatible with the pf client).
type ClaimRequest struct {
	Intent         string `json:"intent"` // claim|resume|takeover
	MachineID      string `json:"machine_id,omitempty"`
	Owner          string `json:"owner,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ClaimResult is the minted attempt + credential (the cached half of the
// claim response; the server attaches a fresh work-item view).
type ClaimResult struct {
	Claim      api.Claim          `json:"claim"`
	Credential api.PushCredential `json:"credential"`
}

// generation row as stored.
type generationRow struct {
	WorkItemID string
	Generation int64
	AttemptID  string
	Owner      string
	MachineID  string
	SecretHash string
	StartedAt  string
}

// credentialTTL is the nominal expiry stamped on minted credentials.
// Enforcement is the generation fence (an ended generation's credential
// fails at the git face immediately); the timestamp is informational.
const credentialTTL = 24 * time.Hour

// idempotencyTTL bounds how long a cached claim response may be replayed.
// Tradeoff (#22): claim_idempotency.response_json deliberately stores the
// PLAINTEXT attempt token/secret — replay-after-lost-response is the point
// of the cache, and owner_generations keeps only the hash, so the plaintext
// would otherwise be unrecoverable. The TTL caps how long that plaintext
// sits at rest: past the window a replay is refused with CORE-409 and the
// caller must claim again with a fresh key (minting a new attempt). Stale
// rows are not deleted here — the gate renders them inert, and physical
// cleanup is a deploy-level sweep.
var idempotencyTTL = 24 * time.Hour

func (m *OwnerManager) activeGeneration(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, wiID string) (*generationRow, bool, error) {
	var g generationRow
	var machine sql.NullString
	err := q.QueryRow(`SELECT work_item_id, generation, attempt_id, owner, machine_id, secret_hash, started_at
		FROM owner_generations WHERE work_item_id=?1 AND ended_at IS NULL`, wiID).
		Scan(&g.WorkItemID, &g.Generation, &g.AttemptID, &g.Owner, &machine, &g.SecretHash, &g.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	g.MachineID = machine.String
	return &g, true, nil
}

// ActiveAttempt returns the active generation's attempt id ("" when none).
func (m *OwnerManager) ActiveAttempt(wiID string) (string, error) {
	g, ok, err := m.activeGeneration(m.db, wiID)
	if err != nil || !ok {
		return "", err
	}
	return g.AttemptID, nil
}

// CheckPushCredential validates a git-face Basic pair: username must be
// the work-item id and the secret must belong to the CURRENT active
// generation's attempt (takeover/ended generations fail here — the
// fencing main layer). Returns the active attempt id on success.
func (m *OwnerManager) CheckPushCredential(wiID, username, secret string) (string, error) {
	if wiID == "" || username != wiID || secret == "" {
		return "", Fail(api.ErrGitUnauthorized, "BAD_CREDENTIAL", "credential rejected")
	}
	g, ok, err := m.activeGeneration(m.db, wiID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", Fail(api.ErrGitUnauthorized, "NO_ACTIVE_ATTEMPT", "credential rejected: no active attempt")
	}
	if sha256Hex(secret) != g.SecretHash {
		return "", Fail(api.ErrGitUnauthorized, "BAD_CREDENTIAL", "credential rejected")
	}
	return g.AttemptID, nil
}

// Claim executes the claim flow (§6.1 v0 semantics, v1 credential shape).
// callerIdentity is the api-key's user id (idempotency triple binding).
func (m *OwnerManager) Claim(wiID string, req ClaimRequest, callerIdentity string) (*ClaimResult, error) {
	switch req.Intent {
	case IntentClaim, IntentResume, IntentTakeover:
	default:
		return nil, Fail(api.ErrCoreBadInput, "", "intent must be one of claim|resume|takeover")
	}
	if req.IdempotencyKey != "" {
		// Idempotency first: same key + same caller + same WI replays the
		// same attempt; a key bound elsewhere is a different request.
		var cached, cachedIdentity, cachedWI, cachedAt string
		err := m.db.QueryRow(`SELECT response_json, caller_identity, work_item_id, created_at
			FROM claim_idempotency WHERE idempotency_key=?1`, req.IdempotencyKey).
			Scan(&cached, &cachedIdentity, &cachedWI, &cachedAt)
		if err == nil {
			if cachedIdentity != callerIdentity || cachedWI != wiID {
				return nil, Fail(api.ErrCoreConflict, "IDEMPOTENCY_KEY_REUSED",
					"key is bound to a different caller or work item")
			}
			// Replay window (#22): the cached response carries the attempt's
			// plaintext credential; past the TTL it must not be handed out
			// again — re-claim with a fresh key instead.
			if createdAt, perr := time.Parse(time.RFC3339Nano, cachedAt); perr == nil &&
				time.Since(createdAt) > idempotencyTTL {
				return nil, Fail(api.ErrCoreConflict, "IDEMPOTENCY_KEY_EXPIRED",
					"the cached claim response is past the replay TTL; claim again with a fresh idempotency key")
			}
			var res ClaimResult
			if err := json.Unmarshal([]byte(cached), &res); err != nil {
				return nil, err
			}
			// The bound generation must still be the live one (a cached
			// attempt of an ended generation is fenced, not replayed).
			g, ok, err := m.activeGeneration(m.db, wiID)
			if err != nil {
				return nil, err
			}
			if !ok || g.AttemptID != res.Claim.AttemptID {
				return nil, Fail(api.ErrCoreForbidden, "STALE_ATTEMPT",
					"the cached attempt's generation is no longer active")
			}
			return &res, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	var status string
	var bundleName sql.NullString
	err := m.db.QueryRow(`SELECT status, bundle_name FROM work_items WHERE id=?1`, wiID).Scan(&status, &bundleName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fail(api.ErrCoreNotFound, "", "work item %s", wiID)
	}
	if err != nil {
		return nil, err
	}
	if isTerminal(status) {
		return nil, Fail(api.ErrCoreGone, "TERMINAL_STATE", "work item is in terminal state %s", status)
	}
	// Active-attempt check first: the precise diagnosis for a double
	// claim is ALREADY_CLAIMED, whatever the current status is.
	if req.Intent != IntentTakeover {
		if _, active, err := m.activeGeneration(m.db, wiID); err != nil {
			return nil, err
		} else if active {
			return nil, Fail(api.ErrCoreConflict, "ALREADY_CLAIMED", "an active attempt exists")
		}
	}

	switch req.Intent {
	case IntentClaim:
		if status != api.StatusQueued {
			return nil, Fail(api.ErrCoreConflict, "NOT_QUEUED", "claim requires a queued work item (got %s)", status)
		}
	case IntentResume:
		if status != api.StatusPaused && status != api.StatusBlocked {
			return nil, Fail(api.ErrCoreConflict, "NOT_RESUMABLE", "resume requires a paused or blocked work item (got %s)", status)
		}
	case IntentTakeover:
		if status != api.StatusRunning {
			return nil, Fail(api.ErrCoreConflict, "NOT_RUNNING", "takeover requires a running work item (got %s)", status)
		}
	}

	if req.Intent != IntentTakeover {
		if blocked, err := unfinishedBlockers(m.db, wiID); err != nil {
			return nil, err
		} else if blocked {
			return nil, Fail(api.ErrCoreConflict, "DEPENDENCY_BLOCKED", "work item has an unfinished blocking dependency")
		}
		// Registration tickets never execute (02 §4.4/§6.1): a WI whose
		// bundle_name IS NULL is not claimable. Takeover is exempt — it
		// seizes an already-running attempt, whatever the ticket kind.
		if !bundleName.Valid {
			return nil, Fail(api.ErrCoreConflict, "NOT_CLAIMABLE",
				"registration-only work item (no workflow bundle) is not claimable")
		}
	}
	return m.claimNew(wiID, req, callerIdentity)
}

// claimNew opens the new generation inside ONE transaction, re-validating
// the state on the transaction's snapshot (single writer ⇒ the recheck is
// authoritative; a lost race dies on the guarded UPDATE or the partial
// unique index).
func (m *OwnerManager) claimNew(wiID string, req ClaimRequest, callerIdentity string) (*ClaimResult, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreNotFound, "", "work item %s", wiID)
		}
		return nil, err
	}
	if isTerminal(status) {
		return nil, Fail(api.ErrCoreGone, "TERMINAL_STATE", "work item is in terminal state %s", status)
	}

	takeover := req.Intent == IntentTakeover
	allowed := map[string][]string{
		IntentClaim:    {api.StatusQueued},
		IntentResume:   {api.StatusPaused, api.StatusBlocked},
		IntentTakeover: {api.StatusRunning},
	}[req.Intent]
	legalState := false
	for _, s := range allowed {
		if s == status {
			legalState = true
		}
	}
	if !legalState {
		return nil, Fail(api.ErrCoreConflict, "", "claim %s not legal from status %s", req.Intent, status)
	}

	// The bundle reference rides the transaction snapshot too (#5/#17):
	// the claim response must carry the WI's pinned bundle, and a
	// registration ticket must not open an execution generation (takeover
	// excepted — it seizes an existing one).
	var bundleName, bundleDigest sql.NullString
	if err := tx.QueryRow(`SELECT bundle_name, bundle_digest FROM work_items WHERE id=?1`, wiID).
		Scan(&bundleName, &bundleDigest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Fail(api.ErrCoreNotFound, "", "work item %s", wiID)
		}
		return nil, err
	}
	if req.Intent != IntentTakeover && !bundleName.Valid {
		return nil, Fail(api.ErrCoreConflict, "NOT_CLAIMABLE",
			"registration-only work item (no workflow bundle) is not claimable")
	}

	now := Now()
	// Takeover: end the old active generation first (revoking its
	// credential — the fencing main layer).
	if takeover {
		if g, active, err := m.activeGeneration(tx, wiID); err != nil {
			return nil, err
		} else if active {
			if _, err := tx.Exec(`UPDATE owner_generations SET ended_at=?, end_reason='takeover'
				WHERE work_item_id=? AND generation=? AND ended_at IS NULL`, now, wiID, g.Generation); err != nil {
				return nil, err
			}
			if err := appendEventTx(tx, wiID, now, callerIdentity, "attempt_superseded",
				map[string]any{"generation": g.Generation, "attempt_id": g.AttemptID}); err != nil {
				return nil, err
			}
		}
	}

	var maxGen sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(generation) FROM owner_generations WHERE work_item_id=?1`, wiID).Scan(&maxGen); err != nil {
		return nil, err
	}
	gen := maxGen.Int64 + 1

	attemptID, err := NewID("ra_")
	if err != nil {
		return nil, err
	}
	secret, err := NewSecret(32)
	if err != nil {
		return nil, err
	}

	var machineAny any
	if req.MachineID != "" {
		machineAny = req.MachineID
	}
	owner := req.Owner
	if owner == "" {
		owner = callerIdentity
	}
	if _, err := tx.Exec(`INSERT INTO owner_generations(work_item_id, generation, attempt_id, owner, machine_id, secret_hash, started_at)
		VALUES (?1,?2,?3,?4,?5,?6,?7)`, wiID, gen, attemptID, owner, machineAny, sha256Hex(secret), now); err != nil {
		if isUniqueViolation(err) {
			return nil, Fail(api.ErrCoreConflict, "ALREADY_CLAIMED", "an active attempt exists")
		}
		return nil, err
	}
	// Guarded transition: 0 rows = concurrent terminalization won.
	// assignee is the caller's user id (FK-bound); the display owner lives
	// on the generation row and the wire claim.
	res, err := tx.Exec(`UPDATE work_items SET assignee=?, status='running', waiting_on=NULL, updated_at=?
		WHERE id=? AND status=?`, callerIdentity, now, wiID, status)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, Fail(api.ErrCoreConflict, "RACE_LOST", "state changed concurrently")
	}

	expires := time.Now().UTC().Add(credentialTTL).Format(time.RFC3339)
	result := &ClaimResult{
		Claim: api.Claim{
			WorkItemID:   wiID,
			AttemptID:    attemptID,
			MachineID:    req.MachineID,
			Owner:        owner,
			Generation:   gen,
			BundleName:   bundleName.String,
			BundleDigest: bundleDigest.String,
			ClaimedAt:    now,
			Token:        secret,
			TokenExpires: expires,
		},
		Credential: api.PushCredential{
			AttemptID: attemptID,
			Username:  wiID,
			Secret:    secret,
			ExpiresAt: expires,
		},
	}
	blob, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if req.IdempotencyKey != "" {
		if _, err := tx.Exec(`INSERT INTO claim_idempotency(idempotency_key, work_item_id, caller_identity, response_json, created_at)
			VALUES (?1,?2,?3,?4,?5)`, req.IdempotencyKey, wiID, callerIdentity, string(blob), now); err != nil {
			return nil, err
		}
	}
	if err := appendEventTx(tx, wiID, now, callerIdentity, "attempt_started",
		map[string]any{"generation": gen, "attempt_id": attemptID, "intent": req.Intent}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// EndGeneration closes the active attempt and applies the terminal or
// pause transition. status ∈ wrapped|failed|blocked|paused. attempt fencing:
// the caller must name the active attempt (old attempts cannot close what
// they no longer hold).
func (m *OwnerManager) EndGeneration(wiID, attemptID string, generation int64, status, note string) error {
	switch status {
	case "wrapped", api.StatusFailed, api.StatusBlocked, api.StatusPaused:
	default:
		return Fail(api.ErrCoreBadInput, "", "status must be one of wrapped|failed|blocked|paused")
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var wiStatus string
	if err := tx.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&wiStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Fail(api.ErrCoreNotFound, "", "work item %s", wiID)
		}
		return err
	}
	if wiStatus != api.StatusRunning {
		return Fail(api.ErrCoreConflict, "NOT_RUNNING", "end-generation requires a running work item (got %s)", wiStatus)
	}
	g, active, err := m.activeGeneration(tx, wiID)
	if err != nil {
		return err
	}
	if !active {
		return Fail(api.ErrCoreConflict, "NO_ACTIVE_ATTEMPT", "no active attempt to end")
	}
	if g.AttemptID != attemptID || g.Generation != generation {
		return Fail(api.ErrCoreForbidden, "STALE_ATTEMPT",
			"attempt %s (generation %d) is not the active attempt %s (generation %d)",
			attemptID, generation, g.AttemptID, g.Generation)
	}
	now := Now()
	res, err := tx.Exec(`UPDATE owner_generations SET ended_at=?, end_reason=? WHERE work_item_id=? AND generation=? AND ended_at IS NULL`,
		now, status, wiID, generation)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Fail(api.ErrCoreForbidden, "STALE_ATTEMPT", "generation ended concurrently")
	}
	closedExpr := "closed_at=closed_at"
	waiting := "waiting_on=waiting_on"
	var setArgs []any
	setArgs = append(setArgs, status, now)
	if status == "wrapped" || status == api.StatusFailed {
		closedExpr = "closed_at=?"
		setArgs = append(setArgs, now)
	}
	if status == api.StatusBlocked {
		waiting = "waiting_on='owner'"
	}
	setArgs = append(setArgs, wiID)
	if _, err := tx.Exec(`UPDATE work_items SET status=?, updated_at=?, `+closedExpr+`, `+waiting+`
		WHERE id=?`, setArgs...); err != nil {
		return err
	}
	eventType := map[string]string{
		"wrapped": "attempt_completed", api.StatusFailed: "attempt_completed",
		api.StatusBlocked: "note", api.StatusPaused: "note",
	}[status]
	payload := map[string]any{"generation": generation, "attempt_id": attemptID, "status": status}
	if note != "" {
		payload["note"] = note
	}
	if err := appendEventTx(tx, wiID, now, "", eventType, payload); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *WIManager) hasUnfinishedBlockers(wiID string) (bool, error) {
	return unfinishedBlockers(m.db, wiID)
}

// unfinishedBlockers is the shared query (owner must not depend on the
// WIManager instance; both reach the same single-writer connection).
func unfinishedBlockers(db *DB, wiID string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM wi_dependencies d
		JOIN work_items b ON b.id = d.blocking_wi_id
		WHERE d.blocked_wi_id=?1 AND d.kind='blocks'
		AND b.status NOT IN ('wrapped','failed','canceled')`, wiID).Scan(&n)
	return n > 0, err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT")
}
