package gitops

// w20_supersede_test.go: the owner-authorized rewind supersede
// (polyforge#20, ruling 2026-11-11) against REAL git — an intentional
// rewind of the WI's own archived line may replace the carrier tip, but
// only when the claim's carrier-restore record (sync/carrier-restores.json,
// the trusted daemon-written record of the head the claim restored each
// task repo to) anchors both ends to the capsule's own line, and only as
// ONE atomic push that preserves the surpassed tip in a timestamped
// refs/pf-task-backup/<name>/<stamp> ref. The structurally identical
// shapes WITHOUT that record (the agent's own local reset — the
// accidental divergence TestTaskRefDivergenceIsRejectedAndRaisedAsEvent
// protects) keep the old refusal, and a hub that refuses half the
// transaction moves nothing.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// w20LayRepo lays one task-repo origin (a single init commit) and clones
// it into the capsule's repos/<name>; it answers the repo dir and the
// origin's init head.
func w20LayRepo(t *testing.T, o Opts, base, name string) (repoDir, init string) {
	t.Helper()
	origin := filepath.Join(base, "origin", name)
	if out, err := run("git", "init", "-q", "-b", "main", origin); err != nil {
		t.Fatalf("task origin init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(origin, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", origin, "add", "-A")
	mustRun(t, "git", "-C", origin, "commit", "-q", "-m", "init")
	repoDir = filepath.Join(o.CapsuleDir, capsule.DirRepos, name)
	mustRun(t, "git", "clone", "-q", origin, repoDir)
	return repoDir, strings.TrimSpace(mustRun(t, "git", "-C", repoDir, "rev-parse", "HEAD"))
}

// w20Commit records one commit in a task repo; it answers the head.
func w20Commit(t *testing.T, repoDir, file, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoDir, file), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", repoDir, "add", "-A")
	mustRun(t, "git", "-C", repoDir, "commit", "-q", "-m", msg)
	return strings.TrimSpace(mustRun(t, "git", "-C", repoDir, "rev-parse", "HEAD"))
}

// w20BackupRefs answers the hub's backup refs for one task repo as
// refname -> sha.
func w20BackupRefs(t *testing.T, bare, name string) map[string]string {
	t.Helper()
	out := mustRun(t, "git", "--git-dir", bare, "for-each-ref", "--format=%(refname) %(objectname)", TaskBackupNamespace+name+"/")
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			refs[f[0]] = f[1]
		}
	}
	return refs
}

