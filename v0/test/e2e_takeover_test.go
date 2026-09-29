package test

// e2e_takeover_test.go: deliverable 2 — the takeover/recovery drill, ONE
// focused test (02 §6.2 / 04 §6.1d / 04 §7.3 step 4 / 05 §2.9 / 05 §7.4):
//
//	force-takeover the running WI from a second machine/identity through
//	the daemon's own takeover intent -> the force-takeover response
//	carries capsule_bootstrap (02 §8, the SAME payload the claim response
//	carries) so machine B's spawn creates its scene with no data-plane
//	reads and no second claim -> the old owner is fenced out of the
//	publish face -> machine B continues from the rebuilt HEAD and wraps.
//
// FIX (polyforge#11 FIX-ROUND D1): the Core's force-takeover route
// (internal/server/handlers_exec.go hForceTakeover) now attaches the
// capsule bootstrap exactly the way the claim route does, so the daemon's
// takeover spawn succeeds end-to-end. Before the fix the spawn died at
// capsule.ParseBootstrap with CAP-400 "empty capsule_bootstrap" AFTER the
// takeover had already landed server-side, and this test pinned that
// divergence; it now pins the fixed behavior.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/daemon"
)

func TestE2ETakeoverRecoveryDrill(t *testing.T) {
	ctx := context.Background()
	core := newCoreEnv(t)

	// --- machine A (worker identity) runs the WI to a real HEAD. ---
	machA := newDaemonEnv(t, core)
	machA.serve()
	wi := core.newWI(t, "e2e takeover drill")
	capc, _ := machA.claimWorkingJob(wi)
	pfA := machA.pf()
	writeCapsuleFile(t, capc, "artifacts/hello.txt", "hello v1\n")
	if _, err := pfA.PublishRequest(wi, "step_completed"); err != nil {
		t.Fatalf("A publish 1: %v", err)
	}
	writeCapsuleFile(t, capc, "artifacts/hello.txt", "hello v2\n")
	if _, err := pfA.PublishRequest(wi, "step_completed"); err != nil {
		t.Fatalf("A publish 2: %v", err)
	}
	headSeq, _ := core.headRow(wi)
	if headSeq != 1 {
		t.Fatalf("two publishes must advance HEAD to 1, got %d", headSeq)
	}

	// --- second machine/identity (admin) takes over via the daemon's own
	// takeover intent (04 §6.1d): the response must carry capsule_bootstrap
	// (02 §8), so the spawn builds machine B's scene with no data-plane
	// reads and no second claim (02 §6.2). ---
	machB := newMachineEnv(t, core, "mac_b", core.adminKey, core.ts.URL)
	machB.serve()
	if err := machB.d.Submit(daemon.SubmitIntent{WorkItemID: wi, Intent: "takeover"}); err != nil {
		t.Fatalf("takeover submit: %v", err)
	}
	ok, spawnErr := machB.d.SpawnNext(ctx)
	if spawnErr != nil || !ok {
		t.Fatalf("the daemon takeover intent must spawn from the response's "+
			"capsule_bootstrap (02 §8; the Core force-takeover response carries it): %v", spawnErr)
	}
	// The takeover landed server-side: generation 1 ended with
	// end_reason=takeover, generation 2 active under machine B.
	ends := core.genEnds(wi)
	if len(ends) != 2 || ends[1] != "takeover" || ends[2] != "" {
		t.Fatalf("takeover must end gen 1 and open gen 2: %+v", ends)
	}
	if s := core.wiStatus(wi); s != "running" {
		t.Fatalf("wi must be running under the new generation: %s", s)
	}
	// The formal jobs[] row exists (generation 2), exactly one WI Agent was
	// derived, and no pending intent survives the successful spawn.
	if j := machB.state().FindJob(wi); j == nil || j.Generation != 2 {
		t.Fatalf("the takeover machine's job row must be generation 2: %+v", j)
	}
	if n := len(machB.spawner.spawns); n != 1 ||
		machB.spawner.spawns[0].WorkItemID != wi || machB.spawner.spawns[0].Generation != 2 {
		t.Fatalf("exactly one gen-2 WI Agent must be derived on the takeover machine: %+v", machB.spawner.spawns)
	}
	for _, p := range machB.state().PendingJobs {
		if p.WorkItemID == wi {
			t.Fatalf("no pending intent may survive the successful takeover spawn: %+v", p)
		}
	}
	// The rebuilt scene is the recovery replay of the remote chain to
	// HEAD (05 §7.4): machine B's capsule carries the v2 content, no
	// data-plane reads needed beyond the takeover response itself.
	capB, _ := machB.openCapsule(wi)
	got, err := readFile(filepath.Join(capB.Dir, "artifacts", "hello.txt"))
	if err != nil || string(got) != "hello v2\n" {
		t.Fatalf("the takeover scene must replay the chain to HEAD's content (v2): %q %v", got, err)
	}

	// --- 05 §2.9: the fenced-out old owner cannot publish anymore. ---
	writeCapsuleFile(t, capc, "artifacts/hello.txt", "hello fenced\n")
	_, err = machA.d.PublishRequest(ctx, wi, "step_completed")
	if err == nil || !strings.Contains(err.Error(), "stale_generation") {
		t.Fatalf("machine A's publish must be refused by generation fencing, got %v", err)
	}

	// A second claim of the running WI is impossible (02 §6.2: recovery
	// consumes the takeover response directly, never a second claim).
	if _, err := core.client(core.adminKey).Claim(wi, "drill-second-claim", "mac_b"); err == nil {
		t.Fatal("a claim of the running WI must be refused")
	} else {
		mustBeAPIError(t, err, "CORE-409", "ALREADY_CLAIMED")
	}

	// --- machine B continues and closes through the real daemon face:
	// gen-2 work, a gen-2 publish, then the wrap verb (00 §5.1). ---
	writeCapsuleFile(t, capB, "artifacts/hello.txt", "hello v3 (by machine B)\n")
	raw, err := machB.pf().PublishRequest(wi, "step_completed")
	if err != nil {
		t.Fatalf("B publish: %v", err)
	}
	var res daemon.PublishOutcome
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("gen-2 publish outcome undecodable: %s (%v)", raw, err)
	}
	if !res.Published || res.HeadSeq != 2 {
		t.Fatalf("gen-2 publish: %+v", res)
	}
	if hseq, hsha := core.headRow(wi); hseq != 2 || hsha != res.ManifestSha || core.headGeneration(wi) != 2 {
		t.Fatalf("core head (%d,%s,gen %d) != gen-2 publish (%d,%s)",
			hseq, hsha, core.headGeneration(wi), res.HeadSeq, res.ManifestSha)
	}
	code, raw := udsBody(t, machB.socket, "POST", "/jobs/"+wi+"/wrap",
		map[string]any{"reason": "wrap"}, nil, nil)
	if code != 200 {
		t.Fatalf("gen-2 wrap: %d %s", code, raw)
	}
	out := daemonFinish(t, raw)
	if s := core.wiStatus(wi); s != "wrapped" {
		t.Fatalf("wi must be wrapped after the drill, got %s", s)
	}
	// The full close story: gen 1 ended exactly once (takeover), gen 2
	// ended exactly once (wrap); no pause/block ever landed.
	ends = core.genEnds(wi)
	if len(ends) != 2 || ends[1] != "takeover" || ends[2] != "wrap" {
		t.Fatalf("generation ends after drill: %+v", ends)
	}
	if core.eventCount(wi, "paused") != 0 || core.eventCount(wi, "wrapped") != 1 {
		t.Fatalf("event story: paused=%d wrapped=%d", core.eventCount(wi, "paused"), core.eventCount(wi, "wrapped"))
	}
	if q := readyQueue(t, core); q.hasExecutable(wi) || q.hasBlocked(wi) {
		t.Fatalf("wrapped wi must be gone from the ready queue: %+v", q)
	}
	// The gen-1 scene stayed on machine A for audit — QUARANTINED (moved
	// under _quarantine, marked, never deleted) and never resurrected:
	// 04 §6.2 B step 3, the takeover fence's local seal of the old owner's
	// scene (05 §2.9/§7.3: the old daemon stops publishing and seals).
	qm, _ := filepath.Glob(filepath.Join(machA.wsRoot(), "capsules", "_quarantine", wi, "*"))
	if len(qm) != 1 {
		t.Fatalf("machine A's capsule must be preserved exactly once under the quarantine tree: %v", qm)
	}
	if j := machA.state().FindJob(wi); j != nil {
		t.Fatalf("the fenced-out old owner's jobs[] record must be dropped: %+v", j)
	}
	if _, err := os.Stat(filepath.Join(capB.Dir, "repos", "inventory.json")); err != nil {
		t.Fatalf("the rebuilt scene must carry the inventory snapshot: %v", err)
	}
	if hseq, hsha := core.headRow(wi); hseq != out.HeadSeq || hsha != out.ManifestSha {
		t.Fatalf("the wrap's close must leave HEAD at the final flush: (%d,%s) vs (%d,%s)",
			hseq, hsha, out.HeadSeq, out.ManifestSha)
	}
}

// readFile is os.ReadFile with the test's fatal-on-error convention.
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
