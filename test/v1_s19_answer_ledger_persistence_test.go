package test

// v1_s19: cross-restart durability of the observe answer ledger. The
// daemon-side answer ledger is no longer memory-only: the entries it
// settles (the `awaiting` relays and the `applied` idempotency
// outcomes) live in `answers.json` under the daemon data directory, so
// a daemon restart or crash can neither strand an owner's unconsumed
// answer nor turn a duplicate submission into a second delivery.
//
// These are acceptance scenarios authored against the spec
// (artifacts/spec.md of wi_tEnGWCxYEXzw), not against the ledger's
// internals: everything asserted here is observable through the real
// surfaces the owner has — the `pf answer` CLI, the capsule's fake-agent
// journal, the daemon's events.log, the sealed execution.json and the
// bytes of `answers.json` itself.
//
// Scenarios:
//
//   - A: an accepted-but-unconsumed answer is on disk as a versioned,
//     0600, in-flight-free `answers.json` entry.
//   - B: the daemon is SIGKILLed while the answer is awaiting; the
//     restarted daemon reloads the ledger, auto-redelivers the relay to
//     the re-derived instance, the run wraps, and the finished work
//     item's entries are pruned from the file.
//   - C: an answer consumed before a daemon crash replays its recorded
//     outcome after the restart — same answer_id, no second pf/answer.
//   - D: a corrupt `answers.json` is quarantined at startup; the daemon
//     boots and serves a normal run.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Scenario A: an accepted, delivered-but-unconsumed answer is durable on
// disk — versioned JSON, 0600, no in-flight state, relay verbatim.

func TestV1S19AwaitingAnswerIsPersistedToDisk(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_s19a"
	actions := []map[string]any{
		{"do": "step", "id": "code"},
		{"do": "ask", "id": qid, "question": "Should the ledger survive a restart?"},
		{"do": "park"},
	}
	wi := inst.newWI(t, "s19 A: awaiting answer persisted to answers.json", actions, nil)
	// The instance lands pf/answer but never consumes or acks it: the
	// answer settles delivered-but-unconsumed, i.e. `retryable` +
	// `awaiting` — the half the file must carry.
	writePlanExtra(t, inst, wi, actions, nil, map[string]any{"ack_answer": false})

	d := newV1Daemon(t, inst, optObserveWindows(0, 0, 0, 3*time.Second))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool { return d.agentPIDOf(t, wi) > 0 })
	waitUntil(t, 30*time.Second, "pending question never became visible", func() bool {
		jv := d.observe(t, wi)
		return jv.Pending != nil && jv.Pending.ID == qid
	})

	const answerID = "a_s19a"
	const answerText = "Yes: persist both halves of the ledger."
	out := d.pf(t, "answer", wi, "--text", answerText, "--question", qid, "--answer-id", answerID, "--by", "usr_root")
	if !strings.Contains(out, answerID) {
		t.Fatalf("answer outcome: %q", out)
	}

	// The relay is on disk, verbatim, in the versioned format.
	waitUntil(t, 10*time.Second, "the accepted answer never reached "+answersPath(d), func() bool {
		return answersFileHas(t, d, answerID)
	})
	doc := assertLedgerFileShape(t, d)
	raw := readAnswersFile(t, d)
	for _, field := range []string{qid, answerID, answerText, "usr_root", wi} {
		if !strings.Contains(raw, field) {
			t.Fatalf("persisted ledger misses %q (the relay must be stored verbatim, keyed by work item):\n%s", field, raw)
		}
	}
	// answered_at is the fifth relay field: whatever timestamp the CLI
	// minted, the file must carry it (a today-dated RFC3339 stamp).
	if !strings.Contains(raw, time.Now().UTC().Format("2006-01-02")) {
		t.Fatalf("persisted ledger misses the relay's answered_at stamp:\n%s", raw)
	}
	if len(doc) < 2 {
		t.Fatalf("persisted ledger carries only %d top-level fields (version + the two halves expected): %s", len(doc), raw)
	}

	// The agent journal proves this is the delivered-but-unconsumed
	// shape: one suppressed landing, nothing archived in the capsule.
	lands := answerControls(t, d.capsuleDir(wi))
	if len(lands) != 1 || lands[0]["suppressed"] != true {
		t.Fatalf("expected exactly one suppressed pf/answer landing, got %+v", lands)
	}
}

