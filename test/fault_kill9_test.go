package test

// fault_kill9_test.go: fault category (b) — kill -9 of the daemon process
// in the middle of the close (04 §10 kill -9 category; 04 §6.2 A path /
// §7.1 crash recovery). The crash point is the dangerous one: closeJob
// interrupted BETWEEN the final flush and the end-generation call.
//
// Simulation drops the process boundary exactly as the deliverable
// prescribes: the daemon runs the REAL close (mark interrupting ->
// boundary notice -> final flush -> Core end-generation), but its Core
// face is fronted by a hold proxy that parks the end-generation request
// on a hijacked connection. When the drill closes that connection the
// "process" is dead: its in-flight call dies mid-air, its memory (which is
// the ONLY place the generation token lives) is gone. A second Daemon
// instance over the same data dir + workspace is the restart; its
// RecoverFromState (04 §7.1) re-adopts the jobs[] row.
//
// Assertions:
//   - the flush landed (HEAD advanced) but the close did NOT: the WI is
//     still running, generation 1 still active, zero close events;
//   - the restarted daemon re-adopts the job (crash recovery picks it
//     up) but CANNOT re-issue the close: the generation token was lost
//     with the process (state.json deliberately records no execution
//     secret material, INV-D1/D8), so its end-generation is fenced out
//     as CORE-403 stale_generation — surfaced by the pause route as the
//     typed CORE-403 passthrough itself (04 §5.1 close-routes amendment,
//     polyforge#11 FIX-ROUND D3; the old DAEMON-500 flattening is gone);
//   - no double close EVER lands: generation 1 receives exactly one end
//     (the arbitration force-takeover), and the drill converges the WI
//     to wrapped through the recovery-consumption path (02 §6.2), with
//     the server's one-close guard refusing any further end.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/daemon"
)

