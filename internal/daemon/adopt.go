package daemon

// adopt.go: the crash-restart re-attachment of an ALIVE persistent WI Agent
// (04 §7.1 step 3): "存活（pid 存在且创建时间戳匹配）：不重新派生，重接该
// WI Agent——Daemon 崩溃重启丢失的只是自己进程内的监督连接与 RPC 通道，
// 不是该 WI Agent 本身……按记录的 harness_session_id 与 harness_adapter
// 重建对它的 RPC 连接与标准进程句柄观察（pid、退出码、stdout/stderr、
// 进程组），重接不打扰其运行、不重置其 session、不重复派发任何 step".
//
// The D2-ruled reality: the design's "重建 RPC 连接" cannot be honored
// literally for the v1 stdio transport — the control channel IS the
// process's stdin/stdout pipes, and those die with the old Daemon process;
// no new pipe pair can be attached to an already-running child. The
// re-attachment therefore rebuilds exactly the face that survives: the
// standard process-handle observation (pid, liveness with the pid-reuse
// guard, whole process group) plus group termination. Control messaging
// (NotifyStop / Quiesce / Resume / DeliverAnswer) is LOST on adopt and
// every such call fails typed with ErrControlChannelLost — never silently
// pretending the agent was notified. A publish for a re-attached agent
// consequently cannot take the quiesce-confirmation path until the agent
// is re-derived; freeze.Acquire fails loudly instead (fail closed).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// ErrControlChannelLost is the typed signal that a re-attached (adopted)
// WI Agent cannot receive control messages: its stdio control channel was
// a pipe of the PREVIOUS daemon process and is gone. Callers must surface
// this to the operator, never treat the notification as delivered.
var ErrControlChannelLost = errors.New("daemon: control messaging lost on adopted agent (stdio channels do not survive a daemon restart)")

// ErrAgentIdentityMismatch is the typed abort of a destructive operation
// (R3N-F1): the recorded agent pid is currently occupied by a live process
// whose creation time does NOT match the recorded started-at — a reused
// pid. NOTHING was signaled; the caller must not retry termination against
// this handle and must leave the innocent replacement process alone.
var ErrAgentIdentityMismatch = errors.New("daemon: recorded agent pid no longer matches its creation identity (pid reuse suspected)")

// adoptedAgent is the process-control-only AgentHandle over a live WI Agent
// pid recorded by a previous daemon life (04 §7.1 step 3). It observes and
// can terminate the WHOLE process group, but cannot send control messages.
type adoptedAgent struct {
	pid       int
	startedAt string
}

// Adopt re-attaches to a live WI Agent process recorded in daemon/state.json
// (04 §7.1 step 3: alive ⇒ re-attach, never re-derive). pid must have passed
// the pid-reuse guard (processAlive with the recorded started_at) BEFORE the
// caller adopts: a zombie or a reused pid must be collected and re-derived,
// not adopted (04 §7.1 step 4). startedAt is the RECORDED
// main_agent_started_at — the same value the pid-reuse guard compared
// against, so Alive() below keeps using it.
func Adopt(pid int, startedAt string) AgentHandle {
	return &adoptedAgent{pid: pid, startedAt: startedAt}
}

func (a *adoptedAgent) PID() int          { return a.pid }
func (a *adoptedAgent) StartedAt() string { return a.startedAt }

// Alive re-runs the pid-reuse-guarded liveness check (04 §7.1 step 2): a
// recycled pid is NOT our agent.
func (a *adoptedAgent) Alive() bool { return processAlive(a.pid, a.startedAt) }

// GroupEmpty implements the 05 §2.7 shortcut condition ① face for an
// adopted group. Fails CLOSED: when the group's emptiness cannot be proven
// (unreadable procfs), false is returned and the error is logged — the
// dead-writers shortcut is never granted on an assumption.
func (a *adoptedAgent) GroupEmpty() bool {
	empty, err := ProcessGroupEmpty(a.pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pf-daemon: adopted agent group %d emptiness unverifiable, failing closed: %v\n", a.pid, err)
		return false
	}
	return empty
}

// Terminate kills the adopted agent's WHOLE process group (04 §6.2 path B /
// §7.1 residue collection): SIGTERM, bounded wait for the group — not
// just the leader — then SIGKILL and a second bounded wait. The pid
// itself is ALSO signaled directly: a real WI Agent is its own group
// leader (Setpgid, 04 §6.1a), but a pid that leads no group (or a foreign
// process tree) makes kill(-pid) answer ESRCH — that is NOT proof the
// process is gone, only that no group bears its id, so the direct signal
// closes the gap. Death is verified through the pid-reuse-guarded
// liveness check (a zombie counts as dead).
//
// R3N-F1: the creation identity (pid + started-at) is verified BEFORE
// ANY destructive signal. The old code signaled first and checked never:
// an exited adopted process whose pid was reused got the REPLACEMENT
// process killed. On an identity mismatch termination aborts TYPED with
// ErrAgentIdentityMismatch and never signals; the SIGKILL escalation is
// guarded by the SAME check, because the pid can be reused inside the
// SIGTERM wait window too. Only a provably-gone or zombie pid (no live
// innocent occupant) lets the residual group collection proceed.
func (a *adoptedAgent) Terminate(timeout time.Duration) error {
	if a.pid <= 0 {
		return nil
	}
	if err := a.guardIdentityBeforeSignal(); err != nil {
		return err
	}
	_ = syscall.Kill(-a.pid, syscall.SIGTERM) // group-wide when pid == pgid
	_ = syscall.Kill(a.pid, syscall.SIGTERM)  // direct: the non-leader case
	if !waitForAdoptedDead(a.pid, a.startedAt, timeout) {
		// Escalation guard (R3N-F1): the leader may have exited and the pid
		// been reused while we waited — re-verify before SIGKILL.
		if err := a.guardIdentityBeforeSignal(); err != nil {
			return err
		}
		_ = syscall.Kill(-a.pid, syscall.SIGKILL)
		_ = syscall.Kill(a.pid, syscall.SIGKILL)
		if !waitForAdoptedDead(a.pid, a.startedAt, 5*time.Second) {
			return fmt.Errorf("daemon: adopted agent pid %d refused to die even after SIGKILL", a.pid)
		}
	}
	return nil
}

