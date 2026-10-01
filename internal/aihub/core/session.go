package core

// session.go — the v1.5 session-id ownership model (Lane A):
// docs/design/polyforge-vnext/v1.5-api-contract.md §1 (frozen wire
// contract) over the design rationale in v1.5-session-model.md
// (B1 pre-claim journal is client-side; B3 identity/credential split and
// the M1–M5 rulings are enforced here).
//
//   - Claim  (POST .../claim, replaced handler): CAS in ONE transaction —
//     status==queued AND session_hash IS NULL AND bundle present AND no
//     unfinished blockers (M1 joint gate). Idempotent on idempotency_key
//     (D4); a cached session that is no longer the live one answers
//     CORE-403 STALE_SESSION.
//   - Verify (POST .../session-verify): compare-only (B3): the hub stores
//     only hex(sha256(session_id)); no state change, no secret echo.
//   - Attach (POST .../session-attach): same-session resume of a paused
//     WI (M3); clean_pause WIs refuse (operator pause is not
//     auto-resumable).
//   - Takeover (POST .../force-takeover): atomically swaps the session to
//     the requester's new secret; the supersession event carries the OLD
//     fingerprint (m4 audit).
//   - End (POST .../session-end): session-fenced terminal verdict; the
//     session row is cleared entirely (M2: blocked = status blocked +
//     cleared session, excluded from the claim gate).
//   - Pause: the M3 pause transition for session-held WIs (session KEPT,
//     status=paused, clean_pause set). Reached through the legacy
//     /end-generation handler with a session_id in the body — the frozen
//     contract defines no pause endpoint of its own.
//
// The git push credential's session path (fencing-on-use, §1.6) lives in
// owner.go's CheckPushCredential and validates against the SAME stored
// hash, so a claim here is the only thing that can make a push valid.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
)

// SessionManager is the v1.5 ownership-arbitration face.
type SessionManager struct{ db *DB }

func NewSessionManager(db *DB) *SessionManager { return &SessionManager{db: db} }

// queryRower is the shared read surface of *DB and *sql.Tx (the single
// writer's snapshot is authoritative for every in-transaction recheck).
type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// ---------------------------------------------------------------------------
// Session row + public view (m4)

// sessionRow is the session block of a work_items row (plus the fields
// the claim/diagnosis paths need on the same read).
type sessionRow struct {
	Status       string
	Hash         sql.NullString
	Fingerprint  sql.NullString
	Machine      sql.NullString
	Owner        sql.NullString
	StartedAt    sql.NullString
	CleanPause   bool
	BundleName   sql.NullString
	BundleDigest sql.NullString
	ProjectID    string
}

// sessionHeld reports whether the row carries a live session.
func sessionHeld(r *sessionRow) bool { return r.Hash.Valid && r.Hash.String != "" }

