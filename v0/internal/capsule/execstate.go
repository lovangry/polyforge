package capsule

// execstate.go: runtime-state/execution.json — the ONLY execution-state
// authority (INV-1). Full schema per 03 §4.2; the JSON object as a whole is
// the externally visible "progress" (02 §5.2 read-through returns it
// verbatim; there is deliberately no `progress` sub-field).
//
// Atomic commit contract (03 §4.2): every write goes to
// runtime-state/.execution.json.tmp, fsync, rename over execution.json. The
// caller-side ordering contract is SYNC-INV-6 (05 §4.2): write step
// artifacts FIRST, execution.json LAST — this Save is the final write of a
// logical commit; the scanning side additionally re-reads execution.json
// last inside the freeze window, giving two layers of protection.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrNoExecutionState is returned when execution.json does not exist yet
// (first step not started).
var ErrNoExecutionState = errors.New("runtime-state/execution.json does not exist")

// Budget is the {requests, tokens, wall_minutes} triple (03 §4.2, budget
// dimensions per 01-workflow-skills.md §6.6).
type Budget struct {
	Requests    int64 `json:"requests"`
	Tokens      int64 `json:"tokens"`
	WallMinutes int64 `json:"wall_minutes"`
}

// CommandRecord is one RunCommand outcome appended to output.commands
// (03 §4.2 fix B10; the only accepted evidence that a command ran).
type CommandRecord struct {
	Argv       []string `json:"argv"`
	ExitCode   int      `json:"exit_code"`
	DurationMs int64    `json:"duration_ms"`
	StartedAt  string   `json:"started_at"`
}

// RepoRef is the per-step diff baseline {head, dirty} (03 §4.2 fix B10/B18).
type RepoRef struct {
	Head  string   `json:"head"`
	Dirty []string `json:"dirty"`
}

// RewindInput is the pending rewind verdict (03 §4.2 D8): the full gate FAIL
// verdict text persisted at rewind time and consumed at target dispatch.
type RewindInput struct {
	GateStepID  string `json:"gate_step_id"`
	VerdictText string `json:"verdict_text"`
	RewoundAt   string `json:"rewound_at"`
}

// AnswerRecord is the raw received answer persisted to pending_answer
// before the step resumes (crash-recoverable "received but not resumed").
type AnswerRecord struct {
	AnswerText string `json:"answer_text"`
	AnsweredAt string `json:"answered_at"`
	AnsweredBy string `json:"answered_by"`
	ReceivedAt string `json:"received_at"`
}

// ArchivedAnswer is one consumed Q&A pair archived into steps[].answers
// (03 §4.2 D9 fix B9), same atomic write that clears pending_question.
type ArchivedAnswer struct {
	QuestionID string `json:"question_id"`
	AnswerText string `json:"answer_text"`
	AnsweredAt string `json:"answered_at"`
	AnsweredBy string `json:"answered_by"`
}

// PendingQuestion is the persisted needs_input question (03 §4.2 D8/D9 fix
// B14): written in the SAME atomic write that puts the step into waiting;
// the comparison basis for the Daemon's answer relay (00 §3.4a).
type PendingQuestion struct {
	QuestionID    string        `json:"question_id"`
	AskedAt       string        `json:"asked_at"`
	Content       string        `json:"content"`
	PendingAnswer *AnswerRecord `json:"pending_answer"`
}

