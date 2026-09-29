// state.go: the per-WI state machine (v1-spec-full §三) plus the Runtime's
// own bookkeeping file (state.json). Daemon state is BOOKKEEPING ONLY —
// workflow execution progress lives in the capsule's
// runtime-state/execution.json and is never mirrored here.
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// JobState is one per-WI Runtime state.
type JobState string

// The v1 state machine:
//
//	RUNNING → SEALING → DONE                  (stop path)
//	RUNNING → RECOVERING → RUNNING            (recovery succeeded)
//	RUNNING → RECOVERING → DONE               (bounded fail, storm guard)
//
// Every other edge is illegal and transition() refuses it.
const (
	StateRunning    JobState = "running"
	StateRecovering JobState = "recovering"
	StateSealing    JobState = "sealing"
	StateDone       JobState = "done"
)

var legalEdges = map[JobState][]JobState{
	StateRunning:    {StateSealing, StateRecovering},
	StateSealing:    {StateDone},
	StateRecovering: {StateRunning, StateDone},
}

// can reports whether s → to is a legal edge of the state machine.
func (s JobState) can(to JobState) bool {
	for _, n := range legalEdges[s] {
		if n == to {
			return true
		}
	}
	return false
}

// transition moves the job's state along one legal edge. Illegal edges are
// refused with an error naming both states — the fencing rule (SEALING
// drops same-attempt WINeedsRecovery, RECOVERING ignores duplicates) rides
// on these refusals.
func (j *job) transition(to JobState) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.state.can(to) {
		return fmt.Errorf("runtime: %s: illegal transition %s → %s", j.wi, j.state, to)
	}
	j.state = to
	return nil
}

func (j *job) snapshot() (state JobState, instance string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state, j.instance
}

// liveAgent reports whether this job currently has a live agent handle
// under supervision (the Restart skip condition for records already
// running under this daemon).
func (j *job) liveAgent() bool {
	j.mu.Lock()
	h := j.agent
	j.mu.Unlock()
	return h != nil && h.Alive()
}

// newInstanceLocked mints the agent-lineage id that fences RT/OB messages
// (protocol.go attempt-id): the first agent instance carries the aihub
// claim's attempt id verbatim; every re-derivation appends #N so Observe's
// stale death signals for a replaced handle are dropped by the fence.
// Caller holds j.mu.
func (j *job) newInstanceLocked() string {
	if j.spawnN <= 1 {
		return j.attempt
	}
	return fmt.Sprintf("%s#%d", j.attempt, j.spawnN-1)
}

// ---------------------------------------------------------------------------
// state.json — the Runtime's persisted bookkeeping (restart input).

const stateFilePerm = 0o600

type stateFile struct {
	Version   int          `json:"version"`
	MachineID string       `json:"machine_id"`
	Jobs      []*jobRecord `json:"jobs"`
}

type jobRecord struct {
	WorkItemID   string `json:"work_item_id"`
	Project      string `json:"project"`
	AttemptID    string `json:"attempt_id"`
	InstanceID   string `json:"instance_id"`
	Generation   int64  `json:"generation"`
	State        string `json:"state"`
	StopReason   string `json:"stop_reason,omitempty"`
	CapsuleDir   string `json:"capsule_dir"`
	GitDir       string `json:"git_dir"`
	RemoteURL    string `json:"remote_url"`
	Username     string `json:"username"`
	Secret       string `json:"secret"` // attempt-scoped push credential
	SessionID    string `json:"session_id"`
	Transcript   string `json:"transcript"`
	BundleName   string `json:"bundle_name"`
	BundleDigest string `json:"bundle_digest"`
	AgentPID     int    `json:"agent_pid"`
	AgentStart   string `json:"agent_started_at"`
	SpawnN       int    `json:"spawn_n"`
	RecoverFails int    `json:"recover_fails"` // consecutive failed recoveries
	ResumeFails  int    `json:"resume_fails"`
	ResumeDead   bool   `json:"resume_dead"`
}

func (j *job) record() *jobRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return &jobRecord{
		WorkItemID:   j.wi,
		Project:      j.project,
		AttemptID:    j.attempt,
		InstanceID:   j.instance,
		Generation:   j.generation,
		State:        string(j.state),
		StopReason:   j.stopReason,
		CapsuleDir:   j.capsuleDir,
		GitDir:       j.git.GitDir,
		RemoteURL:    j.git.RemoteURL,
		Username:     j.git.Username,
		Secret:       j.git.Secret,
		SessionID:    j.sessionID,
		Transcript:   j.transcript,
		BundleName:   j.bundleName,
		BundleDigest: j.bundleDigest,
		AgentPID:     j.agentPID,
		AgentStart:   j.agentStartedAt,
		SpawnN:       j.spawnN,
		RecoverFails: j.recoverFails,
		ResumeFails:  j.resumeFails,
		ResumeDead:   j.resumeDead,
	}
}

func jobFromRecord(rec *jobRecord) *job {
	return &job{
		wi:         rec.WorkItemID,
		project:    rec.Project,
		attempt:    rec.AttemptID,
		instance:   rec.InstanceID,
		generation: rec.Generation,
		state:      JobState(rec.State),
		stopReason: rec.StopReason,
		capsuleDir: rec.CapsuleDir,
		git: gitops.Opts{
			WorkItemID: rec.WorkItemID,
			AttemptID:  rec.AttemptID,
			CapsuleDir: rec.CapsuleDir,
			GitDir:     rec.GitDir,
			RemoteURL:  rec.RemoteURL,
			Username:   rec.Username,
			Secret:     rec.Secret,
		},
		sessionID:      rec.SessionID,
		transcript:     rec.Transcript,
		bundleName:     rec.BundleName,
		bundleDigest:   rec.BundleDigest,
		agentPID:       rec.AgentPID,
		agentStartedAt: rec.AgentStart,
		spawnN:         rec.SpawnN,
		recoverFails:   rec.RecoverFails,
		resumeFails:    rec.ResumeFails,
		resumeDead:     rec.ResumeDead,
	}
}

// loadState reads state.json; a missing file is an empty state (first
// boot), an unparsable one is a hard error (never guess bookkeeping).
func loadState(path string) (*stateFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &stateFile{Version: 1}, nil
		}
		return nil, err
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("runtime: state file unparsable: %w", err)
	}
	return &st, nil
}

// atomicWrite lands data at path via tmp+fsync+rename (the same contract
// every daemon-side state file uses).
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
