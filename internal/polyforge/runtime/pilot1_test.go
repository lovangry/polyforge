package runtime

// pilot1_test.go: the pilot fix-lane regressions.
//
//   - F1: runtime-state/work-item.json is materialized at Start and
//     repaired from aihub when missing (v1 has no agent→server read path;
//     without the file the agent cannot know its own goal).
//   - F2: a slug-submitted claim runs under the CANONICAL work-item id —
//     capsule dir, git dir, push remote, session id — never the
//     "<project>#<seq>" spelling; restart migrates pre-F2 slug-keyed
//     state.json records onto the canonical id.
//   - F3: the live-transcript locator (the harness's dashified session
//     directory naming), and the resume discriminator's live-file
//     fallback when the capsule copy is missing.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// ---------------------------------------------------------------------------
// F2: canonical id resolution on a slug claim.

func TestStartSlugClaimRunsUnderCanonicalID(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	hub.canonical = map[string]string{"proj#7": "wi_canon7"}
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)

	base := t.TempDir()
	r.cfg.GitRemoteBase = base
	_ = base // no bare repo needed: the clone falls back to local init

	ctx := context.Background()
	slug := "proj#7"
	if err := r.Start(ctx, slug); err != nil {
		t.Fatalf("Start by slug: %v", err)
	}
	defer func() { _ = r.Stop(ctx, "wi_canon7", protocol.FinishWrapped) }()

	const canonical = "wi_canon7"
	if j := r.jobOf(canonical); j == nil {
		t.Fatal("the job must be keyed by the canonical id, not the slug")
	}
	if j := r.jobOf(slug); j != nil {
		t.Fatal("the slug must not be a job key")
	}
	// alias resolution: the operator's spelling still finds the job.
	if c := r.Resolve(slug); c != canonical {
		t.Fatalf("Resolve(%q) = %q, want %q", slug, c, canonical)
	}
	ji, ok := r.Info(slug)
	if !ok || ji.WorkItem != canonical {
		t.Fatalf("Info by slug must resolve to the canonical job, got %+v ok=%v", ji.WorkItem, ok)
	}
	// every path is canonical: capsule dir, git dir, push remote.
	if got := ji.CapsuleDir; !strings.HasSuffix(got, filepath.Join("capsules", canonical)) {
		t.Fatalf("capsule dir %q must be canonical-named", got)
	}
	if got := ji.Git.GitDir; !strings.HasSuffix(got, canonical+".git") || strings.Contains(got, "#") {
		t.Fatalf("git dir %q must be canonical-named and # -free", got)
	}
	if got := ji.Git.RemoteURL; !strings.HasSuffix(got, "/"+canonical+".git") {
		t.Fatalf("remote URL %q must be canonical-named", got)
	}
	if strings.ContainsAny(ji.SessionID, "#") {
		t.Fatalf("session secret %q must not carry the slug separator", ji.SessionID)
	}
	if len(ji.SessionID) != 64 {
		t.Fatalf("the session secret must be 64-hex (32 bytes), got %q", ji.SessionID)
	}
	// the capsule identity and the task-definition file carry the
	// canonical id (and the spawn journal's work_item field follows
	// SpawnParams.WorkItemID — asserted via the spawner's record).
	raw, err := os.ReadFile(filepath.Join(ji.CapsuleDir, capsule.CapsuleJSON))
	if err != nil {
		t.Fatal(err)
	}
	var idn capsule.Identity
	if err := json.Unmarshal(raw, &idn); err != nil || idn.WorkItemID != canonical {
		t.Fatalf("capsule identity must carry the canonical id: %v %+v", err, idn)
	}
	if c := sp.call(0); c.WorkItemID != canonical || c.SessionHandle == "" || strings.Contains(c.SessionHandle, "#") {
		t.Fatalf("spawn params must be canonical with a public handle: %+v", c)
	}
	// state.json is keyed canonically too.
	recs := loadRecords(t, r)
	if len(recs) != 1 || recs[0].WorkItemID != canonical {
		t.Fatalf("state.json record must be canonical-keyed, got %+v", recs)
	}
}

// ---------------------------------------------------------------------------
// F1: runtime-state/work-item.json.

