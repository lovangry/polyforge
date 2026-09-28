package test

// usage_drive_helpers_test.go: the S12-S16 fault-injection scenarios'
// shared helpers (polyforge#45). These ride the REAL-BINARY dogfood
// harness of usage_helpers_test.go — real aihub process, real `pf daemon`
// process, real pf CLI, fakepi as the derived WI Agent — and add:
//
//   - an install whose workflows checkout is the testdata FIXTURE bundles
//     directory (test/testdata/bundles), so a scenario bundle is a
//     first-class pin: bundle_name + bundle_digest come from the same
//     directory on both the aihub and the daemon faces;
//   - capsule-side readers for the drive executor's evidence (the
//     execution.json snapshot and the logs/drive.jsonl assertion ledger);
//   - a read-only peek into the live aihub's SQLite for the generation
//     ledger (owner_generations) — the strongest "the flush evidence
//     chain was accepted" assertion available from outside the process.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// usageFixtureBundles is the testdata fixture bundles checkout: the
// directory both the aihub and the S12-S16 machines point their
// --workflows-dir at.
func usageFixtureBundles(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// newFaultInstall boots one install over the fixture bundles checkout.
func newFaultInstall(t *testing.T) *usageInstall {
	return newUsageInstallWithWorkflows(t, usageFixtureBundles(t))
}

// wiCreateBundle files one execution work item pinned to a fixture bundle.
func (u *usageInstall) wiCreateBundle(t *testing.T, goal, bundle string) string {
	t.Helper()
	raw := u.pf(t, "--json", "file", "create", "--project", "usage", "--goal", goal, "--bundle", bundle)
	var out struct {
		ID string `json:"id"`
	}
	if err := jsonUnmarshalStrict(raw, &out); err != nil || out.ID == "" {
		t.Fatalf("file create (bundle %s): %q (%v)", bundle, raw, err)
	}
	return out.ID
}

// capsuleDirOf is the machine's capsule root for one wi.
func capsuleDirOf(m *usageMachine, wi string) string {
	return filepath.Join(m.home, "workspace", "capsules", wi)
}

// capsuleFileOf reads one file out of the machine's capsule for wi (""
// when absent).
func capsuleFileOf(t *testing.T, m *usageMachine, wi, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capsuleDirOf(m, wi), filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(raw)
}

// capsuleExecOf decodes the machine's capsule execution.json for wi
// (nil when not yet written).
func capsuleExecOf(t *testing.T, m *usageMachine, wi string) map[string]any {
	t.Helper()
	raw := capsuleFileOf(t, m, wi, "runtime-state/execution.json")
	if raw == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("execution.json of %s undecodable: %v", wi, err)
	}
	return out
}

// capsuleStep returns one step's record out of the decoded execution.json.
func capsuleStep(t *testing.T, exec map[string]any, stepID string) map[string]any {
	t.Helper()
	steps, _ := exec["steps"].(map[string]any)
	st, _ := steps[stepID].(map[string]any)
	if st == nil {
		t.Fatalf("execution.json carries no step %q: %v", stepID, exec["steps"])
	}
	return st
}

// waitCapsuleExec polls until the capsule's execution.json exists and
// cond holds on it (bounded; fails the test naming `what` on timeout).
func waitCapsuleExec(t *testing.T, m *usageMachine, wi, what string, cond func(map[string]any) bool) map[string]any {
	t.Helper()
	var last map[string]any
	waitUntilUse(t, what, func() bool {
		last = capsuleExecOf(t, m, wi)
		return last != nil && cond(last)
	})
	return last
}

// driveJournal reads the drive executor's assertion ledger
// (logs/drive.jsonl) and decodes each line.
func driveJournal(t *testing.T, m *usageMachine, wi string) []map[string]any {
	t.Helper()
	raw := capsuleFileOf(t, m, wi, "logs/drive.jsonl")
	if raw == "" {
		return nil
	}
	var out []map[string]any
	for _, line := range splitNonEmpty(raw) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("drive journal line undecodable: %q", line)
		}
		out = append(out, rec)
	}
	return out
}

// journalDispatches filters the journal to dispatch records.
func journalDispatches(t *testing.T, m *usageMachine, wi string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range driveJournal(t, m, wi) {
		if rec["type"] == "dispatch" {
			out = append(out, rec)
		}
	}
	return out
}

// dispatchesOfStep filters the dispatch records of one step id.
func dispatchesOfStep(recs []map[string]any, stepID string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["step"] == stepID {
			out = append(out, r)
		}
	}
	return out
}

// journalOfType filters the journal to one record type.
func journalOfType(t *testing.T, m *usageMachine, wi, typ string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range driveJournal(t, m, wi) {
		if rec["type"] == typ {
			out = append(out, rec)
		}
	}
	return out
}

// splitNonEmpty splits a file's content into its non-empty lines.
func splitNonEmpty(raw string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i == len(raw) || raw[i] == '\n' {
			line := raw[start:i]
			start = i + 1
			if line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// wiShow decodes `pf --json file show` for one wi.
func (u *usageInstall) wiShow(t *testing.T, wi string) map[string]any {
	t.Helper()
	raw := u.pf(t, "--json", "file", "show", wi)
	var out map[string]any
	if err := jsonUnmarshalStrict(raw, &out); err != nil {
		t.Fatalf("file show %s: %q (%v)", wi, raw, err)
	}
	return out
}

// wiWaitingOn reads the work item's waiting_on ("" when NULL) through the
// real CLI detail face.
func (u *usageInstall) wiWaitingOn(t *testing.T, wi string) string {
	t.Helper()
	v, _ := u.wiShow(t, wi)["waiting_on"].(string)
	return v
}

// aihubGenRows reads the live aihub's generation ledger for one wi,
// read-only (the flush-evidence acceptance proof: an end-generation with
// a broken chain is CORE-425-refused and never lands a row with an
// end_reason).
func aihubGenRows(t *testing.T, inst *usageInstall, wi string) map[int64]string {
	t.Helper()
	dsn := "file:" + filepath.Join(inst.dataDir, "aihub.db") + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open aihub read-only: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT generation, end_reason FROM owner_generations WHERE work_item_id=?1`, wi)
	if err != nil {
		t.Fatalf("query owner_generations: %v", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var gen int64
		var reason sql.NullString
		if err := rows.Scan(&gen, &reason); err != nil {
			t.Fatal(err)
		}
		out[gen] = reason.String
	}
	return out
}

// faultDriveEnv is the standard drive-mode environment of one machine:
// the proxy socket wired plus the scenario's fault knobs.
func (m *usageMachine) faultDriveEnv(knobs ...string) []string {
	return append([]string{"FAKEPI_MODE=drive", "FAKEPI_PROXY_SOCKET=" + m.socket}, knobs...)
}

// asInt reads one JSON number field as int64 (0 on any miss).
func asInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// asString reads one JSON string field ("" on any miss).
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// writeReleaseFile creates a hold-point release file (the scenario's
// deterministic unblock of a parked drive loop).
func writeReleaseFile(path string) error {
	return writeFileUsage(path, "release\n")
}
