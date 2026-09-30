package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// v1_s12: batch mode. A bare `pf batch-confirm` is a read-only
// recommendation (NO job is created — the daemon queue stays empty);
// the confirmed wave (`--all`) submits exactly the recommendation set,
// and the dispatcher runs the whole wave to wrapped, ordered by
// priority within the machine's concurrency cap.
func TestV1S12BatchWave(t *testing.T) {
	inst := newV1Install(t)
	wiHigh := inst.newWI(t, "s12: batch wave member A", []map[string]any{
		{"do": "step", "id": "code"}, {"do": "wrap"},
	}, nil)
	// file the second ticket with high priority so the wave is ordered
	raw, err := inst.client.CreateWorkItem(aihubclient.CreateWorkItemRequest{
		Project:    v1Project,
		Goal:       "s12: batch wave member B",
		Priority:   "high",
		BundleName: strPtr("quick-fix"),
		Attrs:      map[string]any{"requires_human_session": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wiB struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &wiB); err != nil {
		t.Fatal(err)
	}
	wiBHigh := wiB.ID
	inst.setPlan(t, wiBHigh, []map[string]any{{"do": "step", "id": "code"}, {"do": "wrap"}})
	// filed through the raw client (priority), so register it for the
	// harness's post-teardown orphan sweep by hand.
	inst.trackWI(wiBHigh)

	d := newV1Daemon(t, inst)
	d.start()

	// The bare recommendation: read-only, no job created.
	out := d.pf(t, "batch-confirm")
	if !strings.Contains(out, "2 recommended") {
		t.Fatalf("recommendation output: %q", out)
	}
	// The daemon queue is still empty: the read-only pass created nothing.
	st := d.status(t)
	if st.Queue.Pending != 0 || len(st.Jobs) != 0 {
		t.Fatalf("read-only recommendation created work: %+v", st)
	}

	// The confirmed wave: exactly the recommendation set.
	out = d.pf(t, "batch-confirm", "--all")
	if !strings.Contains(out, "wave submitted: 2") {
		t.Fatalf("wave submission output: %q", out)
	}

	// Both members ran to wrapped.
	inst.waitWIStatus(t, wiHigh, "wrapped", 60*time.Second)
	inst.waitWIStatus(t, wiBHigh, "wrapped", 60*time.Second)

	// The wave never widened: nothing else was claimed.
	items, err := inst.client.ListWorkItems(v1Project, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("project holds %d work items, want 2", len(items))
	}

	// The dispatcher's priority ordering: the high-priority member
	// started first (its capsule was created before its peer's).
	hiBorn := capsuleBornAt(t, d.capsuleDir(wiBHigh))
	loBorn := capsuleBornAt(t, d.capsuleDir(wiHigh))
	if !hiBorn.Before(loBorn) {
		t.Fatalf("high-priority wave member started after its peer (%v ≥ %v)", hiBorn, loBorn)
	}
}

// capsuleBornAt is the capsule's creation time (capsule.json mtime —
// materialization writes it fresh per claim).
func capsuleBornAt(t *testing.T, dir string) time.Time {
	t.Helper()
	st, err := os.Stat(filepath.Join(dir, "capsule.json"))
	if err != nil {
		t.Fatalf("capsule.json of %s: %v", dir, err)
	}
	return st.ModTime()
}