// w20InstallHook writes one pre-receive hook into the bare hub and
// answers nothing; the hook body comes from format.
func w20InstallHook(t *testing.T, bare, format string, args ...any) {
	t.Helper()
	path := filepath.Join(bare, "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(format, args...)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The incident's authorized shape, with every object in the capsule's
// own store: the claim restored the task repo to B (the record), an
// earlier session archived T on top of B, the rewind moved the repo back
// to B and committed replacement work H. The publish must supersede the
// carrier to H, preserve T under exactly one timestamped backup ref,
// raise nothing, keep publishing cleanly afterwards, and leave both the
// carrier (H) and the backup (T) readable for a recovery.
func TestAuthorizedRewindSupersedeBacksUpAndReplaces(t *testing.T) {
	setIdentity(t)
	ctx := context.Background()
	o, base := taskOpts(t, "wi_w20_sup")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	repoDir, _ := w20LayRepo(t, o, base, "pi-web-gmi")
	const name = "pi-web-gmi"

	// The claim's carrier-restore record: the daemon moved the fresh clone
	// to B (what cloneTaskRepos would have written for this restore).
	b := w20Commit(t, repoDir, "base.txt", "the restore point")
	if err := WriteCarrierRestores(o.CapsuleDir, map[string]string{name: b}); err != nil {
		t.Fatalf("write the carrier-restore record: %v", err)
	}
	// The earlier session's archive beyond the restore point.
	tip := w20Commit(t, repoDir, "shared-late.txt", "the earlier session's archive")
	var events []string
	o.EventSink = func(kind string, _ map[string]any) { events = append(events, kind) }
	o.CarrierAlerts = &CarrierAlerts{}
	if _, _, _, err := CommitPush(ctx, o, "v1"); err != nil {
		t.Fatalf("the archive publish: %v", err)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); !ok || got != tip {
		t.Fatalf("carrier after the archive publish = %q (%v), want the archived head %q", got, ok, tip)
	}

	// The intentional rewind: back to the recorded restore point, the
	// replacement session's own work on top.
	mustRun(t, "git", "-C", repoDir, "reset", "-q", "--hard", b)
	head := w20Commit(t, repoDir, "rewind.txt", "the rewound session's work")

	if _, _, _, err := CommitPush(ctx, o, "v2 the rewind"); err != nil {
		t.Fatalf("the rewind publish: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("an authorized rewind raised %v, want no events (the incident: the rewound capsule must republish its own carrier)", events)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); !ok || got != head {
		t.Fatalf("carrier after the rewind = %q (%v), want the rewound head %q", got, ok, head)
	}
	backs := w20BackupRefs(t, o.RemoteURL, name)
	if len(backs) != 1 {
		t.Fatalf("backup refs after one supersede = %v, want exactly one", backs)
	}
	for ref, sha := range backs {
		if !strings.HasPrefix(ref, TaskBackupNamespace+name+"/") {
			t.Fatalf("backup ref %q is outside %s%s/", ref, TaskBackupNamespace, name)
		}
		if sha != tip {
			t.Fatalf("backup ref %s holds %q, want the surpassed tip %q", ref, sha, tip)
		}
	}

	// Recovery reads: a re-claim's fresh clone reads the CURRENT head
	// from the carrier, and the surpassed line stays fetchable from the
	// backup ref — the owner's recovery contract for the ruling.
	fresh := filepath.Join(base, "fresh")
	mustRun(t, "git", "clone", "-q", filepath.Join(base, "origin", name), fresh)
	if sha, err := FetchTaskRef(ctx, o, fresh, name); err != nil || sha != head {
		t.Fatalf("FetchTaskRef answered %q (%v), want the rewound head %q", sha, err, head)
	}
	var backupRef string
	for ref := range backs {
		backupRef = ref
	}
	mustRun(t, "git", "-C", fresh, "fetch", "-q", o.RemoteURL, backupRef)
	if got := strings.TrimSpace(mustRun(t, "git", "-C", fresh, "rev-parse", "FETCH_HEAD")); got != tip {
		t.Fatalf("the backup fetched %q, want the surpassed tip %q", got, tip)
	}
	if out := mustRun(t, "git", "-C", fresh, "show", "FETCH_HEAD:shared-late.txt"); strings.TrimSpace(out) != "the earlier session's archive" {
		t.Fatalf("the surpassed line's content did not survive: %q", out)
	}

	// Repeated publication after the supersede is clean (the incident's
	// repeated rejections are gone), and no second backup appears.
	next := w20Commit(t, repoDir, "more.txt", "work after the supersede")
	if _, _, _, err := CommitPush(ctx, o, "v3"); err != nil {
		t.Fatalf("the publish after the supersede: %v", err)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); !ok || got != next {
		t.Fatalf("carrier after the next publish = %q (%v), want the fast-forwarded head %q", got, ok, next)
	}
	if len(events) != 0 {
		t.Fatalf("publishes after the supersede raised %v, want no events", events)
	}
	if len(w20BackupRefs(t, o.RemoteURL, name)) != 1 {
		t.Fatalf("a fast-forward publish created a backup ref, want exactly one")
	}
}

// The incident's two-session shape: the surpassed tip is ANOTHER
// session's commits this capsule's task repo never held (it was archived
// from a second clone of the same origin). The gate must fetch the tip
// from the hub, prove the capsule's own line on both ends, and supersede
// with the backup.
func TestSupersedeFetchesTipTheCapsuleNeverHeld(t *testing.T) {
	setIdentity(t)
	ctx := context.Background()
	o, base := taskOpts(t, "wi_w20_fetch")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	repoDir, _ := w20LayRepo(t, o, base, "pi-web-gmi")
	const name = "pi-web-gmi"

	b := w20Commit(t, repoDir, "base.txt", "the restore point")
	if err := WriteCarrierRestores(o.CapsuleDir, map[string]string{name: b}); err != nil {
		t.Fatalf("write the carrier-restore record: %v", err)
	}
	var events []string
	o.EventSink = func(kind string, _ map[string]any) { events = append(events, kind) }
	o.CarrierAlerts = &CarrierAlerts{}
	if _, _, _, err := CommitPush(ctx, o, "v1"); err != nil {
		t.Fatalf("the restore-point publish: %v", err)
	}

	// The earlier session's own capsule of the same WI: a clone from the
	// capsule's task repo (the same claim line, carrier-restored to B),
	// archiving work this capsule never held.
	other := filepath.Join(base, "other-session", name)
	mustRun(t, "git", "clone", "-q", repoDir, other)
	tip := w20Commit(t, other, "other.txt", "the other session's archive")
	mustRun(t, "git", "-C", other, "push", "-q", o.RemoteURL, tip+":"+TaskRefName(name))
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); !ok || got != tip {
		t.Fatalf("setup: the hub carrier = %q (%v), want the other session's head %q", got, ok, tip)
	}

	// This capsule rewinds to its restore point and republishes.
	mustRun(t, "git", "-C", repoDir, "reset", "-q", "--hard", b)
	head := w20Commit(t, repoDir, "rewind.txt", "the rewound session's work")
	if _, _, _, err := CommitPush(ctx, o, "v2 the rewind"); err != nil {
		t.Fatalf("the rewind publish: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("the rewind over an unheld tip raised %v, want no events", events)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); !ok || got != head {
		t.Fatalf("carrier after the rewind = %q (%v), want the rewound head %q", got, ok, head)
	}
	backs := w20BackupRefs(t, o.RemoteURL, name)
	if len(backs) != 1 {
		t.Fatalf("backup refs = %v, want exactly one holding the unheld tip", backs)
	}
	for _, sha := range backs {
		if sha != tip {
			t.Fatalf("the backup holds %q, want the other session's tip %q", sha, tip)
		}
	}
}

// The graph alone never authorizes: the SAME rewind shape without the
// claim's carrier-restore record (a first claim, whose clone was never
// carrier-restored) keeps the old refusal — the carrier stays, the event
// pair is raised, and no backup ref appears. This is the strengthened
// accidental-divergence boundary the ruling keeps intact.
func TestRewindWithoutRestoreRecordStaysRefused(t *testing.T) {
	setIdentity(t)
	ctx := context.Background()
	o, base := taskOpts(t, "wi_w20_norec")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	repoDir, _ := w20LayRepo(t, o, base, "pi-web-gmi")
	const name = "pi-web-gmi"

	// No WriteCarrierRestores: nothing records an intentional restore.
	b := w20Commit(t, repoDir, "base.txt", "the earlier head")
	tip := w20Commit(t, repoDir, "shared-late.txt", "the archived work")
	if _, _, _, err := CommitPush(ctx, o, "v1"); err != nil {
		t.Fatalf("the archive publish: %v", err)
	}
	mustRun(t, "git", "-C", repoDir, "reset", "-q", "--hard", b)
	head := w20Commit(t, repoDir, "rewind.txt", "replacement work")

	var kinds []string
	o.EventSink = func(kind string, _ map[string]any) { kinds = append(kinds, kind) }
	o.CarrierAlerts = &CarrierAlerts{}
	if _, _, _, err := CommitPush(ctx, o, "v2 the rewind"); err != nil {
		t.Fatalf("a rejected carrier must not fail the chain publish: %v", err)
	}
	if got, _ := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); got != tip {
		t.Fatalf("carrier moved to %q, want it kept at %q (no record: no authorization)", got, tip)
	}
	if len(kinds) != 2 || kinds[0] != EventCarrierPushFailed || kinds[1] != EventTaskRefDiverged {
		t.Fatalf("events = %v, want [%s %s]", kinds, EventCarrierPushFailed, EventTaskRefDiverged)
	}
	if len(w20BackupRefs(t, o.RemoteURL, name)) != 0 {
		t.Fatalf("a refused rewind created backup refs, want none")
	}
	if head == tip {
		t.Fatalf("the rewind must produce a different head (%q) from the archived tip (%q) — a refusal of a fast-forward proves nothing", head, tip)
	}
}

// Even WITH the record, only the capsule's own line from the recorded
// restore point supersedes: an agent's local reset below the restore
// point (the re-claim's accidental shape — the head no longer descends
// from where the daemon handed the capsule over) keeps the refusal.
func TestDivergedHeadBelowRestoreRecordRefused(t *testing.T) {
	setIdentity(t)
	ctx := context.Background()
	o, base := taskOpts(t, "wi_w20_below")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	repoDir, init := w20LayRepo(t, o, base, "pi-web-gmi")
	const name = "pi-web-gmi"

	b := w20Commit(t, repoDir, "base.txt", "the restore point")
	if err := WriteCarrierRestores(o.CapsuleDir, map[string]string{name: b}); err != nil {
		t.Fatalf("write the carrier-restore record: %v", err)
	}
	tip := w20Commit(t, repoDir, "shared-late.txt", "the archived work")
	if _, _, _, err := CommitPush(ctx, o, "v1"); err != nil {
		t.Fatalf("the archive publish: %v", err)
	}
	// The agent's own local reset below the recorded restore point.
	mustRun(t, "git", "-C", repoDir, "reset", "-q", "--hard", init)
	head := w20Commit(t, repoDir, "diverged.txt", "the agent's replacement")

	var kinds []string
	o.EventSink = func(kind string, _ map[string]any) { kinds = append(kinds, kind) }
	o.CarrierAlerts = &CarrierAlerts{}
	if _, _, _, err := CommitPush(ctx, o, "v2 the divergence"); err != nil {
		t.Fatalf("a rejected carrier must not fail the chain publish: %v", err)
	}
	if got, _ := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); got != tip {
		t.Fatalf("carrier moved to %q, want it kept at %q (below the restore point: not the capsule's line)", got, tip)
	}
	if len(kinds) != 2 || kinds[0] != EventCarrierPushFailed || kinds[1] != EventTaskRefDiverged {
		t.Fatalf("events = %v, want [%s %s]", kinds, EventCarrierPushFailed, EventTaskRefDiverged)
	}
	if len(w20BackupRefs(t, o.RemoteURL, name)) != 0 {
		t.Fatalf("a refused divergence created backup refs, want none")
	}
	if head == tip {
		t.Fatalf("the divergence must produce a different head (%q) from the archived tip (%q) — a refusal of a fast-forward proves nothing", head, tip)
	}
}

// Fail closed: a hub that refuses the backup half of the supersede moves
// NOTHING — the carrier keeps the surpassed tip, no backup exists, and
// the failure is raised (the atomicity the ruling demands: the carrier
// never advances without the surpassed tip being preserved).
func TestAuthorizedSupersedeFailsClosedWhenHubRefusesBackup(t *testing.T) {
	setIdentity(t)
	ctx := context.Background()
	o, base := taskOpts(t, "wi_w20_closed")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	repoDir, _ := w20LayRepo(t, o, base, "pi-web-gmi")
	const name = "pi-web-gmi"

	b := w20Commit(t, repoDir, "base.txt", "the restore point")
	if err := WriteCarrierRestores(o.CapsuleDir, map[string]string{name: b}); err != nil {
		t.Fatalf("write the carrier-restore record: %v", err)
	}
	tip := w20Commit(t, repoDir, "shared-late.txt", "the earlier session's archive")
	var events []string
	o.EventSink = func(kind string, _ map[string]any) { events = append(events, kind) }
	o.CarrierAlerts = &CarrierAlerts{}
	if _, _, _, err := CommitPush(ctx, o, "v1"); err != nil {
		t.Fatalf("the archive publish: %v", err)
	}
	// The hub refuses the backup namespace from here on.
	w20InstallHook(t, o.RemoteURL, `#!/bin/sh
while read -r old new ref; do
  case "$ref" in refs/pf-task-backup/*) echo "backup refused" >&2; exit 1 ;; esac
done
exit 0
`)

	mustRun(t, "git", "-C", repoDir, "reset", "-q", "--hard", b)
	head := w20Commit(t, repoDir, "rewind.txt", "the rewound session's work")
	if _, _, _, err := CommitPush(ctx, o, "v2 the rewind"); err != nil {
		t.Fatalf("a rejected carrier must not fail the chain publish: %v", err)
	}
	if got, _ := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); got != tip {
		t.Fatalf("carrier moved to %q, want it kept at %q (a refused backup fails the whole supersede)", got, tip)
	}
	if len(w20BackupRefs(t, o.RemoteURL, name)) != 0 {
		t.Fatalf("a failed supersede left backup refs, want none")
	}
	if len(events) != 2 || events[0] != EventCarrierPushFailed || events[1] != EventTaskRefDiverged {
		t.Fatalf("events = %v, want exactly [%s %s]: a failed authorized supersede keeps the diverged-carrier signal (the daemon surfaces the conflict for the owner), not the failed backup alone", events, EventCarrierPushFailed, EventTaskRefDiverged)
	}
	if head == tip {
		t.Fatalf("the rewind must produce a different head (%q) from the archived tip (%q) — a fail-closed supersede of a fast-forward proves nothing", head, tip)
	}
}

