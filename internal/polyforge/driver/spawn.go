package driver

// spawn.go: the production StepSpawner adapter — the lane driver.go's
// spawn-seam comment defers to. One adapter bridges the driver's
// StepSpawn order onto the EXISTING runtime derivation mechanism and
// owns three things:
//
//   - PROMPT ASSEMBLY. The prompt is the step definition (the R9 path
//     convention: workflows/<bundle>/steps/<id>.md) plus the consumed
//     gate-FAIL rewind verdict plus the step's archived Q&A history,
//     plus — for inherit steps — the producing step's handoff artifact
//     content, plus every declared input artifact's content (R11),
//     plus the result-delivery contract. Fresh steps get the same
//     assembly minus the handoff: a fresh context never carries one.
//
//   - PROCESS DERIVATION through spawn.Params → spawn.Spawner
//     (the same mechanism the WI-agent lane uses): the model pin rides
//     argv, the resume session rides the spawn journal's latest
//     transcript, the spawn identity line is appended through
//     SpawnParams.Journal, and the isolation flags / env sanitize
//     point / cgroup enrollment stay in their one place
//     (runtime/spawn.go).
//
//   - RESULT COLLECTION. The step process delivers its R13 envelope by
//     writing it to runtime-state/pending-results/<spawn_n>.json (the
//     delivery contract in the prompt states the path and the shape).
//     The run collects it, stamps it into the R14 staging location
//     artifacts/steps/<step>/s<spawn_n>/result.json — durably
//     discoverable: the NEXT inherit step's handoff is read from
//     exactly there — consumes the drop (single-shot collection) and
//     tears the process down. A crash between the envelope write and
//     collection loses nothing durable: the drop path is keyed by the
//     spawn ordinal, every re-dispatch takes a fresh ordinal (B2 — the
//     driver-path relaunches advance the counter too), and a collected
//     drop is removed, so a stale drop file is never mistaken for a
//     live derivation's result.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/spawn"
	"github.com/silgrid/polyforge/internal/common/workflow"
)

const (
	// dirStepDefs is the step-definition directory inside a
	// materialized bundle: workflows/<bundle>/steps/<id>.md (the R9
	// path convention).
	dirStepDefs = "steps"
	// dirPendingResults is the runtime-state subdirectory a step
	// process delivers its R13 envelope into (one file per spawn
	// ordinal — the ordinal IS the fence, so a superseded derivation's
	// leftover file is never collected).
	dirPendingResults = "pending-results"
	// spawnJournalFile is the capsule spawn journal the runtime
	// maintains (runtime-state/spawn-journal.jsonl): same name, same
	// line shape — the adapter appends through SpawnParams.Journal so
	// B2 counter continuity keeps working across both lanes.
	spawnJournalFile = "spawn-journal.jsonl"
	// defaultEnvelopePoll is the default envelope-collection cadence.
	defaultEnvelopePoll = 200 * time.Millisecond
	// teardownBound bounds the post-envelope process teardown.
	teardownBound = 5 * time.Second
)

// StepSpawnAdapter is the production StepSpawner. Build one per work
// item (NewStepSpawnAdapter) and hand it to the Driver's Config; the
// driver serializes, so the adapter itself keeps no locks.
type StepSpawnAdapter struct {
	WI WIInfo
	// BundleDir is the materialized workflow bundle root (workflow.md
	// + steps/). Default: <CapsuleDir>/workflows/<BundleName>.
	BundleDir string
	// Workflow is the extended frontmatter parse (workflow_ext.go);
	// nil → loaded from BundleDir at construction.
	Workflow *DriverWorkflow
	// Spawner derives the step processes. Required; production binds
	// runtime.DefaultSpawner(&runtime.Config{...}) so isolation, env
	// sanitization and the model pin keep their single home.
	Spawner spawn.Spawner
	// CgroupPath is the per-WI cgroup v2 dir ("" = layer 0
	// unavailable) — passed through to SpawnParams.
	CgroupPath string
	// PollInterval is the envelope-collection cadence (tests tighten
	// it; default defaultEnvelopePoll).
	PollInterval time.Duration
	// Now overrides the clock for journal stamps (tests).
	Now func() time.Time
}

