package test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// v1_s13: drain mode. The daemon (booted straight into drain via its
// persisted mode) autonomously claims the project's ready queue in
// waves and runs every ticket to completion with no operator action.
// Single submits are refused in drain (DAEMON-409) — the internal loop
// is the only creation entry — and the refused ticket stays queued at
// aihub, untouched.
func TestV1S13Drain(t *testing.T) {
	inst := newV1Install(t)
	wiA := inst.newWI(t, "s13: drain wave member A", []map[string]any{
		{"do": "step", "id": "code"}, {"do": "wrap"},
	}, nil)
	wiB := inst.newWI(t, "s13: drain wave member B", []map[string]any{
		{"do": "step", "id": "code"}, {"do": "wrap"},
	}, nil)
	wiC := inst.newWI(t, "s13: drain wave member C", []map[string]any{
		{"do": "step", "id": "code"}, {"do": "wrap"},
	}, nil)
	// Park the refusal-probe ticket in the needs-human-session segment so
	// the drain loop itself never claims it (drain only fetches the
	// autonomous segment); the single-submit refusal under test is the
	// MODE's, whatever the ticket.
	rawD, err := inst.client.CreateWorkItem(aihubclient.CreateWorkItemRequest{
		Project:    v1Project,
		Goal:       "s13: drain never claims me (needs a human)",
		BundleName: strPtr("quick-fix"),
		Attrs:      map[string]any{"requires_human_session": true},
	})
	if err != nil {
		t.Fatalf("file refusal-probe ticket: %v", err)
	}
	var wiD struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rawD, &wiD); err != nil {
		t.Fatal(err)
	}

	d := newV1Daemon(t, inst, optMode("drain"))
	d.start()

	// The drain waves claim the ready queue on their own (wave period 5s).
	inst.waitWIStatus(t, wiA, "wrapped", 60*time.Second)
	inst.waitWIStatus(t, wiB, "wrapped", 60*time.Second)
	inst.waitWIStatus(t, wiC, "wrapped", 60*time.Second)

	// Both ran on this daemon and sealed their own chains.
	for _, wi := range []string{wiA, wiB, wiC} {
		if inst.bareCommitCount(t, wi) == 0 {
			t.Fatalf("%s never pushed its chain", wi)
		}
	}

	// Single submissions are refused in drain — and the refusal is the
	// typed DAEMON-409, with the ticket left untouched at aihub.
	out := d.pfCode(t, 1, "submit", wiD.ID)
	if !strings.Contains(out, "DAEMON-409") {
		t.Fatalf("drain single-submit refusal wrong: %q", out)
	}
	if st := inst.wiStatus(t, wiD.ID); st != "queued" {
		t.Fatalf("refused ticket was disturbed: %q", st)
	}

	// The daemon is in drain mode and the queue drained to empty.
	st := d.status(t)
	if st.Mode != "drain" {
		t.Fatalf("daemon mode %q, want drain", st.Mode)
	}
	if st.Queue.Pending != 0 || len(st.Jobs) != 0 {
		t.Fatalf("queue not drained: %+v", st)
	}
}
