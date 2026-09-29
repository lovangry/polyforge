package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/silgrid/polyforge/internal/core/owner"
	"github.com/silgrid/polyforge/internal/core/workitem"
)

// B group (rows 7–13) + D group (row 21). Execution-plane endpoints do no
// membership filtering (02 §8 两面分层): key scope still applies.

func (s *Server) hClaim(w http.ResponseWriter, r *http.Request) {
	idn, wiID, _, ok := s.authenticatedWI(w, r)
	if !ok {
		return
	}
	var req owner.ClaimRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	boot := s.capsuleBootstrap(wiID)
	resp, err := s.Owners.Claim(wiID, req, idn.UserID, boot)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, resp) })
}

// hUnblock is row 10: blocked → running as a fresh claim (§2.5/§6.2).
func (s *Server) hUnblock(w http.ResponseWriter, r *http.Request) {
	idn, wiID, _, ok := s.authenticatedWI(w, r)
	if !ok {
		return
	}
	var status string
	if err := s.DB.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&status); err != nil {
		writeErr(w, ownerAPI("CORE-404", "work item "+wiID))
		return
	}
	if status != "blocked" {
		writeErr(w, ownerAPI("CORE-409", "not blocked"))
		return
	}
	var req owner.ClaimRequest
	_ = decodeBody(r, &req)
	if req.IdempotencyKey == "" {
		writeErr(w, ownerAPI("CORE-400", "idempotency_key required"))
		return
	}
	resp, err := s.Owners.Claim(wiID, req, idn.UserID, s.capsuleBootstrap(wiID))
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, resp) })
}

// capsuleBootstrap assembles the claim response's capsule_bootstrap payload:
// bundle reference + budget baseline + repo inventory (02 §8: the machine
// needs no data-plane reads to execute).
func (s *Server) capsuleBootstrap(wiID string) json.RawMessage {
	d, err := s.WIs.Get(wiID, workitem.GetOptions{})
	if err != nil {
		return nil
	}
	inv, err := s.WIs.Inventory(wiID)
	if err != nil {
		inv = nil
	}
	b, _ := json.Marshal(map[string]any{
		"work_item_id":             wiID,
		"bundle_name":              d.BundleName,
		"bundle_digest":            d.BundleDigest,
		"workflow_budget_baseline": json.RawMessage(orNull(d.BudgetBaseline)),
		"repo_inventory":           inv,
	})
	return b
}

func orNull(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

func (s *Server) hForceTakeover(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if idn.Role != "admin" {
		writeErr(w, ownerAPI("CORE-403", "forbidden: force-takeover requires admin"))
		return
	}
	wiID := r.PathValue("id")
	projectID, aerr := s.wiProject(idn, wiID)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := s.requireScope(idn, projectID); err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		MachineID string `json:"machine_id"`
	}
	_ = decodeBody(r, &body)
	resp, err := s.Owners.ForceTakeover(wiID, idn.UserID, body.MachineID)
	writeResult(w, err, func() {
		// 02 §8 + 04 §6.1d/§7.3 step 4: the takeover response carries the
		// SAME capsule bootstrap the claim response does, so the taking-over
		// machine can create its scene with no data-plane reads. The owner
		// manager owns only the generation bookkeeping; the payload is
		// attached here exactly the way the claim route does.
		resp.CapsuleBootstrap = s.capsuleBootstrap(wiID)
		writeJSON(w, http.StatusOK, resp)
	})
}

func (s *Server) hEndGeneration(w http.ResponseWriter, r *http.Request) {
	_, wiID, _, ok := s.authenticatedWI(w, r)
	if !ok {
		return
	}
	var req owner.EndGenerationRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	err := s.Owners.EndGeneration(wiID, req, r.Header.Get("X-PF-Generation-Token"))
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"ok": true}) })
}

