package daemon

// shutdown.go: the graceful shutdown sequence (04 §6.5, v1 shape). It is
// the daemon PROCESS's termination path — never a mode, never drain:
//
//  1. latch shutting_down FIRST (external submissions now answer
//     DAEMON-410 and the drain loop stops fetching — the same latch,
//     two block paths);
//  2. wait, bounded per job, for the agent's flush(shutdown) — its own
//     commit-and-ack of what exists — or degrade on timeout (the data
//     is in the working tree; the recovery engine's
//     commit-dirty-on-recover picks it up at the next start);
//  4. seal: stop the observe lane (no recovery signal may fire against
//     the teardown), terminate every live agent tree, persist the
//     bookkeeping (Runtime's state.json + the daemon state file);
//  5. close the UDS listener; Run returns; the process exits.
//
// The whole sequence is bounded by ShutdownBudget (5 minutes) — a
// pathological combination of slow windows cannot outlive it. Jobs are
// NOT session-ended here: the WIs stay running server-side (our session
// row intact, the records keep clean_pause=false — exactly the
// "paused-with-session, will attach on restart" marker of M3; the hub's
// pause transition exists and is reached through session-end's paused
// verdict, but the shutdown deliberately does not use it) so the next
// daemon start's restart reconciliation recovers them through the
// recovery engine (transcript resume, seconds — the v1 recovery model,
// unchanged in v1.5). A RETAINED operator-paused job (Finding 2) is
// already StatePaused with no live agent: it is outside liveJobs below
// and its record simply persists through step 4c.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// Shutdown runs the graceful shutdown sequence. Concurrent callers JOIN
// the first sequence (never race it: a second sequence that skipped
// every job would let the process exit mid-close) and receive its
// verdict — write-before-close is the happens-before edge for the error.
func (d *Daemon) Shutdown(ctx context.Context) error {
	first := false
	d.shutOnce.Do(func() { first = true })
	if !first {
		select {
		case <-d.shutDone:
			d.shutMu.Lock()
			err := d.shutdownErr
			d.shutMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), d.cfg.ShutdownBudget)
		defer cancel()
	}
	err := d.runShutdown(ctx)
	d.shutMu.Lock()
	d.shutdownErr = err
	d.shutMu.Unlock()
	close(d.shutDone)
	return err
}

func (d *Daemon) runShutdown(ctx context.Context) error {
	// Step 1: the latch, before any other action.
	d.modes.SetShuttingDown()

	// Steps 2-3 run against the snapshot of live jobs taken here; the
	// set only shrinks from this moment (no source may add a job).
	live := d.liveJobs()

	// Step 2 (polyforge#106): the pf/stop_notice delivery face is
	// deleted with the WI-Agent lane — the driver owns the step state
	// machine, so there is no agent to notify "stop advancing". The
	// bounded flush(shutdown) wait below is the sequence's only
	// per-job grace.
	//
	// The bounded flush(shutdown) wait, one window per job in
	// parallel (bounded by the overall ctx regardless).
	if err := d.awaitShutdownFlushes(ctx, live); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: shutdown flush wait: %v\n", err)
	}

	// Step 4a: stop the observe lane FIRST and WAIT for every monitor to
	// be gone — a monitor that watched the teardown would fire
	// WINeedsRecovery against it and the recovery engine would re-derive
	// an agent inside the shutdown (the exact inversion of the
	// sequence's purpose). The wait is what makes the ordering real:
	// cancel alone is asynchronous.
	if d.cancelObs != nil {
		d.cancelObs()
	}
	if d.obsDone != nil {
		select {
		case <-d.obsDone:
		case <-ctx.Done():
			// the budget ran out mid-wait: proceed; the final sweep below
			// still collects whatever is left.
		}
	}

	// Step 4b: terminate every still-live agent tree.
	for _, ji := range live {
		if ji.Agent == nil {
			continue
		}
		if cur, ok := d.rt.Info(ji.WorkItem); ok && cur.Agent != nil {
			if err := cur.Agent.Terminate(10 * time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "daemon: %s: agent tree teardown incomplete: %v\n", ji.WorkItem, err)
			}
		}
	}

	// Step 4c: persist the bookkeeping (jobs/attempts ride Runtime's
	// state.json; mode/concurrency ride the daemon state file) — but
	// only if this process ever owned the UDS face: a startup abort
	// (another daemon still holds the socket) must never rewrite the
	// shared bookkeeping of the daemon that does.
	if d.faceOwned {
		if err := d.rt.Persist(); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: state.json persist failed: %v\n", err)
		}
		if err := d.persistDaemonState(); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: daemon-state persist failed: %v\n", err)
		}
	}

	// Step 5: close the UDS face, stop the loops, and wait for the
	// runtime message loop to drain — an in-flight recovery triggered
	// before the monitors stopped must never outlive the process.
	d.closeFace()
	if d.cancelRun != nil {
		d.cancelRun()
	}
	if d.rtDone != nil {
		select {
		case <-d.rtDone:
		case <-ctx.Done():
		}
	}
	// The runtime's release lane goes down with it: any observe release
	// still parked on the lane (NEW-2) is abandoned — the observe lane is
	// already stopped above, so nothing could consume it anyway.
	d.rt.Close()
	// Final sweep: no writer survives the shutdown — a recovery that was
	// mid-cascade when the monitors stopped may have installed a fresh
	// handle after the terminate pass above read the job set.
	for _, ji := range d.rt.List() {
		if ji.Agent != nil {
			if err := ji.Agent.Terminate(10 * time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "daemon: %s: residual agent teardown incomplete: %v\n", ji.WorkItem, err)
			}
		}
	}
	return nil
}

