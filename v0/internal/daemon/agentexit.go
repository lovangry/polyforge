package daemon

// agentexit.go: the shutdown/close path's observation of WI-Agent death
// (polyforge#32 pilot finding). The supervisor's reaper goroutine already
// owns the authoritative exit signal — mainagent.Proc closes its `exited`
// channel the moment cmd.Wait() returns — but nothing on the Daemon side
// selected on it: the graceful-shutdown quiesce waits (04 §6.5 step 3,
// closeJob's stop-notice/quiesce acks and the 05 §2.7 write-freeze
// confirmation behind them) waited on the ACK channel only. A pilot run
// where the agent died inside that window left the daemon parked in
// `(shutting down)` for 20+ minutes: the quiesce failure escalated into
// DAEMON-511 and the process main blocked forever (its SIGTERM handler
// already consumed), so only SIGKILL cleared it.
//
// This file adds the observation WITHOUT touching §7.1 semantics: the
// crash-continuation judgment at a daemon RESTART consumes procfs
// liveness (processAlive in RecoverFromState), never this face. Exposing
// the reaper's exit channel here adds an observer and removes nothing —
// a NON-shutdown agent death still crash-continues exactly as before
// (regression-pinned by TestShutdownDeathNoShutdownStillCrashContinues).

import (
	"context"
	"errors"
	"time"
)

// agentExitGrace bounds the extra wait the dead-grant decision below may
// spend confirming an exit that the control-path failure already implies:
// a process can be gone while its corpse is not yet reaped (Alive() still
// true on a zombie) or the exit face has not fired yet.
const agentExitGrace = 2 * time.Second

// AgentExitWaiter is the OPTIONAL extension of the AgentHandle face that
// exposes the agent process's exit signal — the reaper's own channel
// (mainagent.Proc.Wait closes over the reaper's exited channel). Handles
// that do not implement it (adopted agents after a daemon restart, test
// fakes) keep their pre-existing behavior: the control call's own ack
// deadline stays the only window on the wait.
type AgentExitWaiter interface {
	// Wait blocks until the agent process has exited and reports its wait
	// status. Callable from any number of goroutines; it never blocks
	// forever on an already-exited process.
	Wait() error
}

// ErrAgentDied marks a control wait that was SHORT-CIRCUITED by an
// observed agent exit — distinct from an ack, an ack-deadline expiry and
// a control-path failure, so callers can tell "the writer domain is
// dying/dead" from "the writer is alive but stuck".
var ErrAgentDied = errors.New("daemon: wi agent exited during the control wait")

// controlWatch runs ONE blocking agent control call (NotifyStop / Quiesce
// — each already bounded by its own ack deadline on the real handle) with
// the agent's exit signal SELECTED alongside it (04 §6.5 step 3's wait
// must observe a dead WI Agent, polyforge#32):
//
//   - the call returns first -> its verdict is passed through unchanged;
//   - the EXIT signal fires first -> ErrAgentDied: shutdown/close takes
//     precedence over any further coordination with the agent. The close
//     then proceeds on the daemon-exit shape §6.5 step 4 already defines
//     (the daemon publishes on the agent's behalf over a PROVEN-empty
//     writer domain), and the shutdown is NOT crash-continued: the
//     shutting_down latch has already frozen every job-creation source
//     and the close ends the generation, so a later daemon restart finds
//     no running jobs[] record to re-derive (§7.1 applies only to jobs a
//     crash left recorded as running — never to one this close ended);
//   - ctx ends first -> the ctx error (the shutdown-sequence budget).
//
// The abandoned in-flight call goroutine is left to return on its own
// bound (the real handle's ack deadline; a pathological handle could leak
// it until then) — it never blocks the close, which is the point.
func controlWatch(ctx context.Context, agent AgentHandle, call func(context.Context) error) error {
	aw, ok := agent.(AgentExitWaiter)
	if !ok {
		// No exit face on this handle: the call's own bound is the only
		// window, exactly as before this file existed.
		return call(ctx)
	}
	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() { done <- outcome{call(ctx)} }()
	exited := make(chan struct{})
	go func() { _ = aw.Wait(); close(exited) }()
	select {
	case o := <-done:
		return o.err
	case <-exited:
		return ErrAgentDied
	case <-ctx.Done():
		return ctx.Err()
	}
}

// agentGone decides whether a failed control call means "the agent is
// dead" — the precondition for the 05 §2.7 condition-① freeze grant in
// quiescerOf.RequestQuiesce. Alive() alone is not enough (the zombie
// window before cmd.Wait() reaps the corpse), and the exit face may not
// have fired yet when the control path already failed, so the exit
// signal is awaited for a bounded grace. An agent that is merely
// UNHEALTHY (alive, control path closed by overflow) answers false: it
// keeps its writers and earns no dead-writers shortcut (fail closed).
func agentGone(agent AgentHandle) bool {
	if agent == nil {
		return true
	}
	if !agent.Alive() {
		return true
	}
	aw, ok := agent.(AgentExitWaiter)
	if !ok {
		return false
	}
	exited := make(chan struct{})
	go func() { _ = aw.Wait(); close(exited) }()
	select {
	case <-exited:
		return true
	case <-time.After(agentExitGrace):
		return false
	}
}