// ---------------------------------------------------------------------------
// Scenario B: daemon SIGKILL while the answer is awaiting → the restart
// reloads the ledger, re-delivers automatically, the run wraps, and the
// finished work item's entries leave the file.

func TestV1S19AwaitingAnswerRedeliveredAfterDaemonCrash(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_s19b"
	// The resume plan's ask carries a DIFFERENT id on purpose: it must
	// never reach the capsule or a pf/answer correlation.
	const decoyQID = "q_s19b_decoy"
	actions := []map[string]any{
		{"do": "step", "id": "code"},
		{"do": "ask", "id": qid, "question": "Which variant should the recovery take?"},
		{"do": "step", "id": "polish"},
		{"do": "wrap"},
	}
	// The resume derivation re-presents the PERSISTED question id (the
	// decoy id must lose) and then finishes the run.
	resume := []map[string]any{
		{"do": "ask", "id": decoyQID, "question": "Which variant should the recovery take?"},
		{"do": "step", "id": "polish"},
		{"do": "wrap"},
	}
	wi := inst.newWI(t, "s19 B: awaiting answer survives a daemon crash", actions, resume)
	writePlanExtra(t, inst, wi, actions, resume, map[string]any{"ack_answer": false})

	d := newV1Daemon(t, inst, optObserveWindows(0, 0, 0, 3*time.Second))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool { return d.agentPIDOf(t, wi) > 0 })
	waitUntil(t, 30*time.Second, "pending question never became visible", func() bool {
		jv := d.observe(t, wi)
		return jv.Pending != nil && jv.Pending.ID == qid
	})

	const answerID = "a_s19b"
	const answerText = "Take the durable variant."
	d.pf(t, "answer", wi, "--text", answerText, "--question", qid, "--answer-id", answerID, "--by", "usr_root")
	waitUntil(t, 10*time.Second, "the accepted answer never reached "+answersPath(d), func() bool {
		return answersFileHas(t, d, answerID, answerText)
	})
	assertLedgerFileShape(t, d)

	// Let the successor consume normally (the resume derivation re-reads
	// the plan file at process start).
	inst.setPlanFull(t, wi, actions, resume)

	// Crash the daemon hard. The agent survives (parked on its question)
	// and the ledger file is all the restart has.
	d.crash()
	if !answersFileHas(t, d, answerID) {
		t.Fatalf("the awaiting entry did not survive the crash:\n%s", readAnswersFile(t, d))
	}

	d2 := restartSameDaemon(t, d, "daemon2.log")
	waitUntil(t, 60*time.Second, "restarted daemon never re-derived the agent", func() bool {
		return d2.agentPIDOf(t, wi) > 0
	})

	// The restored `awaiting` entry is re-delivered automatically at the
	// attach of the post-restart instance, consumed, and the run wraps.
	waitForEvent(t, d2, wi, "answer_redelivery", 60*time.Second)
	waitForEvent(t, d2, wi, "answered", 60*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

	// The relay reached the successor verbatim and was archived in the
	// capsule (the persistent authority), pending question cleared.
	exec := inst.bareFileAtMain(t, wi, "runtime-state/execution.json")
	// The answer TEXT and the QUESTION ID are the relay fields the
	// capsule archive can carry: capsule.ArchivedAnswer is
	// question_id/answer/answered_by/answered_at and deliberately has no
	// answer_id — that identifier is daemon-ledger scoped (the pf/answer
	// control envelope does not carry it either, protocol.ControlMsg is
	// method/seq/id=question_id/answer), so it is asserted against
	// answers.json above (answersFileHas) and below (the prune). Same
	// contract as the S02 reference scenario.
	if !strings.Contains(exec, answerText) {
		t.Fatalf("sealed execution.json misses the re-delivered answer:\n%s", exec)
	}
	// The re-delivery was bound to the PERSISTED question id, never to
	// the resume plan's decoy (mint once, re-present verbatim): the decoy
	// must appear nowhere, and the persisted id must be archived with the
	// answer.
	if strings.Contains(exec, decoyQID) {
		t.Fatalf("sealed execution.json carries the resume plan's decoy question id %s:\n%s", decoyQID, exec)
	}
	if !strings.Contains(exec, qid) {
		t.Fatalf("sealed execution.json misses the persisted question id %s of the archived answer:\n%s", qid, exec)
	}
	if strings.Contains(exec, `"pending_question"`) {
		t.Fatalf("sealed execution.json still carries a pending question:\n%s", exec)
	}
	// The agent journal: the LAST pf/answer landing is the post-restart
	// re-delivery, consumed (not suppressed) and correlated to the same
	// question id — i.e. the restored entry was re-sent verbatim, and no
	// landing ever carried the decoy.
	lands := answerControls(t, d2.capsuleDir(wi))
	if len(lands) < 2 {
		t.Fatalf("pf/answer landings: got %d, want at least 2 (one suppressed pre-crash, one re-delivered after the restart): %+v", len(lands), lands)
	}
	last := lands[len(lands)-1]
	if last["suppressed"] == true || last["ref"] != qid {
		t.Fatalf("the post-restart landing must be a normal consumption correlated to %s: %+v", qid, last)
	}
	for i, rec := range lands {
		if rec["ref"] == decoyQID {
			t.Fatalf("pf/answer landing #%d was bound to the decoy question id %s: %+v", i, decoyQID, rec)
		}
	}
	// Exactly one needs_input across the whole run: the re-delivery is
	// not a second question.
	if n := countOf(d2.eventKinds(t, wi), "needs_input"); n != 1 {
		t.Fatalf("needs_input fired %d times, want 1: %v", n, d2.eventKinds(t, wi))
	}

	// The finished work item's entries are pruned from the file (the
	// file may itself be gone only if nothing else is persisted; both
	// are acceptable, the entry must not be there).
	waitUntil(t, 30*time.Second, "the finished work item's entries were never pruned from "+answersPath(d2), func() bool {
		raw := readAnswersFile(t, d2)
		return raw == "" || (!strings.Contains(raw, answerID) && !strings.Contains(raw, wi))
	})
}

