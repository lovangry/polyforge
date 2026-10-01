package runtime

// info.go: the exported per-job READ face the phase-3 daemon assembly
// needs (the only surface the daemon may use beside Start/Stop/Restart
// and the protocol channels): the local status projection (GET /status),
// the RT→Attach bridge's observe-attachment synthesis, the shutdown
// sequence's live-job enumeration and the stop_notice delivery. Purely
// additive: the state machine and every writer stay untouched.

import (
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// JobInfo is a point-in-time snapshot of one locally managed job.
// Agent is the live supervised handle (nil when the job holds none —
// blocked after a bounded fail, or a record mid-seal). Everything else
// mirrors the persisted jobRecord fields (state.go), which is what the
// restart reconciliation and the daemon bookkeeping share.
type JobInfo struct {
	WorkItem   string
	Project    string
	// SessionFingerprint is the PUBLIC session handle (B3); the secret
	// never leaves state.json and the one-shot askpass, but it is exposed
	// here for the daemon's in-process faces (the M4 revalidation seam
	// rides RTMsg, not this struct).
	SessionFingerprint string
	// InstanceID is the derivation fence token carried on the protocol
	// attempt-id field during the v1→v1.5 transition: the session
	// fingerprint, "#N"-suffixed after re-derivations.
	InstanceID string
	State      JobState
	StopReason string
	CapsuleDir string
	Git        gitops.Opts
	SessionID  string // the SECRET — in-process consumers only, never a log surface
	SpawnN     int
	Transcript string
	// LiveTranscript is the file the current derivation appends to (the
	// F3 live-transcript face; "" while not located). SpawnAt is its
	// agentStartedAt parsed once, for the observe lane's on-demand
	// live-transcript location.
	LiveTranscript string
	SpawnAt        time.Time
	BundleName     string
	Agent          AgentHandle
	AgentPID       int
	AgentStartedAt string
}

// Info returns the snapshot of one job; ok=false when the work item has
// no locally managed job (never claimed here, or already sealed and
// dropped).
func (r *Runtime) Info(wi string) (JobInfo, bool) {
	j := r.resolveJob(wi)
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
	spawnAt := time.Time{}
	if j.agentStartedAt != "" {
		if at, err := time.Parse(time.RFC3339Nano, j.agentStartedAt); err == nil {
			spawnAt = at
		}
	}
	return JobInfo{
		WorkItem:           j.wi,
		Project:            j.project,
		SessionFingerprint: j.sessionFingerprint,
		InstanceID:         j.instance,
		State:              j.state,
		StopReason:         j.stopReason,
		CapsuleDir:         j.capsuleDir,
		Git:                j.git,
		SessionID:          j.sessionID,
		SpawnN:             j.spawnN,
		Transcript:         j.transcript,
		LiveTranscript: j.liveTranscript,
		SpawnAt:        spawnAt,
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

// resolveJob answers the job behind any id form: the canonical key first,
// then the alias table — so daemon-side route lookups by the operator's
// submitted spelling (a slug, most often) find the live job without an
// aihub round-trip.
func (r *Runtime) resolveJob(wi string) *job {
	if j := r.jobOf(wi); j != nil {
		return j
	}
	r.mu.Lock()
	canonical := r.aliases[wi]
	r.mu.Unlock()
	if canonical == "" {
		return nil
	}
	return r.jobOf(canonical)
}
