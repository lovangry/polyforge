package runtime

// claiming.go: the D5 durable claim journal. A crash anywhere between the
// aihub claim and the first spawned agent orphans a RUNNING generation
// that nobody can name again — the v0 known gap "#Start-crash". The
// journal closes it: StartIntent persists the claim's idempotency key
// BEFORE the claim call, the entry is cleared once the start settles
// (live job registered, or the generation closed blocked), and Restart
// replays every uncleared entry with the SAME key — the server's claim
// idempotency contract (D4) hands back the SAME attempt — then either
// finishes the start or compensates with EndGeneration(blocked).
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
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// claimingFile is the on-disk shape of the journal: one entry per
// submitted id spelling whose start has not settled yet.
type claimingFile struct {
	Version int                     `json:"version"`
	Items   map[string]*claimingRec `json:"items"`
}

// claimingRec is one journaled, not-yet-settled claim intention.
type claimingRec struct {
	WorkItemID     string `json:"work_item_id"`    // the SUBMITTED spelling (id-or-slug)
	Intent         string `json:"intent"`          // claim|resume|takeover
	IdempotencyKey string `json:"idempotency_key"` // the replay token (D4)
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
		// are lost, but the generation-fence (restart reconciliation)
		// still closes whatever aihub knows as ours.
		return map[string]*claimingRec{}
	}
	return cf.Items
}

// writeClaiming persists the whole journal map atomically.
func (r *Runtime) writeClaiming(items map[string]*claimingRec) error {
	if items == nil {
		items = map[string]*claimingRec{}
	}
	blob, err := json.MarshalIndent(&claimingFile{Version: 1, Items: items}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(r.claimingPath(), blob, stateFilePerm)
}

// noteClaiming journals one claim intention before the claim call. A
// write failure is fatal to the start (the caller refuses to claim): a
// claim we cannot journal is exactly the dangling-generation risk this
// file exists to close.
func (r *Runtime) noteClaiming(wi, intent, key string) error {
	items := r.loadClaiming()
	items[wi] = &claimingRec{
		WorkItemID: wi, Intent: intent, IdempotencyKey: key,
		MachineID: r.cfg.MachineID, At: nowStamp(),
	}
	return r.writeClaiming(items)
}

// clearClaiming settles one journal entry (the start completed, the
// generation closed, or the claim never opened). A missing entry is a
// no-op; a write failure is loud but never blocks the caller.
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
// (called from Restart, opMu held, AFTER the state.json records are
// seeded so startClaimed's already-active check sees them). Per entry:
//
//   - aihub no longer knows the work item as running (terminal, blocked,
//     queued, paused) → the generation settled server-side before the
//     crash; the entry is stale, drop it;
//   - aihub unreachable → keep the entry for the next restart, never
//     guess a verdict;
//   - running → replay the claim with the SAME idempotency key (D4: the
//     same attempt comes back) and run the post-claim continuation; its
//     own failure paths compensate with EndGeneration(blocked) and clear
//     the entry (see startClaimed / abandonClaim).
func (r *Runtime) resumeClaiming(ctx context.Context) {
	for wi, rec := range r.loadClaiming() {
		item, err := r.hub.GetWorkItem(rec.WorkItemID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming entry kept (aihub lookup failed): %v\n", wi, err)
			continue
		}
		switch item.Status {
		case api.StatusRunning:
			// the generation is still ours: replay the claim
		default:
			// terminal/blocked/queued/paused: nothing to resume
			fmt.Fprintf(os.Stderr, "runtime: %s: stale claiming entry dropped (work item is %s)\n", wi, item.Status)
			r.clearClaiming(wi)
			continue
		}
		resp, err := r.hub.Claim(aihubclient.ClaimRequest{
			WorkItemID:     rec.WorkItemID,
			Intent:         rec.Intent,
			MachineID:      rec.MachineID,
			Owner:          rec.MachineID,
			IdempotencyKey: rec.IdempotencyKey,
		})
		if err != nil {
			// The claim face refuses the replay (the generation was
			// taken over, ended, or the server disagrees): the entry
			// names an intention that can no longer be honored — drop it
			// rather than looping forever. The restart reconciliation
			// still covers anything aihub records as ours.
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming replay refused, entry dropped: %v\n", wi, err)
			r.clearClaiming(wi)
			continue
		}
		if err := r.startClaimed(ctx, rec.WorkItemID, resp); err != nil {
			// startClaimed already compensated (blocked) and cleared
			// the entry; the log line is for the operator.
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming replay start failed (generation closed blocked): %v\n", wi, err)
		}
	}
}