// The supersede is ONE push of TWO refspecs: the pre-receive hook sees
// the carrier replacement and the backup creation in the SAME
// invocation (git's --atomic applies both or neither), and the carrier
// line's old value is exactly the tip the gate read — the lease's
// compare-and-swap expectation. A writer that moved the tip in between
// fails that CAS and supersedes nothing (receive-pack's own check, the
// same mechanism the containment reconcile's lease already rides).
func TestAuthorizedSupersedeIsOneLeasedAtomicPush(t *testing.T) {
	setIdentity(t)
	ctx := context.Background()
	o, base := taskOpts(t, "wi_w20_mech")
	mkBare(t, o.RemoteURL, false)
	mkTaskCapsule(t, o)
	repoDir, _ := w20LayRepo(t, o, base, "pi-web-gmi")
	const name = "pi-web-gmi"
	log := filepath.Join(base, "pre-receive.log")
	w20InstallHook(t, o.RemoteURL, `#!/bin/sh
echo "begin" >> %q
while read -r old new ref; do
  echo "$old $new $ref" >> %q
done
exit 0
`, log, log)

	b := w20Commit(t, repoDir, "base.txt", "the restore point")
	if err := WriteCarrierRestores(o.CapsuleDir, map[string]string{name: b}); err != nil {
		t.Fatalf("write the carrier-restore record: %v", err)
	}
	tip := w20Commit(t, repoDir, "shared-late.txt", "the earlier session's archive")
	var events []string
	o.EventSink = func(kind string, _ map[string]any) { events = append(events, kind) }
	o.CarrierAlerts = &CarrierAlerts{}
	if _, _, _, err := CommitPush(ctx, o, "v1"); err != nil {
		t.Fatalf("the archive publish: %v", err)
	}
	mustRun(t, "git", "-C", repoDir, "reset", "-q", "--hard", b)
	head := w20Commit(t, repoDir, "rewind.txt", "the rewound session's work")
	if _, _, _, err := CommitPush(ctx, o, "v2 the rewind"); err != nil {
		t.Fatalf("the rewind publish: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("the authorized rewind raised %v, want no events", events)
	}

	// The LAST push is the supersede: one invocation, two ref lines.
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read the pre-receive log: %v", err)
	}
	blocks := strings.Split(strings.TrimRight(string(raw), "\n"), "begin\n")
	var lines []string
	for _, l := range strings.Split(blocks[len(blocks)-1], "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) != 2 {
		t.Fatalf("the supersede push presented %d ref lines (%v), want exactly 2 in one invocation", len(lines), lines)
	}
	var carrierLine, backupLine string
	for _, l := range lines {
		if strings.HasSuffix(l, " "+TaskRefName(name)) {
			carrierLine = l
		}
		if strings.Contains(l, " "+TaskBackupNamespace) {
			backupLine = l
		}
	}
	if carrierLine != tip+" "+head+" "+TaskRefName(name) {
		t.Fatalf("carrier refspec line = %q, want %q (the lease's expected old is the observed tip)", carrierLine, tip+" "+head+" "+TaskRefName(name))
	}
	zero := strings.Repeat("0", 40)
	if !strings.HasPrefix(backupLine, zero+" "+tip+" "+TaskBackupNamespace+name+"/") {
		t.Fatalf("backup refspec line = %q, want a fresh creation of %s%s/<stamp> holding the surpassed tip", backupLine, TaskBackupNamespace, name)
	}
}