func (s *Server) hBudgetIncrement(w http.ResponseWriter, r *http.Request) {
	idn, wiID, projectID, ok := s.authenticatedWI(w, r)
	if !ok {
		return
	}
	// §6.7 authorization: reporter OR project maintainer OR admin; no token.
	var reporter string
	if err := s.DB.QueryRow(`SELECT reporter FROM work_items WHERE id=?1`, wiID).Scan(&reporter); err != nil {
		writeErr(w, ownerAPI("CORE-404", "work item "+wiID))
		return
	}
	role, err := s.memberRole(projectID, idn.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if idn.Role != "admin" && idn.UserID != reporter && role != "maintainer" {
		writeErr(w, ownerAPI("CORE-403", "forbidden"))
		return
	}
	var req struct {
		Dimension  string `json:"dimension"`
		Amount     int64  `json:"amount"`
		Note       string `json:"note"`
		BasedOnSeq int64  `json:"based_on_seq"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	b, err := s.WIs.AddBudgetIncrement(wiID, req.Dimension, req.Amount, req.Note, req.BasedOnSeq, idn.UserID)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, b) })
}

func (s *Server) hGetBudget(w http.ResponseWriter, r *http.Request) {
	_, wiID, _, ok := s.authenticatedWI(w, r)
	if !ok {
		return
	}
	// FA-4 (02 §5.1 tier 3): row 12 (budget read) belongs to the active
	// generation's execution plane — with an active generation the caller
	// must present its X-PF-Generation-Token (checked against the active
	// generation, CORE-403 with the §6.6 reason taxonomy on failure); with
	// no active generation (tier 2) the api_key alone suffices.
	if _, _, _, active, err := s.Owners.ActiveGeneration(wiID); err != nil {
		writeErr(w, err)
		return
	} else if active {
		_, ok, reason, terr := s.Owners.CheckTokenForWI(wiID, r.Header.Get("X-PF-Generation-Token"))
		if terr != nil {
			writeErr(w, terr)
			return
		}
		if !ok {
			writeErr(w, &owner.APIError{Code: "CORE-403", Reason: reason,
				Detail: "budget read of a work item with an active generation requires its X-PF-Generation-Token (02 §5.1 tier 3)"})
			return
		}
	}
	b, err := s.WIs.GetBudget(wiID)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, b) })
}

func (s *Server) hRepoInventory(w http.ResponseWriter, r *http.Request) {
	_, wiID, _, ok := s.authenticatedWI(w, r)
	if !ok {
		return
	}
	inv, err := s.WIs.Inventory(wiID)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"repo_inventory": inv}) })
}

// hHeartbeat is row 21: upsert keyed on machine_id; first call registers the
// machine (creates the machine user + issues an api key whose plaintext is
// returned exactly once), later calls only refresh the heartbeat.
//
// Authorization (fix #2, 02 §5.1 row 21 + §4.10): first-time registration
// requires a HUMAN or ADMIN caller — an unattended machine/viewer key must
// not be able to mint itself a brand-new machine identity with a global
// (unscoped) api_key. A heartbeat refresh on an EXISTING machine requires the
// caller to BE that machine (caller user id == machines.user_id) or a
// human/admin; one machine must not be able to refresh (or, before this fix,
// silently re-register) another's row.
func (s *Server) hHeartbeat(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		MachineID      string `json:"machine_id"`
		RuntimeVersion string `json:"runtime_version"`
		DisplayName    string `json:"display_name"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(body.MachineID) == "" {
		writeErr(w, ownerAPI("CORE-400", "machine_id required"))
		return
	}
	isHumanOrAdmin := idn.Kind == "human" || idn.Role == "admin"

	var existingUser string
	err = s.DB.QueryRow(`SELECT user_id FROM machines WHERE id=?1`, body.MachineID).Scan(&existingUser)
	if errors.Is(err, errNoRowsSentinel()) {
		// Registration path.
		if !isHumanOrAdmin {
			writeErr(w, ownerAPI("CORE-403", "forbidden: machine registration requires a human or admin caller"))
			return
		}
		// 02 §8 (scope only narrows, never widens) composed with 02 §4.10
		// (registration mints a GLOBAL machine key, project_id=NULL — the
		// design pins that key global, so scope-inheriting it at registration
		// is not an option; narrowing happens afterwards via M2). Registration
		// hands the caller the plaintext of a fresh NULL-scope credential, and
		// a project-scoped key must never mint anything outside its scope, so
		// it may not register machines at all. This closes the escalation
		// chain scoped-admin-key -> register machine (unscoped key) -> PATCH
		// role=admin (also refused for scoped keys, handlers_admin.go) ->
		// unscoped admin credential.
		if idn.KeyScope != nil {
			writeErr(w, ownerAPI("CORE-403", "forbidden: machine registration requires an unscoped human/admin api key"))
			return
		}
		resp, rerr := s.registerMachine(body.MachineID, body.RuntimeVersion, body.DisplayName)
		if rerr != nil {
			writeErr(w, rerr)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	// Heartbeat-refresh path: caller must BE the machine, or human/admin.
	if !isHumanOrAdmin && idn.UserID != existingUser {
		writeErr(w, ownerAPI("CORE-403", "forbidden: heartbeat requires the machine itself or a human/admin"))
		return
	}
	if _, err := s.DB.Exec(`UPDATE machines SET last_heartbeat_at=?1, runtime_version=?2 WHERE id=?3`,
		nowStamp(), body.RuntimeVersion, body.MachineID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"machine_id": body.MachineID, "registered": false})
}

// registerMachine creates the machine user + machine row + api_key inside
// one transaction. If a CONCURRENT registration of the same machine_id won
// the race (unique constraint on machines.id, fix #2), this call rolls back
// its own half-done insert, re-reads the winner's row and returns success
// WITHOUT minting a second key — idempotent under a real race, not merely
// under sequential replay.
func (s *Server) registerMachine(machineID, runtimeVersion, displayName string) (map[string]any, error) {
	uid, err := newID("mac_")
	if err != nil {
		return nil, err
	}
	keyID, err := newID("key_")
	if err != nil {
		return nil, err
	}
	plain, err := newSecret(24)
	if err != nil {
		return nil, err
	}
	display := displayName
	if display == "" {
		display = "machine " + machineID
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO users(id, display_name, kind, role) VALUES (?1,?2,'machine','writer')`, uid, display); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO machines(id, user_id, runtime_version, last_heartbeat_at)
		VALUES (?1,?2,?3,?4)`, machineID, uid, runtimeVersion, nowStamp()); err != nil {
		tx.Rollback()
		if isUniqueViolation(err) {
			// Lost the registration race: someone else's row won. Re-read it
			// and just refresh the heartbeat; do NOT mint a second key.
			var winnerUser string
			if qerr := s.DB.QueryRow(`SELECT user_id FROM machines WHERE id=?1`, machineID).Scan(&winnerUser); qerr != nil {
				return nil, qerr
			}
			if _, uerr := s.DB.Exec(`UPDATE machines SET last_heartbeat_at=?1, runtime_version=?2 WHERE id=?3`,
				nowStamp(), runtimeVersion, machineID); uerr != nil {
				return nil, uerr
			}
			return map[string]any{"machine_id": machineID, "user_id": winnerUser, "registered": false}, nil
		}
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO api_keys(id, user_id, project_id, key_hash) VALUES (?1,?2,NULL,?3)`,
		keyID, uid, sha256Hex(plain)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{
		"machine_id": machineID, "user_id": uid, "registered": true,
		"api_key": plain,
	}, nil
}

// isUniqueViolation is a best-effort, driver-agnostic detection of a SQLite
// UNIQUE/PRIMARY KEY constraint failure (modernc.org/sqlite's error strings
// contain this phrase; there is no typed sentinel exported for it here).
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT")
}
