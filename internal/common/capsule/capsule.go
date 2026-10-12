// Package capsule defines the v1 execution-capsule contract shared by pf
// (runtime creates capsules, observe commits them) and aihub (serves the
// matching bare repos; git-dir lives OUTSIDE the capsule).
//
// Design authority: docs/design/polyforge-vnext/v1-architecture-spec.md,
// v1-spec-full.md §六, and 03-execution-capsule.md for layout semantics.
//
// v1 deltas vs v0:
//   - The capsule IS a git worktree; the revision chain IS git history.
//   - git-dir is externalized: <data>/git/wi_xxx.git — agents cannot reach
//     hooks/config/index.
//   - .gitignore covers only repos/ and artifacts/release-*/ (ruling C1:
//     transcripts go INTO git; secret scanning is dropped).
//   - session-data/ side upload is deleted; transcripts are committed like
//     any other capsule file.
package capsule

import "encoding/json"

// WorkspaceYamlAuditDoc is the polyforge#79 audit shape of the parsed
// `.polyforge.yaml` (projects + workflow_defaults, verbatim — declared
// repo list order intact, no normalization or redaction). It is the
// JSON encoding of the same structures the shared strict-schema loader
// answered, so no second mapping layer exists between the loader and
// the record.
type WorkspaceYamlAuditDoc struct {
	Projects         map[string]WorkspaceProjectAudit `json:"projects,omitempty"`
	WorkflowDefaults *WorkspaceDefaultsAuditDoc       `json:"workflow_defaults,omitempty"`
}

// WorkspaceProjectAudit is one project entry of the audited yaml.
type WorkspaceProjectAudit struct {
	Repos []WorkspaceRepoAudit `json:"repos"`
}

// WorkspaceRepoAudit is one repo entry of the audited yaml.
type WorkspaceRepoAudit struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// WorkspaceDefaultsAuditDoc is the audited yaml's workflow_defaults.
type WorkspaceDefaultsAuditDoc struct {
	ModelCandidates []string            `json:"model_candidates,omitempty"`
	StepKinds       map[string][]string `json:"step_kinds,omitempty"`
}

// Capsule directory layout (relative to capsule root).
const (
	DirAgents       = "agents"        // materialized step-agent definitions
	DirArtifacts    = "artifacts"     // step outputs; release trees under artifacts/release-*/
	DirLogs         = "logs"          // daemons logs, step journals
	DirRepos        = "repos"         // task repo checkouts (own git histories)
	DirRuntimeState = "runtime-state" // execution.json, journal, spawn.log
	DirSession      = "session"       // harness transcripts synced at boundaries
	DirWorkflows    = "workflows"     // materialized workflow bundle
	CapsuleJSON     = "capsule.json"  // capsule identity
	SessionJSON     = "session.json"  // session/harness state
	WorkItemJSON    = "runtime-state/work-item.json"
	ExecutionJSON   = "runtime-state/execution.json"
)

// GitignoreContent is the .gitignore written at capsule init (ruling C1).
const GitignoreContent = "/repos/\n/artifacts/release-*/\n"

// Identity is capsule.json (03 §2.2). The declared BaseCommit pins the task
// repos; CapsuleRepo is the bare repo this worktree pushes to. The v1
// generation/attempt_id lineage fields are deleted (v1.5 session model):
// legacy capsules still carrying those keys parse fine — encoding/json
// ignores unknown keys (pinned by TestLooseParseLegacyCapsuleKeys).
type Identity struct {
	WorkItemID   string            `json:"work_item_id"`
	CapsuleID    string            `json:"capsule_id"`
	Project      string            `json:"project"`
	BundleName   string            `json:"bundle_name"`
	BundleDigest string            `json:"bundle_digest"`
	BaseCommit   string            `json:"base_commit"` // task repo baseline each step starts from
	Repos        map[string]string `json:"repos"`       // repo name -> checked-out SHA at claim
	CreatedAt    string            `json:"created_at"`

	// WorkspaceYaml is the polyforge#79 audit: the parsed .polyforge.yaml
	// verbatim (projects + workflow_defaults, declared repo list order
	// intact). nil (omitted) when the claim carried no yaml — a no-yaml
	// claim records no yaml audit content, never a fabricated one. Legacy
	// capsules decode loose: the key is simply absent on old reads.
	WorkspaceYaml *WorkspaceYamlAuditDoc `json:"workspace_yaml,omitempty"`
	// EffectiveCandidates is the polyforge#79 audit: the per-step
	// post-precedence candidate list the claim actually applied (step id
	// → list; a present-empty list governs and means no switchable
	// chain). Doubles as the claim-time SNAPSHOT the recovery path
	// rehydrates from.
	EffectiveCandidates map[string][]string `json:"effective_candidates,omitempty"`
	// RepoSources is the polyforge#79 audit: repo name → the source
	// actually cloned from (the resolved local checkout path for
	// yaml-sourced clones, the registry URL for registry clones under a
	// yaml that governs nothing on the repo side).
	RepoSources map[string]string `json:"repo_sources,omitempty"`
}