// The authorized supersede against the SHIPPED aihub pre-receive hook
// (the w170 fixture's manager): the two-refspec atomic push must land —
// the carrier namespace's non-fast-forward acceptance plus the backup
// namespace's task-history treatment (gitlink exemption, task blob
// limit) — leaving the carrier on the replacement head and the
// surpassed tip preserved and fetchable for a recovery, with no events.
func TestAuthorizedSupersedeLandsThroughRealAihubHook(t *testing.T) {
	setIdentity(t)
	ctx := context.Background()
	o, base := taskOpts(t, "wi_w20_realhook")
	w170RealHookHub(t, &o, base)
	mkTaskCapsule(t, o)
	repoDir, _ := w20LayRepo(t, o, base, "pi-web-gmi")
	const name = "pi-web-gmi"

	// The claim's carrier-restore record: the daemon moved the fresh
	// clone to B; the earlier session archived T beyond it; the rewind
	// reset to B and committed the replacement head H.
	b := w20Commit(t, repoDir, "base.txt", "the restore point")
	if err := WriteCarrierRestores(o.CapsuleDir, map[string]string{name: b}); err != nil {
		t.Fatalf("write the carrier-restore record: %v", err)
	}
	tip := w20Commit(t, repoDir, "shared-late.txt", "the earlier session's archive")
	var events []string
	o.EventSink = func(kind string, _ map[string]any) { events = append(events, kind) }
	o.CarrierAlerts = &CarrierAlerts{}
	if _, _, _, err := CommitPush(ctx, o, "v1"); err != nil {
		t.Fatalf("the archive publish: %v", err)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); !ok || got != tip {
		t.Fatalf("carrier after the archive publish = %q (%v), want the archived head %q", got, ok, tip)
	}

	mustRun(t, "git", "-C", repoDir, "reset", "-q", "--hard", b)
	head := w20Commit(t, repoDir, "rewind.txt", "the rewound session's work")
	if _, _, _, err := CommitPush(ctx, o, "v2 the rewind"); err != nil {
		t.Fatalf("the authorized rewind publish through the shipped hook: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("the authorized rewind raised %v, want no events", events)
	}
	if got, ok := bareRefSHA(t, o.RemoteURL, TaskRefName(name)); !ok || got != head {
		t.Fatalf("carrier after the supersede = %q (%v), want the replacement head %q", got, ok, head)
	}
	backups := w20BackupRefs(t, o.RemoteURL, name)
	if len(backups) != 1 {
		t.Fatalf("the supersede left %d backup refs (%v), want exactly one", len(backups), backups)
	}
	for ref, sha := range backups {
		if sha != tip {
			t.Fatalf("backup %s holds %s, want the surpassed tip %s", ref, sha, tip)
		}
	}
	// The preserved line reads back out of the backup ref (the manual
	// recovery path) and the replacement head out of the carrier (the
	// re-claim restore path).
	fresh := filepath.Join(base, "fresh")
	mustRun(t, "git", "clone", "-q", filepath.Join(base, "origin", name), fresh)
	mustRun(t, "git", "-C", fresh, "fetch", "-q", "--no-tags", o.RemoteURL, TaskRefName(name))
	if got := strings.TrimSpace(mustRun(t, "git", "-C", fresh, "rev-parse", "FETCH_HEAD")); got != head {
		t.Fatalf("carrier read back %s, want the replacement head %s", got, head)
	}
	for ref := range backups {
		mustRun(t, "git", "-C", fresh, "fetch", "-q", "--no-tags", o.RemoteURL, ref)
		if got := strings.TrimSpace(mustRun(t, "git", "-C", fresh, "rev-parse", "FETCH_HEAD")); got != tip {
			t.Fatalf("backup %s read back %s, want the surpassed tip %s", ref, got, tip)
		}
	}
}
