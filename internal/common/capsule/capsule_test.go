package capsule

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLooseParseLegacyCapsuleKeys pins the old-capsule compat contract:
// sealed v1-era capsules (drill/window tickets) carry the deleted
// generation/attempt_id lineage keys in capsule.json, session.json and
// runtime-state/execution.json. The structs deliberately use plain
// encoding/json (NEVER DisallowUnknownFields on capsule files), so the
// unknown keys are ignored and the surviving fields decode normally —
// an old capsule must never fail to parse just for being old.
func TestLooseParseLegacyCapsuleKeys(t *testing.T) {
	const legacyCapsuleJSON = `{
	  "work_item_id": "wi_old",
	  "capsule_id": "cap_wi_old",
	  "project": "polyforge",
	  "generation": 4,
	  "attempt_id": "att_9f8e7d",
	  "bundle_name": "quick-fix",
	  "bundle_digest": "sha256:deadbeef",
	  "base_commit": "",
	  "repos": {"polyforge": "73fda26"},
	  "created_at": "2026-08-01T00:00:00Z"
	}`
	var idn Identity
	if err := json.Unmarshal([]byte(legacyCapsuleJSON), &idn); err != nil {
		t.Fatalf("legacy capsule.json with generation/attempt_id keys must parse: %v", err)
	}
	if idn.WorkItemID != "wi_old" || idn.BundleName != "quick-fix" || idn.Repos["polyforge"] != "73fda26" {
		t.Fatalf("surviving fields must decode: %+v", idn)
	}

	const legacySessionJSON = `{
	  "attempt_id": "att_9f8e7d",
	  "generation": 4,
	  "harness": "pi",
	  "session_id": "ab12cd34",
	  "transcript_path": "/tmp/t.jsonl",
	  "updated_at": "2026-08-01T00:00:00Z"
	}`
	var sess Session
	if err := json.Unmarshal([]byte(legacySessionJSON), &sess); err != nil {
		t.Fatalf("legacy session.json with attempt_id/generation keys must parse: %v", err)
	}
	if sess.Harness != "pi" || sess.SessionID != "ab12cd34" {
		t.Fatalf("surviving fields must decode: %+v", sess)
	}

	const legacyWorkItemJSON = `{
	  "id": "wi_old",
	  "project": "polyforge",
	  "goal": "g",
	  "generation": 4,
	  "updated_at": "2026-08-01T00:00:00Z"
	}`
	var doc WorkItemDoc
	if err := json.Unmarshal([]byte(legacyWorkItemJSON), &doc); err != nil {
		t.Fatalf("legacy work-item.json with generation key must parse: %v", err)
	}
	if doc.ID != "wi_old" || doc.Goal != "g" {
		t.Fatalf("surviving fields must decode: %+v", doc)
	}

	const legacyExecutionJSON = `{
	  "capsule_id": "cap_wi_old",
	  "work_item_id": "wi_old",
	  "generation": 4,
	  "current_step": "code",
	  "workflow_status": "running",
	  "steps": {},
	  "version": 7
	}`
	var exec Execution
	if err := json.Unmarshal([]byte(legacyExecutionJSON), &exec); err != nil {
		t.Fatalf("legacy execution.json with generation key must parse: %v", err)
	}
	if exec.CapsuleID != "cap_wi_old" || exec.CurrentStep != "code" || exec.Version != 7 {
		t.Fatalf("surviving fields must decode: %+v", exec)
	}
}

// TestNoCapsuleStructCarriesLineageKeys guards the fossil cleanup: the
// v1 generation/attempt_id lineage keys must not creep back into the
// frozen capsule shapes (the round-trip JSON of every struct in this
// package carries none of them).
func TestNoCapsuleStructCarriesLineageKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		blob []byte
	}{
		{"Identity", mustJSON(t, Identity{})},
		{"Session", mustJSON(t, Session{})},
		{"Execution", mustJSON(t, Execution{})},
		{"WorkItemDoc", mustJSON(t, WorkItemDoc{})},
	} {
		for _, key := range []string{"generation", "attempt_id"} {
			if strings.Contains(string(tc.blob), `"`+key+`"`) {
				t.Errorf("%s round-trip still carries the v1 lineage key %q: %s", tc.name, key, tc.blob)
			}
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return blob
}