// liveJobs snapshots the jobs the shutdown sequence owns: locally
// managed, with a live agent, not already closing.
func (d *Daemon) liveJobs() []runtime.JobInfo {
	var out []runtime.JobInfo
	for _, ji := range d.rt.List() {
		if ji.Agent == nil || ji.State == runtime.StateSealing || ji.State == runtime.StateDone {
			continue
		}
		out = append(out, ji)
	}
	return out
}

// awaitShutdownFlushes waits, per job, for the monitor's shutdown-flush
// commit to land (the agent's flush(shutdown) acked path writes an
// EvPublished{shutdown:true} event) or the per-job window to expire.
// A timeout DEGRADES — it never blocks the sequence.
func (d *Daemon) awaitShutdownFlushes(ctx context.Context, live []runtime.JobInfo) error {
	if len(live) == 0 {
		return nil
	}
	type res struct {
		wi      string
		flushed bool
	}
	resCh := make(chan res, len(live))
	for _, ji := range live {
		go func(wi string) {
			deadline := time.Now().Add(d.cfg.ShutdownFlushWait)
			for time.Now().Before(deadline) {
				if shutdownFlushed(d.obs.Events(), wi) {
					resCh <- res{wi: wi, flushed: true}
					return
				}
				select {
				case <-ctx.Done():
					resCh <- res{wi: wi}
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
			resCh <- res{wi: wi}
		}(ji.WorkItem)
	}
	for range live {
		r := <-resCh
		if !r.flushed {
			fmt.Fprintf(os.Stderr, "daemon: %s: no flush(shutdown) within the window — degrading (uncommitted state rides the recovery engine)\n", r.wi)
		}
	}
	return ctx.Err()
}

// shutdownFlushed reports whether the monitor already committed this
// work item's shutdown flush (the EvPublished event with the shutdown
// marker written by observe's flushShutdown).
func shutdownFlushed(log *observe.EventLog, wi string) bool {
	if log == nil {
		return false
	}
	for _, e := range log.Since(wi) {
		if e.Kind != observe.EvPublished {
			continue
		}
		var body struct {
			Shutdown bool `json:"shutdown"`
		}
		if len(e.Body) > 0 && json.Unmarshal(e.Body, &body) == nil && body.Shutdown {
			return true
		}
	}
	return false
}
