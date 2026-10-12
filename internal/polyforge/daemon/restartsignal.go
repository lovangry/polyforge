package daemon

// restartsignal.go: the restart-side durable bookkeeping of v2-converge
// §5/§6 — the persisted submit queue's replay, the submit_rejected
// event, and the daemon_restarted signal (with the previous shutdown
// cause, #110, persisted as <DataDir>/last-shutdown.json).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/silgrid/polyforge/internal/common/atomicfile"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
)

const lastShutdownFile = "last-shutdown.json"

type lastShutdown struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	At     string `json:"at"`
}

// recordLastShutdown persists the shutdown attribution (#110) so the next
// start can name why the previous process went away. Best-effort.
func (d *Daemon) recordLastShutdown(cause ShutdownCause) {
	blob, err := json.Marshal(lastShutdown{Kind: cause.Kind, Detail: cause.Detail, At: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return
	}
	path := filepath.Join(d.cfg.DataDir, lastShutdownFile)
	_ = atomicfile.Write(path, blob, 0o600)
}

// takeLastShutdown reads and consumes the previous run's shutdown record.
// Absent = the previous process did not shut down through the graceful
// sequence (crash / SIGKILL) or this is a first boot.
func (d *Daemon) takeLastShutdown() map[string]any {
	path := filepath.Join(d.cfg.DataDir, lastShutdownFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{"cause": "unknown", "note": "no graceful-shutdown record: the previous process crashed or was killed, or this is a first start"}
	}
	_ = os.Remove(path)
	var ls lastShutdown
	if json.Unmarshal(raw, &ls) != nil {
		return map[string]any{"cause": "unknown", "note": "last-shutdown record unparsable"}
	}
	return map[string]any{"cause": ls.Kind, "detail": ls.Detail, "at": ls.At}
}

// announceRestart writes the durable daemon_restarted event into every WI
// the daemon recovered or parked (v2 §6 bug 8).
func (d *Daemon) announceRestart(prev map[string]any) {
	if d.obs == nil {
		return
	}
	for _, ji := range d.rt.List() {
		body := map[string]any{"previous_shutdown": prev, "state": string(ji.State)}
		if ji.RepairPending != "" {
			body["state"] = "repair_pending"
			body["repair_reason"] = ji.RepairPending
		}
		_ = d.obs.Events().Append(ji.WorkItem, observe.EvDaemonRestarted, body)
	}
}

func (d *Daemon) recordSubmitRejected(e qEntry, err error) {
	if d.obs == nil {
		return
	}
	body := map[string]any{
		"error":        err.Error(),
		"intent":       e.Intent,
		"submitted_at": e.SubmittedAt.UTC().Format(time.RFC3339Nano),
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		body["code"] = ae.Code
	}
	if e.Origin.Kind != "" {
		body["origin"] = e.Origin
	}
	_ = d.obs.Events().Append(e.WorkItemID, observe.EvSubmitRejected, body)
}

func (d *Daemon) dropReplay(e qEntry, reason string) {
	fmt.Fprintf(os.Stderr, "daemon: submit replay: %s (%s) dropped: %s\n", e.WorkItemID, e.Intent, reason)
	if d.obs == nil {
		return
	}
	body := map[string]any{
		"reason":       reason,
		"intent":       e.Intent,
		"submitted_at": e.SubmittedAt.UTC().Format(time.RFC3339Nano),
	}
	if e.Origin.Kind != "" {
		body["origin"] = e.Origin
	}
	_ = d.obs.Events().Append(e.WorkItemID, observe.EvSubmitDropped, body)
}

// replaySubmitQueue re-enqueues the submits the previous process accepted
// but never started (v2 §5). It runs strictly after rt.Restart (which
// replayed the B1 claiming journal), before the face serves:
//   - only plain `claim` intents replay; resume is dropped with
//     a submit_dropped event (a stale intent must never fire later);
//   - the WI is read from aihub first: anything but queued (claimed
//     elsewhere, terminal, paused, blocked) is dropped;
//   - a WI the runtime already holds (the journal replay adopted it, or a
//     parked record) is dropped — never claimed twice;
//   - aihub unreachable: the entry stays queued (the claim CAS is the
//     authority and a refused start lands a submit_rejected event).
func (d *Daemon) replaySubmitQueue() {
	entries := d.loadPersistedQueue()
	for _, e := range entries {
		if e.WorkItemID == "" {
			continue
		}
		if e.Intent != "claim" {
			d.dropReplay(e, fmt.Sprintf("intent %q is not replayed (only plain claim submits are)", e.Intent))
			continue
		}
		if _, live := d.rt.Info(e.WorkItemID); live {
			d.dropReplay(e, "the work item is already held by this daemon (claiming-journal replay or kept record)")
			continue
		}
		if item, err := d.hub.GetWorkItem(e.WorkItemID); err == nil && item != nil {
			if item.Status != api.StatusQueued {
				d.dropReplay(e, fmt.Sprintf("aihub shows the work item %s, no longer queued", item.Status))
				continue
			}
		}
		if err := d.q.Enqueue(e); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: submit replay: %s: queue persist failed: %v\n", e.WorkItemID, err)
		}
	}
	// Rewrite the file even when nothing survived, so dropped entries do
	// not replay again on the next start.
	if err := d.q.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: submit replay: queue flush failed: %v\n", err)
	}
}
