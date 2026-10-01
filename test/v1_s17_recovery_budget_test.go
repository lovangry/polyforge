package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// v1_s17: the recovery budget runs OUT. The agent is SIGKILLed, the
// recovery engine starts its ladder from the surviving transcript, and
// every re-derivation fails AT SPAWN: resume, resume (MaxResumeFails=2
// → resumeDead), fresh (one failed recovery, the ladder is retried),
// fresh (the second failed recovery). That spends MaxRecoveries=2 and
// the work item bounded-fails — RTFinished(blocked), the aihub side
// blocked with waiting_on=owner and NO closed_at (blocked is
// resumable-by-owner, not closed), the job dropped from the daemon's
// live set. The whole ladder lives inside ONE recovery event: exactly
// one needs_recovery and ZERO wi_agent_rederived, because no
// re-derivation ever went live. It is the e2e half of the semantics
// runtime/recover_test.go pins in-process
// (TestRecoverBudgetTwoFailuresBoundedFail plus
// TestRecoverSingleFreshFailureIsRetriedNotTerminal): here the ladder
// runs through the REAL daemon, the real spawner and real agent
// processes, so "the spawn failed" means an actual broken stdin pipe
// rather than a scripted spawner error.
//
// WHY THE PROMPT IS HUGE (piPipeCapacity below). A failed spawn in this
// engine is exactly one thing: spawnAgent returning an error. With no
// production code in scope the only reachable shape is the initial
// prompt injection failing, which needs the derived process to be gone
// before the daemon's write to its stdin completes. A fakepi that just
// exits at birth does NOT reliably get there — both sides are a few
// hundred microseconds of start-up, and measured on this ladder's own
// shape the child loses that race ~93% of the time, in which case the
// prompt lands in the pipe buffer, the spawn SUCCEEDS and the death
// becomes an ordinary agent_death recovery that RESETS the budget:
// the wrong lane, and a coin-flip test. Sizing the initial prompt above
// the pipe capacity removes the race instead of narrowing it: the
// daemon's write cannot complete into the buffer, so it is still in
// flight when the must-fail derivation exits and it breaks with EPIPE —
// and in the other ordering (the child gone first) the very first write
// breaks. Both orderings, one answer.
func TestV1S17RecoveryBudgetBoundedFail(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s17: every re-derivation fails at spawn",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "park"},
		}, nil)
	// ONE plan for the whole run: the original derivation executes the
	// actions above, and every derivation the recovery ladder mints
	// afterwards dies at birth. The flags key on the derivation KIND
	// (--session for a resume, the PF_SPAWN_N counter > 1 for a fresh
	// re-derivation), so nothing has to be rewritten mid-run and there
	// is no race against the recovery spawn.
	setFailSpawnPlan(t, inst, wi, []map[string]any{
		{"do": "step", "id": "code"},
		{"do": "park"},
	})

	d := newV1Daemon(t, inst, optExecutorPrompt(oversizedExecutorPrompt(t)))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})

	// The original derivation is healthy: it runs its step and leaves a
	// readable transcript. Both halves matter — the transcript is the
	// recovery engine's ONLY discriminator, and it is what makes the
	// ladder start at RESUME rather than going straight to fresh.
	capDir := d.capsuleDir(wi)
	waitUntil(t, 30*time.Second, "first step never landed", func() bool {
		for _, rec := range fakeJournal(t, capDir) {
			if rec["type"] == "step" && rec["phase"] == "done" {
				return true
			}
		}
		return false
	})
	waitUntil(t, 30*time.Second, "the original derivation left no readable transcript", func() bool {
		return capsuleTranscriptReadable(t, capDir)
	})

	// SIGKILL: no flush, no notice — the confirmed-death recovery.
	pid := d.agentPIDOf(t, wi)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}

	rec := waitForEvent(t, d, wi, "needs_recovery", 60*time.Second)
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body, &body); err != nil {
		t.Fatalf("needs_recovery body: %v (%s)", err, rec.Body)
	}
	if body.Reason != "agent_death" {
		t.Fatalf("recovery reason %q, want agent_death", body.Reason)
	}

	// The terminal state: the budget is spent and the WI is blocked.
	inst.waitWIStatus(t, wi, "blocked", 120*time.Second)

	// blocked is waiting-on-owner, NOT closed: the owner can still
	// resume it (that is the whole difference from a terminal failure).
	item, err := inst.client.GetWorkItem(wi)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	if item.WaitingOn == nil || *item.WaitingOn != "owner" {
		t.Fatalf("blocked work item waiting_on = %v, want \"owner\"", item.WaitingOn)
	}
	if item.ClosedAt != nil {
		t.Fatalf("blocked work item must not be closed, got closed_at %q", *item.ClosedAt)
	}

	// T5 (M2): the bounded fail CLEARED the session row — blocked with a
	// live session would be revivable by a restart and the storm guard
	// would be a lie. The public audit block names no holder.
	if sess := inst.sessionOf(t, wi); sess.Fingerprint != "" {
		t.Fatalf("blocked work item still names session %q (M2: bounded fail must clear the session)", sess.Fingerprint)
	}
	// And the cleared row keeps the work item OUT of the claimable set:
	// the CAS gate answers the typed NOT_CLAIMABLE refusal on the wire
	// (a queued status is required; blocked is excluded until a human
	// unblocks it).
	refuseClaim(t, inst, wi, "NOT_CLAIMABLE")

	// The daemon dropped the job: no relaunch storm, nothing live.
	waitUntil(t, 30*time.Second, "the bounded-failed job never left the daemon's live set", func() bool {
		for _, j := range d.status(t).Jobs {
			if j.WorkItemID == wi {
				return false
			}
		}
		return true
	})

	// ONE recovery event for the WHOLE ladder, and no re-derivation ever
	// went live: wi_agent_rederived rides RTRecovered, which a failed
	// ladder never sends.
	kinds := d.eventKinds(t, wi)
	if n := countOf(kinds, "needs_recovery"); n != 1 {
		t.Fatalf("needs_recovery fired %d times (the ladder must stay inside one recovery): %v", n, kinds)
	}
	if n := countOf(kinds, "wi_agent_rederived"); n != 0 {
		t.Fatalf("re-derivation count %d, want 0 (no derivation went live): %v", n, kinds)
	}

	// The spawn journal pins the ladder shape. Failed derivations ARE
	// journaled — the parent writes the identity line before injecting
	// the prompt — so the file carries the original plus the four
	// failures, in ladder order.
	spawns := spawnJournal(t, capDir)
	if len(spawns) != 5 {
		t.Fatalf("spawn journal has %d entries, want 5 (original + resume,resume,fresh,fresh): %+v",
			len(spawns), spawns)
	}
	wantResume := []bool{false, true, true, false, false}
	for i, want := range wantResume {
		if got, _ := spawns[i]["resume"].(bool); got != want {
			t.Fatalf("spawn %d resume=%v, want %v (ladder shape %+v)", i, got, want, spawns)
		}
	}
	// spawnN advances only on a SUCCESSFUL spawn, so all four failures
	// carry the same local display token AND the same pending counter
	// (2 — the ordinal the ladder kept retrying): the original's handle
	// plus #1.
	attempt, _ := spawns[0]["instance"].(string)
	if attempt == "" || strings.Contains(attempt, "#") {
		t.Fatalf("original derivation instance %q, want the bare session handle", attempt)
	}
	for i := 1; i < len(spawns); i++ {
		if got, _ := spawns[i]["instance"].(string); got != attempt+"#1" {
			t.Fatalf("failed derivation %d instance %q, want %q", i, got, attempt+"#1")
		}
		if n, _ := spawns[i]["spawn_n"].(float64); int(n) != 2 {
			t.Fatalf("failed derivation %d spawn_n %v, want 2 (the counter advances only on a successful spawn)", i, spawns[i]["spawn_n"])
		}
	}

	// And the agent side agrees it died at birth every time: four
	// must-fail births, two resume and two fresh, and no second "spawn"
	// line (a must-fail derivation never reaches the live-agent path).
	var failResume, failFresh, live int
	for _, r := range fakeJournal(t, capDir) {
		switch r["type"] {
		case "spawn_fail":
			if resumed, _ := r["resume"].(bool); resumed {
				failResume++
			} else {
				failFresh++
			}
		case "spawn":
			live++
		}
	}
	if failResume != 2 || failFresh != 2 {
		t.Fatalf("must-fail births: %d resume + %d fresh, want 2 + 2", failResume, failFresh)
	}
	if live != 1 {
		t.Fatalf("%d live derivations, want 1 (only the original)", live)
	}
}

