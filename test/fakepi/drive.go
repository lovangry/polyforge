// drive.go: the v1 fake agent's scripted executor, envelope edition
// (polyforge#106: the WI-Agent RPC lane is deleted — the Go driver
// spawns one process per step and this executor performs the matching
// envelope action). The plan file is a JSON document describing what
// each derivation does on the capsule side — write execution.json
// (Version monotonically increasing, atomic tmp+rename), emit record
// bursts, park or die. Every action is
// journaled to logs/fakepi.jsonl inside the capsule, which the
// scenarios assert on.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// planAction is one scripted step of a derivation.
type planAction struct {
	Do       string `json:"do"`
	ID       string `json:"id"`       // step id / question id
	Question string `json:"question"` // ask: the question text
	MS       int    `json:"ms"`       // sleep / ui_noise: milliseconds
	Name     string `json:"name"`     // repo: the repos/ subdirectory
	Origin   string `json:"origin"`   // repo: the origin remote URL
	// Envelope mode (v2 Driver): the outcome and handoff flags for the
	// result-envelope action.
	Outcome string `json:"outcome,omitempty"` // envelope: completed|needs_input|gate_failed|incomplete
	Handoff bool   `json:"handoff,omitempty"` // envelope: produce handoff_ref
	// NoEvidence suppresses the step's structured command evidence (the
	// wrap-evidence-gate scenario scripts a verification step whose
	// output carries no commands record, then repairs it).
	NoEvidence bool `json:"no_evidence,omitempty"`

	// --- envelope-mode extensions (polyforge#105, confined to fakepi) ---

	// QuestionID/Question are the needs_input question text, copied
	// onto the result envelope VERBATIM (the driver refuses a
	// needs_input result without question_id/question —
	// internal/polyforge/driver/transition.go). An EMPTY question_id is
	// scriptable on purpose: the empty-id refusal tests script the
	// malformed envelope exactly. The question TEXT rides the existing
	// Question field above (shared with the ask action).
	QuestionID string `json:"question_id,omitempty"`
	// Park is the #113 wedge shape: open a turn, emit nothing further,
	// never write the envelope, never exit — the process sits alive at
	// zero activity while the driver's stepRun.Wait watches a drop path
	// that never changes (repro scaffolding for the step-wedge detection
	// lane; test-only, like all of fakepi).
	Park bool `json:"park,omitempty"` // park: silent live wedge, no envelope, no exit
	// Crash shapes on the envelope action: "before" dies BEFORE the
	// envelope write (the envelope never lands — the kill9 / hang
	// lanes), "after" dies right AFTER writing it (the seal-crash lane:
	// the written envelope must still be collected exactly once). Both
	// work on resume derivations too: the plan file is re-read per
	// process and resume_actions script crash + continuation in one
	// file, deterministically (the Nth envelope action fires for spawn
	// N across fresh and resume dispatches alike).
	Crash string `json:"crash,omitempty"` // "" | "before" | "after"
	// CrashStderr is written to stderr just before a crash:"before" exit
	// (a harness dying WITH a reason: a model-class text such as a
	// provider error makes the driver advance the candidate chain; the
	// bare crash stays an unclassified exit).
	CrashStderr string `json:"crash_stderr,omitempty"`
	// Dup writes the IDENTICAL envelope to the same drop path N times
	// back-to-back (the duplicate drop-path shape: the driver must
	// consume it exactly once).
	Dup int `json:"dup,omitempty"`
	// Output replaces the default output payload verbatim (populated
	// error outputs for gate_failed envelopes, schema-shaped payloads,
	// refusal probes).
	Output map[string]any `json:"output,omitempty"`
	// Files writes extra capsule-relative files (path → content) BEFORE
	// the envelope write — the declared-input shape: a later step's
	// declared `inputs:` entry must exist on disk at ITS dispatch, and
	// this is how a scripted step produces it (e.g. the bug-fix lane's
	// root-cause consuming artifacts/repro.md).
	Files map[string]string `json:"files,omitempty"`

	// Evidence carries extra structured command-evidence entries
	// (merged with the synthetic one; suppressed entirely by
	// NoEvidence). The driver copies command_evidence onto
	// execution.json's steps.<id>.output.commands.
	Evidence []map[string]any `json:"evidence,omitempty"`
	// OpenTurn opens a stdout turn and leaves it open before the
	// envelope write (the F15 record-stream shapes on driver step
	// processes).
	OpenTurn bool `json:"open_turn,omitempty"`
	// Noise streams ui_noise records (kind ui | progress) while the
	// pre-write delay runs (F15 shapes); NoiseMS is the emitter interval
	// (MS keeps its meaning as the pre-write delay).
	Noise   string `json:"noise,omitempty"`    // "" | "ui" | "progress"
	NoiseMS int    `json:"noise_ms,omitempty"` // the noise emitter interval (default 100ms)

	// --- supervision shapes (v2-converge 2.3 incident replays) ---

	// Silent is the zero-signal harness (rule 0, #114): the dispatch
	// emits NO work record at all — not even the boot greeting turn — and
	// parks alive. Combined with Noise "ui" it is the F15 shape (UI
	// chatter only).
	Silent bool `json:"silent,omitempty"`
	// ErrorTurn is the F5 shape: the dispatch emits an assistant turn that
	// ENDS on this provider error (message_start/message_end/turn_end and
	// agent_end all carry stopReason=error), then parks alive without ever
	// writing the envelope.
	ErrorTurn string `json:"error_turn,omitempty"`
	// EndTurn is the ok-ended twin of ErrorTurn (rule 1b): the dispatch
	// emits an assistant turn that ends NORMALLY, then parks alive without
	// ever writing the envelope.
	EndTurn bool `json:"end_turn,omitempty"`
	// ToolMS runs one tool execution of this length (tool_execution_start,
	// then tool_execution_end after the delay) before the envelope write:
	// the #83 long-tool shape. ToolHang never ends it (rule 3's shape).
	ToolMS   int    `json:"tool_ms,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	ToolHang bool   `json:"tool_hang,omitempty"`
	// PoisonGitlink / UnpoisonGitlink run the #48 poison shape (lay or
	// clean an embedded git repository) before the envelope write; the
	// version bump inside arms the observe push round.
	PoisonGitlink   bool `json:"poison_gitlink,omitempty"`
	UnpoisonGitlink bool `json:"unpoison_gitlink,omitempty"`
}

// plan is the whole scripted behavior of one derivation.
type plan struct {
	Actions []planAction `json:"actions"`
	// ResumeActions, when set, replace Actions for a RESUME derivation
	// (the recovery engine re-derives with --session): a scenario scripts
	// the crash and the continuation in one file, deterministically —
	// no rewrite race against the recovery spawn.
	ResumeActions []planAction `json:"resume_actions"`
	// BadModels lists pinned model ids this harness cannot resolve (F4):
	// a dispatch spawned with `--model <one of them>` prints the error to
	// stderr and exits non-zero at startup.
	BadModels []string `json:"bad_models,omitempty"`
	// FailSpawnResume true makes every RESUME derivation die AT BIRTH:
	// the process exits before it reads one byte of stdin, so the
	// daemon's initial-prompt injection finds no reader, the pipe write
	// fails and Spawn itself returns an error. A derivation that accepts
	// the prompt and dies AFTERWARDS is a materially different lane — a
	// new agent_death recovery, which RESETS the budgets — so this mode
	// must never degrade into that one; see the pipe-capacity contract
	// below.
	FailSpawnResume bool `json:"fail_spawn_resume"`
	// FailSpawnFresh true makes every fresh RE-derivation die the same
	// way. "Re-derivation" is discriminated on the derivation counter the
	// runtime injects as PF_SPAWN_N (> 1 on every derivation after the
	// first): the ORIGINAL derivation carries counter 1 and is NEVER
	// affected by this flag. One plan file therefore scripts a healthy
	// first run AND a must-fail recovery ladder with no rewrite race
	// against the recovery spawn.
	//
	// PIPE-CAPACITY CONTRACT (both flags). "Dies before the prompt
	// lands" is not something this process can win by racing: the daemon
	// writes the prompt a few hundred microseconds after fork/exec,
	// while a Go binary spends about as long in runtime start-up
	// (measured on the ladder's own shape: a child that exits
	// immediately loses the race ~93% of the time against a ~27 KiB
	// prompt). The mode is deterministic only when the daemon's prompt
	// write CANNOT complete into the stdin pipe buffer — i.e. the
	// initial prompt exceeds the pipe capacity (64 KiB on Linux). Then
	// both orderings converge on the same answer: the parent blocks
	// mid-write and this process's exit breaks the pipe (EPIPE), or this
	// process is gone first and the very first write breaks it. A
	// scenario using these flags MUST size the daemon's executor prompt
	// accordingly (test/v1_s17_recovery_budget_test.go does, and asserts
	// it).
	FailSpawnFresh bool `json:"fail_spawn_fresh"`
}

// driveEnv is the shared state of one fake agent process.
type driveEnv struct {
	wi string
	// handle is the PUBLIC session fingerprint (PF_SESSION_HANDLE,
	// display only); spawnN is the v1.5 B2 derivation counter
	// (PF_SPAWN_N).
	handle     string
	spawnN     int
	sessionArg string
	resumed    bool

	mu         sync.Mutex // guards journal, transcript
	plan       *plan
	transcript string
	// mustFail: this derivation is scripted to die at birth, before it
	// reads the daemon's initial prompt (see plan.FailSpawnResume).
	mustFail bool
}

// newDriveEnv assembles the executor from the spawn identity (the capsule
// spawn journal's entry for THIS process — pid-matched; the deleted
// PF_SESSION_HANDLE / PF_SPAWN_N env injections are re-sourced from there,
// audit subtraction pass B) + the plan file.
// A missing plan parks the agent loudly (the scenario fails on its own
// bounded wait — never a silent no-op).
func newDriveEnv() *driveEnv {
	wi := os.Getenv("PF_WI_ID")
	entry, ok := spawnJournalSelf(wi)
	spawnN := entry.SpawnN
	if !ok || spawnN <= 0 {
		fmt.Fprintln(os.Stderr, "fakepi: PF_WI_ID unset or the spawn journal carries no derivation for this process; parking")
		return nil
	}
	pl, path := loadPlan(wi)
	if pl == nil {
		fmt.Fprintf(os.Stderr, "fakepi: no plan file for %s (looked at %s); parking\n", wi, path)
		return nil
	}
	d := &driveEnv{
		wi:         wi,
		handle:     entry.SessionHandle,
		spawnN:     spawnN,
		sessionArg: transcriptPathOf(entry),
		resumed:    hasSessionArg(),
		transcript: transcriptPathOf(entry),
	}
	if d.resumed && len(pl.ResumeActions) > 0 {
		pl.Actions = pl.ResumeActions
	}
	d.mustFail = mustFailSpawn(pl, d.resumed, spawnN)
	d.plan = pl
	return d
}

// mustFailSpawn decides whether THIS derivation is the scripted
// must-fail-at-spawn kind: a resume derivation under FailSpawnResume, or
// a fresh RE-derivation (counter > 1) under FailSpawnFresh. The original
// derivation (counter 1) is never selected by either flag.
func mustFailSpawn(pl *plan, resumed bool, spawnN int) bool {
	if resumed {
		return pl.FailSpawnResume
	}
	return pl.FailSpawnFresh && spawnN > 1
}
func hasSessionArg() bool {
	for i, a := range os.Args {
		if a == "--session" && i+1 < len(os.Args) {
			return true
		}
	}
	return false
}

// loadPlan reads the plan file: FAKEPI_PLANS/<wi>.json, falling back to
// FAKEPI_PLANS/default.json. The file is re-read at every process start,
// so a scenario rewrites it to steer a re-derived agent.
func loadPlan(wi string) (*plan, string) {
	dir := os.Getenv("FAKEPI_PLANS")
	if dir == "" {
		return nil, ""
	}
	path := filepath.Join(dir, wi+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		path = filepath.Join(dir, "default.json")
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, path
		}
	}
	var pl plan
	if json.Unmarshal(raw, &pl) != nil {
		return nil, path
	}
	return &pl, path
}

// ---------------------------------------------------------------------------
// runDriveLoop executes the plan actions in order. The plan is re-read at
// every process start, so a re-derived agent follows whatever the scenario
// wrote most recently.
func runDriveLoop(d *driveEnv) {
	// Boot settle: a real harness spends its first moments loading
	// context before the first action; the pause also keeps an
	// immediately-flushing plan clear of the spawn-window races (the
	// monitor adopts the new handle on the RTRecovered attach, and the
	// runtime finishes its RECOVERING→RUNNING transition, both of which
	// lag a just-spawned process by microseconds).
	sleepMS(250)
	// polyforge#106: envelope mode IS the mode. The WI-Agent RPC lane
	// this loop used to interpret (the scripted step/ask/wrap/repo
	// action vocabulary of the deleted engine) is gone: each step spawn
	// executes ONLY the envelope action matching its PF_SPAWN_N
	// (the Driver spawns each step as a separate process; all share this
	// plan file). The Nth envelope action in the plan fires for spawn N —
	// across fresh AND resume dispatches alike (every re-dispatch takes
	// the next ordinal, so resume_actions script crash + continuation in
	// one file, deterministically). No matching envelope action for this
	// spawn: park.
	envIdx := 0
	for _, a := range d.plan.Actions {
		if a.Do == "envelope" {
			envIdx++
			if envIdx == int(d.spawnNumber()) {
				d.runEnvelopeAction(a)
				parkForever()
				return
			}
		}
	}
	parkForever()
}

func sleepMS(n int) {
	if n <= 0 {
		n = 50
	}
	time.Sleep(time.Duration(n) * time.Millisecond)
}

// runEnvelopeAction performs one envelope action in full: the optional
// pre-write side effects (turn/noise records, repo, poison shapes, the
// ms delay), the crash shapes, and the envelope write itself (question
// text, output payload, command evidence, duplicates).
func (d *driveEnv) runEnvelopeAction(act planAction) {
	if act.Silent {
		// The zero-signal harness: no record at all (UI noise optionally),
		// no envelope, no exit.
		d.journal("silent", map[string]any{"step": act.ID, "pid": os.Getpid(), "spawn_n": d.spawnN})
		if act.Noise != "" {
			d.startNoise(act.Noise, act.NoiseMS)
		}
		parkForever()
		return
	}
	if act.ErrorTurn != "" {
		// F5: a turn that ends on a fatal provider error, then nothing.
		d.journal("error_turn", map[string]any{"step": act.ID, "error": act.ErrorTurn, "spawn_n": d.spawnN})
		emitAssistantTurn(d, "the provider connection dropped mid-reply", act.ErrorTurn)
		parkForever()
		return
	}
	if act.EndTurn {
		d.journal("end_turn", map[string]any{"step": act.ID, "spawn_n": d.spawnN})
		emitAssistantTurn(d, "I have finished my reply.", "")
		parkForever()
		return
	}
	if act.ToolMS > 0 || act.ToolHang {
		d.emitToolRun(act.ToolName, act.ToolMS, act.ToolHang)
	}
	if act.Park {
		// The silent-live wedge (polyforge#113's e2e-author shape): a
		// real harness whose provider call for the next turn never
		// returns. Open the turn (the in-flight silence the hang
		// watchdog discriminates on), then sit: no envelope write, no
		// exit, no further stdout record, zero activity — indefinitely.
		d.journal("park", map[string]any{
			"step": act.ID, "pid": os.Getpid(), "spawn_n": d.spawnN,
		})
		emitOpenTurn()
		parkForever()
		return
	}
	if act.OpenTurn {
		d.beginOpenTurn("open_turn")
	}
	if act.Noise != "" {
		d.startNoise(act.Noise, act.NoiseMS)
	}
	if act.Name != "" {
		d.makeRepo(act.Name, act.Origin)
	}
	if act.PoisonGitlink {
		// No execution.json write here: on the driver lane the DRIVER is
		// execution.json's single writer, and the envelope this action
		// precedes IS the version bump (the push lane's commit boundary).
		d.poisonGitlink()
	}
	if act.UnpoisonGitlink {
		d.unpoisonGitlink()
	}
	sleepMS(act.MS) // the delayed-envelope-write shape (the hang lane)
	// Extra capsule-relative files, verbatim content (the declared-input
	// producer: a downstream step's declared inputs must exist at its
	// dispatch).
	for p, content := range act.Files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(d.capsuleRoot(), p)), 0o755); err == nil {
			if err := os.WriteFile(filepath.Join(d.capsuleRoot(), p), []byte(content), 0o644); err != nil {
				d.journal("file_error", map[string]any{"path": p, "err": err.Error()})
			} else {
				d.journal("file", map[string]any{"path": p, "bytes": len(content)})
			}
		} else {
			d.journal("file_error", map[string]any{"path": p, "err": err.Error()})
		}
	}
	if act.Crash == "before" {
		d.journal("crash_before", map[string]any{"step": act.ID, "outcome": act.Outcome})
		if act.CrashStderr != "" {
			fmt.Fprintln(os.Stderr, act.CrashStderr)
		}
		os.Exit(101) // die before the envelope write: the envelope never lands
	}
	d.writeEnvelope(act)
	if act.Crash == "after" {
		d.journal("crash_after", map[string]any{"step": act.ID, "outcome": act.Outcome})
		os.Exit(101) // die after the write: the envelope is on the drop path
	}
}

// pinnedModelOf reads the --model argv the runtime pinned this derivation
// with ("" when the spawn carried no pin — the no-chain shape).
func pinnedModelOf() string {
	args := os.Args
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--model" {
			return args[i+1]
		}
	}
	return ""
}

func (d *driveEnv) beginOpenTurn(journalKind string) {
	d.journal(journalKind, map[string]any{"at": nowStamp()})
	emitOpenTurn()
}

// startNoise launches the F15 background record emitter: every interval
// milliseconds one record of the chosen family lands on stdout — kind
// ""/"ui" (default): the extension_ui_request shape a background-task
// extension's UI updates produce continuously (the pilot probe measured
// it dominating a stuck window's record stream — under the old progress
// semantics it kept the stream forever fresh); kind "progress": an
// ordinary message_update delta of the open turn — legal slow work
// (shape B: a turn streaming for a long time keeps proving life).
func (d *driveEnv) startNoise(kind string, intervalMS int) {
	if kind == "" {
		kind = "ui"
	}
	if intervalMS <= 0 {
		intervalMS = 100
	}
	d.journal("ui_noise", map[string]any{"kind": kind, "interval_ms": intervalMS})
	go func() {
		seq := 0
		for {
			if kind == "progress" {
				emitRecord(map[string]any{"type": "message_update",
					"assistantMessageEvent": map[string]any{
						"type": "text_delta", "contentIndex": 0, "delta": " still working",
					}})
			} else {
				emitRecord(map[string]any{"type": "extension_ui_request",
					"request": map[string]any{"method": "setStatus", "payload": "tick"},
					"seq":     seq})
			}
			seq++
			sleepMS(intervalMS)
		}
	}()
}

// badModel answers the pinned model this derivation was spawned with when
// the plan declares it unresolvable ("" otherwise).
func (d *driveEnv) badModel() string {
	m := pinnedModelOf()
	if m == "" {
		return ""
	}
	for _, b := range d.plan.BadModels {
		if b == m {
			return m
		}
	}
	return ""
}

// currentAction answers the envelope action this derivation executes
// (the Nth envelope action fires for spawn N); nil when none.
func (d *driveEnv) currentAction() *planAction {
	idx := 0
	for i := range d.plan.Actions {
		if d.plan.Actions[i].Do == "envelope" {
			idx++
			if idx == int(d.spawnNumber()) {
				return &d.plan.Actions[i]
			}
		}
	}
	return nil
}

// silentThisSpawn reports whether this derivation is the zero-signal
// harness: it must not even answer the initial instruction.
func (d *driveEnv) silentThisSpawn() bool {
	a := d.currentAction()
	return a != nil && a.Silent
}

// emitToolRun plays one tool execution as the real stream does: a model
// reply that ends in a tool call, tool_execution_start, the run, and
// tool_execution_end (unless hanging).
func (d *driveEnv) emitToolRun(name string, ms int, hang bool) {
	if name == "" {
		name = "bash"
	}
	msg := map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{}}
	emitRecord(map[string]any{"type": "agent_start"})
	emitRecord(map[string]any{"type": "turn_start"})
	emitRecord(map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant", "stopReason": "pending", "content": []any{}}})
	emitRecord(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": "running the tool"}})
	emitRecord(map[string]any{"type": "message_end", "message": msg})
	emitRecord(map[string]any{"type": "tool_execution_start", "toolCallId": "call_1", "toolName": name,
		"args": map[string]any{"command": "fakepi " + name}})
	d.journal("tool_start", map[string]any{"name": name, "ms": ms, "hang": hang})
	if hang {
		parkForever()
		return
	}
	sleepMS(ms)
	emitRecord(map[string]any{"type": "tool_execution_end", "toolCallId": "call_1", "toolName": name, "result": map[string]any{}})
	emitRecord(map[string]any{"type": "turn_end", "message": msg, "toolResults": []any{}})
	d.journal("tool_end", map[string]any{"name": name})
}

// appendTranscript lands one JSONL line on the harness transcript — the
// recovery engine's resume discriminator reads this file (exists,
// non-empty, first line parses as a JSON record).
func (d *driveEnv) appendTranscript(rec map[string]any) {
	d.mu.Lock()
	path := d.transcript
	d.mu.Unlock()
	if path == "" {
		return
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// journal appends one record to the capsule's assertion ledger.
func (d *driveEnv) journal(kind string, body map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	body = withKind(body, kind)
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

func withKind(body map[string]any, kind string) map[string]any {
	if body == nil {
		body = map[string]any{}
	}
	out := make(map[string]any, len(body)+2)
	for k, v := range body {
		out[k] = v
	}
	out["type"] = kind
	out["at"] = nowStamp()
	return out
}

// poisonGitlink lays an embedded git repository (with its own commit —
// the shape `git add -A` stages as a mode-160000 gitlink) OUTSIDE the
// gitignored repos/ tree.
func (d *driveEnv) poisonGitlink() {
	dir := filepath.Join("poison")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakepi: poison: git %v: %v: %s\n", args, err, out)
			return err
		}
		return nil
	}
	ok := run("init", "-q", "-b", "main", ".") == nil &&
		os.WriteFile(filepath.Join(dir, "inner.txt"), []byte("embedded history for "+d.wi+"\n"), 0o644) == nil &&
		run("-c", "user.name=fake-agent", "-c", "user.email=fake-agent@invalid", "add", "-A") == nil &&
		run("-c", "user.name=fake-agent", "-c", "user.email=fake-agent@invalid", "commit", "-q", "-m", "embedded history") == nil
	d.journal("poison_gitlink", map[string]any{"path": "poison", "laid": ok})
}

// unpoisonGitlink performs the refusal message's documented cleanup —
// delete the embedded repository's .git: the formerly embedded files
// become plain untracked work.
func (d *driveEnv) unpoisonGitlink() {
	err := os.RemoveAll(filepath.Join("poison", ".git"))
	// the staged gitlink (if the index still carried one) goes with it
	d.journal("unpoison_gitlink", map[string]any{"path": "poison", "err": errString(err)})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// makeRepo lays a real git worktree under repos/<name> with an origin
// remote — the ship scenario's inventory (the daemon's ship bridge pushes
// this worktree's HEAD to its own origin as the fixed-name branch).
// When an origin carries a main baseline, the worktree descends from it
// (the real-agent shape: clone the repo, work on top) so the origin's
// history and the shipped branch share a root — github's PR API refuses
// to open a PR for a head with no history in common with the base. An
// origin without a main (s04's fresh empty bare repo) keeps today's
// fresh-init worktree.
func (d *driveEnv) makeRepo(name, origin string) {
	if name == "" {
		name = "demo"
	}
	dir := filepath.Join("repos", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakepi: repo %s: git %v: %v: %s\n", name, args, err, out)
			return err
		}
		return nil
	}
	if err := run("init", "-q", "-b", "main"); err != nil {
		return
	}
	if origin != "" {
		_ = run("remote", "add", "origin", origin)
		if run("fetch", "--quiet", "origin", "main") == nil {
			_ = run("checkout", "--quiet", "-B", "main", "FETCH_HEAD")
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "work.txt"), []byte("fake agent repo work for "+d.wi+"\n"), 0o644)
	_ = run("add", "-A")
	_ = run("-c", "user.name=fake-agent", "-c", "user.email=fake-agent@invalid",
		"commit", "-q", "-m", "repo work "+d.wi)
	d.journal("repo", map[string]any{"name": name, "origin": origin})
}

// writeEnvelope writes a v2 result envelope to the pending-results drop
// path for the current spawn. The drop path is derived from the
// PF_SPAWN_N environment variable and the capsule root (PF_WI_ID env
// carries the WI; the capsule root is the process cwd).
//
// The needs_input question text rides the ENVELOPE level
// (question_id/question — the driver's delivery contract); an EMPTY
// question_id is emitted verbatim on purpose (the driver refuses a
// needs_input result without question text — the scripted refusal
// shape). The output payload defaults to a summary object and is
// replaced verbatim by the action's own when present. Structured command
// evidence rides command_evidence (the driver copies it onto
// execution.json's output.commands — the face the wrap evidence gate
// judges) unless the action suppressed it.
func (d *driveEnv) writeEnvelope(act planAction) {
	outcome := act.Outcome
	if outcome == "" {
		outcome = "completed"
	}
	stepID := act.ID
	capsuleRoot := d.capsuleRoot()
	spawnN := strconv.Itoa(d.spawnN)
	dropPath := filepath.Join(capsuleRoot, "runtime-state", "pending-results", spawnN+".json")

	output := act.Output
	if output == nil {
		output = map[string]interface{}{"summary": "envelope action"}
	}
	var evidence []interface{}
	if !act.NoEvidence {
		evidence = append(evidence, map[string]any{
			"argv":        []any{"fakepi", "envelope", stepID},
			"cwd":         capsuleRoot,
			"exit_code":   0,
			"duration_ms": 40,
			"started_at":  nowStamp(),
		})
		for _, e := range act.Evidence {
			evidence = append(evidence, e)
		}
	}

	envelope := map[string]interface{}{
		"protocol_version": 1,
		"result_id":        uuid.New().String(),
		"wi_id":            os.Getenv("PF_WI_ID"),
		"step_id":          stepID,
		"spawn_n":          parseIntDefault(spawnN, 1),
		"outcome":          outcome,
		"output":           output,
		"artifacts":        []interface{}{},
		"command_evidence": evidence,
		"submitted_at":     time.Now().UTC().Format(time.RFC3339),
	}
	if outcome == "needs_input" {
		// Verbatim, empty included: the empty question_id is the scripted
		// refusal shape (the driver must refuse it, never mint a pending).
		envelope["question_id"] = act.QuestionID
		envelope["question"] = act.Question
	}

	// Handoff duty: write the handoff file and reference it.
	if act.Handoff {
		hoPath := filepath.Join(capsuleRoot, "artifacts", "handoff-"+stepID+".md")
		hoContent := "# Handoff from " + stepID + "\n\nSummary for next step.\n"
		if err := os.MkdirAll(filepath.Dir(hoPath), 0o755); err == nil {
			os.WriteFile(hoPath, []byte(hoContent), 0o644)
			sum := sha256.Sum256([]byte(hoContent))
			envelope["handoff_ref"] = map[string]interface{}{
				"path":   "artifacts/handoff-" + stepID + ".md",
				"sha256": hex.EncodeToString(sum[:]),
				"bytes":  len(hoContent),
			}
		}
	}

	blob, _ := json.MarshalIndent(envelope, "", "  ")
	if err := os.MkdirAll(filepath.Dir(dropPath), 0o755); err != nil {
		d.journal("envelope_error", map[string]any{"err": err.Error()})
		return
	}
	dupes := act.Dup
	if dupes < 1 {
		dupes = 1
	}
	for i := 0; i < dupes; i++ {
		if err := os.WriteFile(dropPath, blob, 0o644); err != nil {
			d.journal("envelope_error", map[string]any{"err": err.Error()})
			return
		}
	}
	d.journal("envelope", map[string]any{"step": stepID, "outcome": outcome,
		"path": dropPath, "question_id": act.QuestionID, "dup": dupes,
		"evidence": !act.NoEvidence, "crash": act.Crash})
}

// capsuleRoot answers the capsule directory: the fakepi runs with the
// capsule as its cwd.
func (d *driveEnv) capsuleRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func parseIntDefault(s string, def int64) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return def
}

// spawnNumber answers this derivation's counter (the spawn-journal
// identity; 1 when unset).
func (d *driveEnv) spawnNumber() int64 {
	if d.spawnN > 0 {
		return int64(d.spawnN)
	}
	return 1
}
