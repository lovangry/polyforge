package runtime

// capsule_name_test.go (polyforge#144): a new claim names its capsule
// directory for humans — <workspace>/capsules/<project>-<seq>, the slug
// with "#" replaced — while every machine-side path (git-dir, remote,
// state key) stays on the canonical id. The persisted record is the
// truth across a restart; work items claimed under the old id-named
// layout keep their recorded directory and continue.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

func terminateJobAgent(r *Runtime, wi string) {
	if j := r.jobOf(wi); j != nil {
		j.mu.Lock()
		h := j.agent
		j.mu.Unlock()
		if h != nil {
			_ = h.Terminate(2e9)
		}
	}
}

// A new claim lands at capsules/<project>-<seq>, the record carries the
// slug and that directory, and a daemon restart (crash mid-run) finds
// the capsule through the record and recovers in it.
func TestNewClaimCapsuleNamedBySlugAndFoundAfterRestart(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	const wi = "wi_n140"
	hub.items = map[string]*api.WorkItem{
		wi: {ID: wi, Slug: "proj#140", Seq: 140, ProjectName: "proj"},
	}
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	if err := r.Start(context.Background(), wi); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := testCapsuleDir(r, "proj-140")
	ji, ok := r.Info(wi)
	if !ok || ji.CapsuleDir != want || ji.Slug != "proj#140" {
		t.Fatalf("claimed job = dir %q slug %q (ok=%v), want dir %q slug proj#140", ji.CapsuleDir, ji.Slug, ok, want)
	}
	if strings.Contains(ji.CapsuleDir, "#") {
		t.Fatalf("capsule dir %q must not carry #", ji.CapsuleDir)
	}
	if !strings.HasSuffix(ji.Git.GitDir, wi+".git") || !strings.HasSuffix(ji.Git.RemoteURL, wi+".git") {
		t.Fatalf("git dir %q / remote %q must stay id-keyed", ji.Git.GitDir, ji.Git.RemoteURL)
	}
	if recs := loadRecords(t, r); len(recs) != 1 || recs[0].CapsuleDir != want || recs[0].Slug != "proj#140" {
		t.Fatalf("persisted record = %+v, want capsule_dir %q and slug proj#140", recs, want)
	}
	if m := recvRT(t, toOB); m.Kind != protocol.RTStarted {
		t.Fatalf("expected RTStarted, got %+v", m)
	}

	// crash: the agent dies with the daemon; a fresh runtime over the same
	// data dir reconciles the record (the hub still names our session).
	terminateJobAgent(r, wi)
	toOB2 := supervise.NewQueue[protocol.RTMsg]()
	r2 := New(r.cfg, hub, sp, toOB2, supervise.NewQueue[protocol.OBMsg]())
	if err := r2.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	defer terminateJobAgent(r2, wi)
	if m := recvRT(t, toOB2); m.Kind != protocol.RTRecovered || m.WorkItem != wi {
		t.Fatalf("expected RTRecovered for %s, got %+v", wi, m)
	}
	ji2, ok := r2.Info(wi)
	if !ok || ji2.CapsuleDir != want || ji2.Slug != "proj#140" {
		t.Fatalf("restored job = dir %q slug %q (ok=%v), want %q", ji2.CapsuleDir, ji2.Slug, ok, want)
	}
	if n := sp.callCount(); n != 2 || sp.call(1).CapsuleDir != want {
		t.Fatalf("the recovery derivation must run in %q (calls=%d)", want, n)
	}
}