// ---------------------------------------------------------------------------
// Scenario C: cross-restart idempotency — a consumed answer_id replays
// its recorded outcome and never produces a second delivery.

func TestV1S19FinalOutcomeReplayedAfterDaemonCrash(t *testing.T) {
	inst := newV1Install(t)
	const qid = "q_s19c"
	actions := []map[string]any{
		{"do": "step", "id": "code"},
		{"do": "ask", "id": qid, "question": "Replay or re-deliver after a restart?"},
		{"do": "park"},
	}
	// The resumed derivation just parks: the work item stays live (so
	// nothing is pruned) while the duplicate submission is tested.
	resume := []map[string]any{{"do": "park"}}
	wi := inst.newWI(t, "s19 C: duplicate answer_id after a restart", actions, resume)

	d := newV1Daemon(t, inst, optObserveWindows(0, 0, 0, 10*time.Second))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool { return d.agentPIDOf(t, wi) > 0 })
	waitUntil(t, 30*time.Second, "pending question never became visible", func() bool {
		jv := d.observe(t, wi)
		return jv.Pending != nil && jv.Pending.ID == qid
	})

	const answerID = "a_s19c"
	const answerText = "Replay the recorded outcome."
	// answered_at is re-minted by the CLI on the duplicate; the ledger's
	// recorded outcome (not the new stamp) is what must come back.
	first := d.pf(t, "answer", wi, "--text", answerText, "--question", qid, "--answer-id", answerID, "--by", "usr_root")
	waitForEvent(t, d, wi, "answered", 30*time.Second)

	capDir := d.capsuleDir(wi)
	waitUntil(t, 30*time.Second, "the answer was never consumed by the agent", func() bool {
		lands := answerControls(t, capDir)
		return len(lands) == 1 && lands[0]["suppressed"] != true
	})
	// The settled outcome is on disk before the crash.
	waitUntil(t, 10*time.Second, "the settled outcome never reached "+answersPath(d), func() bool {
		return answersFileHas(t, d, answerID)
	})
	assertLedgerFileShape(t, d)

	// Crash and restart on the same state.
	d.crash()
	d2 := restartSameDaemon(t, d, "daemon2.log")
	waitUntil(t, 60*time.Second, "restarted daemon never re-derived the agent", func() bool {
		return d2.agentPIDOf(t, wi) > 0
	})

	// The SAME answer_id, submitted again after the restart: accepted
	// with the recorded (nil) outcome, no second delivery.
	second := d2.pf(t, "answer", wi, "--text", answerText, "--question", qid, "--answer-id", answerID, "--by", "usr_root")
	if !strings.Contains(second, answerID) {
		t.Fatalf("post-restart duplicate submission outcome: %q (first was %q)", second, first)
	}
	// Give any (forbidden) second delivery time to land on the journal.
	time.Sleep(3 * time.Second)
	if lands := answerControls(t, capDir); len(lands) != 1 {
		t.Fatalf("pf/answer landings: got %d, want 1 (a duplicate answer_id must never re-deliver): %+v", len(lands), lands)
	}
	// The work item is still live, so its settled entry is still on disk
	// (retention drops entries on finish, not on replay).
	if !answersFileHas(t, d2, answerID) {
		t.Fatalf("the settled entry vanished from the ledger of a live work item:\n%s", readAnswersFile(t, d2))
	}
	assertLedgerFileShape(t, d2)
}