// piPipeCapacity is Linux's default pipe buffer (16 pages). The
// must-fail-at-spawn mode is deterministic only above it; see the
// scenario's header and test/fakepi/drive.go's pipe-capacity contract.
const piPipeCapacity = 64 << 10

// oversizedExecutorPrompt writes an executor preamble several times the
// pipe capacity and returns its path. The daemon builds every initial
// prompt from this file, so the prompt injection of a derivation that
// does not read its stdin cannot complete — it blocks, and breaks when
// that derivation exits.
func oversizedExecutorPrompt(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("# executor preamble (s17 must-fail ladder)\n\n")
	for b.Len() < 4*piPipeCapacity {
		b.WriteString("Padding: the initial prompt must not fit in the agent's stdin pipe buffer.\n")
	}
	if b.Len() <= piPipeCapacity {
		t.Fatalf("executor prompt %d bytes, must exceed the pipe capacity %d", b.Len(), piPipeCapacity)
	}
	path := filepath.Join(t.TempDir(), "executor-prompt.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// setFailSpawnPlan writes a plan whose RE-derivations — resume and fresh
// alike — die at birth, leaving the original derivation to run actions
// normally.
func setFailSpawnPlan(t *testing.T, u *v1Install, wi string, actions []map[string]any) {
	t.Helper()
	blob, err := json.Marshal(map[string]any{
		"actions":           actions,
		"fail_spawn_resume": true,
		"fail_spawn_fresh":  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u.planDir, wi+".json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

// capsuleTranscriptReadable applies the recovery engine's discriminator
// to the capsule's declared session copy: a file exists, is non-empty
// and its first line parses as a JSON record.
func capsuleTranscriptReadable(t *testing.T, capsuleDir string) bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(capsuleDir, "session", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) == 0 {
			continue
		}
		first := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
		if first == "" {
			continue
		}
		var probe map[string]any
		if json.Unmarshal([]byte(first), &probe) == nil {
			return true
		}
	}
	return false
}
