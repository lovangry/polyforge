package runtime

// claiming.go: the B1 pre-claim durable journal (reference.md §3.5). A
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
//   - a claim call whose answer is lost (transport error, timeout, hub
//     5xx) KEEPS the entry: the claim may have landed and the entry is
//     the only copy of its secret. The in-process retry of the same
//     start reuses it (claimIntent) and so does the next replay; only a
//     typed refusal (4xx) or a verify that no longer matches drops it.
//
// The file is a map keyed by the SUBMITTED id spelling (a slug is legal:
// the claim endpoint accepts id-or-slug), so clearClaiming and the
// restart replay agree on keys. claimMu serializes the read-modify-write
// of every writer: the claim-end retry lane (and a claim's session end
// outside opMu, B-8) clears entries without opMu.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/silgrid/polyforge/internal/common/atomicfile"

	"github.com/silgrid/polyforge/internal/common/api"
)

// claimingFile is the on-disk shape of the journal: one entry per
// submitted id spelling whose start has not settled yet.
type claimingFile struct {
	Version int                     `json:"version"`
	Items   map[string]*claimingRec `json:"items"`
}

// claimingRec is one journaled, not-yet-settled claim intention (the
// reference.md §3.5 shape): the minted session secret is the replay anchor — a
// verify of it answers "did our claim land, and is it still ours?".
type claimingRec struct {
	WorkItemID     string `json:"work_item_id"`    // the SUBMITTED spelling (id-or-slug)
	SessionID      string `json:"session_id"`      // the minted SECRET (replay anchor)
	IdempotencyKey string `json:"idempotency_key"` // the D4 replay token
	MachineID      string `json:"machine_id"`
	At             string `json:"at"`
	// Models is the manual `pf submit --models` list of this start, so a
	// crash-replayed claim keeps it.
	Models []string `json:"models,omitempty"`
}

func (r *Runtime) claimingPath() string {
	return filepath.Join(r.cfg.DataDir, "claiming.json")
}

// loadClaiming reads the journal; a missing file is an empty one. A
// journal that exists but will not parse is QUARANTINED and logged
// (NOTE 6, v1.5 review): a torn/corrupt pre-registration journal is
// LOST — D5's window, which requires fs corruption to hit at all —
// and pretending nothing happened would hide it. The entries it
// carried cannot be replayed, but the session fence (restart
// reconciliation) still closes whatever aihub knows as ours, so a
// quarantined journal never blocks a start and never orphans a session
// the hub can still name.
func (r *Runtime) loadClaiming() map[string]*claimingRec {
	raw, err := os.ReadFile(r.claimingPath())
	if err != nil {
		if !os.IsNotExist(err) {
			// an unreadable-but-present journal is the same corruption
			// class as an unparsable one: loud, never silent.
			fmt.Fprintf(os.Stderr, "runtime: claiming journal unreadable (%v); starting empty\n", err)
		}
		return map[string]*claimingRec{}
	}
	var cf claimingFile
	if json.Unmarshal(raw, &cf) != nil || cf.Items == nil {
		fmt.Fprintf(os.Stderr, "runtime: claiming journal unparsable (%d bytes): the pre-claim entries it carried are LOST (D5's window; requires fs corruption)\n", len(raw))
		// quarantine beside the journal so the operator can inspect the
		// corruption while the next start runs on an empty one.
		ts := strconv.FormatInt(time.Now().UnixNano(), 36)
		if qerr := os.Rename(r.claimingPath(), r.claimingPath()+".corrupt."+ts); qerr != nil {
			fmt.Fprintf(os.Stderr, "runtime: claiming journal quarantine failed: %v\n", qerr)
		} else {
			fmt.Fprintf(os.Stderr, "runtime: corrupt claiming journal quarantined as claiming.json.corrupt.%s\n", ts)
		}
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
	return atomicfile.Write(r.claimingPath(), blob, stateFilePerm)
}

// noteClaiming journals one claim intention — the minted session secret
// plus the idempotency key — BEFORE the claim call. A write failure is
// fatal to the start (the caller refuses to claim): a claim we cannot
// journal is exactly the dangling-session risk this file exists to close.
func (r *Runtime) noteClaiming(wi, sessionID, key string, models []string) error {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	items := r.loadClaiming()
	items[wi] = &claimingRec{
		WorkItemID:     wi,
		SessionID:      sessionID,
		IdempotencyKey: key,
		MachineID:      r.cfg.MachineID,
		At:             nowStamp(),
		Models:         models,
	}
	return r.writeClaiming(items)
}

// clearClaiming settles one journal entry (the start completed, the
// session ended, or the claim never opened). A missing entry is a no-op;
// a write failure is loud but never blocks the caller.
func (r *Runtime) clearClaiming(wi string) { r.clearClaimingSession(wi, "") }

// clearClaimingSession is clearClaiming that, with sessionID set, only
// drops an entry still journaling that session: a close settling outside
// opMu must never drop the entry a newer start of the same work item
// wrote meanwhile.
func (r *Runtime) clearClaimingSession(wi, sessionID string) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	items := r.loadClaiming()
	rec, ok := items[wi]
	if !ok || (sessionID != "" && rec.SessionID != sessionID) {
		return
	}
	delete(items, wi)
	if err := r.writeClaiming(items); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: claiming journal clear failed: %v\n", wi, err)
	}
}

