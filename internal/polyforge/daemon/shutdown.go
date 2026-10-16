package daemon

// shutdown.go: the graceful shutdown sequence (reference.md §6.2, v1 shape). It is
// the daemon PROCESS's termination path — never a mode, never drain:
//
//  1. latch shutting_down FIRST (external submissions now answer
//     DAEMON-410 and the drain loop stops fetching — the same latch,
//     two block paths);
//  2. (no per-job grace: the driver owns the step state machine and
//     there is no agent to ask for a final flush — uncommitted state
//     in the working tree is picked up by the recovery engine's
//     commit-dirty-on-recover at the next start);
//  4. seal: stop the observe lane (no recovery signal may fire against
//     the teardown), terminate every live agent tree, snapshot the task
//     repos' uncommitted work to refs/pf-wip (polyforge#167), persist the
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
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// ShutdownCause names what triggered the graceful shutdown sequence
// (polyforge#110). It is the attribution record a post-mortem reads:
// every Shutdown caller names its trigger explicitly — signal exits
// name the signal (name + number), route exits name the UDS peer
// credential (SO_PEERCRED), the Run arms name their own — so an
// unattributed graceful exit can only ever be one the code explicitly
// passes as such. The zero value is emitted as UNSPECIFIED (never a
// silent "unknown") so a call site that forgets its cause is visible
// in the log instead of re-creating the anonymous exit this WI
// closed.
type ShutdownCause struct {
	Kind   string // signal | route | caller-context | serve-error | direct
	Detail string // trigger specifics (signal number, UDS peer credential, ...)
}

// Cause kinds, one per production Shutdown call site (main.go's signal
// goroutine: CauseSignal; routes.go's POST /shutdown: CauseRoute;
// daemon.go Run's caller-ctx and serve-error arms: CauseCallerContext
// and CauseServeError; test harnesses and embedders calling Shutdown
// directly: CauseDirect).
const (
	CauseSignal        = "signal"
	CauseRoute         = "route"
	CauseCallerContext = "caller-context"
	CauseServeError    = "serve-error"
	CauseDirect        = "direct"
)

func (c ShutdownCause) String() string {
	switch {
	case c.Kind == "":
		return "UNSPECIFIED (a Shutdown caller passed the zero cause)"
	case c.Detail == "":
		return c.Kind
	default:
		return c.Kind + ": " + c.Detail
	}
}

// SignalShutdownCause attributes the shutdown to an OS signal. The
// detail carries the signal's name AND number (e.g. "terminated
// (signal 15)") — the number per the polyforge#110 fix ask, so a
// post-mortem attributes a signal exit in one read.
func SignalShutdownCause(sig os.Signal) ShutdownCause {
	if s, ok := sig.(syscall.Signal); ok {
		return ShutdownCause{Kind: CauseSignal, Detail: fmt.Sprintf("%s (signal %d)", s, int(s))}
	}
	return ShutdownCause{Kind: CauseSignal, Detail: fmt.Sprintf("%v", sig)}
}

// RouteShutdownCause attributes the shutdown to the POST /shutdown
// route over the daemon's UDS face. The detail carries the UDS peer
// credential — the kernel's SO_PEERCRED record of the dialing process
// (pid per the fix ask, uid as corroboration), never anything from the
// request body (caller-supplied; the CLI sends an empty object). An
// unavailable credential is recorded as such, explicitly — it never
// degrades into an anonymous exit.
func RouteShutdownCause(cred peerCred, ok bool) ShutdownCause {
	if !ok {
		return ShutdownCause{Kind: CauseRoute, Detail: "POST /shutdown uds peer credential unavailable"}
	}
	return ShutdownCause{Kind: CauseRoute, Detail: fmt.Sprintf("POST /shutdown uds peer pid=%d uid=%d", cred.pid, cred.uid)}
}

// CallerContextShutdownCause attributes the shutdown to the Run
// caller's context reaching Done (the embedder gave up on the daemon).
func CallerContextShutdownCause() ShutdownCause {
	return ShutdownCause{Kind: CauseCallerContext, Detail: "the Run caller's context is done"}
}

// ServeErrorShutdownCause attributes the shutdown to the UDS face's
// Serve returning an error.
func ServeErrorShutdownCause(err error) ShutdownCause {
	return ShutdownCause{Kind: CauseServeError, Detail: fmt.Sprintf("%v", err)}
}