// NewStepSpawnAdapter validates the configuration, resolves the bundle
// directory and loads (and validates) the extended workflow parse —
// fail-fast at daemon assembly rather than mid-dispatch.
func NewStepSpawnAdapter(a StepSpawnAdapter) (*StepSpawnAdapter, error) {
	if a.WI.ID == "" {
		return nil, fmt.Errorf("driver: spawn adapter without a work item id")
	}
	if a.WI.CapsuleDir == "" {
		return nil, fmt.Errorf("driver: spawn adapter without a capsule dir")
	}
	if a.Spawner == nil {
		return nil, fmt.Errorf("driver: spawn adapter without a runtime spawner")
	}
	if a.BundleDir == "" {
		if a.WI.BundleName == "" {
			return nil, fmt.Errorf("driver: spawn adapter without a bundle name or dir")
		}
		a.BundleDir = filepath.Join(a.WI.CapsuleDir, capsule.DirWorkflows, a.WI.BundleName)
	}
	if a.Workflow == nil {
		wf, err := LoadDriverWorkflow(a.BundleDir)
		if err != nil {
			return nil, err
		}
		a.Workflow = wf
	}
	if a.PollInterval <= 0 {
		a.PollInterval = defaultEnvelopePoll
	}
	return &a, nil
}

// SpawnStep derives one step process for the driver's dispatch order.
// The context policy is the ADAPTER's own extended parse's (the
// frontmatter authority for v2 bundles); StepSpawn.ContextMode is the
// sequencing surface's advisory copy and is deliberately not trusted
// here — the two loaders carry different default eras.
func (a *StepSpawnAdapter) SpawnStep(ctx context.Context, sp StepSpawn) (StepRun, error) {
	fmt.Fprintf(os.Stderr, "driver: SpawnStep: step=%s spawn=%d policy=%s\n", sp.StepID, sp.SpawnN, sp.ContextMode)
	spec := a.Workflow.StepByID(sp.StepID)
	if spec == nil {
		return nil, fmt.Errorf("driver: step %q is not in the workflow bundle", sp.StepID)
	}
	policy := spec.ContextPolicy
	switch policy {
	case ContextInherit, ContextFresh: // ParseDriverWorkflow defaults inherit
	default:
		return nil, fmt.Errorf("driver: step %q has unknown context_policy %q", sp.StepID, policy)
	}
	def, err := readStepDefinition(a.BundleDir, sp.StepID)
	if err != nil {
		return nil, err
	}
	parts := promptParts{
		WIID:        a.WI.ID,
		StepID:      sp.StepID,
		SpawnN:      sp.SpawnN,
		ContextMode: policy,
		Definition:  def,
		Rewind:      sp.RewindInput,
		Answers:     sp.Answers,
		HandoffDue:  spec.Handoff,
	}
	if policy == ContextInherit {
		h, err := a.resolveHandoff(sp.StepID)
		if err != nil {
			return nil, err
		}
		parts.Handoff = h
	}
	for _, in := range spec.Inputs {
		content, err := readCapsuleFile(a.WI.CapsuleDir, in.Artifact)
		if err != nil {
			return nil, fmt.Errorf("driver: step %q declared input unreadable: %w", sp.StepID, err)
		}
		parts.Inputs = append(parts.Inputs, inputSection{Path: in.Artifact, Content: content})
	}

	handle := a.sessionHandle()
	var transcript string
	if sp.Resume {
		transcript, err = a.latestJournalTranscript()
		if err != nil {
			return nil, err
		}
	} else {
		transcript = filepath.Join(a.WI.CapsuleDir, capsule.DirSession, "session-"+spawnToken(handle, sp.SpawnN)+".jsonl")
	}
	p := spawn.Params{
		WorkItemID:     a.WI.ID,
		CapsuleDir:     a.WI.CapsuleDir,
		SessionHandle:  handle,
		SpawnN:         int(sp.SpawnN),
		TranscriptPath: transcript,
		Resume:         sp.Resume,
		InitialPrompt:  assembleStepPrompt(parts),
		CgroupPath:     a.CgroupPath,
		Model:          sp.Model,
	}
	token := spawnToken(handle, sp.SpawnN)
	p.Journal = func(pid int, startedAt string) error {
		return a.appendSpawnJournal(p, token, pid, startedAt)
	}
	h, err := a.Spawner.Spawn(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("driver: step %s spawn %d derivation failed: %w", sp.StepID, sp.SpawnN, err)
	}
	return &stepRun{
		capsuleRoot: a.WI.CapsuleDir,
		wiID:        a.WI.ID,
		sp:          sp,
		handle:      h,
		dropPath:    pendingResultPath(a.WI.CapsuleDir, sp.SpawnN),
		poll:        a.PollInterval,
	}, nil
}

