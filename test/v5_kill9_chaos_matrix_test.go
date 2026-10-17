package test

// v5: the kill -9 chaos matrix. The real daemon is SIGKILLed at an exact
// point of its work (crashpoint.At, armed through PF_CRASH_AT — no
// timing), restarted on the same data dir and workspace against the
// same aihub, and the work item must converge: no verdict lost, no
// completed step run again, no work item wrongly blocked, no accepted
// submit lost.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/crashpoint"
)

// armed is d with the crash point armed for its next start.
func armed(d *v1Daemon, point string) *v1Daemon {
	d.extraEnv = append(d.extraEnv, crashpoint.EnvVar+"="+point)
	return d
}

// waitCrashed waits until the daemon died at its armed crash point.
func (d *v1Daemon) waitCrashed(bound time.Duration) {
	d.t.Helper()
	select {
	case <-d.waitedExit:
	case <-time.After(bound):
		d.t.Fatalf("the daemon never reached its crash point (log tail: %s)", d.logTail())
	}
	d.cmd = nil
}

// restarted prepares the same daemon (socket, data dir, workspace,
// machine) for a fresh start, with extraArgs and no crash point unless
// one is armed on the result.
func (d *v1Daemon) restarted(extraArgs ...string) *v1Daemon {
	d2 := &v1Daemon{
		t: d.t, inst: d.inst, sock: d.sock, dataDir: d.dataDir, workspace: d.workspace,
		machineID: d.machineID, extraArgs: extraArgs,
		logPath: filepath.Join(d.t.TempDir(), "daemon-restarted.log"),
	}
	d.t.Cleanup(func() { d2.stop() })
	return d2
}

// pfAnyExit runs the pf CLI against d whatever its exit (the daemon may
// die while answering).
func (d *v1Daemon) pfAnyExit(args ...string) {
	cmd := exec.Command(d.inst.pfPath, append([]string{"--config", hermeticConfigPath(d.t)}, args...)...)
	cmd.Env = append(os.Environ(), "PF_SERVER="+d.inst.url, "PF_API_KEY="+d.inst.adminKey,
		"PF_USER_ID=usr_root", "PF_DAEMON_SOCKET="+d.sock)
	_ = cmd.Run()
}

func gateFailPlan() []map[string]any {
	verdict := map[string]any{"verdict": "FAIL", "error": "chaos: injected verification failure"}
	return []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "gate_failed", "output": verdict},
		{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "verification", "outcome": "completed"},
	}
}

// assertConverged: the work item wrapped (not blocked) with exactly
// spawns dispatches — no step ran twice.
func assertConverged(t *testing.T, inst *v1Install, d *v1Daemon, wi string, spawns int) *executionView {
	t.Helper()
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)
	capDir := d.capsuleDir(wi)
	if got := spawnJournal(t, capDir); len(got) != spawns {
		t.Fatalf("spawn journal has %d dispatches, want %d (a completed step ran again): %+v", len(got), spawns, got)
	}
	x := readExecution(t, capDir)
	for _, step := range []string{"grill", "fix", "verification"} {
		if st := x.Steps[step]; st == nil || st.Status != "done" {
			t.Fatalf("step %s: %+v, want done", step, st)
		}
	}
	return x
}

func acceptedAttempts(st *execStepView) int {
	n := 0
	for _, a := range st.Attempts {
		if a["cause"] == "accepted" {
			n++
		}
	}
	return n
}