func TestFaultInjectKill9MidClose(t *testing.T) {
	ctx := context.Background()
	core := newCoreEnv(t)

	// Machine A runs behind the hold proxy: every end-generation request
	// parks until the drill kills it.
	proxySrv, held := newHoldProxy(t, core.ts.Config.Handler)
	machA := newMachineEnv(t, core, "mac_a", core.workerKey, proxySrv.URL)
	machA.serve()
	pfA := machA.pf()

	wi := core.newWI(t, "fault: kill -9 mid-close")
	capc, _ := machA.claimWorkingJob(wi)
	if _, err := pfA.PublishRequest(wi, "step_completed"); err != nil {
		t.Fatalf("working publish: %v", err)
	}
	headSeq, headSha := core.headRow(wi)
	if headSeq != 0 {
		t.Fatalf("expected HEAD at 0 after the working publish, got %d", headSeq)
	}
	// Unpublished work: the crashed close's final flush will have to do a
	// REAL publish (HEAD -> 1) before it can generate its evidence.
	writeCapsuleFile(t, capc, "artifacts/work.md", "unpublished work\n")

	// The close: real route, real closeJob. The flush will find the
	// unpublished artifacts and do a REAL publish (HEAD -> 1); the
	// end-generation parks on the held connection.
	parked := make(chan int, 1)
	go func() {
		code, _ := udsBody(t, machA.socket, "POST", "/jobs/"+wi+"/pause", map[string]any{}, nil, nil)
		parked <- code
	}()
	// The flush landed (HEAD advanced to 1) and the close is now parked
	// between flush and end-generation.
	waitUntil(t, "the close's final flush to publish HEAD 1", func() bool {
		seq, _ := core.headRow(wi)
		return seq == 1
	})
	select {
	case conn := <-held:
		// The end-generation request arrived and is parked: kill -9.
		_ = conn.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the end-generation request never reached the hold proxy")
	}
	if code := <-parked; code == 200 {
		t.Fatal("the interrupted close must not answer success")
	}
	// The daemon process is dead from here on: nothing else may touch
	// machA.d (a graceful shutdown would run a SECOND close through the
	// parked proxy — exactly what the one-close guard exists to prevent).

	// Crash residue, both sides:
	// - Core: the flush's revision is HEAD, but NO close landed.
	if seq, sha := core.headRow(wi); seq != 1 || sha == headSha {
		t.Fatalf("the flush's revision must be HEAD: (%d,%s) vs (%d,%s)", seq, sha, 1, headSha)
	}
	if s := core.wiStatus(wi); s != "running" {
		t.Fatalf("the WI must still be running (the close never landed): %s", s)
	}
	if ends := core.genEnds(wi); len(ends) != 1 || ends[1] != "" {
		t.Fatalf("generation 1 must still be active: %+v", ends)
	}
	if core.eventCount(wi, "paused") != 0 {
		t.Fatal("no paused event may exist (one-close: nothing landed)")
	}
	// - Daemon state: the interrupting mark the close wrote before dying.
	if j := machA.state().FindJob(wi); j == nil || j.Status != daemon.JobInterrupting {
		t.Fatalf("the crashed close must leave the honest interrupting mark: %+v", j)
	}

	// --- restart: a second Daemon over the same data dir + workspace; the
	// crash-recovery path (04 §7.1) re-adopts the job. ---
	machB := newMachineEnv(t, core, "mac_a", core.workerKey, core.ts.URL)
	// The restart shares the dead process's on-disk scene, not its memory
	// (the first Daemon instance newMachineEnv built over its own empty
	// dirs is simply never driven).
	machB.cfg.DataDir = machA.cfg.DataDir
	machB.cfg.WorkspaceRoot = machA.cfg.WorkspaceRoot
	d2, err := daemon.New(machB.cfg, newFakeSpawner(t))
	if err != nil {
		t.Fatal(err)
	}
	machB.d = d2
	if err := d2.RecoverFromState(ctx); err != nil {
		t.Fatalf("crash recovery: %v", err)
	}
	machB.serve()
	waitUntil(t, "the recovered daemon to expose the job", func() bool {
		_, raw := udsBody(t, machB.socket, "GET", "/status", nil, nil, nil)
		return strings.Contains(string(raw), wi)
	})
	// The recovered scene is the same generation, same capsule.
	if j := machB.state().FindJob(wi); j == nil || j.Generation != 1 {
		t.Fatalf("the recovered job row must keep generation 1: %+v", j)
	}
	// The dead process's WI Agent is NOT resurrected as a new instance:
	// the recorded pid was still alive, so recovery re-attached it.
	if len(machB.spawner.spawns) != 0 {
		t.Fatalf("crash recovery must not re-derive a live WI Agent: %d", len(machB.spawner.spawns))
	}

	// --- the recovered daemon re-attempts the close — and CANNOT land it:
	// the generation token lived only in the dead process's memory
	// (daemon jobs[] carries no execution secret, INV-D1/D8), so its
	// end-generation is fenced out. FIX (polyforge#11 FIX-ROUND D3): the
	// close routes pass the typed Core verdict through verbatim now — the
	// answer IS the Core's CORE-403 with its reason in the detail, at the
	// Core's own HTTP status, not a DAEMON-500 with the code buried inside.
	code, env := udsErr(t, machB.socket, "POST", "/jobs/"+wi+"/pause", map[string]any{}, nil)
	if code != 403 || env.Error.Code != "CORE-403" ||
		!strings.Contains(env.Error.Detail, "stale_generation") {
		t.Fatalf("the recovered daemon's close must be fenced out by the lost "+
			"generation token (CORE-403 stale_generation, passed through verbatim): %d %+v", code, env.Error)
	}
	if core.eventCount(wi, "paused") != 0 || len(core.genEnds(wi)) != 1 {
		t.Fatalf("the refused close must land nothing: %+v", core.genEnds(wi))
	}

	// --- convergence: the only arbitration left is force-takeover (02
	// §6.2): it ends generation 1 (its ONLY end — no double close) and
	// hands the new generation's credentials to the recovery side, which
	// consumes them directly (04 §7.3 step 4) and wraps. ---
	tk, err := core.client(core.adminKey).ForceTakeover(wi, "mac_b")
	if err != nil {
		t.Fatalf("force-takeover: %v", err)
	}
	if tk.Generation != 2 || tk.GenerationToken == "" {
		t.Fatalf("takeover must return gen-2 credentials: %+v", tk)
	}
	ends := core.genEnds(wi)
	if len(ends) != 2 || ends[1] != "takeover" || ends[2] != "" {
		t.Fatalf("generation 1 must have exactly one end (takeover): %+v", ends)
	}
	// The recovery machine rebuilds the scene from the remote chain (05
	// §7.4) — which includes the flush the crashed close DID publish.
	site := rebuildSite(ctx, t, core, core.adminKey, "mac_b", wi, 2, tk.GenerationToken,
		t.TempDir(), t.TempDir())
	if raw, err := os.ReadFile(site.cap.Dir + "/artifacts/work.md"); err != nil || string(raw) != "unpublished work\n" {
		t.Fatalf("the recovered scene must contain the flush's content: %q %v", raw, err)
	}
	site.work("artifacts/work.md", "recovered and finished\n")
	res := site.publish(ctx)
	fe := site.finalFlush(ctx)
	if err := site.endGeneration(ctx, "wrap", fe); err != nil {
		t.Fatalf("gen-2 wrap: %v", err)
	}
	if s := core.wiStatus(wi); s != "wrapped" {
		t.Fatalf("the drill must converge to wrapped, got %s", s)
	}
	if ends = core.genEnds(wi); len(ends) != 2 || ends[1] != "takeover" || ends[2] != "wrap" {
		t.Fatalf("final generation story: %+v", ends)
	}
	if hseq, _ := core.headRow(wi); hseq != res.HeadSeq+0 || core.headGeneration(wi) != 2 {
		t.Fatalf("HEAD must have advanced under generation 2: seq=%d gen=%d", hseq, core.headGeneration(wi))
	}

	// --- the server one-close guard: any further end is CORE-409. ---
	err = site.endGeneration(ctx, "wrap", fe)
	mustBeAPIError(t, err, "CORE-409", "")

	// The dead process's WI Agent was never terminated by the harness
	// mid-test; its child is reaped at cleanup. Nothing more to assert.
}
