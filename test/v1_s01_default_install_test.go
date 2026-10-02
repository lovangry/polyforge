package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v1_s01: default-install smoke. A real aihub, a real `pf daemon`
// (interactive), a real `pf submit`, a scripted agent that walks two
// steps, the capsule git chain visible on the server side BEFORE the
// close (the observe lane's async push), a real `pf wrap`, the terminal
// wrapped state, and the sealed chain carrying the transcript — plus the
// no-orphan invariant.
func TestV1S01DefaultInstallSmoke(t *testing.T) {
	inst, remote := newV1InstallTaskRepo(t)
	wi := inst.newWI(t, "s01: smoke the default install end to end",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "sleep", "ms": 1500}, // let the async push land the first step
			{"do": "step", "id": "review"},
		}, nil)
	d := newV1Daemon(t, inst)
	d.start()

	// Interactive submit through the real CLI.
	out := d.pf(t, "submit", wi)
	if !strings.Contains(out, "accepted") {
		t.Fatalf("submit answer: %q", out)
	}

	// The job runs: a live agent pid, a materialized capsule.
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})
	capDir := d.capsuleDir(wi)
	rawID, err := os.ReadFile(filepath.Join(capDir, "capsule.json"))
	if err != nil {
		t.Fatalf("capsule.json: %v", err)
	}
	var idnMap map[string]any
	if err := json.Unmarshal(rawID, &idnMap); err != nil {
		t.Fatalf("capsule.json unparsable: %v", err)
	}
	// v1.5 ASSERTION CHANGE: the v1 assertion (Generation==1, AttemptID!="")
	// pinned the deleted lineage; the fossil round removed even the zero
	// fields from the frozen Identity shape, so the correct pin is the
	// KEYS' ABSENCE — the runtime no longer writes generation/attempt_id
	// into capsule.json at all, and session ownership lives in the
	// daemon-private sync/session-handle marker (contract §5).
	for _, gone := range []string{"generation", "attempt_id"} {
		if _, ok := idnMap[gone]; ok {
			t.Fatalf("capsule.json still carries the deleted v1 lineage key %q: %v", gone, idnMap)
		}
	}
	if idnMap["work_item_id"] != wi || idnMap["bundle_name"] != "quick-fix" {
		t.Fatalf("capsule identity wrong: %v", idnMap)
	}
	// T1 session trail, first half: the claim set the hub's session row
	// — the public audit block names this machine and a fingerprint,
	// never a hash and never the secret.
	sess := inst.sessionOf(t, wi)
	if sess.Fingerprint == "" || sess.Machine != "m-e2e" {
		t.Fatalf("running work item carries no session block: %+v", sess)
	}
	// The claim's task-repo inventory was cloned before the agent
	// spawned: repos/demo-src is a real clone of the local fake remote,
	// its own .git, the remote's content, and the claim-time HEAD
	// recorded as the capsule baseline.
	repoDir := filepath.Join(capDir, "repos", "demo-src")
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		t.Fatalf("task repo not cloned into the capsule: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(repoDir, "hello.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("cloned task repo content wrong: %q, %v", b, err)
	}
	repos, _ := idnMap["repos"].(map[string]any)
	if got, _ := repos["demo-src"].(string); got != remote.head {
		t.Fatalf("capsule baseline for demo-src = %q, want remote HEAD %q", got, remote.head)
	}
	// The task repo stays outside the capsule's own git history.
	if inst.bareHasPath(t, wi, "repos") {
		t.Fatal("capsule chain must not carry repos/ (gitignored, own .git)")
	}
	// The materialized bundle rides the capsule.
	if _, err := os.Stat(filepath.Join(capDir, "workflows", "quick-fix", "workflow.md")); err != nil {
		t.Fatalf("materialized bundle missing: %v", err)
	}

	// The observe projection shows both steps done (the CLI face).
	waitUntil(t, 30*time.Second, "steps never reached done", func() bool {
		jv := d.observe(t, wi)
		return jv.Steps["code"] != nil && jv.Steps["code"].Status == "done" &&
			jv.Steps["review"] != nil && jv.Steps["review"].Status == "done"
	})

	// The capsule chain is visible on the SERVER side before the close:
	// the observe lane's async push landed the agent's writes — one
	// commit per landed wave, so the mid-run chain already has history.
	inst.waitBareCommits(t, wi, 2, 40*time.Second)

	// Close through the real CLI.
	d.pf(t, "wrap", wi)
	inst.waitWIStatus(t, wi, "wrapped", 30*time.Second)
	// T1 session trail, second half: the wrap CLEARED the session row —
	// a terminal work item names no holder (m4).
	if sess2 := inst.sessionOf(t, wi); sess2.Fingerprint != "" {
		t.Fatalf("wrapped work item still names session %q", sess2.Fingerprint)
	}

	// The sealed chain: seq trailers strictly increasing from 1, the
	// transcript and the execution state committed, the final push done.
	seqs := inst.bareSeqTrailers(t, wi)
	if len(seqs) < 2 {
		t.Fatalf("sealed chain too short: %d commits", len(seqs))
	}
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("seq trailer %d is %d, want %d (chain: %v)", i, s, i+1, seqs)
		}
	}
	if !inst.bareHasPath(t, wi, "runtime-state/execution.json") {
		t.Fatal("sealed chain misses runtime-state/execution.json")
	}
	transcript := inst.bareFileAtMain(t, wi, "runtime-state/execution.json")
	if !strings.Contains(transcript, `"work_item_id":"`+wi+`"`) {
		t.Fatalf("execution.json at main is not this work item's: %s", transcript)
	}
	// The harness transcript rides the chain (ruling C1).
	hasTranscript := false
	out2 := gitRun(t, inst.bareRepo(wi), "ls-tree", "--name-only", "-r", "main", "session")
	for _, l := range strings.Split(out2, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "session/session-") {
			hasTranscript = true
		}
	}
	if !hasTranscript {
		t.Fatalf("sealed chain misses the harness transcript:\n%s", out2)
	}

	// The daemon holds no job for a sealed work item.
	if st := d.status(t); len(st.Jobs) != 0 {
		t.Fatalf("daemon still holds jobs after wrap: %+v", st.Jobs)
	}

	// No orphaned agent survives the close (PF_WI_ID marker scan).
	waitUntil(t, 10*time.Second, "agent processes survived the wrap", func() bool {
		return len(orphanPIDs(wi)) == 0
	})

	// The events file tells the whole story in order.
	kinds := d.eventKinds(t, wi)
	for _, want := range []string{"job_started", "published"} {
		if !contains(kinds, want) {
			t.Fatalf("events %v miss %q", kinds, want)
		}
	}
	if contains(kinds, "needs_recovery") {
		t.Fatalf("healthy run raised a recovery: %v", kinds)
	}
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
