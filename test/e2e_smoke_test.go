package test

// e2e_smoke_test.go: deliverable 1 — ONE end-to-end smoke over the real
// components in-process: Core (real internal/server + SQLite), Daemon
// (real internal/daemon + UDS face), driven through the REAL internal/cli
// clients wherever the CLI can express the call (create / ready queue /
// progress read-through / claim / observe / answer / pause-verbs), with
// raw UDS calls only where the CLI cannot forge the negative shapes.
//
// The hop chain (each one asserted, with the typed error envelopes):
//
//	create execution wi -> claim -> Capsule checkout -> publish chain
//	(block -> manifest -> HEAD, manifest_sha == Core's stored head) ->
//	progress read-through via observe layer-1 (job alive) -> needs_input
//	round trip (answer relay, five-field verbatim, X-PF-User) ->
//	end-generation via wrap (flush evidence) -> wrapped status +
//	ready-queue evolution (wrapped wi absent from the queue).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/cli"
	"github.com/silgrid/polyforge/internal/daemon"
)

func TestE2ESmokeClaimPublishObserveAnswerWrap(t *testing.T) {
	core := newCoreEnv(t)
	mach := newDaemonEnv(t, core)
	mach.serve()
	pf := mach.pf()          // CLI daemon client (UDS)
	corepf := core.pf(core.workerKey) // CLI Core client (HTTP)

	// --- create: two execution tickets; both land in the ready queue. ---
	wi := core.newWI(t, "e2e smoke: full lifecycle")
	wi2 := core.newWI(t, "e2e smoke: stays queued")
	if s := core.wiStatus(wi); s != "queued" {
		t.Fatalf("fresh wi status: %s", s)
	}
	q := readyQueue(t, core)
	if !q.hasExecutable(wi) || !q.hasExecutable(wi2) {
		t.Fatalf("both fresh wis must be executable: %+v", q)
	}

	// --- claim through the CLI submit face (route spawns the job). ---
	if raw, err := pf.Claim(wi, ""); err != nil {
		t.Fatalf("pf claim: %v", err)
	} else {
		var sub cli.SubmitResponse
		json.Unmarshal(raw, &sub)
		if !sub.Accepted || sub.WorkItemID != wi {
			t.Fatalf("claim answer: %s", raw)
		}
	}
	waitUntil(t, "the claim to derive the WI Agent", func() bool {
		return len(mach.spawner.spawns) == 1
	})
	// The claimed WI left the ready queue (running is not schedulable).
	if q = readyQueue(t, core); q.hasExecutable(wi) || q.hasBlocked(wi) {
		t.Fatalf("claimed wi must leave the ready queue: %+v", q)
	}

	// --- Capsule checkout (03 §6.1 create/checkout/harness chain). ---
	capc, idn := mach.openCapsule(wi)
	if idn.Status != capsule.StatusReady || idn.Generation != 1 {
		t.Fatalf("capsule identity: %+v", idn)
	}
	for _, d := range []string{"runtime-state", "artifacts", "logs", ".pi/agents"} {
		if fi, err := os.Stat(filepath.Join(capc.Dir, filepath.FromSlash(d))); err != nil || !fi.IsDir() {
			t.Fatalf("capsule layout dir %s missing: %v", d, err)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(capc.Dir, "repos", "inventory.json")); err != nil {
		t.Fatalf("inventory checkout must write repos/inventory.json: %v", err)
	} else if !strings.Contains(string(raw), `"repos"`) {
		t.Fatalf("inventory snapshot shape: %s", raw)
	}
	sess, err := capc.ReadSessionIdentity("pi")
	if err != nil || !strings.HasPrefix(sess.SessionID, "sess_"+wi+"_1") {
		t.Fatalf("session identity: %+v %v", sess, err)
	}
	sp := mach.spawner.spawns[0]
	if !strings.Contains(sp.InitialPrompt, "# pf-execute") || !strings.Contains(sp.InitialPrompt, "bundle_name: demo") {
		t.Fatalf("initial instruction must inject pf-execute + bundle ref: %.200s", sp.InitialPrompt)
	}
	if sp.Generation != 1 || sp.SessionID != sess.SessionID || sp.Adapter != "pi" {
		t.Fatalf("spawn params: %+v", sp)
	}
	if j := mach.state().FindJob(wi); j == nil || j.MainAgentPID == 0 || j.Status != daemon.JobRunning {
		t.Fatalf("daemon state jobs[] after spawn: %+v", j)
	}

	// --- the WI Agent works; publish chain block -> manifest -> HEAD. ---
	writeExecution(t, capc, wi, 1, "code", 1)
	writeCapsuleFile(t, capc, "artifacts/notes.md", "# working notes\nstep code in progress\n")
	raw, err := pf.PublishRequest(wi, "step_completed")
	if err != nil {
		t.Fatalf("pf publish_request: %v", err)
	}
	var pub daemon.PublishOutcome
	if err := json.Unmarshal(raw, &pub); err != nil {
		t.Fatalf("publish outcome: %s", raw)
	}
	if !pub.Published || pub.HeadSeq != 0 {
		t.Fatalf("publish outcome: %+v", pub)
	}
	// manifest_sha equals the Core's stored HEAD row (row 15 -> row 16).
	if hseq, hsha := core.headRow(wi); hseq != 0 || hsha != pub.ManifestSha {
		t.Fatalf("core head (%d,%s) != publish outcome (%d,%s)", hseq, hsha, pub.HeadSeq, pub.ManifestSha)
	}
	// The manifest's blocks really live in the Core CAS (data before state).
	for _, op := range opsOfHead(t, core, wi, 0) {
		if op.Op == "put" {
			if _, err := os.Stat(core.casPath(wi, op.Sha256)); err != nil {
				t.Fatalf("block %s for %s not in Core CAS: %v", op.Sha256, op.Path, err)
			}
		}
	}

	// --- observe layer-1 (job alive): live ring + state snapshot. ---
	raw, err = pf.Observe(wi, 0)
	if err != nil {
		t.Fatalf("pf observe: %v", err)
	}
	var obs cli.ObserveResponse
	if err := json.Unmarshal(raw, &obs); err != nil {
		t.Fatalf("observe body: %s", raw)
	}
	if obs.ReadThrough {
		t.Fatal("a live local job must answer on layer 1, not read-through")
	}
	seen := map[string]bool{}
	for _, ev := range obs.Events {
		seen[ev.Type] = true
	}
	if !seen[daemon.EvJobStarted] || !seen[daemon.EvPublished] {
		t.Fatalf("layer-1 events must include job_started + published: %s", raw)
	}
	if !strings.Contains(string(obs.StateSnapshot), `"current_step":"code"`) {
		t.Fatalf("layer-1 snapshot must render execution.json: %s", obs.StateSnapshot)
	}
	// Core progress read-through (02 §5.2): the latest published HEAD,
	// rendered verbatim — at most one publish behind the live capsule.
	raw, err = corepf.GetProgress(wi)
	if err != nil {
		t.Fatalf("pf progress: %v", err)
	}
	var prog struct {
		HeadSeq  *int64          `json:"head_seq"`
		Progress json.RawMessage `json:"progress"`
	}
	if err := json.Unmarshal(raw, &prog); err != nil || prog.HeadSeq == nil || *prog.HeadSeq != 0 {
		t.Fatalf("progress read-through: %s", raw)
	}
	if !strings.Contains(string(prog.Progress), `"current_step":"code"`) {
		t.Fatalf("read-through progress must render the published execution.json: %s", prog.Progress)
	}

	// --- needs_input: park the step on an open pending_question, publish,
	// observe it in the 00 §3.4a shape, then the answer round trip. ---
	question := parkNeedsInput(t, capc, wi, 1, "q_smoke01")
	if _, err := pf.PublishRequest(wi, "step_interrupted"); err != nil {
		t.Fatalf("publish needs_input state: %v", err)
	}
	raw, err = pf.Observe(wi, obs.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	obs = cli.ObserveResponse{}
	json.Unmarshal(raw, &obs)
	var snap struct {
		PendingQuestion *struct {
			QuestionID string `json:"question_id"`
			StepID     string `json:"step_id"`
			AskedAt    string `json:"asked_at"`
			Content    string `json:"content"`
			Status     string `json:"status"`
		} `json:"pending_question"`
	}
	if err := json.Unmarshal(obs.StateSnapshot, &snap); err != nil {
		t.Fatalf("layer-1 snapshot: %s", obs.StateSnapshot)
	}
	pq := snap.PendingQuestion
	if pq == nil || pq.QuestionID != question.QuestionID || pq.StepID != "code" ||
		pq.AskedAt != question.AskedAt || pq.Content != question.Content || pq.Status != "pending" {
		t.Fatalf("00 §3.4a pending question shape: %+v (want %+v)", pq, question)
	}

	// Answer identity rule first (00 §3.4a): answered_by must equal the
	// authenticated caller (X-PF-User); a mismatch is DAEMON-403.
	bad := cli.AnswerPayload{
		QuestionID: question.QuestionID, AnswerID: "a_bad", AnswerText: "nope",
		AnsweredAt: daemon.NowStamp(), AnsweredBy: "usr_other",
	}
	if code, env := udsErr(t, mach.socket, "POST", "/jobs/"+wi+"/answer", bad,
		map[string]string{"X-PF-User": "usr_worker"}); code != 403 || env.Error.Code != "DAEMON-403" {
		t.Fatalf("identity mismatch: %d %+v", code, env)
	}

	// The real round trip: five fields, relayed verbatim via X-PF-User.
	relay := cli.AnswerPayload{
		QuestionID: question.QuestionID, AnswerID: "a_smoke01", AnswerText: "Take design B",
		AnsweredAt: daemon.NowStamp(), AnsweredBy: "usr_worker",
	}
	agent := mach.spawner.last()
	teachAnswerConsumption(agent, capc, t)
	if raw, err := pf.Answer(wi, relay); err != nil {
		t.Fatalf("pf answer: %v", err)
	} else if !strings.Contains(string(raw), `"applied":true`) {
		t.Fatalf("answer answer: %s", raw)
	}
	// Five-field verbatim: the agent received exactly the relay as sent.
	got := agent.relays()
	if len(got) != 1 {
		t.Fatalf("the agent must receive exactly one relay: %+v", got)
	}
	if got[0].QuestionID != relay.QuestionID || got[0].AnswerID != relay.AnswerID ||
		got[0].AnswerText != relay.AnswerText || got[0].AnsweredAt != relay.AnsweredAt ||
		got[0].AnsweredBy != relay.AnsweredBy {
		t.Fatalf("relay not verbatim: %+v (sent %+v)", got[0], relay)
	}
	// Consumption cleared the question; the answered event is on the ring.
	raw, _ = pf.Observe(wi, obs.NextCursor)
	obs = cli.ObserveResponse{}
	json.Unmarshal(raw, &obs)
	var snap2 struct {
		PendingQuestion *struct {
			QuestionID string `json:"question_id"`
			StepID     string `json:"step_id"`
			AskedAt    string `json:"asked_at"`
			Content    string `json:"content"`
			Status     string `json:"status"`
		} `json:"pending_question"`
	}
	if err := json.Unmarshal(obs.StateSnapshot, &snap2); err != nil {
		t.Fatalf("layer-1 snapshot: %s", obs.StateSnapshot)
	}
	if snap2.PendingQuestion != nil {
		t.Fatalf("consumed question must be cleared: %s", obs.StateSnapshot)
	}
	answered := false
	for _, ev := range obs.Events {
		if ev.Type == daemon.EvAnswered {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("the answered event must ride the ring: %s", raw)
	}
	// Idempotency (D9): replaying the same answer_id returns the same
	// first result and never applies twice.
	if raw, err := pf.Answer(wi, relay); err != nil {
		t.Fatalf("answer replay: %v", err)
	} else if !strings.Contains(string(raw), `"applied":true`) {
		t.Fatalf("answer replay must be idempotent: %s", raw)
	}
	if n := len(agent.relays()); n != 1 {
		t.Fatalf("replayed answer must not re-deliver: %d", n)
	}
	// A NEW answer_id against the consumed question is DAEMON-409.
	stale := relay
	stale.AnswerID = "a_smoke02"
	if code, env := udsErr(t, mach.socket, "POST", "/jobs/"+wi+"/answer", stale, nil); code != 409 ||
		env.Error.Code != "DAEMON-409" {
		t.Fatalf("consumed question: %d %+v", code, env)
	}

	// --- end-generation via wrap: the final flush publishes the answer's
	// effect and the close carries the daemon-generated evidence. ---
	raw, err = pf.Wrap(wi, "wrap")
	if err != nil {
		t.Fatalf("pf wrap: %v", err)
	}
	fin := daemonFinish(t, raw)
	if fin.Reason != "wrap" || fin.WorkItemID != wi || fin.FlushEvidence == nil {
		t.Fatalf("wrap outcome: %+v", fin)
	}
	fe := fin.FlushEvidence
	if fe.PublishedGeneration != 1 || fe.PublishedSeq != fin.HeadSeq || fe.ManifestSha != fin.ManifestSha {
		t.Fatalf("flush evidence: %+v", fe)
	}
	if fe.FrozenCheckpoint == nil || !fe.FrozenCheckpoint.MatchesHead {
		t.Fatalf("frozen checkpoint: %+v", fe.FrozenCheckpoint)
	}
	// The final flush did a REAL publish (the answered execution.json had
	// not been published): HEAD advanced by one, under generation 1.
	if hseq, hsha := core.headRow(wi); hseq != fin.HeadSeq || hsha != fin.ManifestSha || core.headGeneration(wi) != 1 {
		t.Fatalf("core head (%d,%s,gen%d) != wrap outcome (%d,%s)",
			hseq, hsha, core.headGeneration(wi), fin.HeadSeq, fin.ManifestSha)
	}

	// --- wrapped status + ready-queue evolution. ---
	if s := core.wiStatus(wi); s != "wrapped" {
		t.Fatalf("wi status after wrap: %s", s)
	}
	if ends := core.genEnds(wi); len(ends) != 1 || ends[1] != "wrap" {
		t.Fatalf("exactly one generation must be ended by wrap: %+v", ends)
	}
	if q = readyQueue(t, core); q.hasExecutable(wi) || q.hasBlocked(wi) {
		t.Fatalf("wrapped wi must be absent from the ready queue: %+v", q)
	}
	if !q.hasExecutable(wi2) {
		t.Fatalf("the untouched queued wi must remain executable: %+v", q)
	}
	// Local residue: job sealed and gone, capsule sealed read-only.
	if j := mach.state().FindJob(wi); j != nil {
		t.Fatalf("jobs[] must be empty after wrap: %+v", j)
	}
	if fi, err := os.Stat(capc.Dir); err != nil || fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("capsule must remain on disk, sealed read-only: %v", err)
	}
	// Observe now falls to layer-2: no local capsule, Core read-through.
	raw, err = pf.Observe(wi, 0)
	if err != nil {
		t.Fatal(err)
	}
	obs = cli.ObserveResponse{}
	json.Unmarshal(raw, &obs)
	if !obs.ReadThrough || obs.HeadSeq == nil || *obs.HeadSeq != fin.HeadSeq || obs.FreshnessNote == "" {
		t.Fatalf("layer-2 observe after wrap: %s", raw)
	}
	if !strings.Contains(string(obs.StateSnapshot), `"answers"`) {
		t.Fatalf("read-through must render the published answered execution.json: %s", obs.StateSnapshot)
	}
}

// opsOfHead decodes one revision's manifest ops off the Core's on-disk
// audit copy (the same file the read-through reconstruction reads).
func opsOfHead(t *testing.T, core *coreEnv, wi string, seq int64) []opView {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(core.dataDir, "capsules", wi, "rev", strconv64(seq), "manifest.json"))
	if err != nil {
		t.Fatalf("core manifest audit copy: %v", err)
	}
	var m struct {
		Ops []opView `json:"ops"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest unparsable: %v", err)
	}
	return m.Ops
}

type opView struct {
	Op     string `json:"op"`
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}