// resumeClaiming replays every uncleared journal entry at daemon start
// (called from the restart reconciliation, opMu held — released around
// every hub call: the verify and the claim replay (hubUnlocked), a
// compensation SessionEnd (claimEndOffLock) — BEFORE the state.json
// records are reconciled; a journaled start wins over any stale record of
// the same WI). A shutdown latched meanwhile stops it, every remaining
// entry kept. Per entry:
//
//   - aihub unreachable → keep the entry for the next restart, never
//     guess a verdict;
//   - SessionVerify says the journaled session still holds the WI →
//     adopt: replay the claim with the SAME idempotency key (D4: the
//     same response — bundle pin, repos, fingerprint — comes back) and
//     enter its CLAIMING placeholder; the answered replays are carried
//     to their agents by runReplays once Restart's records are seeded
//     (B-8: the clone runs outside opMu), and their own failure paths
//     compensate with SessionEnd(blocked) and clear the entry;
//   - verify says otherwise (another session holds it, the WI went
//     terminal, the session was cleared) → the intention can no longer
//     be honored: drop the entry. The restart reconciliation still
//     covers anything aihub records as ours.
//
// One guard before the replay: a state.json record carrying the SAME
// session secret means the crashed start had already registered the job
// (the crash hit between persist and clearClaiming) — the record path
// owns the reconciliation then, and only the journal entry is stale.
func (r *Runtime) resumeClaiming(ctx context.Context) (replays []claimReplay) {
	for wi, rec := range r.loadClaiming() {
		if r.restartStopping(ctx) {
			return replays // shutdown: the remaining entries stay for the next start
		}
		var v *api.SessionVerifyResponse
		var err error
		r.hubUnlocked(func() { r.laneHub(func() { v, err = r.hub.SessionVerify(rec.WorkItemID, rec.SessionID) }) })
		if r.restartStopping(ctx) {
			return replays // nothing decided: the entry stays
		}
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
		var resp *api.SessionClaimResponse
		r.hubUnlocked(func() {
			r.laneHub(func() {
				resp, err = r.hub.SessionClaim(api.SessionClaimRequest{
					WorkItemID:     rec.WorkItemID,
					SessionID:      rec.SessionID,
					MachineID:      rec.MachineID,
					IdempotencyKey: rec.IdempotencyKey,
				})
			})
		})
		if r.restartStopping(ctx) {
			// shutdown: whatever the answer, the entry stays and the next
			// start replays it (the same key hands the same answer back)
			return replays
		}
		if err != nil {
			if !claimRefused(err) {
				// transport loss / 5xx: the outcome is unknown and the
				// entry is the only copy of the secret — keep it for the
				// next replay, exactly like the verify failure
				// above.
				fmt.Fprintf(os.Stderr, "runtime: %s: claiming entry kept (aihub claim replay failed): %v\n", wi, err)
				continue
			}
			// The claim face refuses the replay of a session the verify
			// just proved is still ours: the intention can no longer be
			// honored, but the session is OPEN at the hub under the
			// secret only this entry holds — end it blocked with that
			// secret (abandonClaim; the entry stays until the close
			// lands) rather than dropping the only copy.
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming replay refused, ending the open session blocked: %v\n", wi, err)
			r.abandonClaim(&job{wi: rec.WorkItemID, sessionID: rec.SessionID}, wi, err,
				"the replayed claim of an open session was refused at restart: "+err.Error())
			continue
		}
		// The claim is already open server-side, so this is the one place
		// the yaml is read after a claim: an invalid file here
		// compensates with the D5 blocked end like any post-claim refusal.
		wcfg, werr := r.LoadWorkspace(resp.WorkItem.ProjectName)
		if werr != nil {
			var ae *api.Error
			note := werr.Error()
			if errors.As(werr, &ae) && ae.Detail != "" {
				note = ae.Detail
			}
			id := claimAnswerID(resp)
			r.abandonClaim(&job{wi: id, sessionID: rec.SessionID}, rec.WorkItemID, werr, note)
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming replay refused (session ended blocked): %v\n", wi, werr)
			continue
		}
		// A journaled start wins over a stale record of the same work item
		// (the record predates the secret the journal carries): Restart
		// seeded the records before this lane ran, so the still-unreconciled
		// one is dropped here, exactly as if it had never been seeded.
		var stale *job
		id := claimAnswerID(resp)
		if ex := r.jobOf(id); ex != nil {
			ex.mu.Lock()
			pending := ex.restartPending
			ex.mu.Unlock()
			if pending {
				stale = ex
				r.forgetRecord(&jobRecord{WorkItemID: id})
			}
		}
		j, err := r.claimPlaceholder(ctx, rec.WorkItemID, rec.SessionID, resp, wcfg, rec.Models)
		if err != nil {
			if stale != nil && r.jobOf(id) == nil {
				// the replay did not take the work item after all: the
				// record is reconciled as if never dropped
				r.mu.Lock()
				r.jobs[id] = stale
				r.mu.Unlock()
			}
			// claimPlaceholder already compensated (blocked) and cleared
			// the entry; the log line is for the operator.
			fmt.Fprintf(os.Stderr, "runtime: %s: claiming replay start failed (session ended blocked): %v\n", wi, err)
			continue
		}
		replays = append(replays, claimReplay{wi: rec.WorkItemID, j: j})
	}
	return replays
}

