// state.go: the per-WI state machine (v1-spec-full §三) plus the Runtime's
// own bookkeeping file (state.json). Daemon state is BOOKKEEPING ONLY —
// workflow execution progress lives in the capsule's
// runtime-state/execution.json and is never mirrored here.
//
// v1.5 (session model): the record carries the session identity — the
// SECRET session_id plus its public fingerprint — and the per-WI monotonic
// derivation counter spawn_n. The v1 attempt/generation/instance/credential
// fields are deleted; the JobState machine is UNCHANGED.
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// JobState is one per-WI Runtime state.
type JobState string

// The v1 state machine, plus the v1.5 review Finding 2 pause edges:
//
//	RUNNING → SEALING → DONE                  (stop path)
//	RUNNING → RECOVERING → RUNNING            (recovery succeeded)
//	RUNNING → RECOVERING → DONE               (bounded fail, storm guard)
//	RUNNING → PAUSED                          (operator pause close, Finding 2)
//	PAUSED → RUNNING                          (restart attach/resume, M3)
//	PAUSED → DONE                             (later close of a retained record)
//
// Every other edge is illegal and transition() refuses it.
const (
	StateRunning    JobState = "running"
	StateRecovering JobState = "recovering"
	StateSealing    JobState = "sealing"
	StateDone       JobState = "done"
	// StatePaused is the OPERATOR-pause retention state (v1.5 review
	// Finding 2): a pause close KILLS the tree and seals the dirty
	// capsule state server-side (SessionEnd paused, clean_pause=true)
	// but RETAINS the job record — the session secret survives locally
	// so the holder daemon's restart can attach (M3) once the operator
	// clears clean_pause. No read-only seal, no dropJob, no DONE.
	StatePaused JobState = "paused"
	// StateEndPending is the durable "blocked, but the hub has not heard"
	// state (v2-converge §6 bug 5): a bounded-fail whose SessionEnd(blocked)
	// failed keeps the record — the session secret only that record holds —
	// and retries the close (in-process with backoff, and at every daemon
	// start) until the hub accepts it. Only then is the record dropped.
	StateEndPending JobState = "end_pending"
)