// WorkItemDoc is runtime-state/work-item.json: the daemon-materialized
// copy of the work item's task definition (goal, content, labels,
// priority, milestone, bundle pin and project name). v1 has NO
// agent→server read path (ruling C3: the whole agent contact surface is
// the capsule), so this file is the ONE
// authoritative statement of what the WI agent is working on — the
// claim answer is its only source, and recovery re-materializes it from
// aihub when a legacy capsule predates it.
type WorkItemDoc struct {
	ID           string   `json:"id"`
	Slug         string   `json:"slug,omitempty"`
	Project      string   `json:"project"`
	Goal         string   `json:"goal"`
	Content      *string  `json:"content"`
	Labels       []string `json:"labels"`
	Priority     string   `json:"priority"`
	Milestone    *string  `json:"milestone"`
	BundleName   string   `json:"bundle_name"`
	BundleDigest string   `json:"bundle_digest"`
	UpdatedAt    string   `json:"updated_at"`
}

// Session is session.json: per-derivation harness session bookkeeping
// used by the recovery engine (v1-spec-full §七; discriminator =
// transcript exists). The v1 attempt_id/generation lineage fields are
// deleted (v1.5 session model); legacy files carrying those keys parse
// fine (TestLooseParseLegacyCapsuleKeys).
type Session struct {
	Harness        string `json:"harness"`         // pi|cc
	SessionID      string `json:"session_id"`      // harness-native session identifier
	TranscriptPath string `json:"transcript_path"` // absolute path to the .jsonl transcript
	UpdatedAt      string `json:"updated_at"`
}

// The workflow_status vocabulary (03 §4.2): the closed status set the
// execution.json contract speaks. The DRIVER owns the writes and the
// transitions (its own aliases in internal/polyforge/driver); these
// constants live at the SCHEMA owner so non-driver lanes (the runtime's
// recovery wiring) can read a terminal verdict without importing the
// driver (import cycle: the driver's tests import the runtime).
const (
	WorkflowStatusRunning               = "running"
	WorkflowStatusCompleted             = "completed"
	WorkflowStatusFailedGate            = "failed_gate"
	WorkflowStatusFailedRewindExhausted = "failed_rewind_exhausted"
	// WorkflowStatusBlocked is the NON-terminal waiting-on-owner state.
	WorkflowStatusBlocked = "blocked"
)

// Execution is runtime-state/execution.json (03 §4.2): the single source of
// truth the WI Agent writes and the daemon observes (ruling C3: agent is
// zero-blocking; observe watches this file).
type Execution struct {
	CapsuleID      string           `json:"capsule_id"`
	WorkItemID     string           `json:"work_item_id"`
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

// Step statuses.
const (
	StepPending    = "pending"
	StepInProgress = "in_progress"
	StepDone       = "done"
)

type Step struct {
	Status string `json:"status"`
	// Model is the effective model id the step's dispatches currently
	// run under (the F4 lane: the executor records the candidate it
	// dispatched with into every step-starting execution.json write, so
	// the observe face can render it — the daemon never derives it).
	Model              string             `json:"model,omitempty"`
	Output             map[string]any     `json:"output,omitempty"`
	StepStartedAt      string             `json:"step_started_at,omitempty"`
	StepStartRefs      map[string]RepoRef `json:"step_start_refs,omitempty"`
	BudgetConsumed     Budget             `json:"budget_consumed"`
	Attempts           []Attempt          `json:"attempts"`
	Answers            []ArchivedAnswer   `json:"answers"`
	PendingQuestion    *PendingQuestion   `json:"pending_question,omitempty"`
	PendingRewindInput *RewindInput       `json:"pending_rewind_input,omitempty"`
}

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
	// FaultClass records the classified failure category (R4):
	// provider / host / operator / schema. Empty for non-infra attempts.
	FaultClass string `json:"fault_class,omitempty"`
	// AcceptedResultID is the R13 envelope result_id committed at the
	// R14 commit point — populated only for accepted step results.
	AcceptedResultID string `json:"accepted_result_id,omitempty"`
}

type Budget struct {
	USD    float64 `json:"usd"`
	WallMs int64   `json:"wall_ms"`
	Tokens int64   `json:"tokens"`
}

type RepoRef struct {
	Repo string `json:"repo"`
	SHA  string `json:"sha"`
}

type RewindInput struct {
	ToStep string             `json:"to_step"`
	Note   string             `json:"note"`
	Refs   map[string]RepoRef `json:"refs"`
	Reason string             `json:"reason"`
}

type PendingQuestion struct {
	ID       string          `json:"id"`
	Step     string          `json:"step"`
	Question string          `json:"question"`
	Context  json.RawMessage `json:"context,omitempty"`
	AskedAt  string          `json:"asked_at"`
}

type ArchivedAnswer struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
	AnsweredBy string `json:"answered_by"`
	AnsweredAt string `json:"answered_at"`
}

type RewindStats struct {
	Count  int64    `json:"count"`
	ToStep []string `json:"to_step"`
}

// SeqTrailer is the git commit trailer used by pre-receive hooks to enforce
// monotonically increasing commit sequence per work item (v1-spec-full §六).
const SeqTrailer = "Polyforge-Seq"

// MaxBlobBytes is the pre-receive blob size limit (default 16 MiB; the
// secret scanner is gone per ruling C1 but size stays bounded).
const MaxBlobBytes = 16 << 20
