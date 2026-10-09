package runtime

// adopted.go: durable custody for sessions minted OUTSIDE any runtime
// start (#109). The CLI-side takeover mints the session secret in the
// operator's terminal; before this journal the secret died with the CLI
// process and the hub row kept naming a session no process could ever
// use again (attach, verify, or fence) — the #97/#109 orphaned holder.
// The CLI now hands the minted secret to the local daemon right after
// the hub swap (the daemon's POST /jobs/{id}/adopt-session route,
// verify-backed against the hub) and the daemon records it HERE.
//
// The journal is deliberately NOT the claiming journal (claiming.go):
// an adopted session never carries a start intention — the takeover
// starts nothing, and the adopted WI has no local capsule to resume —
// so a restart must never replay it into a claim. The reconciliation
// only keeps custody alive while the hub still names the session as the
// live holder and drops dead entries; the actual recovery is the
// operator's daemon-side takeover (`pf submit <id> --takeover`), whose
// fresh daemon-minted session supersedes the adopted one (startClaimed
// clears the entry).
//
// Exactly one entry per work item: the hub holds one session at a time,
// so a new adoption supersedes the previous one.
//
// Locking: opMu serializes AdoptSession against StartIntent/Restart
// (the same discipline as the claiming journal). The helpers called
// from opMu-held contexts (clearAdopted, resumeAdopted) take no lock —
// callers hold it already.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// adoptedFile is the on-disk shape of the custody journal.
type adoptedFile struct {
	Version int                    `json:"version"`
	Items   map[string]*adoptedRec `json:"items"`
}

// adoptedRec is one custody entry: the minted SECRET is the anchor —
// a SessionVerify of it answers whether custody is still meaningful.
type adoptedRec struct {
	WorkItemID string `json:"work_item_id"`
	SessionID  string `json:"session_id"` // the SECRET (custody anchor)
	MachineID  string `json:"machine_id"`
	At         string `json:"at"`
}

func (r *Runtime) adoptedPath() string {
	return filepath.Join(r.cfg.DataDir, "adopted.json")
}

// loadAdopted reads the custody journal; a missing file is an empty one.
// A journal that exists but will not parse is logged loud and treated as
// empty — never fatal: the hub row names the session regardless, and the
// recovery path (`pf submit <id> --takeover`) fences it with a fresh
// daemon-minted secret. (The lost custody of a foreign-minted session is
// the pre-#109 status quo for that one session, not a new hazard.)
func (r *Runtime) loadAdopted() map[string]*adoptedRec {
	raw, err := os.ReadFile(r.adoptedPath())
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "runtime: adopted-session journal unreadable (%v); starting empty\n", err)
		}
		return map[string]*adoptedRec{}
	}
	var af adoptedFile
	if json.Unmarshal(raw, &af) != nil || af.Items == nil {
		fmt.Fprintf(os.Stderr, "runtime: adopted-session journal unparsable (%d bytes); custody entries lost\n", len(raw))
		return map[string]*adoptedRec{}
	}
	return af.Items
}

func (r *Runtime) writeAdopted(items map[string]*adoptedRec) error {
	if items == nil {
		items = map[string]*adoptedRec{}
	}
	blob, err := json.MarshalIndent(&adoptedFile{Version: 1, Items: items}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(r.adoptedPath(), blob, stateFilePerm)
}

// AdoptSession records custody of one externally minted session secret
// (the CLI-side takeover's mint, handed over the daemon's UDS face).
// The caller — the daemon's adopt-session route — has already verified
// with the hub that the secret is the live holder before journaling it.
func (r *Runtime) AdoptSession(wi, sessionID string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	items := r.loadAdopted()
	items[wi] = &adoptedRec{
		WorkItemID: wi,
		SessionID:  sessionID,
		MachineID:  r.cfg.MachineID,
		At:         nowStamp(),
	}
	return r.writeAdopted(items)
}

// clearAdopted settles one custody entry: this daemon minted its own
// session for the work item (startClaimed — fresh claim or takeover), so
// the adopted secret is superseded. No-op when nothing is journalled; a
// write failure is loud but never blocks the caller. Caller holds opMu.
func (r *Runtime) clearAdopted(wi string) {
	items := r.loadAdopted()
	if _, ok := items[wi]; !ok {
		return
	}
	delete(items, wi)
	if err := r.writeAdopted(items); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: adopted-session journal clear failed: %v\n", wi, err)
	}
}

// resumeAdopted reconciles custody at daemon start (Restart, opMu held,
// after the claiming replay). Per entry:
//
//   - aihub unreachable → keep the entry, try again on the next restart;
//   - the hub still names the adopted session as the live holder →
//     custody is intact — say so once, loudly (the operator's recovery
//     is `pf submit <id> --takeover`; the session is attachable by THIS
//     daemon, not an orphan);
//   - anything else (another session took over, the WI went terminal,
//     the row cleared) → the custody is dead: drop the entry.
func (r *Runtime) resumeAdopted() {
	for wi, rec := range r.loadAdopted() {
		v, err := r.hub.SessionVerify(wi, rec.SessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: adopted session kept (aihub verify failed): %v\n", wi, err)
			continue
		}
		if !v.Matches {
			fmt.Fprintf(os.Stderr, "runtime: %s: adopted session no longer the holder (hub says %s, holder %q); custody dropped\n",
				wi, v.Status, v.CurrentFingerprint)
			r.clearAdopted(wi)
			continue
		}
		fmt.Fprintf(os.Stderr, "runtime: %s: adopted session still holds the WI; custody kept (`pf submit %s --takeover` runs it)\n", wi, wi)
	}
}
