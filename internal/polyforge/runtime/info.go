package runtime

// info.go: the exported per-job READ face the phase-3 daemon assembly
// needs (the only surface the daemon may use beside Start/Stop/Restart
// and the protocol channels): the local status projection (GET /status),
// the RT→Attach bridge's observe-attachment synthesis, the shutdown
// sequence's live-job enumeration and the stop_notice delivery. Purely
// additive: the state machine and every writer stay untouched.

import "github.com/silgrid/polyforge/internal/polyforge/gitops"

// JobInfo is a point-in-time snapshot of one locally managed job.
// Agent is the live supervised handle (nil when the job holds none —
// blocked after a bounded fail, or a record mid-seal). Everything else
// mirrors the persisted jobRecord fields (state.go), which is what the
// restart reconciliation and the daemon bookkeeping share.
type JobInfo struct {
	WorkItem       string
	Project        string
	AttemptID      string
	InstanceID     string // the protocol attempt-id fence (agent lineage)
	Generation     int64
	State          JobState
	StopReason     string
	CapsuleDir     string
	Git            gitops.Opts
	SessionID      string
	Transcript     string
	BundleName     string
	Agent          AgentHandle
	AgentPID       int
	AgentStartedAt string
}

// Info returns the snapshot of one job; ok=false when the work item has
// no locally managed job (never claimed here, or already sealed and
// dropped).
func (r *Runtime) Info(wi string) (JobInfo, bool) {
	j := r.jobOf(wi)
	if j == nil {
		return JobInfo{}, false
	}
	return j.info(), true
}

// List returns a snapshot of every locally managed job.
func (r *Runtime) List() []JobInfo {
	r.mu.Lock()
	js := make([]*job, 0, len(r.jobs))
	for _, j := range r.jobs {
		js = append(js, j)
	}
	r.mu.Unlock()
	out := make([]JobInfo, 0, len(js))
	for _, j := range js {
		out = append(out, j.info())
	}
	return out
}

func (j *job) info() JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	return JobInfo{
		WorkItem:       j.wi,
		Project:        j.project,
		AttemptID:      j.attempt,
		InstanceID:     j.instance,
		Generation:     j.generation,
		State:          j.state,
		StopReason:     j.stopReason,
		CapsuleDir:     j.capsuleDir,
		Git:            j.git,
		SessionID:      j.sessionID,
		Transcript:     j.transcript,
		BundleName:     j.bundleName,
		Agent:          j.agent,
		AgentPID:       j.agentPID,
		AgentStartedAt: j.agentStartedAt,
	}
}

// Persist rewrites state.json on demand. The daemon's graceful-shutdown
// sequence calls it as its final durability step (04 §6.5: the persisted
// bookkeeping must reflect reality before the process exits).
func (r *Runtime) Persist() error { return r.persist() }