// TestV5Kill9ChaosMatrix kills the daemon at each key point of a work
// item's life and asserts the restart converges.
func TestV5Kill9ChaosMatrix(t *testing.T) {
	cases := []struct {
		name, point string
		plan        []map[string]any
		check       func(t *testing.T, inst *v1Install, d *v1Daemon, wi string)
	}{
		{"claim before the answer", "claim.before-answer", envelopeAutopilot(),
			func(t *testing.T, inst *v1Install, d *v1Daemon, wi string) { assertConverged(t, inst, d, wi, 3) }},
		{"claim after the answer", "claim.after-answer", envelopeAutopilot(),
			func(t *testing.T, inst *v1Install, d *v1Daemon, wi string) { assertConverged(t, inst, d, wi, 3) }},
		{"step result collected", "driver.collected:fix", envelopeAutopilot(),
			func(t *testing.T, inst *v1Install, d *v1Daemon, wi string) {
				x := assertConverged(t, inst, d, wi, 3)
				if n := acceptedAttempts(x.Steps["fix"]); n != 1 {
					t.Fatalf("fix accepted %d times, want once", n)
				}
			}},
		{"gate verdict", "driver.settled:verification", gateFailPlan(),
			func(t *testing.T, inst *v1Install, d *v1Daemon, wi string) {
				x := assertConverged(t, inst, d, wi, 5)
				if n, _ := x.RewindStats["count"].(float64); n != 1 {
					t.Fatalf("the FAIL verdict must rewind exactly once: %+v", x.RewindStats)
				}
			}},
		{"seal pushed", "seal.pushed", envelopeAutopilot(),
			func(t *testing.T, inst *v1Install, d *v1Daemon, wi string) { assertConverged(t, inst, d, wi, 3) }},
		{"session ended", "seal.ended", envelopeAutopilot(),
			func(t *testing.T, inst *v1Install, d *v1Daemon, wi string) { assertConverged(t, inst, d, wi, 3) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst := newV1Install(t)
			wi := inst.newWI(t, "v5 chaos: "+c.name, c.plan, nil)
			t.Cleanup(func() { killOrphans(wi) })
			d := armed(newV1Daemon(t, inst), c.point)
			d.start()
			d.pfAnyExit("submit", wi)
			d.waitCrashed(90 * time.Second)

			d2 := d.restarted()
			d2.start()
			c.check(t, inst, d2, wi)
		})
	}

	t.Run("pause", func(t *testing.T) {
		inst := newV1Install(t)
		const qid = "q_v5_pause"
		wi := inst.newWI(t, "v5 chaos: pause", []map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": qid, "question": "v5: park here"},
		}, nil)
		t.Cleanup(func() { killOrphans(wi) })
		d := armed(newV1Daemon(t, inst), "pause.persisted")
		d.start()
		d.pf(t, "submit", wi)
		waitQuestion(t, d, wi, qid, 30*time.Second)
		d.pfAnyExit("pause", wi)
		d.waitCrashed(60 * time.Second)

		d2 := d.restarted()
		d2.start()
		inst.waitWIStatus(t, wi, "paused", 60*time.Second)
		if s := inst.sessionOf(t, wi); !s.CleanPause {
			t.Fatalf("the operator pause must land clean: %+v", s)
		}
		time.Sleep(2 * time.Second) // a wrongly resumed WI would dispatch again here
		if got := spawnJournal(t, d2.capsuleDir(wi)); len(got) != 1 {
			t.Fatalf("the paused work item was resumed by the restart: %d dispatches", len(got))
		}
		if st := inst.wiStatus(t, wi); st != "paused" {
			t.Fatalf("work item %s after the restart, want paused", st)
		}
	})

	t.Run("submit queue replay", func(t *testing.T) {
		inst := newV1Install(t)
		const qid = "q_v5_slot"
		holder := inst.newWI(t, "v5 chaos: slot holder", []map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "needs_input", "question_id": qid, "question": "v5: hold the slot"},
		}, nil)
		queued := []string{
			inst.newWI(t, "v5 chaos: queued one", envelopeAutopilot(), nil),
			inst.newWI(t, "v5 chaos: queued two", envelopeAutopilot(), nil),
		}
		t.Cleanup(func() {
			for _, wi := range append([]string{holder}, queued...) {
				killOrphans(wi)
			}
		})
		one := []string{"--max-concurrent-jobs", "1"}
		d := newV1Daemon(t, inst)
		d.extraArgs = one
		d.start()
		d.pf(t, "submit", holder)
		waitQuestion(t, d, holder, qid, 30*time.Second)
		for _, wi := range queued {
			if out := d.pf(t, "submit", wi); !strings.Contains(out, "accepted") {
				t.Fatalf("submit %s: %q", wi, out)
			}
		}
		d.crash()

		// killed while replaying the second queued entry
		d2 := armed(d.restarted(one...), "queue.replay:1")
		d2.launch()
		d2.waitCrashed(60 * time.Second)

		d3 := d.restarted("--max-concurrent-jobs", "3")
		d3.start()
		for _, wi := range queued {
			inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)
		}
	})
}
