package daemon

import "time"

// stalledRow reports whether a status row reads `stalled` (polyforge#42):
// stub, filled in with the implementation.
func stalledRow(status string, agentPID int, inDispatch bool, stepAge, window time.Duration) bool {
	return false
}