// Shutdown runs the graceful shutdown sequence. Concurrent callers JOIN
// the first sequence (never race it: a second sequence that skipped
// every job would let the process exit mid-close) and receive its
// verdict — write-before-close is the happens-before edge for the error.
// cause is the FIRST caller's trigger record (polyforge#110): only the
// trigger of the first sequence is attributable, so joiners' causes are
// ignored.
func (d *Daemon) Shutdown(ctx context.Context, cause ShutdownCause) error {
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
	err := d.runShutdown(ctx, cause)
	d.shutMu.Lock()
	d.shutdownErr = err
	d.shutMu.Unlock()
	close(d.shutDone)
	return err
}

func (d *Daemon) runShutdown(ctx context.Context, cause ShutdownCause) error {
	// Step 0 (polyforge#110): the cause record, FIRST — before the
	// latch's side effects — so the attribution survives a
	// budget-exhausted or mid-sequence exit. One line per sequence on
	// the daemon's stderr: the sink a post-mortem of the process
	// window reads.
	fmt.Fprintf(os.Stderr, "daemon: shutdown: cause=%s\n", cause)
	d.recordLastShutdown(cause)

	// Step 1: the latch, before any other action.
	d.modes.SetShuttingDown()

	// Step 1b: claims under way run outside the Run loop and
	// outside the runtime's lock. Stop them before the live-job snapshot: no
	// new claim starts, every capsule/task-repo clone is cancelled, and each
	// in-flight claim is DEFERRED — its claiming-journal entry (and its
	// queue entry) stay for the next start, the work item is never turned
	// blocked — and none can install an agent behind the teardown below.
	// Only claims already CLAIMING are waited for, and at most
	// claimAbortWait — the WIP snapshots below need the rest of the budget.
	actx, acancel := context.WithTimeout(ctx, claimAbortWait(ctx))
	d.rt.AbortClaims(actx)
	acancel()

	// Steps 2-3 run against the snapshot of live jobs taken here; the
	// set only shrinks from this moment (no source may add a job).
	live := d.liveJobs()

	// Step 2 (polyforge#106): no pf/stop_notice delivery and no agent
	// flush(shutdown) wait — the driver owns the step state machine, so
	// there is no agent to notify or wait on.

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
	// Step 4b': with the step processes gone, snapshot each live job's
	// task-repo uncommitted work to refs/pf-wip (polyforge#167) — the
	// local capsule keeps it for the attach, the hub keeps it for a
	// re-claim. Each job gets its own slice of what is left of the budget
	// (its git calls are killed at the slice's end), so one hung hub
	// cannot starve the other jobs or outlive the process.
	for i, ji := range live {
		d.rt.SaveTaskWIP(ctx, ji.WorkItem, wipSlice(ctx, len(live)-i))
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
	// ...and for a restart reconciliation step still in flight (a
	// recovery's push and relaunch: the relaunch meets the cancelled run
	// context; the lane starts no new step after the latch), then persist
	// what it settled — no lane step outlives the sequence.
	d.rt.WaitRestartStep(ctx)
	if d.faceOwned {
		if err := d.rt.Persist(); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: state.json persist failed: %v\n", err)
		}
	}
	// Stop the runtime's durable-close retry goroutines.
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
// wipSlice is one job's share of the shutdown budget left for the WIP
// snapshots: the time to ctx's deadline split over the jobs still to go,
// capped at wipSliceMax (and wipSliceMax when ctx has no deadline).
func wipSlice(ctx context.Context, jobsLeft int) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok || jobsLeft < 1 {
		return wipSliceMax
	}
	slice := time.Until(dl) / time.Duration(jobsLeft)
	if slice > wipSliceMax {
		return wipSliceMax
	}
	return slice
}

// claimAbortWait bounds the shutdown's wait for in-flight claims to
// settle (B-8): min(30s, a quarter of what is left of the budget).
func claimAbortWait(ctx context.Context) time.Duration {
	const maxWait = 30 * time.Second
	dl, ok := ctx.Deadline()
	if !ok {
		return maxWait
	}
	if q := time.Until(dl) / 4; q < maxWait {
		return q
	}
	return maxWait
}

// wipSliceMax caps one job's WIP snapshot at shutdown.
const wipSliceMax = 2 * time.Minute

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