// guardIdentityBeforeSignal verifies that the pid still belongs to the
// RECORDED process before any destructive signal (R3N-F1): alive-and-ours
// passes; a live process with a mismatched creation time aborts typed
// (pid reuse — never signal); a gone or zombie pid has no innocent live
// occupant, so the residual group collection may proceed.
//
// R4N-5: only ESRCH PROVES the pid gone. A kill(pid,0) probe answering
// EPERM proves a process EXISTS (one this daemon may not signal with 0);
// any other error leaves the pid's occupancy unverifiable. The old code
// treated every non-nil probe error as "gone" and let the group signal
// proceed — an EPERM-occupied pid could have its whole (unrelated) group
// killed. Both cases now fail closed: typed identity abort, nothing
// signaled.
func (a *adoptedAgent) guardIdentityBeforeSignal() error {
	if processAlive(a.pid, a.startedAt) {
		return nil // ours and alive: safe to signal
	}
	// A corpse is only provably OURS when its creation time matches the
	// record (R3N-F1/R3N-F3): a foreign zombie that reused the pid pins a
	// foreign group id, and its members must not be signaled either.
	probe := killProbe(a.pid, 0)
	switch {
	case probe == nil:
		if !(procZombie(a.pid) && procCreationMatches(a.pid, a.startedAt)) {
			return fmt.Errorf("%w: pid %d is occupied by an unrelated process; no signal was sent", ErrAgentIdentityMismatch, a.pid)
		}
		// Our reapable corpse: the group id is still provably ours.
		return nil
	case errors.Is(probe, syscall.ESRCH):
		// Provably gone: no innocent replacement occupies the pid — the
		// group id can only hold our agent's orphans now.
		return nil
	default:
		// EPERM (or anything else): occupied or unverifiable — the
		// identity cannot be proven, so nothing may be signaled.
		return fmt.Errorf("%w: pid %d probe answered %v — occupied or unverifiable, identity cannot be proven, no signal was sent", ErrAgentIdentityMismatch, a.pid, probe)
	}
}

// waitForAdoptedDead polls until the adopted pid is gone (or a reapable
// zombie) AND no group bearing its id holds live members. The group
// check is skipped when the pid already leads no group.
//
// R3N-F2: an UNVERIFIABLE group is NEVER success. The old code returned
// true when the leader was dead but the group's emptiness could not be
// proven — converting unprovable into success and letting Terminate
// report a collected group it never saw. Unverifiable now fails closed:
// false, and the typed error is logged.
func waitForAdoptedDead(pid int, startedAt string, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	loggedUnprovable := false
	for {
		leaderDead := !processAlive(pid, startedAt)
		empty, err := ProcessGroupEmpty(pid)
		if err != nil {
			// R3N-F2: unprovable is NEVER success — but it may be TRANSIENT
			// (a member exiting mid-scan makes one entry unreadable), so
			// keep polling within the bound; if it persists to the deadline
			// the wait fails closed exactly like a survivor would.
			if !loggedUnprovable {
				fmt.Fprintf(os.Stderr, "pf-daemon: adopted group %d emptiness unverifiable; refusing to conclude death until proven (fail closed): %v\n", pid, err)
				loggedUnprovable = true
			}
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if leaderDead && empty {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The control-message face is gone after a restart (see the package comment):
// every call fails typed, loudly, never silently.

func (a *adoptedAgent) NotifyStop(ctx context.Context) error {
	return fmt.Errorf("%w: pf/stop_notice undeliverable for agent %d", ErrControlChannelLost, a.pid)
}

func (a *adoptedAgent) Quiesce(ctx context.Context) error {
	return fmt.Errorf("%w: pf/quiesce undeliverable for agent %d", ErrControlChannelLost, a.pid)
}

func (a *adoptedAgent) Resume(ctx context.Context) error {
	return fmt.Errorf("%w: pf/resume undeliverable for agent %d", ErrControlChannelLost, a.pid)
}

func (a *adoptedAgent) DeliverAnswer(relay AnswerRelay) error {
	return fmt.Errorf("%w: pf/answer undeliverable for agent %d", ErrControlChannelLost, a.pid)
}
