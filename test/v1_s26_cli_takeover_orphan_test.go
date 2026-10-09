package test

// v1_s26_cli_takeover_orphan_test.go: the polyforge#109 repro oracles —
// the #97 orphaned-holder wedge, authored by the repro step as the
// failing oracle on the broken tree (the s25 pattern: the fix lane lands
// the same file as the acceptance proof, unmodified).
//
// THE SCENE (#97, 2026-10-08 night, replayed with production
// credentials): a paused WI whose holder daemon runs under its real
// machine identity — the machine user registered through the heartbeat
// face, project member "writer" — and an operator who runs `pf takeover`
// from a terminal. The daemon is NOT the bootstrap admin: it holds the
// machine credential every real execution box holds.
//
// What the broken tree does (observed, deterministic):
//
//	$ pf takeover <id> --reason ...
//	  session: <fp> (machine cli-<hostname>, owner usr_root)   ← the mint
//	$ pf submit <id>          → "accepted"; the dispatcher then logs
//	  daemon: start <id> (claim) failed: CORE-409: the work item is
//	          already held by session <orphan-fp>
//	$ pf submit <id> --takeover → "accepted"; the dispatcher logs
//	  daemon: start <id> (takeover) failed: AUTH-403: force-takeover
//	          requires admin or project maintainer   ← no fence, no claim
//	$ pf resume <id>          → clean_pause cleared; the holder machine
//	          (cli-<hostname>) never attaches — it is not a daemon
//	holder-daemon restart     → "held by session <orphan-fp> — local
//	          tree killed, capsule quarantined, record dropped"; the WI
//	          stays held by the orphan at the hub, forever
//
// Both CLI submits answer exit 0 ("accepted by the local daemon") — the
// refusal is visible only in the daemon log, which is exactly how the
// #97 operator experienced "both submit and submit --takeover refused".
//
// The two oracles below pin the WI's acceptance:
//
//   - TestV1S26CLITakeoverNoOrphan: paused WI → CLI takeover → the local
//     daemon's machine identity, and the local daemon can attach and
//     run it (no orphan) — acceptance e2e #1.
//   - TestV1S26SubmitTakeoverFencesForeignHolder: `pf submit
//     --takeover` fences a foreign holder and the machine-credential
//     daemon claims and runs the WI — acceptance e2e #2, and #3 (the
//     #97 scene recovers without hub-side manual surgery: the foreign
//     holder stands in for the dead orphan session).
//
// NOTE (fix lane, #109 — the two deltas from the repro step's bytes,
// both proven necessary, neither weakens an oracle):
//   1. s26Plan gains the post-answer RE-ENTRY ordinal (grill completed,
//      the s02/s11 convention): without it the driver re-dispatches the
//      parked step after the answer and the wrap tail dies with
//      `driver: stepRun.Wait defect: ... envelope names step "fix",
//      dispatch is "grill"` — the repro's own oracles could never pass
//      even on a fully fixed tree (root-cause XP5/XP6: with the ordinal
//      inserted the same scenes pass end to end).
//   2. Oracle 1's `pf takeover` runs against the box's daemon
//      (daemonA.pf sets PF_DAEMON_SOCKET): on the fixed tree the verb
//      is a participant of the local daemon — it reads the daemon's
//      machine id (GET /status) and hands the minted secret to it
//      (custody) — so the operator's terminal must address the daemon,
//      exactly as `pf submit` always has. The old inst.pf call pointed
//      at the default system socket, which nothing in the harness
//      listens on.
//
// NOTE (why s11 never caught this): v1_s11 drives daemon-side takeovers
// with the bootstrap ADMIN key, where the force-takeover authorization
// gate is silently satisfied. The machine credential — the shape every
// production daemon holds — is what the 403 refusal needs, so the bug
// was invisible to the whole existing takeover lane.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// registerMachineCred registers one machine identity through the REAL
// heartbeat face (bootstrap-admin credential admits the first
// registration) and returns the machine user's id plus the PLAINTEXT
// api key the face issues exactly once — the credential a production
// `pf daemon` runs with. The machine user is created with role "writer"
// (internal/aihub/core/admin.go registration), i.e. NOT admin and NOT
// a project maintainer.
func registerMachineCred(t *testing.T, inst *v1Install, machineID string) (string, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"machine_id": machineID, "live_wi_ids": []string{}})
	req, err := http.NewRequest("POST", inst.url+"/api/v1/machines/heartbeat", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+inst.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := inst.client.HTTP.Do(req)
	if err != nil {
		t.Fatalf("machine %s registration heartbeat: %v", machineID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("machine %s registration heartbeat: HTTP %d: %s", machineID, resp.StatusCode, raw)
	}
	var out struct {
		Registered bool   `json:"registered"`
		UserID     string `json:"user_id"`
		APIKey     struct {
			Plain string `json:"plain"`
		} `json:"api_key"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("machine %s registration body unparsable: %v (%s)", machineID, err, raw)
	}
	if !out.Registered || out.UserID == "" || out.APIKey.Plain == "" {
		t.Fatalf("machine %s registration incomplete: %s", machineID, raw)
	}
	return out.UserID, out.APIKey.Plain
}

// addProjectMember adds one user to the e2e project with the given role
// through the real PATCH /projects face (admin credential).
func addProjectMember(t *testing.T, inst *v1Install, userID, role string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"members": []map[string]string{{"user_id": userID, "role": role}},
	})
	req, err := http.NewRequest("PATCH", inst.url+"/api/v1/projects/"+v1Project, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+inst.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := inst.client.HTTP.Do(req)
	if err != nil {
		t.Fatalf("project membership patch (%s as %s): %v", userID, role, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("project membership patch (%s as %s): HTTP %d: %s", userID, role, resp.StatusCode, raw)
	}
}

// cliHostnameDefault is the machine identity the CLI-side takeover
// reports when neither --machine nor POLYFORGE_MACHINE_ID is set: the
// "cli-<hostname>" spelling no daemon ever runs as.
func cliHostnameDefault() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return "cli-" + host
}

// s26Plan is the parked-question plan both oracles run: ordinal 1 parks
// on q_s26 (the pre-takeover hold, the #97 paused shape); ordinal 2 is
// the RE-ENTRY of the parked step after the answer (the s02/s11 harness
// convention — the driver re-dispatches the parked step once the owner
// answers); ordinals 3+ carry the continuation to the wrap.
func s26Plan() []map[string]any {
	return []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": "q_s26",
			"question": "s26: parked across the takeover."},
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
		{"do": "envelope", "id": "release", "outcome": "completed"},
	}
}

// TestV1S26CLITakeoverNoOrphan: acceptance e2e #1 — a paused WI, an
// operator's `pf takeover` from the CLI (addressed to THIS box's
// daemon — the fixed verb reads its machine id and hands it the minted
// secret), and the session the CLI mints must be one this box's daemon
// holds/attaches as: the local daemon's machine identity, never the
// "cli-<hostname>" orphan spelling. The recovery tail then proves
// attachability end to end: the operator's self-service path (the
// daemon-side takeover the verb itself directs: "the session secret is
// in the local daemon's custody; `pf submit <id> --takeover` runs the
// work item on it") re-runs the parked question on the local daemon and
// the workflow wraps. On the broken tree this fails at the mint: the
// hub session row's machine is "cli-<hostname>" — an identity no daemon
// can attach as, the #97 orphaned holder.
func TestV1S26CLITakeoverNoOrphan(t *testing.T) {
	inst := newV1Install(t)
	uidA, keyA := registerMachineCred(t, inst, "m-e2e-a")
	addProjectMember(t, inst, uidA, "writer")

	wi := inst.newWI(t, "s26: CLI takeover must not mint an orphan session (#109)",
		s26Plan(), nil)
	daemonA := newV1Daemon(t, inst, optMachineID("m-e2e-a"), optAPIKey(keyA), optUseDriver())
	daemonA.start()
	daemonA.pf(t, "submit", wi)
	waitQuestion(t, daemonA, wi, "q_s26", 60*time.Second)
	t.Cleanup(func() { killOrphans(wi) })

	// The #97 shape: the WI is PAUSED on the holder daemon (session kept
	// at the hub, capsule record retained locally), then the operator
	// takes it over from a terminal with the CLI's own credential — the
	// terminal addressed to THIS box's daemon (PF_DAEMON_SOCKET), the
	// fixed verb's participant.
	daemonA.pf(t, "pause", wi)
	inst.waitWIStatus(t, wi, "paused", 30*time.Second)
	// The paused chain must be durable before any takeover clones it (the
	// parked question rides the capsule git chain; s11 §4.3).
	inst.waitBareCommits(t, wi, 1, 30*time.Second)
	sessBefore := inst.sessionOf(t, wi)
	if sessBefore.Machine != "m-e2e-a" || sessBefore.Fingerprint == "" {
		t.Fatalf("holder baseline wrong before the CLI takeover: %+v", sessBefore)
	}

	out := daemonA.pf(t, "takeover", wi, "--reason", "s26: the #97 operator move")
	sessAfter := inst.sessionOf(t, wi)

	// ORACLE 1 — the mint: the freshly minted session must carry the
	// LOCAL DAEMON's machine identity. The broken tree reports
	// "cli-<hostname>" (observed: machine cli-opencode, owner usr_root) —
	// an identity no daemon ever runs as, so the holder can never attach
	// and the WI is wedged the moment the CLI process exits.
	if sessAfter.Machine == cliHostnameDefault() {
		t.Fatalf("pf takeover minted the session under %q — an identity no daemon on this box runs as "+
			"(the #97 orphaned-holder mint; the hub row: %+v). The local daemon runs as %q; a session "+
			"minted by an operator on this box must be attachable by this box's daemon.\npf takeover output:\n%s",
			sessAfter.Machine, sessAfter, "m-e2e-a", out)
	}
	if sessAfter.Machine != "m-e2e-a" || sessAfter.Fingerprint == "" || sessAfter.Fingerprint == sessBefore.Fingerprint {
		t.Fatalf("pf takeover minted an unusable session: %+v (was %+v); want a fresh session under "+
			"the local daemon's machine id m-e2e-a", sessAfter, sessBefore)
	}

	// ORACLE 2 — attachability, end to end: the operator's self-service
	// recovery path (the one `pf takeover`'s own output directs: a
	// daemon-side takeover) fences the CLI-minted session and the LOCAL
	// daemon claims and runs the WI — no orphan, no hub-side surgery.
	daemonA.pf(t, "submit", wi, "--takeover")
	waitQuestion(t, daemonA, wi, "q_s26", 90*time.Second)
	answerQuestion(t, daemonA, wi, "q_s26", "s26: the CLI-takeover continuation is cleared.")
	inst.waitWIStatus(t, wi, "wrapped", 180*time.Second)
	if sess := inst.sessionOf(t, wi); sess.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sess.Fingerprint)
	}
}

// TestV1S26SubmitTakeoverFencesForeignHolder: acceptance e2e #2 and #3 —
// `pf submit --takeover` from a MACHINE-CREDENTIAL daemon (machine user,
// project role writer — the production shape) must fence a foreign
// holder and claim the WI; the #97 scene recovers without hub-side
// manual surgery. The foreign holder here is a second machine's paused
// session (deterministic and fix-agnostic; on the broken tree the dead
// orphan session is the same shape: a holder this daemon cannot be).
//
// On the broken tree this fails at the fence: the CLI answers
// "accepted by the local daemon" (exit 0) but the daemon's takeover
// start is refused — observed dispatcher line: `daemon: start <id>
// (takeover) failed: AUTH-403: force-takeover requires admin or project
// maintainer` — the hub session never moves, and the WI stays held by
// the foreign machine with no self-service recovery (the plain-submit
// leg dies on CORE-409 "already held by session ...").
func TestV1S26SubmitTakeoverFencesForeignHolder(t *testing.T) {
	inst := newV1Install(t)
	// Two production-credential machines: m-e2e-b is the foreign holder,
	// m-e2e-a is this box's daemon attempting the self-service recovery.
	uidB, keyB := registerMachineCred(t, inst, "m-e2e-b")
	addProjectMember(t, inst, uidB, "writer")
	uidA, keyA := registerMachineCred(t, inst, "m-e2e-a")
	addProjectMember(t, inst, uidA, "writer")

	wi := inst.newWI(t, "s26: submit --takeover must fence a foreign holder (#109)",
		s26Plan(), nil)
	daemonB := newV1Daemon(t, inst, optMachineID("m-e2e-b"), optAPIKey(keyB), optUseDriver())
	daemonB.start()
	daemonB.pf(t, "submit", wi)
	waitQuestion(t, daemonB, wi, "q_s26", 60*time.Second)
	t.Cleanup(func() { killOrphans(wi) })

	// The foreign hold: the WI is paused under m-e2e-b's session (parked,
	// retained, exactly the #97 paused shape) and m-e2e-b's daemon stays
	// alive-but-idle — the fencing-on-use design leaves it to learn at
	// its next fenced use.
	daemonB.pf(t, "pause", wi)
	inst.waitWIStatus(t, wi, "paused", 30*time.Second)
	// The paused chain must be durable before the takeover clones it (the
	// parked question rides the capsule git chain; s11 §4.3).
	inst.waitBareCommits(t, wi, 1, 30*time.Second)
	sessB := inst.sessionOf(t, wi)
	if sessB.Machine != "m-e2e-b" || sessB.Fingerprint == "" {
		t.Fatalf("foreign holder baseline wrong: %+v", sessB)
	}

	// This box's daemon, holding only its machine credential.
	daemonA := newV1Daemon(t, inst, optMachineID("m-e2e-a"), optAPIKey(keyA), optUseDriver())
	daemonA.start()
	daemonA.pf(t, "submit", wi, "--takeover")

	// ORACLE — the fence and the claim: the hub session must swap off the
	// foreign holder onto m-e2e-a's fresh session. On the broken tree the
	// CLI's "accepted" is followed by silence: the dispatcher's takeover
	// start is refused and the holder never moves.
	var sessA wiSession
	deadline := time.Now().Add(30 * time.Second)
	for {
		sessA = inst.sessionOf(t, wi)
		if sessA.Fingerprint != "" && sessA.Fingerprint != sessB.Fingerprint && sessA.Machine == "m-e2e-a" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("`pf submit --takeover` never fenced the foreign holder: the hub session is still "+
				"%+v (want a fresh session under machine m-e2e-a; the foreign holder was %+v). The CLI "+
				"accepted the submit — the refusal is in the daemon log:\n%s",
				sessA, sessB, daemonA.logTail())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The claimed WI RUNS on the machine-credential daemon: the parked
	// question reopens on A, the answer unlocks the continuation, and the
	// workflow wraps — the #97 scene recovered without hub-side surgery.
	waitQuestion(t, daemonA, wi, "q_s26", 90*time.Second)
	answerQuestion(t, daemonA, wi, "q_s26", "s26: the fenced takeover continuation is cleared.")
	inst.waitWIStatus(t, wi, "wrapped", 180*time.Second)
	if sess := inst.sessionOf(t, wi); sess.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sess.Fingerprint)
	}
	// The fenced foreign holder kept its own record untouched (B still
	// holds its retained paused record; the hub row belongs to A) — the
	// two-machine shape s11 pins in full; here only the no-orphan claim
	// is this oracle's business.
	for _, j := range daemonB.status(t).Jobs {
		if j.WorkItemID == wi && j.Status != "paused" {
			t.Fatalf("daemon B's record moved unexpectedly: %+v", j)
		}
	}
}
