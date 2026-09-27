package test

// fault_dualclose_test.go: fault category (c) — two daemon processes
// racing one UDS job face (04 §7.2 "双 Daemon 同机" + the one-close guard
// of the 04 §6.2 A path).
//
// Part 1 (single-instance lock): a second daemon process cannot bind the
// first one's UDS socket — NewAPIServer's connect probe fails and the
// bind refuses, so two daemons can never manage one machine (04 §7.2).
//
// Part 2 (racing closes): while one close is mid-flight (parked at its
// boundary notice inside the REAL closeJob), a second close of the same
// WI arrives over the same face: the one-close-at-a-time guard answers
// the typed DAEMON-409 and NOTHING double-closes — the Core sees exactly
// one generation end, one close event, one HEAD advance matching the
// winner's reported facts, and the local residue (sealed capsule, empty
// jobs[], freed queue slot) is consistent with the winner alone.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/daemon"
)

func TestFaultInjectDualDaemonAndRacingCloses(t *testing.T) {
	core := newCoreEnv(t)
	mach := newDaemonEnv(t, core)
	mach.serve()
	pf := mach.pf()

	wi := core.newWI(t, "fault: two daemons, one job face")
	capc, agent := mach.claimWorkingJob(wi)
	if _, err := pf.PublishRequest(wi, "step_completed"); err != nil {
		t.Fatalf("working publish: %v", err)
	}
	headSeq, _ := core.headRow(wi)
	// Unpublished work: the winning close's flush must publish it (a real
	// HEAD advance) before its end-generation lands.
	writeCapsuleFile(t, capc, "artifacts/work.md", "racing close\n")

	// --- Part 1: the second daemon process cannot take the socket. ---
	other := newMachineEnv(t, core, "mac_b", core.workerKey, core.ts.URL)
	if _, err := daemon.NewAPIServer(other.d, mach.socket); err == nil {
		t.Fatal("a second daemon must not be able to bind the first one's UDS socket")
	} else if !strings.Contains(err.Error(), "served by another daemon") {
		t.Fatalf("the refusal must be the single-instance lock: %v", err)
	}
	// The first face is still healthy after the refused bind.
	if code, _ := udsBody(t, mach.socket, "GET", "/status", nil, nil, nil); code != 200 {
		t.Fatalf("the serving face must be undisturbed: %d", code)
	}

	// --- Part 2: racing closes over the one face. The first close parks
	// inside its boundary notice (the exact window where a second close
	// can arrive); the second must get the typed DAEMON-409. ---
	entered := make(chan struct{})
	release := make(chan struct{})
	agent.onNotifyStop = func() { close(entered); <-release }

	winner := make(chan int, 1)
	go func() {
		code, raw := udsBody(t, mach.socket, "POST", "/jobs/"+wi+"/pause", map[string]any{}, nil, nil)
		winner <- code
		_ = raw
	}()
	// The first close is mid-flight (inside closeJob, guard held).
	<-entered

	// The racing second close — over the same UDS face, real route.
	var env errEnvelope
	code, raw := udsBody(t, mach.socket, "POST", "/jobs/"+wi+"/wrap", map[string]any{"reason": "wrap"}, nil, nil)
	json.Unmarshal(raw, &env)
	if code != 409 || env.Error.Code != daemon.ErrCodeConflict {
		t.Fatalf("the racing second close must be the typed DAEMON-409: %d %s", code, raw)
	}
	// The refused close landed NOTHING: the WI is untouched while the
	// first close is still parked.
	if s := core.wiStatus(wi); s != "running" {
		t.Fatalf("the refused close must land nothing, status: %s", s)
	}

	// Let the winner finish.
	close(release)
	if code := <-winner; code != 200 {
		t.Fatal("the parked close must complete after the race is lost by the other side")
	}

	// --- no torn state, either side. ---
	if s := core.wiStatus(wi); s != "paused" {
		t.Fatalf("the winning pause must be the only close that landed: %s", s)
	}
	if ends := core.genEnds(wi); len(ends) != 1 || ends[1] != "pause" {
		t.Fatalf("exactly one generation end (pause): %+v", ends)
	}
	if core.eventCount(wi, "paused") != 1 || core.eventCount(wi, "wrapped") != 0 {
		t.Fatalf("close events: paused=%d wrapped=%d",
			core.eventCount(wi, "paused"), core.eventCount(wi, "wrapped"))
	}
	if hseq, _ := core.headRow(wi); hseq != headSeq+1 {
		t.Fatalf("the winner's final flush must have advanced HEAD exactly once: %d != %d", hseq, headSeq+1)
	}
	// Local residue consistent with ONE close: sealed capsule, no job row,
	// no queue slot.
	if j := mach.state().FindJob(wi); j != nil {
		t.Fatalf("jobs[] must be empty: %+v", j)
	}
	if fi, err := os.Stat(capc.Dir); err != nil || fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("capsule must remain on disk, sealed read-only: %v", err)
	}
	if code, env := udsErr(t, mach.socket, "POST", "/jobs/"+wi+"/wrap", map[string]any{"reason": "wrap"}, nil); code != 404 ||
		env.Error.Code != daemon.ErrCodeNotFound {
		t.Fatalf("any late close must be the typed DAEMON-404: %d %+v", code, env)
	}
}