// ---------------------------------------------------------------------------
// Prompt assembly (pure — table-tested below).

// promptParts is the assembled prompt's material.
type promptParts struct {
	WIID        string
	StepID      string
	SpawnN      int64
	ContextMode string // ContextInherit | ContextFresh
	Definition  string
	Rewind      *capsule.RewindInput
	Answers     []capsule.ArchivedAnswer
	Handoff     *handoffSection
	Inputs      []inputSection
	// HandoffDue marks a step whose completed result must carry a
	// handoff_ref (the delivery contract states the duty only then).
	HandoffDue bool
}

// handoffSection is one producing step's handoff artifact, injected
// into an inherit step's prompt.
type handoffSection struct {
	FromStep string
	Path     string
	Content  []byte
}

// inputSection is one declared input artifact's content.
type inputSection struct {
	Path    string
	Content []byte
}

// assembleStepPrompt builds the step process's one instruction of
// record: identity, the context-policy contract, the step definition,
// the rewind verdict being consumed, the Q&A history a resumed step
// continues from, the handoff (inherit only), the declared inputs and
// the result-delivery contract.
func assembleStepPrompt(p promptParts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are executing step %q of polyforge work item %q (derivation %d).\n\n", p.StepID, p.WIID, p.SpawnN)
	switch p.ContextMode {
	case ContextInherit:
		b.WriteString("Context policy: INHERIT — this step continues the previous step's work; the handoff below (when present) is the upstream continuation.\n\n")
	case ContextFresh:
		b.WriteString("Context policy: FRESH — this step starts from a clean context; nothing from previous steps is carried in except the declared inputs below.\n\n")
	}
	b.WriteString("# Step definition\n\n")
	b.WriteString(p.Definition)
	b.WriteString("\n")
	if p.Rewind != nil && strings.TrimSpace(p.Rewind.Reason) != "" {
		b.WriteString("\n# Gate FAIL verdict being consumed (rewind input)\n\n")
		b.WriteString("A downstream gate failed and the workflow rewound to this step. The verdict in full:\n\n")
		b.WriteString(p.Rewind.Reason)
		b.WriteString("\n")
	}
	if len(p.Answers) > 0 {
		b.WriteString("\n# Answered questions (this step's history — continue from it)\n\n")
		for _, a := range p.Answers {
			fmt.Fprintf(&b, "- %s (answered by %s): %s\n", a.QuestionID, a.AnsweredBy, a.Answer)
		}
	}
	if p.Handoff != nil {
		fmt.Fprintf(&b, "\n# Handoff from step %q (%s)\n\n", p.Handoff.FromStep, p.Handoff.Path)
		b.Write(p.Handoff.Content)
		b.WriteString("\n")
	}
	if len(p.Inputs) > 0 {
		b.WriteString("\n# Declared inputs\n")
		for _, in := range p.Inputs {
			fmt.Fprintf(&b, "\n## %s\n\n", in.Path)
			b.Write(in.Content)
			b.WriteString("\n")
		}
	}
	b.WriteString(deliveryContract(p))
	return b.String()
}