// claimReplay is one journal replay whose CLAIMING placeholder is in the
// table, waiting for its capsule clone and materialize (runReplays).
type claimReplay struct {
	wi string // the journal key (the submitted spelling)
	j  *job
}

// runReplays carries the restart's journal replays to their agents, each
// on its own goroutine like Start's lane: the capsule
// clone and materialize do not run under opMu inside Restart, so a hung
// clone holds neither the other records' reconciliation nor the
// daemon's socket. A shutdown meanwhile defers them (AbortClaims) with
// their journal entries kept.
//
// The caller holds opMu. The shutdown latch and the WaitGroup's Add are
// read and done under mu, so AbortClaims (which latches under mu before it
// waits) either waits for these goroutines or they are never started: a
// replay the shutdown caught is deferred here and then, journal kept.
func (r *Runtime) runReplays(ctx context.Context, replays []claimReplay) {
	r.mu.Lock()
	if r.claimsAborted {
		r.mu.Unlock()
		r.deferReplays(ctx, replays)
		return
	}
	r.replays.Add(len(replays))
	r.mu.Unlock()
	for _, cr := range replays {
		go func(cr claimReplay) {
			defer r.replays.Done()
			if err := r.runClaim(ctx, cr.wi, cr.j); err != nil {
				if IsClaimDeferred(err) {
					return // a shutdown during the replay: the entry stays
				}
				fmt.Fprintf(os.Stderr, "runtime: %s: claiming replay start failed: %v\n", cr.wi, err)
			}
		}(cr)
	}
}

// deferReplays settles journal replays whose clones must not start — a
// shutdown caught them, or the startup aborts (opMu held): each
// placeholder goes, its journal entry stays for the next start
// (finishClaim's deferral), and a Stop that requested a close meanwhile
// still gets its close.
func (r *Runtime) deferReplays(_ context.Context, replays []claimReplay) {
	for _, cr := range replays {
		cr.j.claim.cancel()
		// the claim's own (now cancelled) context: finishClaim defers
		// whatever the caller's context says
		_ = r.finishClaim(cr.j.claim.ctx, cr.wi, cr.j, context.Canceled, context.Canceled.Error())
	}
}

// claimAnswerID is the canonical work-item id a claim answer carries.
func claimAnswerID(resp *api.SessionClaimResponse) string {
	if resp.WorkItem.ID != "" {
		return resp.WorkItem.ID
	}
	return resp.Claim.WorkItemID
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
