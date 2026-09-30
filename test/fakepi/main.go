// Command fakepi is the v1 e2e stand-in for the real `pi --mode rpc`
// harness binary. The daemon derives it as the WI-agent process
// (--pi-bin); it speaks the REAL v1 RPC envelope pinned by
// internal/polyforge/runtime/spawn.go and the record shapes frozen in
// internal/polyforge/observe/stream.go:
//
//   - stdin: one JSON command object per line; prompt commands
//     {"id":..,"type":"prompt","message":..} are the only commands
//     honored; the daemon's control methods (pf/answer, pf/probe,
//     pf/stop_notice) ride prompt commands whose message text is the
//     protocol.ControlMsg JSON line VERBATIM;
//   - stdout: response records {"id","type":"response","command":"prompt",
//     "success":true} echo the command id; assistant text rides the
//     message_start/message_update/message_end event burst; the control
//     ACK is a JSON line embedded in a MULTI-LINE assistant message
//     (the v1 ack contract: the ack carries method + correlation —
//     pf/answer echoes the question id in "ref", pf/probe echoes the
//     nonce in "ref", pf/stop_notice carries no string token and acks
//     with the method alone);
//   - capsule-side execution state: runtime-state/execution.json is
//     written atomically (tmp+rename) with a MONOTONIC Version per
//     write — the v1 zero-blocking agent contract (ruling C3) the
//     observe lane polls;
//   - the agent-side UDS proxy: flush(reason) rides
//     POST /jobs/<wi>/flush on PF_DAEMON_SOCKET with the v1
//     protocol.FlushRequest shape; the attempt-id fence value is read
//     from PF_INSTANCE_ID (the spawn env injection);
//   - a harness transcript is appended at the session path the recovery
//     engine discriminates on (the --session argv on resume, the
//     capsule.json attempt_id otherwise), so crash-recovery scenarios
//     exercise the real resume path.
//
// Scripted behavior: the process reads a JSON plan file at startup
// (FAKEPI_PLANS/<PF_WI_ID>.json, falling back to
// FAKEPI_PLANS/default.json). The plan is re-read on every process
// start, so a scenario rewrites the file to change what a RE-DERIVED
// agent does. Actions: step / ask / wrap / fail / hang / provider_error /
// park / exit / sleep / repo (see drive.go).
//
// Besides the action list the plan carries per-derivation-kind modes.
// fail_spawn_resume / fail_spawn_fresh are the must-fail-AT-SPAWN mode:
// the selected derivation exits before reading one byte of stdin, so the
// daemon's initial-prompt injection breaks the pipe and Spawn returns an
// error — the spawn-failure ladder, as opposed to the observe lane's
// agent-death lane that every other death here produces. The original
// derivation is never selected (the flags key on `--session` and on the
// `#N` lineage suffix in PF_INSTANCE_ID). drive.go states the mode's
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
	"sync"
	"time"
)

// execPath is the capsule-side execution state file (cwd = capsule dir).
const execPath = "runtime-state/execution.json"

// journalPath is the fake agent's own assertion ledger inside the
// capsule (logs/ is a committed sync surface, so the journal rides the
// capsule git chain and is assertable from the sealed scene too).
const journalPath = "logs/fakepi.jsonl"

// outMu serializes stdout record emission between the control loop and
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
			"pid":      os.Getpid(),
			"instance": drv.instance,
			"resume":   drv.resumed,
			"wi":       drv.wi,
			"mode":     "must_fail_at_spawn",
		})
		os.Exit(9)
	}
	drv.journal("spawn", map[string]any{
		"pid":      os.Getpid(),
		"instance": drv.instance,
		"resume":   drv.resumed,
		"session":  drv.sessionArg,
		"wi":       drv.wi,
	})
	go runDriveLoop(drv)

	// RPC startup: the real harness emits agent_settled for the fresh
	// session before any prompt arrives.
	emitRecord(map[string]any{"type": "agent_settled"})
	drv.appendTranscript(map[string]any{"t": "spawn", "at": nowStamp(), "instance": drv.instance, "resume": drv.resumed})

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
		var ctl struct {
			Method string `json:"method"`
			Seq    int64  `json:"seq"`
			ID     string `json:"id"`
			Answer string `json:"answer"`
			Nonce  string `json:"nonce"`
		}
		if json.Unmarshal([]byte(cmd.Message), &ctl) != nil || ctl.Method == "" {
			// free text (the initial instruction): a plain assistant turn
			emitAssistantTurn(drv, "fakepi: instruction received; standing by for control methods.", "")
			continue
		}
		switch ctl.Method {
		case "pf/answer":
			if !drv.ackAnswer {
				// The died-mid-consumption shape: the relay reached stdin but
				// this instance never consumes or acks it — the daemon's ledger
				// keeps the answer re-deliverable (F9).
				drv.journal("control", map[string]any{"method": "pf/answer", "ref": ctl.ID, "suppressed": true})
				continue
			}
			drv.consumeAnswer(ctl.ID, ctl.Answer)
			ackViaAssistantMessage(map[string]any{"method": "pf/answer", "ref": ctl.ID, "ok": true})
			drv.journal("control", map[string]any{"method": "pf/answer", "ref": ctl.ID})
		case "pf/probe":
			if drv.suppressProbeAck() {
				// the hang scenario: no ack, the liveness probe must time out
				drv.journal("control", map[string]any{"method": "pf/probe", "suppressed": true, "nonce": ctl.Nonce})
				continue
			}
			ack := map[string]any{"method": "pf/probe", "ok": true}
			if ctl.Nonce != "" {
				ack["ref"] = ctl.Nonce
			} else {
				ack["seq"] = ctl.Seq
			}
			ackViaAssistantMessage(ack)
			drv.journal("control", map[string]any{"method": "pf/probe", "ref": ctl.Nonce})
		case "pf/stop_notice":
			ackViaAssistantMessage(map[string]any{"method": "pf/stop_notice", "ok": true})
			drv.journal("control", map[string]any{"method": "pf/stop_notice"})
			drv.beginShutdownFlush()
		default:
			drv.journal("control", map[string]any{"method": ctl.Method, "unknown": true})
		}
	}
	// stdin EOF (the daemon died): park alive — the crash drills need
	// the WI agent to survive the parent's kill.
	parkForever()
}

// ackViaAssistantMessage acks one control method the way the v1 transport
// contract defines: the ack JSON line is embedded in a MULTI-LINE
// assistant message — never a raw stdout line.
func ackViaAssistantMessage(ack map[string]any) {
	raw, _ := json.Marshal(ack)
	text := "control method received; acking at a safe boundary\n" + string(raw) + "\nstanding by"
	emitAssistantTurn(drv, text, "")
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

// capsuleAttemptID reads the aihub attempt id from the materialized
// capsule identity (the fresh-derivation transcript path derives from
// it; a resumed derivation gets the path from its --session argv).
func capsuleAttemptID() string {
	raw, err := os.ReadFile("capsule.json")
	if err != nil {
		return ""
	}
	var idn struct {
		AttemptID string `json:"attempt_id"`
	}
	if json.Unmarshal(raw, &idn) != nil {
		return ""
	}
	return idn.AttemptID
}

// transcriptPathOf resolves this process's transcript file: the --session
// argv on a resume derivation, else session/session-<attempt_id>.jsonl
// under the capsule (the path the runtime records in session.json).
func transcriptPathOf() string {
	args := os.Args
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--session" {
			return args[i+1]
		}
	}
	if a := capsuleAttemptID(); a != "" {
		return filepath.Join("session", "session-"+a+".jsonl")
	}
	return ""
}