// ---------------------------------------------------------------------------
// Scenario D: a corrupt ledger file is quarantined at startup, never
// fatal — the daemon boots with an empty ledger and serves a normal run.

func TestV1S19CorruptLedgerFileQuarantinedAtStartup(t *testing.T) {
	inst := newV1Install(t)
	d := newV1Daemon(t, inst)

	// A file that is neither valid JSON nor a known version.
	const garbage = "{not json at all — s19 corruption probe"
	if err := os.WriteFile(answersPath(d), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}

	// The daemon must still come up (start() fails the test otherwise).
	d.start()

	// The bad file is moved aside, verbatim, under the quarantine name.
	var quarantined []string
	waitUntil(t, 20*time.Second, "the corrupt ledger was never quarantined", func() bool {
		hits, err := filepath.Glob(filepath.Join(d.dataDir, "answers.json.corrupt-*"))
		if err != nil {
			t.Fatal(err)
		}
		quarantined = hits
		return len(hits) > 0
	})
	if len(quarantined) != 1 {
		t.Fatalf("quarantine files: %v, want exactly 1", quarantined)
	}
	saved, err := os.ReadFile(quarantined[0])
	if err != nil {
		t.Fatalf("read quarantined ledger: %v", err)
	}
	if string(saved) != garbage {
		t.Fatalf("the quarantined file is not the original bytes: %q", saved)
	}
	stamp := strings.TrimPrefix(filepath.Base(quarantined[0]), "answers.json.corrupt-")
	if _, perr := time.Parse(time.RFC3339, stamp); perr != nil {
		t.Fatalf("quarantine suffix %q is not an RFC3339 UTC stamp: %v", stamp, perr)
	}
	// The corrupt file is no longer in the live path (either removed or
	// replaced by a fresh, empty, valid ledger).
	if raw := readAnswersFile(t, d); raw != "" {
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("the live ledger after quarantine is still unparsable: %v\n%s", err, raw)
		}
		if strings.Contains(raw, "corruption probe") {
			t.Fatalf("the corrupt bytes are still in the live ledger:\n%s", raw)
		}
	}

	// The startup recorded the quarantine (the spec requires one event;
	// it does not pin the kind's spelling, so the token is matched
	// loosely against the durable event log).
	logPath := filepath.Join(d.dataDir, "events.log")
	waitUntil(t, 20*time.Second, "no quarantine event in "+logPath, func() bool {
		raw, err := os.ReadFile(logPath)
		if err != nil {
			return false
		}
		low := strings.ToLower(string(raw))
		return strings.Contains(low, "quarantin") || strings.Contains(low, "corrupt")
	})

	// And the daemon is fully functional on the empty ledger: a normal
	// question/answer run still completes.
	const qid = "q_s19d"
	wi := inst.newWI(t, "s19 D: normal run after a quarantined ledger",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "ask", "id": qid, "question": "Does the daemon still serve answers?"},
			{"do": "wrap"},
		}, nil)
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool { return d.agentPIDOf(t, wi) > 0 })
	waitUntil(t, 30*time.Second, "pending question never became visible", func() bool {
		// observeSoft: the submit->monitored window answers DAEMON-404 by
		// design and must be polled through, not fatal'd on.
		jv := observeSoft(d, wi)
		return jv != nil && jv.Pending != nil && jv.Pending.ID == qid
	})
	d.pf(t, "answer", wi, "--text", "It does.", "--question", qid, "--answer-id", "a_s19d", "--by", "usr_root")
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)
	if _, err := os.Stat(answersPath(d)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat %s after the run: %v", answersPath(d), err)
	}
	fmt.Fprintf(os.Stderr, "s19 D: quarantined %s\n", filepath.Base(quarantined[0]))
}
