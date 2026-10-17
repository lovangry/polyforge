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
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
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

// peekLastShutdown reads the previous run's shutdown record (Run consumes
// it with consumeLastShutdown once the start succeeded).
// Absent = the previous process did not shut down through the graceful
// sequence (crash / SIGKILL) or this is a first boot.
func (d *Daemon) peekLastShutdown() map[string]any {
	path := filepath.Join(d.cfg.DataDir, lastShutdownFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{"cause": "unknown", "note": "no graceful-shutdown record: the previous process crashed or was killed, or this is a first start"}
	}
	var ls lastShutdown
	if json.Unmarshal(raw, &ls) != nil {
		return map[string]any{"cause": "unknown", "note": "last-shutdown record unparsable"}
	}
	return map[string]any{"cause": ls.Kind, "detail": ls.Detail, "at": ls.At}
}

// consumeLastShutdown removes the previous run's shutdown record.
func (d *Daemon) consumeLastShutdown() {
	_ = os.Remove(filepath.Join(d.cfg.DataDir, lastShutdownFile))
}

// announceRestart writes the durable daemon_restarted event into every WI
// the daemon recovered (v2 §6 bug 8).
func (d *Daemon) announceRestart(prev map[string]any) {
	if d.obs == nil {
		return
	}
	for _, ji := range d.rt.List() {
		body := map[string]any{"previous_shutdown": prev, "state": string(ji.State)}
		_ = d.obs.Events().Append(ji.WorkItem, observe.EvDaemonRestarted, body)
	}
}

func (d *Daemon) recordSubmitRejected(e qEntry, err error) {
	if d.obs == nil {
		return
	}
	body := map[string]any{
		"error":        err.Error(),
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
	fmt.Fprintf(os.Stderr, "daemon: submit replay: %s dropped: %s\n", e.WorkItemID, reason)
	if d.obs == nil {
		return
	}
	body := map[string]any{
		"reason":       reason,
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
//   - the WI is read from aihub first: anything but queued (claimed
//     elsewhere, terminal, paused, blocked) is dropped;
//   - a WI the runtime already holds (the journal replay adopted it, or a
//     parked record) is dropped — never claimed twice;
//   - aihub unreachable: the entry stays queued (the claim CAS is the
//     authority and a refused start lands a submit_rejected event).
func (d *Daemon) replaySubmitQueue() {
	entries := d.loadPersistedQueue()
	// The restart reconciliation's wait ran out (aihub hung): the per-entry
	// "still queued?" probe would hold the socket on the same hub. Skip it —
	// the entries are enqueued, their Starts answer DAEMON-503 until the
	// reconciliation finished, and the claim's own probe and the hub's CAS
	// decide them then.
	probe := !d.rt.Reconciling()
	var keep []qEntry
	for _, e := range entries {
		if e.WorkItemID == "" {
			continue
		}
		if e.Origin.Kind == runtime.OriginAuto && !d.modes.Auto() {
			// v2-converge §5 at replay (audit B3b): the previous process's
			// auto entries must not re-enter a machine the operator turned
			// auto off on — the same rule the live auto-off purge applies;
			// explicit (cli/chat) entries replay as before.
			d.dropReplay(e, "auto is off on this machine (only explicit pf submit replays)")
			continue
		}
		if _, live := d.rt.Info(e.WorkItemID); live {
			d.dropReplay(e, "the work item is already held by this daemon (claiming-journal replay or kept record)")
			continue
		}
		if probe {
			if item, err := d.hub.GetWorkItem(e.WorkItemID); err == nil && item != nil && item.Status != api.StatusQueued {
				d.dropReplay(e, fmt.Sprintf("aihub shows the work item %s, no longer queued", item.Status))
				continue
			}
		}
		keep = append(keep, e)
	}
	// ONE write, after the filter: the file keeps the previous process's
	// entries until then, so a crash mid-replay loses none of them; it is
	// rewritten even when nothing survived, so dropped entries do not
	// replay again on the next start.
	if err := d.q.Restore(keep); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: submit replay: queue persist failed: %v\n", err)
	}
}