// Attempt is one archived step attempt (03 §4.2): cause ∈
// {gate_fail_rewind, infra_failure} — a closed two-value set; Q&A history
// never lands in attempts.
type Attempt struct {
	Cause          string          `json:"cause"`
	AttemptSeq     int             `json:"attempt_seq"`
	StartedAt      string          `json:"started_at"`
	ConsumedBudget *Budget         `json:"consumed_budget,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	ArchivedAt     string          `json:"archived_at,omitempty"`
	RewindGate     string          `json:"rewind_gate,omitempty"`
	InterruptedAt  string          `json:"interrupted_at,omitempty"`
	Detail         string          `json:"detail,omitempty"`
}

// Step is the per-step execution record (03 §4.2).
type Step struct {
	Status             string             `json:"status"` // pending|in_progress|done
	Output             map[string]any     `json:"output,omitempty"`
	StepStartedAt      string             `json:"step_started_at,omitempty"`
	StepStartRefs      map[string]RepoRef `json:"step_start_refs,omitempty"`
	BudgetConsumed     Budget             `json:"budget_consumed"`
	Attempts           []Attempt          `json:"attempts"`
	Answers            []ArchivedAnswer   `json:"answers"`
	PendingQuestion    *PendingQuestion   `json:"pending_question"`
	PendingRewindInput *RewindInput       `json:"pending_rewind_input"`
}

// RewindStats mirrors 01-workflow-skills.md §6.2 counters.
type RewindStats struct {
	Total  int64            `json:"total"`
	ByStep map[string]int64 `json:"by_step"`
}

// WorkflowStatus values (03 §4.2): running | completed | failed_gate |
// failed_rewind_exhausted | blocked (closed set).
const (
	StatusRunning               = "running"
	StatusCompleted             = "completed"
	StatusFailedGate            = "failed_gate"
	StatusFailedRewindExhausted = "failed_rewind_exhausted"
	StatusBlocked               = "blocked"
)

// StepStatus values.
const (
	StepPending    = "pending"
	StepInProgress = "in_progress"
	StepDone       = "done"
)

// Execution is the complete runtime-state/execution.json object (03 §4.2).
type Execution struct {
	CapsuleID      string           `json:"capsule_id"`
	WorkItemID     string           `json:"work_item_id"`
	Generation     int64            `json:"generation"`
	BundleDigest   string           `json:"bundle_digest"`
	CurrentStep    string           `json:"current_step"`
	WorkflowStatus string           `json:"workflow_status"`
	Steps          map[string]*Step `json:"steps"`
	ReviewRounds   map[string]int64 `json:"review_rounds"`
	RewindStats    RewindStats      `json:"rewind_stats"`
	BudgetConsumed Budget           `json:"budget_consumed"`
	UpdatedAt      string           `json:"updated_at"`
	Version        int64            `json:"version"`
}

// ExecPath returns the absolute path of runtime-state/execution.json.
func (c *Capsule) ExecPath() string {
	return filepath.Join(c.Dir, "runtime-state", "execution.json")
}

// LoadExecution reads execution.json; ErrNoExecutionState when absent.
// Readers (watcher, subagent self-check, publish scan) see only complete
// versions — tmp+fsync+rename means no torn reads, no locks needed.
func (c *Capsule) LoadExecution() (*Execution, error) {
	raw, err := os.ReadFile(c.ExecPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoExecutionState
	}
	if err != nil {
		return nil, err
	}
	var e Execution
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("execution.json unparsable: %w", err)
	}
	return &e, nil
}

// SaveExecution is the single controlled update interface for
// runtime-state/execution.json (03 §4.2): tmp + fsync + rename, then the
// INV-6 read-only permission flip (dir 0555, file 0444). The WI Agent
// re-opens writability for the next write by flipping back through
// MakeWritable first; subagent writes hit EACCES → CAP-403 (03 §6.2).
//
// Version discipline: callers bump Version (+1) themselves; this writer
// stamps UpdatedAt and refuses a version regression.
func (c *Capsule) SaveExecution(e *Execution) error {
	if err := os.MkdirAll(filepath.Join(c.Dir, "runtime-state"), 0o755); err != nil {
		return err
	}
	if existing, lerr := c.LoadExecution(); lerr == nil && e.Version <= existing.Version {
		return errf("CAP-400", "execution version %d would not advance past %d", e.Version, existing.Version)
	}
	// Stamp UpdatedAt with the commit moment (version regression was refused above).
	e.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	blob, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	execPath := c.ExecPath()
	tmpPath := filepath.Join(c.Dir, "runtime-state", ".execution.json.tmp")
	if err := AtomicWrite(tmpPath, blob, 0o644); err != nil {
		return err
	}
	// INV-6 mechanism B: read-only permission bits after every write.
	if err := os.Chmod(tmpPath, 0o444); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(execPath), 0o555); err != nil {
		return err
	}
	return nil
}

// MakeWritable reopens runtime-state for the WI Agent's next controlled
// write (03 §6.2: the permission flip is executed only by the WI Agent
// write path, never exposed to subagent execution paths).
func (c *Capsule) MakeWritable() error {
	if err := os.Chmod(filepath.Join(c.Dir, "runtime-state"), 0o755); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Chmod(c.ExecPath(), 0o644); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// PendingQuestion returns the current step's pending question, or nil.
// This is the persisted comparison basis for the Daemon's answer relay
// (00 §3.4a / 04 §5.1).
func (e *Execution) PendingQuestion() (*PendingQuestion, string) {
	if e.CurrentStep == "" {
		return nil, ""
	}
	s, ok := e.Steps[e.CurrentStep]
	if !ok || s.PendingQuestion == nil {
		return nil, ""
	}
	return s.PendingQuestion, e.CurrentStep
}