func TestWorkItemFileMaterializedAtStart(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	hub.canonical = map[string]string{"proj#9": "wi_goal9"}
	bg := "the full background content"
	goal := "do the thing"
	hub.items = map[string]*api.WorkItem{
		"wi_goal9": {ID: "wi_goal9", Slug: "proj#9", ProjectName: "proj",
			Goal: goal, Content: &bg, Labels: []string{"pilot"},
			Priority: api.PriorityHigh, Milestone: nil},
	}
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)

	ctx := context.Background()
	if err := r.Start(ctx, "proj#9"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = r.Stop(ctx, "wi_goal9", protocol.FinishWrapped) }()

	path := filepath.Join(r.capsuleDir("wi_goal9"), filepath.FromSlash(capsule.WorkItemJSON))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("work-item.json missing: %v", err)
	}
	var doc capsule.WorkItemDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("work-item.json unparsable: %v", err)
	}
	if doc.ID != "wi_goal9" || doc.Project != "proj" || doc.Goal != goal {
		t.Fatalf("work-item.json identity wrong: %+v", doc)
	}
	if len(doc.Labels) != 1 || doc.Labels[0] != "pilot" || doc.Priority != api.PriorityHigh {
		t.Fatalf("work-item.json must carry labels/priority: %+v", doc)
	}
	if doc.BundleName != "demo-bundle" || doc.BundleDigest != "deadbeef" {
		t.Fatalf("work-item.json must carry the bundle pin: %+v", doc)
	}
	if doc.Content == nil || *doc.Content != bg {
		t.Fatalf("work-item.json must carry the content verbatim: %+v", doc.Content)
	}
}

func TestWorkItemFileRepairOnRecover(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	hub.items = map[string]*api.WorkItem{"wi_repair": {}}
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, hub, sp)
	j := seedJob(r, "wi_repair")
	// a legacy pre-F1 capsule: the file does not exist, and the job record
	// carries no claim snapshot (as after a restart) — the repair fetches
	// from aihub.
	if err := os.MkdirAll(j.capsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := installOldAgent(t, sp, j)

	r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))

	path := filepath.Join(j.capsuleDir, filepath.FromSlash(capsule.WorkItemJSON))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("recovery must repair work-item.json: %v", err)
	}
	// and the repair is idempotent: a second recovery pass leaves it.
	rawFirst, _ := os.ReadFile(path)
	r.handleOB(context.Background(), recoveryMsgFor(j, "again"))
	rawSecond, _ := os.ReadFile(path)
	if string(rawFirst) != string(rawSecond) {
		t.Fatal("the work-item.json repair must be idempotent")
	}
	_ = old
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

// recoveryMsgFor is recoveryMsg with an explicit cause string.
func recoveryMsgFor(j *job, cause string) protocol.OBMsg {
	return protocol.OBMsg{
		Kind: protocol.OBNeedsRecovery, SpawnN: j.spawnN,
		WorkItem: j.wi, Reason: cause,
	}
}

// A pre-v1.5 state.json record (slug-keyed, generation-era fields, no
// session secret) has nothing to reconcile against: the record is
// dropped with a log line, never revived, never migrated — the session
// model's reconciliation key is the secret, and a record without one
// names nothing this daemon can prove ownership of.
func TestRestartDropsPreSessionRecord(t *testing.T) {
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)

	// a pre-v1.5 record: keyed by a slug, generation-era fields, and NO
	// session secret — there is nothing to verify against.
	slug := "proj#3"
	oldCapsule := filepath.Join(r.cfg.Workspace, "capsules", slug)
	if err := os.MkdirAll(filepath.Join(oldCapsule, capsule.DirRuntimeState), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &jobRecord{
		WorkItemID: slug,
		Project:    "proj",
		State:      string(StateRunning),
		CapsuleDir: oldCapsule,
		GitDir:     r.gitDirFor(slug),
		RemoteURL:  "http://hub/git/" + slug + ".git",
		SpawnN:     1,
	}
	blob, _ := json.MarshalIndent(&stateFile{Version: 1, MachineID: r.cfg.MachineID, Jobs: []*jobRecord{rec}}, "", "  ")
	if err := atomicWrite(r.statePath, blob, stateFilePerm); err != nil {
		t.Fatal(err)
	}

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if r.jobOf(slug) != nil {
		t.Fatal("a pre-session record must not be seeded")
	}
	if r.jobOf("wi_migrated") != nil {
		t.Fatal("a pre-session record must not be migrated into a live job")
	}
	if recs := loadRecords(t, r); len(recs) != 0 {
		t.Fatalf("a pre-session record must be dropped, got %+v", recs)
	}
	if sp.callCount() != 0 {
		t.Fatal("a pre-session record must never be revived")
	}
	assertNoRT(t, toOB)
}

// ---------------------------------------------------------------------------
// F3: the live-transcript locator.

