// Command fakepi is the v1 e2e stand-in for the real `pi --mode rpc`
// harness binary — envelope edition (polyforge#106: the WI-Agent RPC
// lane is deleted; the Go driver spawns one process per step). The
// daemon derives it as the step process (--pi-bin); it speaks the REAL
// v1 envelope pinned by internal/polyforge/runtime/spawn.go and the
// record shapes frozen in internal/polyforge/observe/stream.go:
//
//   - stdin: one JSON command object per line; prompt commands
//     {"id":..,"type":"prompt","message":..} are the only commands
//     honored;
//   - stdout: response records {"id","type":"response","command":"prompt",
//     "success":true} echo the command id; assistant text rides the
//     message_start/message_update/message_end event burst; tool
//     executions ride tool_execution_start/tool_execution_end;
//   - capsule-side execution state: runtime-state/execution.json is
//     written atomically (tmp+rename) with a MONOTONIC Version per
//     write — the v1 zero-blocking agent contract (ruling C3) the
//     observe lane polls;
//   - a harness transcript is appended at the session path the driver's
//     derivation declares (--session on a resume, the token-named
//     capsule copy session/session-<handle>[#N].jsonl otherwise), so
//     crash-recovery scenarios exercise the real resume path.
//
// Scripted behavior: the process reads a JSON plan file at startup
// (FAKEPI_PLANS/<PF_WI_ID>.json, falling back to
// FAKEPI_PLANS/default.json). The plan is re-read on every process
// start, so a scenario rewrites the file to change what a RE-DERIVED
// agent does. The Nth envelope action fires for spawn N (see drive.go).
//
// Besides the action list the plan carries per-derivation-kind modes.
// fail_spawn_resume / fail_spawn_fresh are the must-fail-AT-SPAWN mode:
// the selected derivation exits before reading one byte of stdin, so the
// daemon's initial-prompt injection breaks the pipe and Spawn returns an
// error — the spawn-failure ladder, as opposed to the observe lane's
// agent-death lane that every other death here produces. The original
// derivation is never selected (the flags key on `--session` and on
// the derivation counter PF_SPAWN_N: a fresh RE-derivation carries a
// counter > 1, the original carries 1). drive.go states the mode's
// pipe-capacity contract; it is not optional.
//
// Known deviation from the real harness (inherited from the v0 double):
// stdin EOF does not exit — the process parks alive, which the
// daemon-crash scenarios need (the WI agent must survive its parent's
// death). There is no model behind a turn: every "run" is a canned
// record burst.
//
// Test-only helper; nothing in production imports or references it.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// journalPath is the fake agent's own assertion ledger inside the
// capsule (logs/ is a committed sync surface, so the journal rides the
// capsule git chain and is assertable from the sealed scene too).
const journalPath = "logs/fakepi.jsonl"

// outMu serializes stdout record emission between the stdin command loop and
// the drive goroutine.
var outMu sync.Mutex

// drv is the drive-mode state (nil never happens here: the v1 fake is
// always a scripted executor).
var drv *driveEnv

func main() {
	drv = newDriveEnv()
	if drv == nil {
		parkForever()
	}
	if drv.mustFail {
		// The must-fail-at-spawn mode (plan.FailSpawnResume /
		// plan.FailSpawnFresh): die at birth. Nothing of this derivation
		// may be observable as a LIVE agent — no agent_settled record, no
		// transcript line, no stdin read — because the daemon's Spawn must
		// fail at prompt injection rather than hand back a working handle.
		// The journal line is the one exception and it is safe: the parent
		// already wrote this derivation's spawn-journal entry BEFORE the
		// prompt, so the failed derivation is on the record either way,
		// and a scenario needs to see that the mode really engaged.
		drv.journal("spawn_fail", map[string]any{
			"pid":     os.Getpid(),
			"handle":  drv.handle,
			"spawn_n": drv.spawnN,
			"resume":  drv.resumed,
			"wi":      drv.wi,
			"mode":    "must_fail_at_spawn",
		})
		os.Exit(9)
	}
	if bad := drv.badModel(); bad != "" {
		// the model-pin failure (F4): the harness cannot resolve the
		// pinned model and exits non-zero at startup with the error on
		// stderr — an exit WITHOUT a result envelope, model-class.
		drv.journal("model_fail", map[string]any{"model": bad, "spawn_n": drv.spawnN, "pid": os.Getpid()})
		fmt.Fprintf(os.Stderr, "error: model %q not found (unknown model); available models: none\n", bad)
		os.Exit(1)
	}
	drv.journal("spawn", map[string]any{
		"pid":     os.Getpid(),
		"handle":  drv.handle,
		"spawn_n": drv.spawnN,
		"resume":  drv.resumed,
		"session": drv.sessionArg,
		"model":   pinnedModelOf(),
		"wi":      drv.wi,
	})
	go runDriveLoop(drv)

	// RPC startup: the real harness emits agent_settled for the fresh
	// session before any prompt arrives.
	emitRecord(map[string]any{"type": "agent_settled"})
	drv.appendTranscript(map[string]any{"t": "spawn", "at": nowStamp(), "handle": drv.handle, "spawn_n": drv.spawnN, "resume": drv.resumed})

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var cmd struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &cmd) != nil || cmd.Type != "prompt" {
			continue
		}
		emitRecord(map[string]any{
			"id": cmd.ID, "type": "response", "command": "prompt", "success": true,
		})
		drv.journal("prompt", map[string]any{
			"spawn_n":                   drv.spawnN,
			"ended_without_result_hint": strings.Contains(cmd.Message, "ended without you writing the result envelope"),
		})
		// the initial instruction: a plain assistant turn — unless this
		// dispatch's action is the zero-signal harness (Silent: not even
		// the greeting turn; rule 0's shape)
		if !drv.silentThisSpawn() {
			emitAssistantTurn(drv, "fakepi: instruction received; standing by.", "")
		}
	}
	// stdin EOF (the daemon died): park alive — the crash drills need
	// the WI agent to survive the parent's kill.
	parkForever()
}

