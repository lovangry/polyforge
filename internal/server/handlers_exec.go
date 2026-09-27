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
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
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
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
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
		"work_item_id": wiID,
		"bundle_name":  d.BundleName,
		"bundle_digest": d.BundleDigest,
		"workflow_budget_baseline": json.RawMessage(orNull(d.BudgetBaseline)),
		"repo_inventory": inv,
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
	if _, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		MachineID string `json:"machine_id"`
	}
	_ = decodeBody(r, &body)
	resp, err := s.Owners.ForceTakeover(wiID, idn.UserID, body.MachineID)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, resp) })
}

func (s *Server) hEndGeneration(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	if _, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	}
	var req owner.EndGenerationRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	err = s.Owners.EndGeneration(wiID, req, r.Header.Get("X-PF-Generation-Token"))
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"ok": true}) })
}

func (s *Server) hBudgetIncrement(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
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
		Dimension string `json:"dimension"`
		Amount    int64  `json:"amount"`
		Note      string `json:"note"`
		BasedOnSeq int64 `json:"based_on_seq"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	b, err := s.WIs.AddBudgetIncrement(wiID, req.Dimension, req.Amount, req.Note, req.BasedOnSeq, idn.UserID)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, b) })
}

func (s *Server) hGetBudget(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	if _, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	}
	b, err := s.WIs.GetBudget(wiID)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, b) })
}

func (s *Server) hRepoInventory(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	if _, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	}
	inv, err := s.WIs.Inventory(wiID)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"repo_inventory": inv}) })
}

// hHeartbeat is row 21: upsert keyed on machine_id; first call registers the
// machine (creates the machine user + issues an api key whose plaintext is
// returned exactly once), later calls only refresh the heartbeat.
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
	var existingUser string
	err = s.DB.QueryRow(`SELECT user_id FROM machines WHERE id=?1`, body.MachineID).Scan(&existingUser)
	created := false
	if errors.Is(err, errNoRowsSentinel()) {
		// Registration: create machine user + key.
		created = true
		uid, _ := newID("mac_")
		keyID, _ := newID("key_")
		plain, _ := newSecret(24)
		display := body.DisplayName
		if display == "" {
			display = "machine " + body.MachineID
		}
		tx, txerr := s.DB.Begin()
		if txerr != nil {
			writeErr(w, txerr)
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`INSERT INTO users(id, display_name, kind, role) VALUES (?1,?2,'machine','writer')`, uid, display); err != nil {
			writeErr(w, err)
			return
		}
		if _, err := tx.Exec(`INSERT INTO machines(id, user_id, runtime_version, last_heartbeat_at)
			VALUES (?1,?2,?3,?4)`, body.MachineID, uid, body.RuntimeVersion, nowStamp()); err != nil {
			writeErr(w, err)
			return
		}
		if _, err := tx.Exec(`INSERT INTO api_keys(id, user_id, project_id, key_hash) VALUES (?1,?2,NULL,?3)`,
			keyID, uid, sha256Hex(plain)); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"machine_id": body.MachineID, "user_id": uid, "registered": true,
			"api_key": plain,
		})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.DB.Exec(`UPDATE machines SET last_heartbeat_at=?1, runtime_version=?2 WHERE id=?3`,
		nowStamp(), body.RuntimeVersion, body.MachineID); err != nil {
		writeErr(w, err)
		return
	}
	_ = idn
	writeJSON(w, http.StatusOK, map[string]any{"machine_id": body.MachineID, "registered": created})
}