var legalEdges = map[JobState][]JobState{
	StateRunning:    {StateSealing, StateRecovering, StatePaused},
	StateSealing:    {StateDone},
	StateRecovering: {StateRunning, StateSealing, StateDone, StateEndPending},
	StateEndPending: {StateDone},
	StatePaused:     {StateRunning, StateDone},
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
// drops same-fence WINeedsRecovery, RECOVERING ignores duplicates) rides
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

// snapshot answers the job's state plus the derivation counter the OB
// fence compares against (B2).
func (j *job) snapshot() (state JobState, spawn int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state, j.spawnN
}

// fingerprint answers the job's PUBLIC session handle (the NEW-1 fence
// payload OBSuperseded is compared against).
func (j *job) fingerprint() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.sessionFingerprint
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

// ---------------------------------------------------------------------------
// state.json — the Runtime's persisted bookkeeping (restart input).

const stateFilePerm = 0o600

type stateFile struct {
	Version   int          `json:"version"`
	MachineID string       `json:"machine_id"`
	Jobs      []*jobRecord `json:"jobs"`
}

// jobRecord is one persisted job (v1.5 contract §4.1): the session
// identity replaces the v1 attempt/generation/instance/credential
// lineage. The SECRET never leaves this file (0600) and the one-shot
// GIT_ASKPASS.
type jobRecord struct {
	WorkItemID string `json:"work_item_id"`
	Project    string `json:"project"`
	// SessionID is the v1.5 SECRET (B3): 64-hex, minted by this daemon
	// at claim, the git push password. It lives ONLY here and in the
	// one-shot GIT_ASKPASS — never in the capsule, never in agent env,
	// never in logs or events.
	SessionID string `json:"session_id"`
	// SessionFingerprint is the PUBLIC handle (hex(sha256(secret))[:16]):
	// the value logs, events and PF_SESSION_HANDLE carry.
	SessionFingerprint string `json:"session_fingerprint"`
	State              string `json:"state"`
	StopReason         string `json:"stop_reason,omitempty"`
	// SealedPush: the SEALING record's seal-knowledge (see job.sealedPush)
	// — the restart's stop finisher skips its own seal push when set.
	SealedPush bool   `json:"sealed_push,omitempty"`
	CapsuleDir string `json:"capsule_dir"`
	GitDir     string `json:"git_dir"`
	RemoteURL  string `json:"remote_url"`
	// CleanPause mirrors the hub's operator-pause flag (contract §4.1).
	// Since the v1.5 review Finding 2 an operator pause RETAINS the
	// record (StatePaused): the flag is latched true at the pause close
	// — exactly the marker the restart reconciliation reads alongside
	// the hub's own clean_pause verdict. A graceful shutdown leaves it
	// false — the "paused-with-session, will attach on restart" shape
	// (M3).
	CleanPause bool `json:"clean_pause"`
	// SpawnN is the per-WI monotonic derivation counter (B2): the number
	// of derivations this session has run. Same-machine restarts and
	// recoveries continue from it; a fresh clone (takeover) re-seeds it
	// from the capsule's spawn journal.
	SpawnN       int `json:"spawn_n"`
	RecoverFails int `json:"recover_fails"` // consecutive failed recoveries
	// StormFails/LastRecoveryAt persist the entry-rate storm guard (v2):
	// rapid recoverCore re-entries survive a daemon crash inside the
	// storm window. RFC3339Nano; zero = never recovered.
	StormFails     int    `json:"storm_fails"`
	LastRecoveryAt string `json:"last_recovery_at,omitempty"`
	Transcript     string `json:"transcript"`
	// LiveTranscript is the file the current derivation appends to (a
	// fresh pi derivation's harness-store live path, or the resume
	// source); "" while not located. Restart keeps it so the resume
	// discriminator's live fallback survives a daemon restart.
	LiveTranscript string `json:"live_transcript,omitempty"`
	BundleName     string `json:"bundle_name"`
	BundleDigest   string `json:"bundle_digest"`
	AgentPID       int    `json:"agent_pid"`
	AgentStart     string `json:"agent_started_at"`
	// Origin is how the job was submitted (v2 identity model). Absent on
	// records written before it existed: such a job loads fine and is
	// treated as owned by the WI reporter by later consumers.
	Origin *Origin `json:"origin,omitempty"`
	// RepairPending is the v2 §6 local pending-repair mark (see
	// job.repairPending): "capsule_missing" | "project_unmapped" | "".
	RepairPending string `json:"repair_pending,omitempty"`
	// EndNote/EndHead persist the pending SessionEnd(blocked) of an
	// end_pending record: the verdict's note and the capsule head the
	// sealing daemon pushed (the cross-daemon continue check, §7).
	EndNote string `json:"end_note,omitempty"`
	EndHead string `json:"end_head,omitempty"`
}

// Origin kinds.
const (
	OriginChat = "chat" // interactive submit from a harness chat
	OriginCLI  = "cli"  // interactive/batch submit without a chat
	OriginAuto = "auto" // drain-mode auto claim
)

// Origin is the daemon-side record of where a job came from. ChatID is a
// notification label, not a credential. Owner may be empty (unknown).
type Origin struct {
	Kind   string `json:"kind"`
	ChatID string `json:"chat_id,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

func (j *job) record() *jobRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return &jobRecord{
		WorkItemID:         j.wi,
		Project:            j.project,
		SessionID:          j.sessionID,
		SessionFingerprint: j.sessionFingerprint,
		State:              string(j.state),
		StopReason:         j.stopReason,
		SealedPush:         j.sealedPush,
		CapsuleDir:         j.capsuleDir,
		GitDir:             j.git.GitDir,
		RemoteURL:          j.git.RemoteURL,
		CleanPause:         j.cleanPause,
		SpawnN:             j.spawnN,
		RecoverFails:       j.recoverFails,
		StormFails:         j.stormFails,
		LastRecoveryAt:     stampIfSet(j.lastRecoveryAt),
		Transcript:         j.transcript,
		LiveTranscript:     j.liveTranscript,
		BundleName:         j.bundleName,
		BundleDigest:       j.bundleDigest,
		AgentPID:           j.agentPID,
		AgentStart:         j.agentStartedAt,
		Origin:             j.origin,
		RepairPending:      j.repairPending,
		EndNote:            j.endNote,
		EndHead:            j.endHead,
	}
}

func jobFromRecord(rec *jobRecord) *job {
	fp := rec.SessionFingerprint
	if fp == "" && rec.SessionID != "" {
		fp = api.SessionFingerprint(rec.SessionID)
	}
	return &job{
		wi:        rec.WorkItemID,
		project:   rec.Project,
		sessionID: rec.SessionID,
		// SessionFingerprint is recomputed from the secret when the
		// stored copy is empty (defensive: the two are redundant by
		// construction).
		sessionFingerprint: fp,
		state:              JobState(rec.State),
		stopReason:         rec.StopReason,
		sealedPush:         rec.SealedPush,
		capsuleDir:         rec.CapsuleDir,
		git: gitops.Opts{
			// v1.5 gitops call sites: Username = the work-item id,
			// Secret = the session id (the push password),
			// Fingerprint = the PUBLIC handle (§1.6 backup layer).
			WorkItemID:  rec.WorkItemID,
			Fingerprint: fp,
			CapsuleDir:  rec.CapsuleDir,
			GitDir:      rec.GitDir,
			RemoteURL:   rec.RemoteURL,
			Username:    rec.WorkItemID,
			Secret:      rec.SessionID,
		},
		cleanPause:     rec.CleanPause,
		spawnN:         rec.SpawnN,
		recoverFails:   rec.RecoverFails,
		stormFails:     rec.StormFails,
		lastRecoveryAt: parseStamp(rec.LastRecoveryAt),
		transcript:     rec.Transcript,
		liveTranscript: rec.LiveTranscript,
		bundleName:     rec.BundleName,
		bundleDigest:   rec.BundleDigest,
		agentPID:       rec.AgentPID,
		agentStartedAt: rec.AgentStart,
		origin:         rec.Origin,
		repairPending:  rec.RepairPending,
		endNote:        rec.EndNote,
		endHead:        rec.EndHead,
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

// stampIfSet renders t as RFC3339Nano ("" for the zero time — the
// storm guard's never-recovered marker).
func stampIfSet(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// parseStamp is stampIfSet's inverse; an unparsable value reads as
// never (a torn record must never look like a live storm window).
func parseStamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