// loadSessionRow reads the session block (one query, works on *DB and
// *sql.Tx alike). A missing WI is the typed CORE-404.
func loadSessionRow(q queryRower, wiID string) (*sessionRow, error) {
	var r sessionRow
	err := q.QueryRow(`SELECT status, session_hash, session_fingerprint, session_machine,
		session_owner, session_started_at, clean_pause, bundle_name, bundle_digest, project_id
		FROM work_items WHERE id=?1`, wiID).
		Scan(&r.Status, &r.Hash, &r.Fingerprint, &r.Machine, &r.Owner, &r.StartedAt,
			&r.CleanPause, &r.BundleName, &r.BundleDigest, &r.ProjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fail(api.ErrCoreNotFound, "", "work item %s", wiID)
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SessionPublic is the m4 public audit block every WorkItem JSON response
// carries. NEVER a hash, never the secret — the fingerprint is the public
// handle (B3).
type SessionPublic struct {
	SessionFingerprint string `json:"session_fingerprint,omitempty"`
	SessionMachine     string `json:"session_machine,omitempty"`
	SessionOwner       string `json:"session_owner,omitempty"`
	SessionStartedAt   string `json:"session_started_at,omitempty"`
	CleanPause         bool   `json:"clean_pause"`
}

// WIView is a WorkItem with its public session audit block attached. It
// marshals as the api.WorkItem JSON plus the SessionPublic fields
// (embedded structs flatten); Phase 0's api.WorkItem itself is frozen and
// gains nothing, so the block rides alongside it at the server boundary.
type WIView struct {
	api.WorkItem
	SessionPublic
}

// sessionPublicOf reads one WI's public session block.
func (db *DB) sessionPublicOf(wiID string) (SessionPublic, error) {
	var sp SessionPublic
	var fp, machine, owner, started sql.NullString
	err := db.QueryRow(`SELECT session_fingerprint, session_machine, session_owner,
		session_started_at, clean_pause FROM work_items WHERE id=?1`, wiID).
		Scan(&fp, &machine, &owner, &started, &sp.CleanPause)
	if errors.Is(err, sql.ErrNoRows) {
		return sp, Fail(api.ErrCoreNotFound, "", "work item %s", wiID)
	}
	if err != nil {
		return sp, err
	}
	sp.SessionFingerprint, sp.SessionMachine, sp.SessionOwner, sp.SessionStartedAt =
		fp.String, machine.String, owner.String, started.String
	return sp, nil
}

// View wraps one WorkItem with its public session audit block.
func (m *WIManager) View(wi *api.WorkItem) (*WIView, error) {
	sp, err := m.db.sessionPublicOf(wi.ID)
	if err != nil {
		return nil, err
	}
	return &WIView{WorkItem: *wi, SessionPublic: sp}, nil
}

// sessionPublicBatch reads many WIs' public session blocks in ONE query.
// Missing ids simply keep a zero block.
func (db *DB) sessionPublicBatch(ids []string) (map[string]SessionPublic, error) {
	out := map[string]SessionPublic{}
	if len(ids) == 0 {
		return out, nil
	}
	var placeholders []string
	var args []any
	for i, id := range ids {
		placeholders = append(placeholders, "?"+strconv.Itoa(i+1))
		args = append(args, id)
	}
	rows, err := db.Query(`SELECT id, session_fingerprint, session_machine, session_owner,
		session_started_at, clean_pause FROM work_items WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var fp, machine, owner, started sql.NullString
		var sp SessionPublic
		if err := rows.Scan(&id, &fp, &machine, &owner, &started, &sp.CleanPause); err != nil {
			return nil, err
		}
		sp.SessionFingerprint, sp.SessionMachine, sp.SessionOwner, sp.SessionStartedAt =
			fp.String, machine.String, owner.String, started.String
		out[id] = sp
	}
	return out, rows.Err()
}

// Views wraps a batch of WorkItems with their session blocks (one query).
// Order is preserved; a WI that vanished mid-read keeps a zero block.
func (m *WIManager) Views(wis []api.WorkItem) ([]WIView, error) {
	out := make([]WIView, 0, len(wis))
	if len(wis) == 0 {
		return out, nil
	}
	ids := make([]string, len(wis))
	for i := range wis {
		ids[i] = wis[i].ID
	}
	blocks, err := m.db.sessionPublicBatch(ids)
	if err != nil {
		return nil, err
	}
	for i := range wis {
		out = append(out, WIView{WorkItem: wis[i], SessionPublic: blocks[wis[i].ID]})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Claim (contract §1.1)

// Claim executes the v1.5 CAS claim. callerIdentity is the api-key's
// user id (idempotency triple binding). The response carries the public
// fingerprint only — the secret never echoes.
func (m *SessionManager) Claim(wiID string, req api.SessionClaimRequest, callerIdentity string) (*api.SessionClaim, error) {
	if strings.TrimSpace(req.SessionID) == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "session_id required (the client-minted secret)")
	}
	if req.IdempotencyKey != "" {
		if cached, err := m.replay(wiID, req.IdempotencyKey, callerIdentity); err != nil || cached != nil {
			return cached, err
		}
	}
	// Fast-fail diagnosis on the live row; the transaction below rechecks
	// every gate on the single writer's snapshot (a lost race dies on the
	// guarded UPDATE).
	row, err := loadSessionRow(m.db, wiID)
	if err != nil {
		return nil, err
	}
	if err := diagnoseClaimGate(row); err != nil {
		return nil, err
	}
	if blocked, err := unfinishedBlockers(m.db, wiID); err != nil {
		return nil, err
	} else if blocked {
		return nil, Fail(api.ErrCoreConflict, "DEPENDENCY_BLOCKED", "work item has an unfinished blocking dependency")
	}
	if err := diagnoseClaimBundle(row); err != nil {
		return nil, err
	}
	return m.claimTx(wiID, req, callerIdentity)
}

// diagnoseClaimGate is the M1 joint CAS gate's error face. Order matters:
// terminal first (CORE-410), then session-held (the precise double-claim
// diagnosis), then status; blockers and the bundle are checked after
// these by the caller (the v1 precedence — DEPENDENCY_BLOCKED wins over
// NOT_CLAIMABLE for a WI with both problems — is preserved).
func diagnoseClaimGate(row *sessionRow) error {
	if isTerminal(row.Status) {
		return Fail(api.ErrCoreGone, "TERMINAL_STATE", "work item is in terminal state %s", row.Status)
	}
	if sessionHeld(row) {
		return Fail(api.ErrCoreConflict, "ALREADY_HELD",
			"the work item is already held by session %s", row.Fingerprint.String)
	}
	if row.Status != api.StatusQueued {
		return Fail(api.ErrCoreConflict, "NOT_CLAIMABLE",
			"claim requires a queued work item (got %s)", row.Status)
	}
	return nil
}

// diagnoseClaimBundle is the gate's bundle leg (registration tickets never
// execute, #17).
func diagnoseClaimBundle(row *sessionRow) error {
	if !row.BundleName.Valid || row.BundleName.String == "" {
		return Fail(api.ErrCoreConflict, "NOT_CLAIMABLE",
			"registration-only work item (no workflow bundle) is not claimable")
	}
	return nil
}

// claimTx opens the session inside ONE transaction. The guarded UPDATE
// re-validates the joint gate (status + empty session) on the writer's
// own snapshot: 0 rows = the race was lost, and the fresh row is
// re-diagnosed for the precise wire error.
func (m *SessionManager) claimTx(wiID string, req api.SessionClaimRequest, callerIdentity string) (*api.SessionClaim, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row, err := loadSessionRow(tx, wiID)
	if err != nil {
		return nil, err
	}
	if err := diagnoseClaimGate(row); err != nil {
		return nil, err
	}
	if blocked, err := unfinishedBlockers(tx, wiID); err != nil {
		return nil, err
	} else if blocked {
		return nil, Fail(api.ErrCoreConflict, "DEPENDENCY_BLOCKED", "work item has an unfinished blocking dependency")
	}
	if err := diagnoseClaimBundle(row); err != nil {
		return nil, err
	}

	hash := api.SessionHashHex(req.SessionID)
	fp := api.SessionFingerprint(req.SessionID)
	now := Now()
	owner := req.Owner
	if owner == "" {
		owner = callerIdentity
	}
	var machineAny any
	if req.MachineID != "" {
		machineAny = req.MachineID
	}
	res, err := tx.Exec(`UPDATE work_items SET status='running', session_hash=?1, session_fingerprint=?2,
		session_machine=?3, session_owner=?4, session_started_at=?5, clean_pause=0, assignee=?6,
		waiting_on=NULL, updated_at=?5
		WHERE id=?7 AND status='queued' AND session_hash IS NULL`,
		hash, fp, machineAny, owner, now, callerIdentity, wiID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Re-diagnose inside the same snapshot for the typed refusal.
		fresh, err := loadSessionRow(tx, wiID)
		if err != nil {
			return nil, err
		}
		if derr := diagnoseClaimGate(fresh); derr != nil {
			return nil, derr
		}
		return nil, Fail(api.ErrCoreConflict, "RACE_LOST", "state changed concurrently")
	}

	claim := &api.SessionClaim{
		WorkItemID:         wiID,
		SessionFingerprint: fp,
		MachineID:          req.MachineID,
		Owner:              owner,
		BundleName:         row.BundleName.String,
		BundleDigest:       row.BundleDigest.String,
		ClaimedAt:          now,
	}
	claim.Repos, err = projectReposTx(tx, row.ProjectID)
	if err != nil {
		return nil, err
	}

	if req.IdempotencyKey != "" {
		if err := cacheIdempotentClaimTx(tx, req.IdempotencyKey, wiID, callerIdentity, claim); err != nil {
			return nil, err
		}
	}
	if err := appendEventTx(tx, wiID, now, callerIdentity, "session_claimed",
		map[string]any{"session_fingerprint": fp, "machine_id": req.MachineID, "owner": owner}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claim, nil
}

// replay answers a cached claim response when the SAME idempotency key,
// caller and WI arrive again (D4). The cached response carries no secret,
// so no replay TTL is needed (unlike the generation-era cache, which held
// a plaintext credential at rest). The cached session must still be the
// LIVE one — a cache whose session was superseded or ended answers
// CORE-403 STALE_SESSION, never a stale claim.
func (m *SessionManager) replay(wiID, key, callerIdentity string) (*api.SessionClaim, error) {
	var cached, cachedIdentity, cachedWI string
	err := m.db.QueryRow(`SELECT response_json, caller_identity, work_item_id
		FROM claim_idempotency WHERE idempotency_key=?1`, key).
		Scan(&cached, &cachedIdentity, &cachedWI)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // no cache: proceed to the real claim
	}
	if err != nil {
		return nil, err
	}
	if cachedIdentity != callerIdentity || cachedWI != wiID {
		return nil, Fail(api.ErrCoreConflict, "IDEMPOTENCY_KEY_REUSED",
			"key is bound to a different caller or work item")
	}
	var claim api.SessionClaim
	if err := json.Unmarshal([]byte(cached), &claim); err != nil {
		return nil, err
	}
	row, err := loadSessionRow(m.db, wiID)
	if err != nil {
		return nil, err
	}
	if !sessionHeld(row) || row.Fingerprint.String != claim.SessionFingerprint {
		return nil, Fail(api.ErrCoreForbidden, "STALE_SESSION",
			"the cached session %s is no longer the live one (current holder %s)",
			claim.SessionFingerprint, row.Fingerprint.String)
	}
	return &claim, nil
}

// cacheIdempotentClaimTx stores the claim half of the response (the
// work-item view is re-assembled fresh on every replay).
func cacheIdempotentClaimTx(tx *sql.Tx, key, wiID, callerIdentity string, claim *api.SessionClaim) error {
	blob, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO claim_idempotency(idempotency_key, work_item_id, caller_identity, response_json, created_at)
		VALUES (?1,?2,?3,?4,?5)`, key, wiID, callerIdentity, string(blob), Now())
	return err
}

// projectReposTx reads the project's task-repo inventory on the claim's
// own snapshot (v0 claim semantics: the runtime clones each entry into
// the capsule before the agent starts).
func projectReposTx(tx *sql.Tx, projectID string) ([]api.ProjectRepo, error) {
	var reposJSON string
	err := tx.QueryRow(`SELECT repos FROM projects WHERE id=?1`, projectID).Scan(&reposJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fail(api.ErrCoreNotFound, "", "project %s of the work item not found", projectID)
	}
	if err != nil {
		return nil, err
	}
	repos := []api.ProjectRepo{}
	if reposJSON != "" {
		if err := json.Unmarshal([]byte(reposJSON), &repos); err != nil {
			return nil, err
		}
		if repos == nil {
			repos = []api.ProjectRepo{}
		}
	}
	return repos, nil
}

// ---------------------------------------------------------------------------
// Verify (contract §1.2, compare-only B3)

// Verify compares the presented secret's hash against the stored one.
// No state change, no event, no secret echo: the answer says whether the
// caller's session is still the holder and — for a superseded daemon to
// self-diagnose — who holds it now (public fingerprint only).
func (m *SessionManager) Verify(wiID, sessionID string) (*api.SessionVerifyResponse, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "session_id required")
	}
	row, err := loadSessionRow(m.db, wiID)
	if err != nil {
		return nil, err
	}
	resp := &api.SessionVerifyResponse{
		WorkItemID: wiID,
		Status:     row.Status,
		CleanPause: row.CleanPause,
	}
	if sessionHeld(row) {
		resp.CurrentFingerprint = row.Fingerprint.String
		resp.Matches = api.SessionHashHex(sessionID) == row.Hash.String
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Attach (contract §1.3, M3 same-session resume)

// Attach resumes a paused WI under the SAME session: hash matches AND
// status==paused AND clean_pause==0 → running. Machine/owner refresh to
// the requester's values; session_started_at keeps the original claim
// time (the session never changed).
func (m *SessionManager) Attach(wiID string, req api.SessionAttachRequest, callerIdentity string) (*api.SessionClaim, error) {
	if strings.TrimSpace(req.SessionID) == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "session_id required")
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row, err := loadSessionRow(tx, wiID)
	if err != nil {
		return nil, err
	}
	if !sessionHeld(row) || api.SessionHashHex(req.SessionID) != row.Hash.String {
		return nil, Fail(api.ErrCoreForbidden, "STALE_SESSION",
			"session does not match the current holder %s", row.Fingerprint.String)
	}
	if row.Status != api.StatusPaused || row.CleanPause {
		return nil, Fail(api.ErrCoreConflict, "NOT_ATTACHABLE",
			"attach requires a paused, non-clean-paused work item (got status %s, clean_pause %v)",
			row.Status, row.CleanPause)
	}
	owner := req.Owner
	if owner == "" {
		owner = row.Owner.String
	}
	if owner == "" {
		owner = callerIdentity
	}
	var machineAny any
	if req.MachineID != "" {
		machineAny = req.MachineID
	} else {
		machineAny = row.Machine // keep the recorded machine
	}
	now := Now()
	res, err := tx.Exec(`UPDATE work_items SET status='running', session_machine=?1, session_owner=?2,
		assignee=?3, updated_at=?4
		WHERE id=?5 AND status='paused' AND session_hash=?6 AND clean_pause=0`,
		machineAny, owner, callerIdentity, now, wiID, row.Hash.String)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, Fail(api.ErrCoreConflict, "NOT_ATTACHABLE", "state changed concurrently")
	}
	if err := appendEventTx(tx, wiID, now, callerIdentity, "session_attached",
		map[string]any{"session_fingerprint": row.Fingerprint.String, "machine_id": req.MachineID, "owner": owner}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &api.SessionClaim{
		WorkItemID:         wiID,
		SessionFingerprint: row.Fingerprint.String,
		MachineID:          req.MachineID,
		Owner:              owner,
		BundleName:         row.BundleName.String,
		BundleDigest:       row.BundleDigest.String,
		ClaimedAt:          row.StartedAt.String, // the session's own start
	}, nil
}

// ---------------------------------------------------------------------------
// Force takeover (contract §1.4)

// Takeover atomically swaps the session to the requester's new secret.
// Gate: status non-terminal (running or paused) and a non-empty reason.
// The session_superseded event carries the OLD fingerprint (m4 audit) so
// the superseded daemon can be identified from the timeline alone.
func (m *SessionManager) Takeover(wiID string, req api.SessionClaimRequest, callerIdentity string) (*api.SessionClaim, error) {
	if strings.TrimSpace(req.SessionID) == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "session_id required (the client-minted secret)")
	}
	if strings.TrimSpace(req.Reason) == "" {
		return nil, Fail(api.ErrCoreBadInput, "", "reason required")
	}
	if req.IdempotencyKey != "" {
		if cached, err := m.replay(wiID, req.IdempotencyKey, callerIdentity); err != nil || cached != nil {
			return cached, err
		}
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row, err := loadSessionRow(tx, wiID)
	if err != nil {
		return nil, err
	}
	if isTerminal(row.Status) {
		return nil, Fail(api.ErrCoreGone, "TERMINAL_STATE", "work item is in terminal state %s", row.Status)
	}
	if row.Status != api.StatusRunning && row.Status != api.StatusPaused {
		return nil, Fail(api.ErrCoreConflict, "NOT_TAKEOVERABLE",
			"force-takeover requires a running or paused work item (got %s)", row.Status)
	}

	oldFP := row.Fingerprint.String
	hash := api.SessionHashHex(req.SessionID)
	fp := api.SessionFingerprint(req.SessionID)
	now := Now()
	owner := req.Owner
	if owner == "" {
		owner = callerIdentity
	}
	var machineAny any
	if req.MachineID != "" {
		machineAny = req.MachineID
	}
	res, err := tx.Exec(`UPDATE work_items SET status='running', session_hash=?1, session_fingerprint=?2,
		session_machine=?3, session_owner=?4, session_started_at=?5, clean_pause=0, assignee=?6,
		waiting_on=NULL, updated_at=?5
		WHERE id=?7 AND status IN ('running','paused')`,
		hash, fp, machineAny, owner, now, callerIdentity, wiID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, Fail(api.ErrCoreConflict, "RACE_LOST", "state changed concurrently")
	}
	claim := &api.SessionClaim{
		WorkItemID:         wiID,
		SessionFingerprint: fp,
		MachineID:          req.MachineID,
		Owner:              owner,
		BundleName:         row.BundleName.String,
		BundleDigest:       row.BundleDigest.String,
		ClaimedAt:          now,
	}
	claim.Repos, err = projectReposTx(tx, row.ProjectID)
	if err != nil {
		return nil, err
	}
	if req.IdempotencyKey != "" {
		if err := cacheIdempotentClaimTx(tx, req.IdempotencyKey, wiID, callerIdentity, claim); err != nil {
			return nil, err
		}
	}
	if err := appendEventTx(tx, wiID, now, callerIdentity, "session_superseded",
		map[string]any{
			"old_session_fingerprint": oldFP,
			"new_session_fingerprint": fp,
			"machine_id":              req.MachineID,
			"owner":                   owner,
			"reason":                  req.Reason,
		}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claim, nil
}

// ---------------------------------------------------------------------------
// Session end (contract §1.5) and pause (M3)

// End is the session-fenced terminal verdict: hash matches → set the
// status (wrapped|failed|blocked), CLEAR the session row entirely (M2:
// blocked = status blocked + cleared session, excluded from the claim
// gate until a human unblocks), event session_ended.
func (m *SessionManager) End(wiID string, req api.SessionEndRequest, callerIdentity string) error {
	switch req.Status {
	case api.StatusWrapped, api.StatusFailed, api.StatusBlocked:
	default:
		return Fail(api.ErrCoreBadInput, "", "status must be one of wrapped|failed|blocked")
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	row, err := loadSessionRow(tx, wiID)
	if err != nil {
		return err
	}
	if !sessionHeld(row) || api.SessionHashHex(req.SessionID) != row.Hash.String {
		return Fail(api.ErrCoreForbidden, "STALE_SESSION",
			"session does not match the current holder %s", row.Fingerprint.String)
	}
	now := Now()
	sets := []string{"status=?", "updated_at=?",
		"session_hash=NULL", "session_fingerprint=NULL", "session_machine=NULL",
		"session_owner=NULL", "session_started_at=NULL", "clean_pause=0"}
	args := []any{req.Status, now}
	if req.Status == api.StatusWrapped || req.Status == api.StatusFailed {
		sets = append(sets, "closed_at=?")
		args = append(args, now)
	}
	if req.Status == api.StatusBlocked {
		sets = append(sets, "waiting_on='owner'")
	}
	// Fenced: the guarded hash in the WHERE is the fence — a session that
	// was swapped concurrently refuses instead of terminalizing someone
	// else's WI.
	args = append(args, wiID, row.Hash.String)
	res, err := tx.Exec(`UPDATE work_items SET `+strings.Join(sets, ",")+` WHERE id=? AND session_hash=?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Fail(api.ErrCoreForbidden, "STALE_SESSION", "the session changed concurrently")
	}
	payload := map[string]any{
		"status":              req.Status,
		"session_fingerprint": row.Fingerprint.String,
	}
	if req.Note != "" {
		payload["note"] = req.Note
	}
	if err := appendEventTx(tx, wiID, now, callerIdentity, "session_ended", payload); err != nil {
		return err
	}
	return tx.Commit()
}

// Pause is the M3 pause transition for session-held WIs, reached through
// the legacy /end-generation body carrying a session_id (the frozen
// contract defines no pause endpoint): the session is KEPT (a same-machine
// restart attaches back through it), status becomes paused, and
// clean_pause records WHO paused — 1 = operator pause (never auto-resumed;
// only force-takeover or a CLI unpause may resume), 0 = daemon shutdown.
func (m *SessionManager) Pause(wiID, sessionID string, cleanPause bool, note, callerIdentity string) error {
	if strings.TrimSpace(sessionID) == "" {
		return Fail(api.ErrCoreBadInput, "", "session_id required")
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	row, err := loadSessionRow(tx, wiID)
	if err != nil {
		return err
	}
	if !sessionHeld(row) || api.SessionHashHex(sessionID) != row.Hash.String {
		return Fail(api.ErrCoreForbidden, "STALE_SESSION",
			"session does not match the current holder %s", row.Fingerprint.String)
	}
	if row.Status != api.StatusRunning {
		return Fail(api.ErrCoreConflict, "NOT_RUNNING",
			"pause requires a running work item (got %s)", row.Status)
	}
	now := Now()
	var cp int
	if cleanPause {
		cp = 1
	}
	res, err := tx.Exec(`UPDATE work_items SET status='paused', clean_pause=?1, updated_at=?2
		WHERE id=?3 AND session_hash=?4 AND status='running'`, cp, now, wiID, row.Hash.String)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Fail(api.ErrCoreForbidden, "STALE_SESSION", "the session changed concurrently")
	}
	payload := map[string]any{
		"session_fingerprint": row.Fingerprint.String,
		"clean_pause":         cleanPause,
	}
	if note != "" {
		payload["note"] = note
	}
	if err := appendEventTx(tx, wiID, now, callerIdentity, "session_paused", payload); err != nil {
		return err
	}
	return tx.Commit()
}
