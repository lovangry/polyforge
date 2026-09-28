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
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	// BundleName/BundleDigest carry the WI's immutable bundle reference
	// (02 §4.4) into the persisted record, so the crash-recovery
	// re-derivation can run the materialization trust gate (03 §4.5/§7)
	// WITHOUT any data-plane read: the pinned identity is already local.
	// Absent on pre-upgrade rows — those fail closed on the gate and are
	// left for human attention rather than silently re-derived.
	BundleName   string `json:"bundle_name,omitempty"`
	BundleDigest string `json:"bundle_digest,omitempty"`
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
	// CapsuleBootstrap is the RAW capsule_bootstrap JSON the Core returned
	// with the claim/force-takeover response, journaled at claim_confirmed
	// so a takeover re-run that must NOT re-call ForceTakeover (02 §6.2:
	// re-calling force-takeover ends the just-opened generation and opens
	// ANOTHER one — generation churn) still holds every input the spawn
	// sequence needs (02 §8: the bootstrap is the only data-plane read).
	CapsuleBootstrap string `json:"capsule_bootstrap,omitempty"`
	// MainAgentPID/MainAgentStartedAt journal the derived process identity
	// at spawn time (R3N-F6): the spawner's OnProcessStarted callback
	// writes them into this row BEFORE Spawn returns, so a crash between
	// the derivation and the formal jobs[] record (step 10) leaves a
	// DISCOVERABLE orphan — the next boot checks the recorded identity
	// with the 04 §7.1 pid-reuse guard and ADOPTS the live agent instead of
	// re-spawning a second one over it. They describe the CURRENT attempt
	// only: a re-run of the spawn sequence clears any stale values first.
	// Absent (pre-upgrade rows, or a crash inside the spawn prologue): no
	// discovery is possible — 03 §4.4 records only {session_id, adapter,
	// created_at} in harness/<adapter>/session.json, no pid — and the
	// orphan requeue re-spawns as before.
	MainAgentPID       int    `json:"main_agent_pid,omitempty"`
	MainAgentStartedAt string `json:"main_agent_started_at,omitempty"`
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
// write contract as capsule state files, 04 §6.1 preamble). R3N-F8: every
// mutation+persist pair is serialized by a store-level mutex and every
// save writes a UNIQUE tmp name, so a concurrent mode change and job
// bookkeeping write can never interleave a half-mutated State into a
// marshal or collide on a shared state.json.tmp.
type StateStore struct {
	Path string
	mu   sync.Mutex
}

// NewStateStore creates a store at <dataDir>/state.json.
func NewStateStore(dataDir string) *StateStore {
	return &StateStore{Path: filepath.Join(dataDir, "state.json")}
}

// Load reads the state file; a missing file yields a zero State (fresh
// daemon) so crash recovery can treat "no jobs" as the empty set. A file
// that EXISTS but is semantically broken is rejected LOUDLY: this file is
// the input crash recovery re-derives live agents from, and silently
// accepting a malformed record (empty ids, unknown status, duplicate WI,
// generation 0) would mis-adopt or double-adopt jobs — reject and let the
// operator look at the file instead of "recovering" from garbage.
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
	if err := st.validate(); err != nil {
		return nil, fmt.Errorf("daemon: %s is invalid: %w", s.Path, err)
	}
	if st.Jobs == nil {
		st.Jobs = []Job{}
	}
	if st.PendingJobs == nil {
		st.PendingJobs = []PendingJob{}
	}
	return &st, nil
}

// StateHeader is the state-level identity a daemon instance injects when
// it creates or adopts a state file (04 §4.1 top-level fields / §11
// backward-compatible read of an older format).
type StateHeader struct {
	MachineID     string
	Mode          string
	WorkspaceRoot string
	StartedAt     string
}