func TestPiSessionDirName(t *testing.T) {
	cases := map[string]string{
		"/root":                              "--root--",
		"/root/.local/share/x/capsules/wi_1": "--root-.local-share-x-capsules-wi_1--",
		"/root/ws/capsules/wi_3Ot791uZaXqL":  "--root-ws-capsules-wi_3Ot791uZaXqL--",
		"/tmp/ox/capsules/polyforge#9":       "--tmp-ox-capsules-polyforge#9--",
	}
	for in, want := range cases {
		if got := piSessionDirName(in); got != want {
			t.Fatalf("piSessionDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocateLiveTranscript(t *testing.T) {
	root := t.TempDir()
	capsuleDir := filepath.Join(t.TempDir(), "capsules", "wi_live")
	if err := os.MkdirAll(capsuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, piSessionDirName(capsuleDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	spawnAt := time.Now().Add(-time.Hour)
	// a predecessor's transcript: created and last-touched long before
	// the spawn moment — must never be picked.
	old := filepath.Join(dir, "2000-01-01T00-00-00-000Z_old.jsonl")
	if err := os.WriteFile(old, []byte("{\"type\":\"session\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, spawnAt.Add(-time.Minute), spawnAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	// nothing after the spawn moment yet: "" (the typed warning path).
	if got := LocateLiveTranscript(root, capsuleDir, spawnAt); got != "" {
		t.Fatalf("no live transcript existed yet, got %q", got)
	}
	// the live transcript appears (created after the spawn moment).
	live := filepath.Join(dir, "2026-09-30T01-02-03-000Z_new.jsonl")
	if err := os.WriteFile(live, []byte("{\"type\":\"session\"}\n{\"m\":\"hi\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LocateLiveTranscript(root, capsuleDir, spawnAt)
	if got != live {
		t.Fatalf("LocateLiveTranscript = %q, want %q", got, live)
	}
	// a still newer file wins (the newest after the floor).
	newer := filepath.Join(dir, "2026-09-30T01-03-03-000Z_newest.jsonl")
	if err := os.WriteFile(newer, []byte("{\"type\":\"session\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := LocateLiveTranscript(root, capsuleDir, spawnAt); got != newer {
		t.Fatalf("the newest qualifying transcript must win, got %q want %q", got, newer)
	}
	// an absent session directory: "" — never an error.
	if got := LocateLiveTranscript(root, filepath.Join(t.TempDir(), "capsules", "wi_none"), time.Now()); got != "" {
		t.Fatalf("absent session dir must answer \"\", got %q", got)
	}
}

// ---------------------------------------------------------------------------
// F3: the resume discriminator's live-file fallback.

func TestStopSealSyncsLiveTranscriptIntoTheChain(t *testing.T) {
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_sealsync"
	j := seedJob(r, wi)
	bare := initGitCapsule(t, j)
	// the recorded live transcript exists; the capsule copy does not.
	live := filepath.Join(t.TempDir(), "live-session.jsonl")
	liveBody := []byte("{\"type\":\"session\"}\n{\"t\":\"final turn\"}\n")
	if err := os.WriteFile(live, liveBody, 0o644); err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	j.liveTranscript = live
	j.mu.Unlock()
	installOldAgent(t, sp, j)

	if err := r.Stop(context.Background(), wi, protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// the seal commit carried the transcript: the capsule copy exists
	// with the live bytes, and the pushed chain's tip holds it.
	raw, err := os.ReadFile(j.transcript)
	if err != nil || string(raw) != string(liveBody) {
		t.Fatalf("the seal must land the transcript in the capsule: %v (%q)", err, raw)
	}
	if !bareHasMain(t, bare) {
		t.Fatal("seal push never landed")
	}
	out, err := run(t, "git", "--git-dir", bare, "ls-tree", "--name-only", "-r", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session/session-"+j.sessionFingerprint+".jsonl") {
		t.Fatalf("the pushed chain misses the transcript:\n%s", out)
	}
}

func TestStopSealPushFailureEmitsPushFailedEvent(t *testing.T) {
	sp := &scriptedSpawner{}
	r, _, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_sealpushfail"
	j := seedJob(r, wi)
	j.git.RemoteURL = "" // no push remote configured: every push fails
	installOldAgent(t, sp, j)

	var tapped []struct {
		wi   string
		kind string
	}
	r.cfg.EventTap = func(wi, kind string, body map[string]any) {
		tapped = append(tapped, struct {
			wi   string
			kind string
		}{wi, kind})
	}
	// The degraded close is the CORRECT outcome (blocked, local bytes
	// kept) — the event is the diagnosability layer on top.
	if err := r.Stop(context.Background(), wi, protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(tapped) != 1 || tapped[0].wi != wi || tapped[0].kind != "push_failed" {
		t.Fatalf("expected exactly one push_failed tap, got %+v", tapped)
	}
	if st := hub_endStatus(t, r, wi); st != protocol.FinishBlocked {
		t.Fatalf("the close must degrade to blocked, got %q", st)
	}
}

func hub_endStatus(t *testing.T, r *Runtime, wi string) string {
	t.Helper()
	hub := r.hub
	fh, ok := hub.(*fakeHub)
	if !ok {
		t.Fatalf("expected the fake hub, got %T", hub)
	}
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if len(fh.ends) == 0 {
		return ""
	}
	return fh.ends[len(fh.ends)-1].Status
}
