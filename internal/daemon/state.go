// Package daemon implements the pf-daemon core (04-local-runtime.md): the
// Daemon state file, the job queue, the Interactive/Batch/Drain mode
// admission, the observe event ring, the Unix-domain-socket operations
// face (04 §5.1), the claim→capsule→WI-Agent sequence (04 §6.1), the
// graceful shutdown sequence (00 §3.5 / 04 §6.5) and crash recovery
// (04 §7.1).
package daemon

// state.go: daemon/state.json — pure BOOKKEEPING (INV-D1): which jobs this
// machine manages, each job's persistent WI Agent pid/harness/session id,
// the current mode, plus the pending_jobs[] claim-intent journal (04 §6.1
// steps 4–10). It deliberately records NO workflow execution state: the only
// execution authority is the capsule's runtime-state/execution.json (INV-1),
// and this file is invisible to subagents (04 §4.1).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Job is one jobs[] entry (04 §4.1).
type Job struct {
	WorkItemID         string `json:"work_item_id"`
	CapsuleID          string `json:"capsule_id"`
	Generation         int64  `json:"generation"`
	HarnessAdapter     string `json:"harness_adapter"`
	HarnessSessionID   string `json:"harness_session_id"`
	MainAgentPID       int    `json:"main_agent_pid"`
	MainAgentStartedAt string `json:"main_agent_started_at"`
	// Status: running | interrupting | sealing (04 §4.1).
	Status string `json:"status"`
}

// Job statuses.
const (
	JobRunning      = "running"
	JobInterrupting = "interrupting"
	JobSealing      = "sealing"
)

// PendingJob is one claim-intent journal entry (04 §6.1 step 4/5/7): the
// irreversible-action-precedes-persistence record. status progresses
// intent_persisted → claim_confirmed → session_persisted; the entry is
// deleted once the formal jobs[] record exists (step 10).
type PendingJob struct {
	WorkItemID       string `json:"work_item_id"`
	IdempotencyKey   string `json:"idempotency_key"`
	Intent           string `json:"intent"` // claim | resume | takeover
	CreatedAt        string `json:"created_at"`
	Status           string `json:"status"`
	Generation       int64  `json:"generation,omitempty"`
	GenerationToken  string `json:"generation_token,omitempty"`
	HarnessAdapter   string `json:"harness_adapter,omitempty"`
	HarnessSessionID string `json:"harness_session_id,omitempty"`
}

// Pending statuses (04 §6.1).
const (
	PendingIntentPersisted  = "intent_persisted"
	PendingClaimConfirmed   = "claim_confirmed"
	PendingSessionPersisted = "session_persisted"
)

// State is the whole daemon/state.json (04 §4.1).
type State struct {
	MachineID     string       `json:"machine_id"`
	Mode          string       `json:"mode"` // interactive | batch | drain
	Version       string       `json:"version,omitempty"`
	WorkspaceRoot string       `json:"workspace_root"`
	StartedAt     string       `json:"started_at"`
	Jobs          []Job        `json:"jobs"`
	PendingJobs   []PendingJob `json:"pending_jobs,omitempty"`
}

// StateStore reads/writes state.json with tmp+fsync+rename (the same atomic
// write contract as capsule state files, 04 §6.1 preamble).
type StateStore struct {
	Path string
}

// NewStateStore creates a store at <dataDir>/state.json.
func NewStateStore(dataDir string) *StateStore {
	return &StateStore{Path: filepath.Join(dataDir, "state.json")}
}

// Load reads the state file; a missing file yields a zero State (fresh
// daemon) so crash recovery can treat "no jobs" as the empty set.
func (s *StateStore) Load() (*State, error) {
	raw, err := os.ReadFile(s.Path)
	if os.IsNotExist(err) {
		return &State{Jobs: []Job{}, PendingJobs: []PendingJob{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if st.Jobs == nil {
		st.Jobs = []Job{}
	}
	if st.PendingJobs == nil {
		st.PendingJobs = []PendingJob{}
	}
	return &st, nil
}

// Save atomically persists the whole state.
func (s *StateStore) Save(st *State) error {
	blob, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(blob); werr != nil {
		f.Close()
		os.Remove(tmp)
		return werr
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		os.Remove(tmp)
		return serr
	}
	if cerr := f.Close(); cerr != nil {
		os.Remove(tmp)
		return cerr
	}
	return os.Rename(tmp, s.Path)
}

// FindJob returns the jobs[] entry for a WI, if any.
func (st *State) FindJob(wi string) *Job {
	for i := range st.Jobs {
		if st.Jobs[i].WorkItemID == wi {
			return &st.Jobs[i]
		}
	}
	return nil
}

// UpsertJob inserts or replaces a jobs[] entry and persists.
func (s *StateStore) UpsertJob(st *State, j Job) error {
	for i := range st.Jobs {
		if st.Jobs[i].WorkItemID == j.WorkItemID {
			st.Jobs[i] = j
			return s.Save(st)
		}
	}
	st.Jobs = append(st.Jobs, j)
	return s.Save(st)
}

// RemoveJob drops a jobs[] entry and persists.
func (s *StateStore) RemoveJob(st *State, wi string) error {
	out := st.Jobs[:0]
	for _, j := range st.Jobs {
		if j.WorkItemID != wi {
			out = append(out, j)
		}
	}
	st.Jobs = out
	return s.Save(st)
}

// AppendPending appends a pending_jobs[] entry and persists.
func (s *StateStore) AppendPending(st *State, p PendingJob) error {
	st.PendingJobs = append(st.PendingJobs, p)
	return s.Save(st)
}

// UpdatePending replaces the pending entry for a WI (by idempotency key)
// and persists.
func (s *StateStore) UpdatePending(st *State, p PendingJob) error {
	for i := range st.PendingJobs {
		if st.PendingJobs[i].WorkItemID == p.WorkItemID && st.PendingJobs[i].IdempotencyKey == p.IdempotencyKey {
			st.PendingJobs[i] = p
			return s.Save(st)
		}
	}
	st.PendingJobs = append(st.PendingJobs, p)
	return s.Save(st)
}

// RemovePending drops the pending entry for a WI and persists.
func (s *StateStore) RemovePending(st *State, wi, idemKey string) error {
	out := st.PendingJobs[:0]
	for _, p := range st.PendingJobs {
		if p.WorkItemID != wi || (idemKey != "" && p.IdempotencyKey != idemKey) {
			out = append(out, p)
		}
	}
	st.PendingJobs = out
	return s.Save(st)
}

// NowStamp is the shared RFC3339Nano UTC stamp.
func NowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