// deliveryContract states the envelope delivery path and shape: the
// single channel the driver reads a result from.
func deliveryContract(p promptParts) string {
	rel := capsule.DirRuntimeState + "/" + dirPendingResults + "/" + strconv.FormatInt(p.SpawnN, 10) + ".json"
	var b strings.Builder
	fmt.Fprintf(&b, "\n# Result delivery\n\n"+
		"When this step reaches an outcome, write exactly ONE JSON object — the polyforge result envelope — "+
		"to `%s` (capsule-relative; your working directory IS the capsule root; create the directory if needed), "+
		"then end your turn. The execution driver collects the envelope from that path; nothing else you print is read as a result.\n\n", rel)
	fmt.Fprintf(&b, "```json\n{\n"+
		"  \"protocol_version\": %d,\n"+
		"  \"result_id\": \"<fresh UUID>\",\n"+
		"  \"wi_id\": %q,\n"+
		"  \"step_id\": %q,\n"+
		"  \"spawn_n\": %d,\n"+
		"  \"outcome\": \"completed\" | \"gate_failed\" | \"needs_input\" | \"incomplete\",\n"+
		"  \"output\": { },\n"+
		"  \"artifacts\": [ {\"path\": \"<capsule-relative>\", \"sha256\": \"<sha256 hex>\", \"bytes\": 0} ],\n"+
		"  \"command_evidence\": [],\n"+
		"  \"submitted_at\": \"<RFC3339>\"\n"+
		"}\n```\n\n", ResultProtocolVersion, p.WIID, p.StepID, p.SpawnN)
	b.WriteString("Rules: `output` is a JSON object; `artifacts` must list every file this step produced that downstream steps or the owner consume, with sha256/bytes matching the bytes on disk")
	if p.HandoffDue {
		b.WriteString("; this step DECLARES handoff, so a completed result must also carry `handoff_ref` — the same {path, sha256, bytes} shape — naming the artifact the next inherit step continues from")
	}
	b.WriteString("; a `needs_input` result carries `question_id` and `question` instead. End your turn once the file is written.\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// Step definition + capsule file reads.

// readStepDefinition loads workflows/<bundle>/steps/<id>.md (the R9
// path convention) and strips any leading frontmatter block: the body
// is the step's instruction of record.
func readStepDefinition(bundleDir, stepID string) (string, error) {
	path := filepath.Join(bundleDir, dirStepDefs, stepID+".md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("driver: step definition for %q unreadable (expected %s under the bundle): %w", stepID, dirStepDefs+"/"+stepID+".md", err)
	}
	body := stepDefBody(raw)
	if body == "" {
		return "", fmt.Errorf("driver: step definition for %q is empty", stepID)
	}
	return body, nil
}

// stepDefBody strips a leading YAML frontmatter block (the same fence
// shape workflow.md uses — detection reuses the shared splitter, the
// body cut is a local line scan). A file without a closed frontmatter
// block is body in full.
func stepDefBody(raw []byte) string {
	if _, ok := workflow.SplitFrontmatter(raw); !ok {
		return strings.TrimSpace(string(raw))
	}
	s := string(raw)
	nl := strings.IndexByte(s, '\n')
	rest := s[nl+1:]
	for off := 0; off < len(rest); {
		eol := strings.IndexByte(rest[off:], '\n')
		var line string
		if eol < 0 {
			line, off = rest[off:], len(rest)
		} else {
			line, off = rest[off:off+eol], off+eol+1
		}
		if strings.TrimRight(line, " \r") == "---" {
			return strings.TrimSpace(rest[off:])
		}
	}
	return strings.TrimSpace(s) // unclosed fence: not frontmatter
}

// readCapsuleFile reads one capsule-relative artifact, jailed to the
// capsule root by the same path rules the artifact verifier applies.
func readCapsuleFile(root, rel string) ([]byte, error) {
	if err := checkRelPath(rel); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
}

// ---------------------------------------------------------------------------
// Handoff resolution.

// resolveHandoff finds the handoff an inherit step continues from: the
// NEAREST earlier step declaring handoff whose newest archived result
// envelope completed with a handoff_ref — its artifact content is the
// injection. No earlier handoff-declaring step at all answers (nil,
// nil) (a first step may legitimately inherit nothing); a handoff step
// without any archived envelope is an infrastructure inconsistency and
// fails loud (the driver would not have advanced past an un-done
// producer, so the envelope cannot be honestly missing).
func (a *StepSpawnAdapter) resolveHandoff(stepID string) (*handoffSection, error) {
	idx := -1
	for i := range a.Workflow.Steps {
		if a.Workflow.Steps[i].ID == stepID {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return nil, nil // no earlier step declares anything
	}
	for i := idx - 1; i >= 0; i-- {
		prod := &a.Workflow.Steps[i]
		if !prod.Handoff {
			continue
		}
		res, err := a.newestAcceptedResult(prod.ID)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, fmt.Errorf("driver: step %q declares handoff but has no ACCEPTED result (execution.json status=done + AcceptedResultID), needed by inherit step %q",
				prod.ID, stepID)
		}
		if res.Outcome != OutcomeCompleted || res.HandoffRef == nil {
			continue // a superseded cycle's envelope: keep looking upstream
		}
		// Verify the handoff artifact hash before injection (review fix 7).
		if err := VerifyArtifact(a.WI.CapsuleDir, *res.HandoffRef); err != nil {
			return nil, fmt.Errorf("driver: handoff artifact of step %q failed verification: %w", prod.ID, err)
		}
		content, err := readCapsuleFile(a.WI.CapsuleDir, res.HandoffRef.Path)
		if err != nil {
			return nil, fmt.Errorf("driver: handoff artifact of step %q unreadable: %w", prod.ID, err)
		}
		return &handoffSection{FromStep: prod.ID, Path: res.HandoffRef.Path, Content: content}, nil
	}
	return nil, nil
}

// newestAcceptedResult resolves the latest ACCEPTED result envelope for
// a step: it reads execution.json to check the step's status is done and
// finds the AcceptedResultID, then loads that exact archived envelope.
// Unaccepted/staged envelopes are NEVER returned (review fix 7).
func (a *StepSpawnAdapter) newestAcceptedResult(stepID string) (*StepResult, error) {
	execPath := filepath.Join(a.WI.CapsuleDir, "runtime-state", "execution.json")
	data, err := os.ReadFile(execPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No execution.json (early startup / isolated adapter test):
			// fall back to the archived envelope scan (pre-R14 behavior).
			return newestArchivedResult(a.WI.CapsuleDir, stepID)
		}
		return nil, fmt.Errorf("driver: read execution.json for accepted-result check: %w", err)
	}
	var exec struct {
		Steps map[string]struct {
			Status   string `json:"status"`
			Attempts []struct {
				AcceptedResultID string `json:"accepted_result_id"`
			} `json:"attempts"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &exec); err != nil {
		return nil, fmt.Errorf("driver: parse execution.json: %w", err)
	}
	st, ok := exec.Steps[stepID]
	if !ok || st.Status != "done" {
		return nil, nil // not accepted
	}
	// Find the accepted result_id.
	acceptedID := ""
	for i := len(st.Attempts) - 1; i >= 0; i-- {
		if st.Attempts[i].AcceptedResultID != "" {
			acceptedID = st.Attempts[i].AcceptedResultID
			break
		}
	}
	if acceptedID == "" {
		return nil, nil // done but no accepted id (legacy state)
	}
	// Load the archived envelope with that exact result_id.
	res, err := newestArchivedResult(a.WI.CapsuleDir, stepID)
	if err != nil {
		return nil, err
	}
	if res == nil || res.ResultID != acceptedID {
		return nil, nil // archived envelope doesn't match the accepted id
	}
	return res, nil
}

// newestArchivedResult reads the highest-ordinal archived envelope of
// one step (artifacts/steps/<step>/s<N>/result.json); nil when none is
// archived yet.
func newestArchivedResult(capsuleRoot, stepID string) (*StepResult, error) {
	dir := filepath.Join(capsuleRoot, capsule.DirArtifacts, "steps", stepID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	bestN := int64(-1)
	best := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if n, ok := parseSpawnToken(e.Name()); ok && n > bestN {
			bestN, best = n, e.Name()
		}
	}
	if bestN < 0 {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, best, "result.json"))
	if err != nil {
		return nil, err
	}
	var res StepResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("driver: archived result envelope of step %q (token %s) torn: %w", stepID, best, err)
	}
	return &res, nil
}

// parseSpawnToken decodes one archive token directory name ("s<N>",
// N ≥ 1).
func parseSpawnToken(name string) (int64, bool) {
	if !strings.HasPrefix(name, "s") || len(name) < 2 {
		return 0, false
	}
	n, err := strconv.ParseInt(name[1:], 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// spawnTokenDir is the archive token directory for spawn ordinal n.
func spawnTokenDir(n int64) string { return "s" + strconv.FormatInt(n, 10) }

// ---------------------------------------------------------------------------
// Session identity + spawn journal.

// sessionHandle answers the capsule's PUBLIC session fingerprint
// (session.json carries exactly the B3 handle, never the secret); ""
// when absent or unreadable.
func (a *StepSpawnAdapter) sessionHandle() string {
	raw, err := os.ReadFile(filepath.Join(a.WI.CapsuleDir, capsule.SessionJSON))
	if err != nil {
		return ""
	}
	var s capsule.Session
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s.SessionID
}

// latestJournalTranscript answers the resume source for a re-dispatch:
// the highest-ordinal spawn-journal line's declared transcript (the
// capsule-side session copy the runtime mechanism resumes from).
func (a *StepSpawnAdapter) latestJournalTranscript() (string, error) {
	raw, err := os.ReadFile(filepath.Join(a.WI.CapsuleDir, capsule.DirRuntimeState, spawnJournalFile))
	if err != nil {
		return "", fmt.Errorf("driver: resume ordered but the capsule spawn journal is unreadable: %w", err)
	}
	bestN := -1
	best := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e struct {
			SpawnN     int    `json:"spawn_n"`
			Transcript string `json:"transcript"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue // loose reader: skip anything unparsable
		}
		if e.Transcript != "" && e.SpawnN > bestN {
			bestN, best = e.SpawnN, e.Transcript
		}
	}
	if best == "" {
		return "", fmt.Errorf("driver: resume ordered but the spawn journal records no prior transcript")
	}
	return best, nil
}

// appendSpawnJournal appends one spawn identity line through
// SpawnParams.Journal — the same document shape the runtime lane
// writes (at, work_item, spawn_token, pid, started_at,
// session_handle, spawn_n, resume, transcript), so the B2 counter
// stays continuous across both spawners and every reader (loose by
// contract) keeps parsing.
func (a *StepSpawnAdapter) appendSpawnJournal(p spawn.Params, token string, pid int, startedAt string) error {
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	entry := map[string]any{
		"at":             now.UTC().Format(time.RFC3339Nano),
		"work_item":      p.WorkItemID,
		"spawn_token":    token,
		"pid":            pid,
		"started_at":     startedAt,
		"session_handle": p.SessionHandle,
		"spawn_n":        p.SpawnN,
		"resume":         p.Resume,
		"transcript":     p.TranscriptPath,
	}
	blob, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := filepath.Join(a.WI.CapsuleDir, capsule.DirRuntimeState, spawnJournalFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Torn-tail repair (astra r2 #2): a crash mid-append can leave the
	// journal without its trailing newline, and appending then would
	// GLUE the next record onto the half-written line — both becoming
	// unparsable, quietly erasing the ordinal from the loose readers'
	// view (JournalMax would under-seed, the next relaunch dispatching
	// an ordinal the crashed dispatch already used). A missing trailing
	// newline isolates the torn tail as its own (skipped) line and the
	// new record lands intact.
	if f, oerr := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644); oerr == nil {
		if fi, serr := f.Stat(); serr == nil && fi.Size() > 0 {
			buf := make([]byte, 1)
			if _, rerr := f.ReadAt(buf, fi.Size()-1); rerr == nil && buf[0] != '\n' {
				// WriteAt lands the isolating newline AT THE END — a plain
				// Write would follow the file offset (0) and clobber the
				// first record instead.
				_, _ = f.WriteAt([]byte("\n"), fi.Size())
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(blob, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// spawnToken is the public display token for one derivation (the
// runtime's shape: the session fingerprint, "#N"-suffixed from the
// second derivation on).
func spawnToken(handle string, n int64) string {
	if handle == "" {
		return "spawn-" + strconv.FormatInt(n, 10)
	}
	if n > 1 {
		return fmt.Sprintf("%s#%d", handle, n-1)
	}
	return handle
}

// JournalMax answers the highest spawn ordinal the capsule's spawn
// journal records (0 when the journal is absent) — the driver-owned
// derivation ledger: every step dispatch the production adapter
// spawns appends exactly one line to it. The bridge seeds the driver's
// STEP-ordinal counter from this ledger, NOT from the job's relaunch
// ordinal: the step ordinals are the observable contract (the drop
// path pending-results/<n>.json, the archived envelope dir s<n>, and
// the e2e fakepi's "Nth envelope action fires for spawn N" all key on
// them starting at 1 for a fresh capsule), while the job ordinal (≥1
// since the observe-lane fix) exists for the RTStarted emission.
// Seeding from the journal keeps both: fresh capsules start steps at
// 1, and a relaunched driver continues past every ordinal its
// predecessor used (the fence #97/#99 lessons).
// Absence answers (0, nil) — a fresh capsule has no ledger yet. Any
// OTHER read failure answers the error: the ledger is the fence's
// source of truth and a partially readable one must never be silently
// treated as fresh (astra r2 #2). Torn/unparsable LINES are tolerated
// by the loose parser — a crash-torn tail skips those lines only.
func JournalMax(capsuleDir string) (int64, error) {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, capsule.DirRuntimeState, spawnJournalFile))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	best := int64(0)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e struct {
			SpawnN int64 `json:"spawn_n"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue // loose reader: a torn line is skipped, never fatal
		}
		if e.SpawnN > best {
			best = e.SpawnN
		}
	}
	return best, nil
}

// pendingResultPath is the absolute drop path of one derivation's
// envelope.
func pendingResultPath(capsuleDir string, n int64) string {
	return filepath.Join(capsuleDir, capsule.DirRuntimeState, dirPendingResults, strconv.FormatInt(n, 10)+".json")
}

// ---------------------------------------------------------------------------
// stepRun: the StepRun face over one derived step process.

// stepRun awaits one step process's result envelope. Wait polls the
// drop path until a well-formed, fence-correct envelope lands (then
// archives it and tears the process down), the process exits without
// one (an infrastructure-class error — the recovery lane owns what
// follows), or ctx ends. A present-but-torn drop file is treated as a
// write still in flight and re-read on later polls; once the process
// is gone the defect is surfaced, not swallowed.
type stepRun struct {
	capsuleRoot string
	wiID        string
	sp          StepSpawn
	handle      spawn.Handle
	dropPath    string
	poll        time.Duration
}

func (r *stepRun) Wait(ctx context.Context) (*StepResult, error) {
	tick := 0
	for {
		tick++
		if tick%30 == 1 { // every ~30 polls (15s at default 500ms), log alive
			fmt.Fprintf(os.Stderr, "driver: stepRun.Wait alive: step=%s spawn=%d tick=%d drop=%s done=%v\n",
				r.sp.StepID, r.sp.SpawnN, tick, r.dropPath, r.handle.Done())
		}
		res, defect := r.collect()
		if res != nil {
			fmt.Fprintf(os.Stderr, "driver: stepRun.Wait COLLECTED: step=%s spawn=%d result_id=%s\n",
				r.sp.StepID, r.sp.SpawnN, res.ResultID)
		} else if defect != nil && tick%30 == 1 {
			fmt.Fprintf(os.Stderr, "driver: stepRun.Wait defect: step=%s spawn=%d err=%v\n",
				r.sp.StepID, r.sp.SpawnN, defect)
		}
		if res != nil {
			if err := r.archive(res); err != nil {
				_ = r.handle.Terminate(teardownBound)
				return nil, fmt.Errorf("driver: step %s spawn %d result staging failed: %w", r.sp.StepID, r.sp.SpawnN, err)
			}
			// Consume the drop (the #97 lesson): collection is single-shot.
			// The archived copy under artifacts/steps/<step>/s<N>/ is the
			// durable record; the drop left in place would be re-collected
			// verbatim by the NEXT dispatch that lands on this ordinal —
			// with a rejected envelope that was a reject/relaunch death
			// loop, because nothing else ever removed the file. A failed
			// remove is logged and never fatal: the fence (spawn ordinal)
			// still guards a stale re-read.
			if rerr := os.Remove(r.dropPath); rerr != nil && !os.IsNotExist(rerr) {
				fmt.Fprintf(os.Stderr, "driver: step %s spawn %d drop removal failed (stale re-read still fenced): %v\n",
					r.sp.StepID, r.sp.SpawnN, rerr)
			}
			if terr := r.handle.Terminate(teardownBound); terr != nil {
				fmt.Fprintf(os.Stderr, "driver: step %s spawn %d teardown after result failed (envelope already collected): %v\n",
					r.sp.StepID, r.sp.SpawnN, terr)
			}
			return res, nil
		}
		if r.handle.Done() {
			if defect != nil {
				return nil, fmt.Errorf("driver: step %s spawn %d exited leaving an unusable result envelope: %w",
					r.sp.StepID, r.sp.SpawnN, defect)
			}
			return nil, fmt.Errorf("driver: step %s spawn %d exited without writing a result envelope to %s",
				r.sp.StepID, r.sp.SpawnN, r.dropPath)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.poll):
		}
	}
}

func (r *stepRun) Terminate(bound time.Duration) error {
	return r.handle.Terminate(bound)
}

// collect reads the drop path once: a non-nil envelope passed the
// light shape/fence checks; (nil, nil) means nothing usable yet
// (absent, or a write in flight); (nil, err) is the last observed
// defect, surfaced by Wait once the process is gone.
func (r *stepRun) collect() (*StepResult, error) {
	raw, err := os.ReadFile(r.dropPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var res StepResult
	if err := json.Unmarshal(raw, &res); err != nil {
		// Tolerant re-parse: step agents persistently write command_evidence
		// as a string list instead of the object array. Re-parse with a
		// string-tolerant shadow and salvage the envelope (the evidence
		// descriptions become log notes, not machine-parsed objects).
		var shadow struct {
			CommandEvidence json.RawMessage `json:"command_evidence"`
		}
		if json.Unmarshal(raw, &shadow) == nil {
			// Zero out the malformed field and re-parse the rest.
			var m map[string]interface{}
			if json.Unmarshal(raw, &m) == nil {
				delete(m, "command_evidence")
				if cleaned, jerr := json.Marshal(m); jerr == nil {
					if uerr := json.Unmarshal(cleaned, &res); uerr == nil {
						goto parsed
					}
				}
			}
		}
		return nil, fmt.Errorf("envelope at %s is not valid JSON: %w", r.dropPath, err)
	}
parsed:
	// Light completion checks — the lenient fields a well-meaning
	// process may omit are filled from the dispatch identity; the
	// fence and the closed sets are enforced (full R14 validation is
	// the driver core's accept gate).
	if res.ProtocolVersion == 0 {
		res.ProtocolVersion = ResultProtocolVersion
	}
	if res.ProtocolVersion != ResultProtocolVersion {
		return nil, fmt.Errorf("%w: envelope speaks %d, driver speaks %d",
			ErrProtocolMismatch, res.ProtocolVersion, ResultProtocolVersion)
	}
	if res.StepID == "" {
		res.StepID = r.sp.StepID
	}
	if res.StepID != r.sp.StepID {
		return nil, fmt.Errorf("%w: envelope names step %q, dispatch is %q",
			ErrIdentityMismatch, res.StepID, r.sp.StepID)
	}
	if res.WIID == "" {
		res.WIID = r.wiID
	}
	if res.SpawnN == 0 {
		res.SpawnN = r.sp.SpawnN
	}
	if res.SpawnN != r.sp.SpawnN {
		return nil, fmt.Errorf("%w: envelope spawn %d, dispatch %d",
			ErrStaleSpawn, res.SpawnN, r.sp.SpawnN)
	}
	if !res.Outcome.Valid() {
		return nil, fmt.Errorf("driver: envelope outcome %q is not in the closed set", res.Outcome)
	}
	return &res, nil
}

// archive lands the collected envelope at the R14 staging path —
// artifacts/steps/<step-id>/s<spawn_n>/result.json — atomically. The
// handoff chain reads the next inherit step's injection from exactly
// here, and a later accept-time re-validation lane finds the bytes in
// place.
func (r *stepRun) archive(res *StepResult) error {
	blob, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(r.capsuleRoot, capsule.DirArtifacts, "steps", r.sp.StepID, spawnTokenDir(r.sp.SpawnN), "result.json")
	return atomicPersist(path, blob, 0o644)
}