// The restart's fallback for a record without a capsule_dir builds ONE
// name from the record: <project>-<seq> when the record carries a slug
// (an id-named directory is then not found), the id name when it does not.
func TestRestartFallbackBuildsOneNameFromTheRecord(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)

	if err := r.requireMidflightCapsule("wi_fb", "proj", "proj#7", ""); err == nil {
		t.Fatal("no directory exists yet: the fallback must report it missing")
	}
	if err := os.MkdirAll(testCapsuleDir(r, "wi_fb"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := r.requireMidflightCapsule("wi_fb", "proj", "proj#7", "")
	var me *MissingCapsuleError
	if !errors.As(err, &me) || me.Dir != testCapsuleDir(r, "proj-7") {
		t.Fatalf("an id-named directory must not be found by the fallback, got %v", err)
	}
	if err := os.MkdirAll(testCapsuleDir(r, "proj-7"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.requireMidflightCapsule("wi_fb", "proj", "proj#7", ""); err != nil {
		t.Fatalf("the slug-named directory must be found: %v", err)
	}
	// a record without a slug falls back to the id name
	if err := r.requireMidflightCapsule("wi_fb", "proj", "", ""); err != nil {
		t.Fatalf("a slug-less record must fall back to the id-named directory: %v", err)
	}
}

// The upgrade: work items claimed under the old id-named layout are
// recorded with their capsule_dir and no slug. After the upgrade a
// running one recovers in its old directory, a paused one attaches in
// its old directory, and a blocked one (no record; the owner's resume
// re-claims it) is cloned into the new <project>-<seq> directory, its old
// id-named worktree quarantined TOGETHER with the id-keyed git-dir (one
// unit, one timestamp — the scene is never split).
func TestUpgradeOldNamedCapsulesContinue(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	hub.items = map[string]*api.WorkItem{
		"wi_old_run":   {ID: "wi_old_run", Slug: "proj#1", Seq: 1, ProjectName: "proj"},
		"wi_old_pause": {ID: "wi_old_pause", Slug: "proj#2", Seq: 2, ProjectName: "proj"},
		"wi_old_block": {ID: "wi_old_block", Slug: "proj#3", Seq: 3, ProjectName: "proj"},
	}
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)

	pidRun, startRun := spawnSleeper(t, "wi_old_run")
	run := matrixRecord(r, "wi_old_run", pidRun, startRun)
	hub.hold("wi_old_run", run.SessionID, api.StatusRunning)
	pidPause, startPause := spawnSleeper(t, "wi_old_pause")
	pause := matrixRecord(r, "wi_old_pause", pidPause, startPause)
	hub.hold("wi_old_pause", pause.SessionID, api.StatusPaused)
	for _, rec := range []*jobRecord{run, pause} {
		if rec.Slug != "" || filepath.Base(rec.CapsuleDir) != rec.WorkItemID {
			t.Fatalf("fixture must model an old id-named record, got %+v", rec)
		}
		if err := os.MkdirAll(rec.CapsuleDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript(t, jobFromRecord(pause))
	writeStateFile(t, r, []*jobRecord{run, pause})
	// the blocked one's old scene: an id-named worktree whose .git points
	// at the id-keyed external git-dir
	oldBlocked := testCapsuleDir(r, "wi_old_block")
	oldGitDir := r.gitDirFor("wi_old_block")
	for _, d := range []string{oldBlocked, oldGitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldBlocked, ".git"), []byte("gitdir: "+oldGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	defer terminateJobAgent(r, "wi_old_run")
	defer terminateJobAgent(r, "wi_old_pause")
	waitDead(t, pidRun)
	waitDead(t, pidPause)
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		m := recvRT(t, toOB)
		if m.Kind != protocol.RTRecovered {
			t.Fatalf("expected RTRecovered, got %+v", m)
		}
		got[m.WorkItem] = true
	}
	if !got["wi_old_run"] || !got["wi_old_pause"] {
		t.Fatalf("both old work items must continue, got %v", got)
	}
	for _, rec := range []*jobRecord{run, pause} {
		ji, ok := r.Info(rec.WorkItemID)
		if !ok || ji.CapsuleDir != rec.CapsuleDir {
			t.Fatalf("%s must keep its recorded directory %q, got %q", rec.WorkItemID, rec.CapsuleDir, ji.CapsuleDir)
		}
	}

	// the blocked one: the owner's resume re-claims it
	hub.status["wi_old_block"] = api.StatusQueued
	if err := r.Start(context.Background(), "wi_old_block"); err != nil {
		t.Fatalf("re-claim of the blocked work item: %v", err)
	}
	defer terminateJobAgent(r, "wi_old_block")
	ji, ok := r.Info("wi_old_block")
	if !ok || ji.CapsuleDir != testCapsuleDir(r, "proj-3") {
		t.Fatalf("the re-claim must use the new name, got %q", ji.CapsuleDir)
	}
	if dirExists(oldBlocked) {
		t.Fatal("the old id-named worktree must be quarantined, not left with a dangling .git pointer")
	}
	movedCap, _ := filepath.Glob(oldBlocked + ".quarantined.*")
	movedGit, _ := filepath.Glob(oldGitDir + ".quarantined.*")
	if len(movedCap) != 1 || len(movedGit) != 1 ||
		strings.TrimPrefix(movedCap[0], oldBlocked) != strings.TrimPrefix(movedGit[0], oldGitDir) {
		t.Fatalf("worktree and git-dir must move aside together under one timestamp: %v %v", movedCap, movedGit)
	}
	if !isRegularFile(filepath.Join(ji.CapsuleDir, ".git")) || !dirExists(oldGitDir) {
		t.Fatal("the new capsule must be a fresh clone with its own externalized git-dir")
	}
}
