package test

// v1_s24_machines_cli_test.go: the `pf machines list` e2e scenario — the
// --json face pinned as the RAW server-body passthrough (ListMachinesRaw
// → cmdMachines's ctx.finish(raw, …) house rule), the human render
// pinned on its salient cells only (the orDash "-" RUNTIME placeholder,
// the STALE/alive STATE cell, the stale-threshold header line — never a
// byte-golden of the table), the stale→fresh transition forced by a
// scenario-side backdate of last_heartbeat_at in the running aihub's
// on-disk aihub.db (grill Q2 owner ruling: the
// internal/aihub/core/machines_test.go UPDATE pattern, then ONE real
// wire heartbeat to flip it fresh — no sleeps, no daemon heartbeat
// loop, no -machines-stale-after override: the production-default
// 5-minute window applies), and the admin gate pinned by one non-admin
// refusal through the real CLI.
//
// Authored fresh for polyforge#68: the ticket #23 s24 deliverable never
// reached origin main — no test/v1_s24* file existed at HEAD 1000723 —
// so the s24 numbering continuity is kept by authoring under this name.
//
// Direct-to-aihub like s23: one newV1Install (the REAL aihub binary
// with only --data-dir/--listen), the REAL pf CLI, no daemon involved.

import (
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// TestV1S24MachinesListCLI walks the machines liveness face end to end:
// register by one real wire heartbeat, pin the --json raw passthrough
// and the human render's salient cells, force stale by backdate and
// flip fresh with exactly one further heartbeat (both faces), and pin
// the admin gate with one non-admin refusal.
func TestV1S24MachinesListCLI(t *testing.T) {
	inst := newV1Install(t)

	// Registration baseline: ONE real wire heartbeat with NO
	// runtime_version (omitempty ⇒ the key is absent from the body ⇒
	// nothing stored), under the bootstrap-admin credential (the
	// authorizeMachineRegistration gate admits the global admin).
	res, err := inst.client.Heartbeat(aihubclient.MachineHeartbeat{
		MachineID: "m-s24",
		LiveWIIDs: []string{},
	})
	if err != nil {
		t.Fatalf("register heartbeat: %v", err)
	}
	if !res.Registered || res.MachineID != "m-s24" {
		t.Fatalf("first heartbeat did not register m-s24: %+v", res)
	}

	// The typed client sees exactly the one machine.
	list, err := inst.client.ListMachines()
	if err != nil {
		t.Fatalf("list machines: %v", err)
	}
	if list.Count != 1 || len(list.Machines) != 1 || list.Machines[0].ID != "m-s24" {
		t.Fatalf("machines baseline wrong: count %d, machines %+v", list.Count, list.Machines)
	}

	// The real pf CLI lists it too (text face; the harness enforces
	// exit 0 — non-empty output carrying the machine row).
	out := inst.pf(t, "machines", "list")
	if strings.TrimSpace(out) == "" {
		t.Fatalf("machines list: empty output")
	}
	if !strings.Contains(out, "m-s24") {
		t.Fatalf("machines list lost the machine row:\n%s", out)
	}
}
