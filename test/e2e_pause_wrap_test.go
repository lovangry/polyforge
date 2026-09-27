package test

// e2e_pause_wrap_test.go: deliverable 3 — the pause/wrap acceptance
// criterion (04 §5.1 pause/wrap rows, merged amendment; 04 §6.2 A path):
//
//	pf pause  -> WI paused server-side (generation ended with reason
//	             "pause", no auto re-claim) + capsule sealed read-only +
//	             observe falls to the layer-2 Core read-through
//	pf resume -> a FRESH claim: generation 2, old capsule quarantined by
//	             capsule create, new session id
//	pf wrap   -> terminal close carrying generation-2 flush evidence
//
// Driven through the real internal/cli daemon verbs over the real UDS
// face; every state transition asserted on both sides (Core DB + daemon
// state.json + capsule dir).

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

func TestE2EPauseWrapResumeRoundTrip(t *testing.T) {
	core := newCoreEnv(t)
	mach := newDaemonEnv(t, core)
	mach.serve()
	pf := mach.pf()

	wi := core.newWI(t, "e2e pause/wrap round trip")
	capc, _ := mach.claimWorkingJob(wi) // claim + spawn + one written step
	oldDir := capc.Dir
	if _, err := pf.PublishRequest(wi, "step_completed"); err != nil {
		t.Fatalf("working publish: %v", err)
	}
	seqBefore, _ := core.headRow(wi)

	// --- pf pause: the 04 §6.2 A-path close with reason "pause". ---
	raw, err := pf.Pause(wi)
	if err != nil {
		t.Fatalf("pf pause: %v", err)
	}
	fin := daemonFinish(t, raw)
	if fin.Reason != "pause" || fin.FlushEvidence == nil || fin.FlushEvidence.PublishedGeneration != 1 {
		t.Fatalf("pause outcome: %+v", fin)
	}
	if s := core.wiStatus(wi); s != "paused" {
		t.Fatalf("wi must be paused server-side, got %s", s)
	}
	if ends := core.genEnds(wi); len(ends) != 1 || ends[1] != "pause" {
		t.Fatalf("generation 1 must be ended exactly once, reason pause: %+v", ends)
	}
	if core.eventCount(wi, "paused") != 1 {
		t.Fatal("exactly one paused event")
	}
	// No auto re-claim / re-queue: nothing running locally, capsule sealed.
	if j := mach.state().FindJob(wi); j != nil {
		t.Fatalf("jobs[] must be empty after pause: %+v", j)
	}
	if fi, err := os.Stat(oldDir); err != nil || fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("capsule must stay on disk sealed read-only: %v", err)
	}
	// Second pause: the job is sealed and gone — typed DAEMON-404.
	if code, env := udsErr(t, mach.socket, "POST", "/jobs/"+wi+"/pause", map[string]any{}, nil); code != 404 ||
		env.Error.Code != daemon.ErrCodeNotFound {
		t.Fatalf("second pause: %d %+v", code, env)
	}

	// --- observe falls to layer-2: Core read-through of the last HEAD. ---
	raw, err = pf.Observe(wi, 0)
	if err != nil {
		t.Fatal(err)
	}
	var obs cli.ObserveResponse
	json.Unmarshal(raw, &obs)
	if !obs.ReadThrough || obs.HeadSeq == nil || *obs.HeadSeq != seqBefore {
		t.Fatalf("layer-2 observe after pause: %s", raw)
	}
	if obs.FreshnessNote == "" {
		t.Fatal("layer-2 must carry the freshness note")
	}
	// The read-through renders the PUBLISHED execution.json verbatim.
	if !strings.Contains(string(obs.StateSnapshot), `"current_step":"code"`) {
		t.Fatalf("read-through snapshot: %s", obs.StateSnapshot)
	}

	// --- pf resume: a fresh claim opens generation 2. ---
	if _, err := pf.Resume(wi, ""); err != nil {
		t.Fatalf("pf resume: %v", err)
	}
	waitUntil(t, "resume to derive the second WI Agent", func() bool {
		return len(mach.spawner.spawns) == 2
	})
	sp := mach.spawner.spawns[1]
	if sp.Generation != 2 {
		t.Fatalf("resume must be a fresh claim of generation 2: %+v", sp)
	}
	if !strings.HasPrefix(sp.SessionID, "sess_"+wi+"_2") {
		t.Fatalf("generation 2 must carry a new session id: %s", sp.SessionID)
	}
	if s := core.wiStatus(wi); s != "running" {
		t.Fatalf("wi must be running after resume, got %s", s)
	}
	if ends := core.genEnds(wi); len(ends) != 2 || ends[1] != "pause" || ends[2] != "" {
		t.Fatalf("resume must leave generation 2 active: %+v", ends)
	}
	// Capsule create for generation 2 quarantined the sealed gen-1 scene.
	capc2, idn2 := mach.openCapsule(wi)
	if idn2.Generation != 2 || idn2.Status != capsule.StatusReady {
		t.Fatalf("gen-2 capsule identity: %+v", idn2)
	}
	if capc2.Dir != oldDir {
		t.Fatal("the new capsule must reuse the canonical capsules/<wi> dir")
	}
	qs, _ := filepath.Glob(filepath.Join(mach.wsRoot(), "capsules", "_quarantine", wi, "*"))
	if len(qs) != 1 {
		t.Fatalf("the sealed gen-1 capsule must be quarantined exactly once: %v", qs)
	}
	// Remote site reconstruction (05 §7.4) rebuilt the sync surface from
	// the published HEAD: the execution.json is back in the new capsule,
	// byte-verbatim as published (the capsule writer's indented form).
	execRaw, err := os.ReadFile(filepath.Join(capc2.Dir, "runtime-state", "execution.json"))
	if err != nil || !strings.Contains(string(execRaw), `"current_step": "code"`) {
		t.Fatalf("recovered capsule must carry the published execution.json: %v %s", err, execRaw)
	}
	// Observe is back on layer 1 (local capsule alive again).
	raw, _ = pf.Observe(wi, 0)
	obs = cli.ObserveResponse{}
	json.Unmarshal(raw, &obs)
	if obs.ReadThrough {
		t.Fatalf("a resumed local job must answer on layer 1: %s", raw)
	}

	// --- the resumed generation works and wraps terminally. ---
	writeExecution(t, capc2, wi, 2, "code", 2)
	raw, err = pf.Wrap(wi, "wrap")
	if err != nil {
		t.Fatalf("pf wrap: %v", err)
	}
	fin = daemonFinish(t, raw)
	if fin.Reason != "wrap" || fin.FlushEvidence == nil || fin.FlushEvidence.PublishedGeneration != 2 {
		t.Fatalf("gen-2 wrap must carry gen-2 evidence: %+v", fin)
	}
	if s := core.wiStatus(wi); s != "wrapped" {
		t.Fatalf("wi must be wrapped, got %s", s)
	}
	// The whole state-transition story, asserted end to end:
	// gen1 ended pause, gen2 ended wrap, each exactly once.
	ends := core.genEnds(wi)
	if len(ends) != 2 || ends[1] != "pause" || ends[2] != "wrap" {
		t.Fatalf("generation ends: %+v", ends)
	}
	if core.eventCount(wi, "paused") != 1 || core.eventCount(wi, "wrapped") != 1 ||
		core.eventCount(wi, "claimed") != 2 {
		t.Fatalf("event story: paused=%d wrapped=%d claimed=%d",
			core.eventCount(wi, "paused"), core.eventCount(wi, "wrapped"), core.eventCount(wi, "claimed"))
	}
	if hseq, _ := core.headRow(wi); hseq <= seqBefore || core.headGeneration(wi) != 2 {
		t.Fatalf("HEAD must have advanced under generation 2: seq=%d gen=%d", hseq, core.headGeneration(wi))
	}
	if j := mach.state().FindJob(wi); j != nil {
		t.Fatalf("jobs[] must be empty after wrap: %+v", j)
	}
	if q := readyQueue(t, core); q.hasExecutable(wi) || q.hasBlocked(wi) {
		t.Fatalf("wrapped wi must be gone from the ready queue: %+v", q)
	}
}