// LoadOrAdopt loads state.json the way the daemon does at startup: a
// missing file yields a FRESH state carrying the header (persisted
// immediately — the file is the crash-recovery input, 04 §4.1); an
// existing file written by an older build may lack the state-level header
// fields, and 04 §11 pins that as a backward-COMPATIBLE read ("旧格式文件
// 被识别为「无该字段」而非解析失败"), so the empty header fields are
// BACKFILLED and persisted BEFORE the strict validation below runs — an
// upgrade never bricks the daemon on its own previous state file.
func (s *StateStore) LoadOrAdopt(h StateHeader) (*State, error) {
	raw, err := os.ReadFile(s.Path)
	if os.IsNotExist(err) {
		st := &State{
			MachineID: h.MachineID, Mode: h.Mode,
			WorkspaceRoot: h.WorkspaceRoot, StartedAt: h.StartedAt,
			Jobs: []Job{}, PendingJobs: []PendingJob{},
		}
		if err := st.validate(); err != nil {
			return nil, err
		}
		if err := s.Save(st); err != nil {
			return nil, err
		}
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	changed := false
	if st.MachineID == "" && h.MachineID != "" {
		st.MachineID = h.MachineID
		changed = true
	}
	if st.Mode == "" && h.Mode != "" {
		st.Mode = h.Mode
		changed = true
	}
	if st.WorkspaceRoot == "" && h.WorkspaceRoot != "" {
		st.WorkspaceRoot = h.WorkspaceRoot
		changed = true
	}
	if st.StartedAt == "" && h.StartedAt != "" {
		st.StartedAt = h.StartedAt
		changed = true
	}
	if changed {
		if err := s.Save(&st); err != nil {
			return nil, err
		}
	}
	if err := st.validate(); err != nil {
		return nil, fmt.Errorf("daemon: %s is invalid: %w", s.Path, err)
	}
	if st.Jobs == nil {
		st.Jobs = []Job{}
	}
	if st.PendingJobs == nil {
		st.PendingJobs = []PendingJob{}
	}
	return &st, nil
}

// validate applies the semantic contract of 04 §4.1 on load: state-level
// required fields (machine_id/mode/workspace_root/started_at), required
// fields per record, unique work_item_id per jobs[] entry AND per
// pending_jobs[] journal (the journal is keyed per WI: UpdatePending
// replaces in place, so two rows for one WI can only be corruption),
// statuses inside their enums, generation >= 1, and pending/jobs
// generation consistency for the same WI (R2N-8: a journal row that
// already holds a confirmed generation must agree with the formal jobs[]
// record — a mismatch would double-adopt the WI into two generations).
func (st *State) validate() error {
	switch {
	case st.MachineID == "":
		return fmt.Errorf("state: empty machine_id (04 §4.1)")
	case st.Mode == "":
		return fmt.Errorf("state: empty mode (04 §4.1)")
	case st.Mode != ModeInteractive && st.Mode != ModeBatch && st.Mode != ModeDrain:
		return fmt.Errorf("state: unknown mode %q", st.Mode)
	case st.WorkspaceRoot == "":
		return fmt.Errorf("state: empty workspace_root (04 §4.1)")
	case st.StartedAt == "":
		return fmt.Errorf("state: empty started_at (04 §4.1)")
	}
	seen := map[string]bool{}
	for i, j := range st.Jobs {
		where := fmt.Sprintf("jobs[%d] (%s)", i, j.WorkItemID)
		switch {
		case j.WorkItemID == "":
			return fmt.Errorf("%s: empty work_item_id", where)
		case seen[j.WorkItemID]:
			return fmt.Errorf("%s: duplicate work_item_id %s (one jobs[] entry per WI)", where, j.WorkItemID)
		case j.CapsuleID == "":
			return fmt.Errorf("%s: empty capsule_id", where)
		case j.Generation < 1:
			return fmt.Errorf("%s: generation must be >= 1, got %d", where, j.Generation)
		case j.HarnessAdapter == "":
			return fmt.Errorf("%s: empty harness_adapter", where)
		case j.HarnessSessionID == "":
			return fmt.Errorf("%s: empty harness_session_id", where)
		case j.MainAgentPID < 1:
			return fmt.Errorf("%s: main_agent_pid must be >= 1, got %d", where, j.MainAgentPID)
		case j.MainAgentStartedAt == "":
			return fmt.Errorf("%s: empty main_agent_started_at", where)
		case j.Status != JobRunning && j.Status != JobInterrupting && j.Status != JobSealing:
			return fmt.Errorf("%s: unknown status %q", where, j.Status)
		}
		seen[j.WorkItemID] = true
	}
	pendingSeen := map[string]bool{}
	for i, p := range st.PendingJobs {
		where := fmt.Sprintf("pending_jobs[%d] (%s)", i, p.WorkItemID)
		switch {
		case p.WorkItemID == "":
			return fmt.Errorf("%s: empty work_item_id", where)
		case pendingSeen[p.WorkItemID]:
			// The journal is keyed per WI (UpdatePending/AppendPending
			// SUPERSEDE any existing row for the WI, R3N-F7 — a key change
			// on retry is expected, so two rows for one WI can only be
			// external corruption), and a crash-recovery orphan sweep would
			// requeue the WI twice.
			return fmt.Errorf("%s: duplicate pending intent for %s (one journal row per WI)", where, p.WorkItemID)
		case p.IdempotencyKey == "":
			return fmt.Errorf("%s: empty idempotency_key", where)
		case p.Intent != "claim" && p.Intent != "resume" && p.Intent != "takeover":
			return fmt.Errorf("%s: unknown intent %q", where, p.Intent)
		case p.CreatedAt == "":
			return fmt.Errorf("%s: empty created_at", where)
		case p.Status != PendingIntentPersisted && p.Status != PendingClaimConfirmed && p.Status != PendingSessionPersisted:
			return fmt.Errorf("%s: unknown status %q", where, p.Status)
		case p.Status != PendingIntentPersisted && p.Generation < 1:
			return fmt.Errorf("%s: status %s requires generation >= 1, got %d", where, p.Status, p.Generation)
		case p.Status != PendingIntentPersisted && p.GenerationToken == "":
			return fmt.Errorf("%s: status %s requires the generation_token", where, p.Status)
		case p.MainAgentPID < 0:
			return fmt.Errorf("%s: main_agent_pid must be >= 0, got %d", where, p.MainAgentPID)
		case p.MainAgentPID >= 1 && p.MainAgentStartedAt == "":
			// R3N-F6: a journaled pid without its creation started-at cannot run
			// the 04 §7.1 pid-reuse guard — it would adopt whatever happens to
			// occupy the pid. The identity pair is written atomically or not at
			// all.
			return fmt.Errorf("%s: journaled main_agent_pid %d requires main_agent_started_at", where, p.MainAgentPID)
		}
		pendingSeen[p.WorkItemID] = true
		// Pending/jobs generation consistency for the same WI: both rows
		// coexist only in the crash window between the claim and the
		// formal jobs[] record — they must describe the SAME generation,
		// or recovery would requeue the WI into a different generation
		// than the one the jobs[] row re-adopts.
		if j := st.FindJob(p.WorkItemID); j != nil && p.Generation != 0 && p.Generation != j.Generation {
			return fmt.Errorf("%s: pending generation %d disagrees with jobs[] generation %d for %s",
				where, p.Generation, j.Generation, p.WorkItemID)
		}
	}
	return nil
}

// Save atomically persists the whole state. The mutation+persist pairs
// above hold the store mutex; Save takes it too, so a bare Save (the
// LoadOrAdopt backfill path) can never interleave with them.
func (s *StateStore) Save(st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(st)
}

// ApplyMode persists a mode transition as ONE atomic mutation+persist
// step (04 §6.7): the state file's mode field is the AUTHORITATIVE
// record, the in-memory ModeCtl copy is a fast-path cache for admission.
// On a persistence failure the mutation is rolled back INSIDE the lock,
// so the visible state never advertises a mode the state file does not
// hold (R3N-F8: the old code mutated first and left the NEW mode active
// when Save failed).
func (s *StateStore) ApplyMode(st *State, mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := st.Mode
	st.Mode = mode
	if err := s.saveLocked(st); err != nil {
		st.Mode = prev
		return err
	}
	return nil
}

// saveLocked persists st under the store mutex: a UNIQUE tmp file per
// save (R3N-F8 — concurrent or crashed-over saves can never collide on a
// shared state.json.tmp), fsync, rename over the canonical path.
func (s *StateStore) saveLocked(st *State) error {
	blob, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, werr := tmp.Write(blob); werr != nil {
		tmp.Close()
		os.Remove(name)
		return werr
	}
	if serr := tmp.Sync(); serr != nil {
		tmp.Close()
		os.Remove(name)
		return serr
	}
	if cerr := tmp.Close(); cerr != nil {
		os.Remove(name)
		return cerr
	}
	if rerr := os.Rename(name, s.Path); rerr != nil {
		os.Remove(name)
		return rerr
	}
	return nil
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

// UpsertJob inserts or replaces a jobs[] entry and persists. The mutation
// and the persist hold the store mutex as ONE step (R3N-F8).
func (s *StateStore) UpsertJob(st *State, j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range st.Jobs {
		if st.Jobs[i].WorkItemID == j.WorkItemID {
			st.Jobs[i] = j
			return s.saveLocked(st)
		}
	}
	st.Jobs = append(st.Jobs, j)
	return s.saveLocked(st)
}

// RemoveJob drops a jobs[] entry and persists (mutex-held, R3N-F8).
func (s *StateStore) RemoveJob(st *State, wi string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := st.Jobs[:0]
	for _, j := range st.Jobs {
		if j.WorkItemID != wi {
			out = append(out, j)
		}
	}
	st.Jobs = out
	return s.saveLocked(st)
}

// FindPending returns the pending_jobs[] entry for a WI + idempotency key,
// if any — the journal row a crash-recovery requeue continues from.
func (st *State) FindPending(wi, idemKey string) *PendingJob {
	for i := range st.PendingJobs {
		if st.PendingJobs[i].WorkItemID == wi && (idemKey == "" || st.PendingJobs[i].IdempotencyKey == idemKey) {
			return &st.PendingJobs[i]
		}
	}
	return nil
}

// AppendPending appends a pending_jobs[] entry and persists. Like
// UpdatePending it keeps the one-row-per-WI invariant (R3N-F7): an
// existing row for the same WI is superseded, never duplicated. The two
// entry points are behaviorally IDENTICAL (the historical append was
// folded into UpdatePending's supersede semantics); this one stays as
// an alias for its callers and delegates WITHOUT taking the mutex
// first — UpdatePending holds it.
func (s *StateStore) AppendPending(st *State, p PendingJob) error {
	return s.UpdatePending(st, p)
}

// UpdatePending replaces the pending entry for a WI and persists.
//
// R3N-F7: the replacement is keyed by WORK ITEM, not by (WI, idempotency
// key). The journal is one row per WI admission — 04 §6.1 speaks of
// "一条记录" whose fields are WRITTEN BACK as the claim progresses
// (steps 5/7: "回写到同一条 pending_jobs[] 记录"), and the idempotency
// key is a FIELD of that row, not its identity: a retry is EXPECTED to
// change the key (a fresh takeover after a failed spawn mints a new one).
// The old WI+key match let a spawn-failure leftover row plus a fresh
// takeover's new-key row COEXIST — two valid-looking rows for one WI
// that validation (correctly) rejects, so the next crash left the daemon
// unable to load its own state. Superseding any existing row for the WI
// keeps the one-row-per-WI invariant the validator enforces.
func (s *StateStore) UpdatePending(st *State, p PendingJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range st.PendingJobs {
		if st.PendingJobs[i].WorkItemID == p.WorkItemID {
			st.PendingJobs[i] = p // supersede: a key change on retry is expected
			return s.saveLocked(st)
		}
	}
	st.PendingJobs = append(st.PendingJobs, p)
	return s.saveLocked(st)
}

// RemovePending drops the pending entry for a WI and persists.
func (s *StateStore) RemovePending(st *State, wi, idemKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := st.PendingJobs[:0]
	for _, p := range st.PendingJobs {
		if p.WorkItemID != wi || (idemKey != "" && p.IdempotencyKey != idemKey) {
			out = append(out, p)
		}
	}
	st.PendingJobs = out
	return s.saveLocked(st)
}

// NowStamp is the shared RFC3339Nano UTC stamp.
func NowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
