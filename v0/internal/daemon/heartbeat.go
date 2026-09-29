package daemon

// heartbeat.go: stale-heartbeat exposure (04 §7.2/§9). The daemon beats for
// its own machine every 60s (RunHeartbeat); a machine whose last successful
// beat is older than a configurable threshold is listed in the daemon
// status output as a MONITORING SIGNAL ONLY — no auto-takeover is ever
// derived from it (takeover is a human decision per 02 §7, and the daemon
// side explicitly does not auto re-claim WIs held elsewhere, avoiding the
// 05 §2.9 takeover-window race).
//
// OBSERVABILITY SEAM (recorded honestly): in v1 the only machine whose
// heartbeat THIS daemon can observe is its own — the Core HTTP face (02
// §5.1) exposes no machines-enumeration endpoint, and cross-machine
// staleness is a Core-side monitoring-face metric (02 §7/§9: "stale
// Daemon/机器数量"). The tracker is nevertheless keyed per machine and
// fed through Record(), so when a machines face lands, the status output
// gains cross-machine coverage without changing this file's contract.

import (
	"sort"
	"sync"
	"time"
)

// DefaultHeartbeatInterval matches 04 §7.2 (60s cadence).
const DefaultHeartbeatInterval = 60 * time.Second

// DefaultStaleHeartbeatThreshold is three missed beats (04 §7.2: "heartbeat
// stopped past a threshold" — no canonical value is pinned; three
// consecutive missed beats is the conservative v1 default).
const DefaultStaleHeartbeatThreshold = 3 * DefaultHeartbeatInterval

// StaleMachine is one status-output row of the stale-heartbeat list.
type StaleMachine struct {
	MachineID       string `json:"machine_id"`
	LastHeartbeatAt string `json:"last_heartbeat_at"`
	AgeSeconds      int64  `json:"age_seconds"`
}

// BeatTracker records the last successful heartbeat per observed machine
// and derives the stale list. Only successful beats advance a machine's
// clock: a machine whose beats keep failing ages out exactly like one that
// stopped beating, which is the signal we want.
type BeatTracker struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// NewBeatTracker creates the tracker.
func NewBeatTracker() *BeatTracker {
	return &BeatTracker{last: map[string]time.Time{}}
}

// Record records one successful heartbeat for machineID at time at.
func (t *BeatTracker) Record(machineID string, at time.Time) {
	if machineID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last[machineID] = at
}

// Stale lists every observed machine whose last successful heartbeat is
// older than threshold at time now, ordered by machine id for stable
// output. Machines never observed are not listed (unknown is not stale).
func (t *BeatTracker) Stale(now time.Time, threshold time.Duration) []StaleMachine {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []StaleMachine{}
	for id, at := range t.last {
		age := now.Sub(at)
		if age > threshold {
			out = append(out, StaleMachine{
				MachineID:       id,
				LastHeartbeatAt: at.UTC().Format(time.RFC3339Nano),
				AgeSeconds:      int64(age.Seconds()),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MachineID < out[j].MachineID })
	return out
}
