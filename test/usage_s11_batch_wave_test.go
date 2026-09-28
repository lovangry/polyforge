package test

// usage_s11_batch_wave_test.go: S11 — the Batch wave END TO END, the
// pilot's Batch mode (00 §3.4b; 04 §6.1b): real aihub, real `pf daemon`
// process (started WITH --project, without which the recommendation face
// has nothing to read), real socket, real pf CLI. Until now this path had
// ZERO tests anywhere: no unit test, no usage test, no subprocess
// coverage. The full operator walk is pinned here:
//
//   1. `pf batch-confirm` with no arguments is READ-ONLY: it shows the
//      recommended wave (all three queued WIs) and confirms NOTHING — no
//      WI Agent is derived, no claim happens;
//   2. `pf batch-confirm <id>` confirms and submits EXACTLY that set —
//      one agent spawns, the other two WIs stay queued;
//   3. in batch mode (the wave's home), `--all` submits the WHOLE
//      remaining recommendation — the daemon enqueues every member and
//      spawns them all (whole wave, never truncated);
//   4. `--all` against an empty recommendation is refused with the
//      operator error, exit 1.

import (
	"strings"
	"testing"
)

func TestUsageS11BatchConfirmWave(t *testing.T) {
	inst := newUsageInstall(t)
	m := inst.newUsageMachine(t, "mac_s11")
	// --project usage: the daemon's recommendation face targets this
	// project's ready queue. FAKEPI default (idle): agents park running,
	// so the wave's members stay "running" for stable assertions.
	m.startWithArgs([]string{"--project", "usage"})

	w1 := inst.wiCreate(t, "S11 wave: member one")
	w2 := inst.wiCreate(t, "S11 wave: member two")
	w3 := inst.wiCreate(t, "S11 wave: member three")

	// 1. Read-only recommendation display: every queued WI is listed with
	// the confirm hint, and NOTHING is created (04 §6.1b step 1).
	out := m.pfOn("batch-confirm")
	for _, want := range []string{w1, w2, w3, "confirm with: pf batch-confirm --all"} {
		if !strings.Contains(out, want) {
			t.Fatalf("recommendation display must contain %q:\n%s", want, out)
		}
	}
	if m.spawnCount() != 0 {
		t.Fatalf("a recommendation display must not spawn; got %d agents", m.spawnCount())
	}

	// JSON mode: the recommendation body is the ready queue as-is.
	raw := m.pfOn("--json", "batch-confirm")
	var rec struct {
		Project    string `json:"project"`
		Executable []struct {
			ID string `json:"id"`
		} `json:"executable"`
	}
	if err := jsonUnmarshalStrict(raw, &rec); err != nil {
		t.Fatalf("json recommendation: %q (%v)", raw, err)
	}
	if rec.Project != "usage" {
		t.Fatalf("recommendation project: %q", rec.Project)
	}
	if len(rec.Executable) != 3 {
		t.Fatalf("the recommendation must list all three queued WIs, got %+v", rec.Executable)
	}

	// 2. Explicit-set confirmation: EXACTLY the named WI is submitted.
	out = m.pfOn("batch-confirm", w1)
	if !strings.Contains(out, "wave submitted: 1 work item(s)") {
		t.Fatalf("explicit-set confirmation answer:\n%s", out)
	}
	waitUntilUse(t, "the confirmed member's WI Agent to be derived", func() bool { return m.spawnCount() == 1 })
	waitUntilUse(t, "the confirmed member to be running", func() bool { return inst.wiStatus(t, w1) == "running" })

	// The other two members are untouched: still queued, still the
	// recommendation.
	for _, id := range []string{w2, w3} {
		if s := inst.wiStatus(t, id); s != "queued" {
			t.Fatalf("unconfirmed member %s must stay queued, got %s", id, s)
		}
	}

	// 3. Batch mode is the wave's home; --all submits the WHOLE
	// remaining recommendation.
	if out := m.pfOn("ops", "mode", "set", "--mode", "batch"); !strings.Contains(out, `"mode": "batch"`) {
		t.Fatalf("mode set must answer batch:\n%s", out)
	}
	out = m.pfOn("batch-confirm", "--all")
	if !strings.Contains(out, "wave submitted: 2 work item(s)") {
		t.Fatalf("whole-wave confirmation answer:\n%s", out)
	}
	waitUntilUse(t, "both remaining wave members to be derived", func() bool { return m.spawnCount() == 3 })
	for _, id := range []string{w2, w3} {
		waitUntilUse(t, "wave member "+id+" to be running", func() bool { return inst.wiStatus(t, id) == "running" })
	}

	// 4. An empty recommendation refuses --all with the operator error
	// (exit 1) — the daemon never fabricates a wave from nothing.
	errs := m.pfOnExit(1, "batch-confirm", "--all")
	if !strings.Contains(errs, "the recommendation is empty") {
		t.Fatalf("empty-recommendation refusal must say so, got: %s", errs)
	}

	// 5. Graceful shutdown WITH the whole wave live (three running jobs):
	// the close lands server-side (every member paused) before the
	// daemon process exits 0 — the operator's end-of-batch-day walk. This
	// also makes the daemon exit CLEANLY, which a coverage-instrumented
	// manual round needs (a SIGKILLed daemon never flushes its counters).
	if out := m.pfOn("ops", "shutdown"); !strings.Contains(out, "graceful shutdown requested") {
		t.Fatalf("ops shutdown must confirm, got:\n%s", out)
	}
	waitUntilUse(t, "the daemon process to exit", func() bool { return daemonExitedUsage(m.cmd) })
	daemonAssertExit0Usage(t, m.cmd)
	for _, id := range []string{w1, w2, w3} {
		waitUntilUse(t, "wave member "+id+" to close as paused", func() bool { return inst.wiStatus(t, id) == "paused" })
	}
}
