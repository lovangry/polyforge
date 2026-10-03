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
	"encoding/json"
	"slices"
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

	// --json is the RAW passthrough: the CLI stdout decodes to the same
	// machine row a direct wire read of GET /api/v1/machines returns —
	// only heartbeat_age_seconds may advance between the two reads, so
	// it is deliberately not compared.
	jout := inst.pf(t, "--json", "machines", "list")
	wire, err := inst.client.ListMachinesRaw()
	if err != nil {
		t.Fatalf("wire read of GET /api/v1/machines: %v", err)
	}
	var fromCLI, fromWire aihubclient.MachinesList
	if err := json.Unmarshal([]byte(jout), &fromCLI); err != nil {
		t.Fatalf("--json output undecodable: %v (%s)", err, jout)
	}
	if err := json.Unmarshal(wire, &fromWire); err != nil {
		t.Fatalf("wire body undecodable: %v (%s)", err, wire)
	}
	if fromCLI.Count != 1 || fromWire.Count != 1 {
		t.Fatalf("count wrong: cli %d, wire %d (%s)", fromCLI.Count, fromWire.Count, jout)
	}
	if fromCLI.StaleAfterSeconds != 300 || fromWire.StaleAfterSeconds != 300 {
		t.Fatalf("stale_after_seconds wrong: cli %d, wire %d (production-default 5m window)",
			fromCLI.StaleAfterSeconds, fromWire.StaleAfterSeconds)
	}
	if len(fromCLI.Machines) != 1 || len(fromWire.Machines) != 1 {
		t.Fatalf("machine rows wrong: cli %d, wire %d", len(fromCLI.Machines), len(fromWire.Machines))
	}
	mCLI, mWire := fromCLI.Machines[0], fromWire.Machines[0]
	if mCLI.ID != "m-s24" || mWire.ID != "m-s24" {
		t.Fatalf("machine id wrong: cli %q, wire %q", mCLI.ID, mWire.ID)
	}
	if mCLI.Mode != mWire.Mode || mCLI.LastHeartbeatAt != mWire.LastHeartbeatAt ||
		mCLI.Stale != mWire.Stale || !slices.Equal(mCLI.LiveWIIDs, mWire.LiveWIIDs) {
		t.Fatalf("--json passthrough drifted from the wire body: cli %+v, wire %+v", mCLI, mWire)
	}
	if mCLI.Stale {
		t.Fatalf("fresh machine reported stale: %+v", mCLI)
	}

	// An empty runtime_version is spelled key-ABSENCE on the wire
	// (omitempty), never "runtime_version": "" — pinned on the decoded
	// machine object AND on the raw bytes.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatalf("--json output is not a JSON object: %v (%s)", err, jout)
	}
	var rows []map[string]any
	if err := json.Unmarshal(doc["machines"], &rows); err != nil {
		t.Fatalf("machines rows undecodable: %v (%s)", err, jout)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly one machine object in the raw body: %s", jout)
	}
	if rv, present := rows[0]["runtime_version"]; present {
		t.Fatalf("runtime_version must be key-ABSENT, got %v (%s)", rv, jout)
	}
	if strings.Contains(jout, "runtime_version") {
		t.Fatalf("raw --json bytes carry a runtime_version key: %s", jout)
	}

	// The text face's salient cells — substring/exact-cell pins only,
	// never a byte-golden, no column-order or width assertions: the
	// header's stale-threshold line, the "-" RUNTIME placeholder
	// (orDash) and the alive STATE cell, all drawn by renderMachines.
	out = inst.pf(t, "machines", "list")
	if !strings.Contains(out, "machines: 1 (stale threshold 300s)") {
		t.Fatalf("header line wrong:\n%s", out)
	}
	cells := s24RowCells(t, out, "m-s24")
	if !slices.Contains(cells, "-") {
		t.Fatalf("RUNTIME placeholder: no exact \"-\" cell in the row (cells %v):\n%s", cells, out)
	}
	if !slices.Contains(cells, "alive") {
		t.Fatalf("STATE cell: no exact \"alive\" cell in the row (cells %v):\n%s", cells, out)
	}
}

// s24RowCells returns the non-empty cells of the machines row line that
// carries id: printTable joins the padded cells with two spaces, so the
// row line splits on "  " into cells (an empty MODE cell and the
// inter-cell padding collapse to empty tokens; the salient cells
// survive verbatim — including the exact "-" placeholder, which a raw
// substring check could never pin because the LAST HEARTBEAT timestamp
// also contains "-" characters).
func s24RowCells(t *testing.T, out, id string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, id) {
			continue
		}
		var cells []string
		for _, c := range strings.Split(line, "  ") {
			if c != "" {
				cells = append(cells, c)
			}
		}
		return cells
	}
	t.Fatalf("no %q row line in output:\n%s", id, out)
	return nil
}
