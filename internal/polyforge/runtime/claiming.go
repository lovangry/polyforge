package runtime

// claiming.go: the B1 pre-claim durable journal (v1.5 contract §4.2). A
// crash anywhere between the aihub session claim and the first spawned
// agent would otherwise orphan a RUNNING session that nobody can name
// again — the v0 known gap "#Start-crash" (D5). The journal closes it
// from both sides:
//
//   - BEFORE the claim call, the minted session secret and the claim's
//     idempotency key are persisted durably. The entry is cleared once
//     the start settles (live job registered, or the session ended
//     blocked as compensation).
//   - at startup, every uncleared entry is replayed: SessionVerify of
//     the journaled session decides — matches → the session is still
//     ours, adopt it and finish the start (the SAME idempotency key
//     hands back the SAME claim answer, D4); not matches / the hub says
//     otherwise → the intention is dead, drop the entry.
//
// The file is a map keyed by the SUBMITTED id spelling (a slug is legal:
// the claim endpoint accepts id-or-slug), so clearClaiming and the
// restart replay agree on keys. opMu serializes every writer (StartIntent
// and Restart both hold it), so no lock beyond the file's own atomic
// write is needed.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/common/api"
)

// claimingFile is the on-disk shape of the journal: one entry per
// submitted id spelling whose start has not settled yet.
type claimingFile struct {
	Version int                     `json:"version"`
	Items   map[string]*claimingRec `json:"items"`
}

// claimingRec is one journaled, not-yet-settled claim intention (the
// v1.5 §4.2 shape): the minted session secret is the replay anchor — a
// verify of it answers "did our claim land, and is it still ours?".
type claimingRec struct {
	WorkItemID     string `json:"work_item_id"`    // the SUBMITTED spelling (id-or-slug)
	SessionID      string `json:"session_id"`      // the minted SECRET (replay anchor)
	IdempotencyKey string `json:"idempotency_key"` // the D4 replay token
	MachineID      string `json:"machine_id"`
	At             string `json:"at"`
}

func (r *Runtime) claimingPath() string {
	return filepath.Join(r.cfg.DataDir, "claiming.json")
}

// loadClaiming reads the journal; a missing file is an empty one.
func (r *Runtime) loadClaiming() map[string]*claimingRec {
	raw, err := os.ReadFile(r.claimingPath())
	if err != nil {
		return map[string]*claimingRec{}
	}
	var cf claimingFile
	if json.Unmarshal(raw, &cf) != nil || cf.Items == nil {
		// a torn journal never blocks a start: the entries it carried
		// are lost, but the session fence (restart reconciliation) still
		// closes whatever aihub knows as ours.
		return map[string]*claimingRec{}
	}
	return cf.Items
}

// writeClaiming persists the whole journal map atomically.
func (r *Runtime) writeClaiming(items map[string]*claimingRec) error {
	if items == nil {
		items = map[string]*claimingRec{}
	}
	blob, err := json.MarshalIndent(&claimingFile{Version: 2, Items: items}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(r.claimingPath(), blob, stateFilePerm)
}

// noteClaiming journals one claim intention — the minted session secret
// plus the idempotency key — BEFORE the claim call. A write failure is
// fatal to the start (the caller refuses to claim): a claim we cannot
// journal is exactly the dangling-session risk this file exists to close.
func (r *Runtime) noteClaiming(wi, sessionID, key string) error {
	items := r.loadClaiming()
	items[wi] = &claimingRec{
		WorkItemID:     wi,
		SessionID:      sessionID,
		IdempotencyKey: key,
		MachineID:      r.cfg.MachineID,
		At:             nowStamp(),
	}
	return r.writeClaiming(items)
}

// clearClaiming settles one journal entry (the start completed, the
// session ended, or the claim never opened). A missing entry is a no-op;
// a write failure is loud but never blocks the caller.
func (r *Runtime) clearClaiming(wi string) {
	items := r.loadClaiming()
	if _, ok := items[wi]; !ok {
		return
	}
	delete(items, wi)
	if err := r.writeClaiming(items); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: claiming journal clear failed: %v\n", wi, err)
	}
}

// resumeClaiming replays every uncleared journal entry at daemon start
// (called from Restart, opMu held, BEFORE the state.json records are
// seeded — a journaled start must win over any stale record of the same
// WI). Per entry:
//
//   - aihub unreachable → keep the entry for the next restart, never
//     guess a verdict;
//   - SessionVerify says the journaled session still holds the WI →
//     adopt: replay the claim with the SAME idempotency key (D4: the
//     same response — bundle pin, repos, fingerprint — comes back) and
//     run the post-claim continuation; its own failure paths compensate
//     with SessionEnd(blocked) and clear the entry;
//   - verify says otherwise (another session holds it, the WI went
//     terminal, the session was cleared) → the intention can no longer
//     be honored: drop the entry. The restart reconciliation still
//     covers anything aihub records as ours.
//
// One guard before the replay: a state.json record carrying the SAME
// session secret means the crashed start had already registered the job
// (the crash hit between persist and clearClaiming) — the record path
// owns the reconciliation then, and only the journal entry is stale.
func (r *Runtime) resumeClaiming(ctx context.Context) {
	for wi, rec := range r.loadClaiming() {
		v, err := r.hub.SessionVerify(rec.WorkItemID, rec.SessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming entry kept (aihub verify failed): %v\n", wi, err)
			continue
		}
		if !v.Matches {
			fmt.Fprintf(os.Stderr, "runtime: %s: stale claiming entry dropped (hub says %s, holder %q)\n",
				wi, v.Status, v.CurrentFingerprint)
			r.clearClaiming(wi)
			continue
		}
		if r.sessionIsRecorded(rec.SessionID) {
			// the start settled past registration before the crash: the
			// state.json record reconciles it; only the journal is stale.
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming entry dropped (start already registered, record owns the restart)\n", wi)
			r.clearClaiming(wi)
			continue
		}
		resp, err := r.hub.SessionClaim(api.SessionClaimRequest{
			WorkItemID:     rec.WorkItemID,
			SessionID:      rec.SessionID,
			MachineID:      rec.MachineID,
			Owner:          rec.MachineID,
			IdempotencyKey: rec.IdempotencyKey,
		})
		if err != nil {
			// The claim face refuses the replay (the session was taken
			// over, ended, or the server disagrees): the entry names an
			// intention that can no longer be honored — drop it rather
			// than looping forever. The restart reconciliation still
			// covers anything aihub records as ours.
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming replay refused, entry dropped: %v\n", wi, err)
			r.clearClaiming(wi)
			continue
		}
		if err := r.startClaimed(ctx, rec.WorkItemID, rec.SessionID, resp); err != nil {
			// startClaimed already compensated (blocked) and cleared
			// the entry; the log line is for the operator.
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming replay start failed (session ended blocked): %v\n", wi, err)
		}
	}
}

// sessionIsRecorded reports whether a state.json record already carries
// this session secret (the crashed start registered the job).
func (r *Runtime) sessionIsRecorded(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	st, err := loadState(r.statePath)
	if err != nil {
		return false
	}
	for _, rec := range st.Jobs {
		if rec.SessionID == sessionID {
			return true
		}
	}
	return false
}