// emitAssistantTurn streams one complete assistant message as the real
// event sequence. stopReason != "" marks a provider-error turn: the
// failure rides the ordinary records (message_start, message_end AND
// turn_end all carry stopReason=error + errorMessage — there is no
// type:"error" record).
func emitAssistantTurn(d *driveEnv, text, errMsg string) {
	message := map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}
	if errMsg != "" {
		message["stopReason"] = "error"
		message["errorMessage"] = errMsg
	}
	usage := map[string]any{
		"input": 0, "output": len(text), "cacheRead": 0, "cacheWrite": 0,
		"totalTokens": len(text),
		"cost":        map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0},
	}
	firstDelta := text
	if len(firstDelta) > 16 { // two deltas: the ack line may straddle chunks
		firstDelta = text[:16]
	}
	emitRecord(map[string]any{"type": "agent_start"})
	emitRecord(map[string]any{"type": "turn_start"})
	emitRecord(map[string]any{"type": "message_start", "message": message})
	emitRecord(map[string]any{"type": "message_update", "usage": usage,
		"assistantMessageEvent": map[string]any{"type": "text_start", "contentIndex": 0}})
	emitRecord(map[string]any{"type": "message_update", "usage": usage,
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": firstDelta}})
	if firstDelta != text {
		emitRecord(map[string]any{"type": "message_update", "usage": usage,
			"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": text[len(firstDelta):]}})
	}
	emitRecord(map[string]any{"type": "message_update", "usage": usage,
		"assistantMessageEvent": map[string]any{"type": "text_end", "contentIndex": 0, "content": text}})
	emitRecord(map[string]any{"type": "message_end", "message": message})
	emitRecord(map[string]any{"type": "turn_end", "message": message, "toolResults": []any{}})
	emitRecord(map[string]any{"type": "agent_end", "messages": []any{message}, "willRetry": false})
	emitRecord(map[string]any{"type": "agent_settled"})
	if d != nil {
		d.appendTranscript(map[string]any{"t": "turn", "at": nowStamp(), "text": text, "stop_reason": stopOf(errMsg)})
	}
}

// emitOpenTurn opens a turn and never closes it: the in-flight silence
// the hang watchdog discriminates on.
func emitOpenTurn() {
	emitRecord(map[string]any{"type": "agent_start"})
	emitRecord(map[string]any{"type": "turn_start"})
	emitRecord(map[string]any{"type": "message_start", "message": map[string]any{
		"role": "assistant", "stopReason": "pending", "content": []any{},
	}})
}

func stopOf(errMsg string) string {
	if errMsg != "" {
		return "error"
	}
	return "stop"
}

// emitRecord writes ONE protocol record to stdout (JSON + LF).
func emitRecord(rec map[string]any) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Println(string(raw))
}

func parkForever() {
	for {
		time.Sleep(time.Hour)
	}
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// capsuleSessionHandle reads the PUBLIC session fingerprint from the
// spawn env (PF_SESSION_HANDLE, the v1.5 B3 identity: display only,
// NEVER a credential — the session secret never reaches agent env).
func capsuleSessionHandle() string {
	return os.Getenv("PF_SESSION_HANDLE")
}

// spawnNOf reads the derivation counter from the spawn env (0 when
// absent — never a legal derivation, which is exactly the fail-closed
// fence value the daemon refuses).
func spawnNOf() int {
	n, err := strconv.Atoi(os.Getenv("PF_SPAWN_N"))
	if err != nil {
		return 0
	}
	return n
}

// transcriptPathOf resolves this process's transcript file: the --session
// argv on a resume derivation, else the token-named capsule copy the
// runtime declared for this derivation (session/session-<handle>.jsonl
// for the first derivation, session/session-<handle>#<N-1>.jsonl for a
// fresh re-derivation #N — the path the runtime records in session.json
// and the spawn journal, rekeyed onto the session handle in v1.5).
func transcriptPathOf() string {
	args := os.Args
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--session" {
			return args[i+1]
		}
	}
	handle := capsuleSessionHandle()
	if handle == "" {
		return ""
	}
	token := handle
	if n := spawnNOf(); n > 1 {
		token = fmt.Sprintf("%s#%d", handle, n-1)
	}
	return filepath.Join("session", "session-"+token+".jsonl")
}
