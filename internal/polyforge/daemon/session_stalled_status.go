package daemon

import (
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/runtime"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// stalledRow reports whether a status row reads `stalled` (polyforge#42): a
// running job with no agent pid whose live dispatch has outlived the
// session_stall_window the supervisor judges by. Presentation only — never
// workflow state. A pending question (already presented as needs_input) and
// every closing state keep their own name; a negative window disables it.
func stalledRow(status string, agentPID int, inDispatch bool, stepAge, window time.Duration) bool {
	return status == string(runtime.StateRunning) && agentPID == 0 && inDispatch &&
		window > 0 && stepAge >= window
}

// sessionStallWindow is the window the status row shares with the verdict:
// the configured session_stall_window, zero taking the default.
func (d *Daemon) sessionStallWindow() time.Duration {
	return supervise.Defaults{SessionStall: d.cfg.Supervision.SessionStallWindow}.Fill().SessionStall
}
