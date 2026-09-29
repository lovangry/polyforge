package daemon

// state_validate_test.go: FD-8 — Load applies SEMANTIC validation to an
// existing state file (04 §4.1): required fields, unique work_item_id,
// status enums, generation >= 1. A broken file is rejected LOUDLY instead
// of being silently "recovered" into mis-adopted or double-adopted jobs.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// validHeaderState is the state-level baseline every case starts from
// (04 §4.1: machine_id/mode/workspace_root/started_at are required).
func validHeaderState() *State {
	return &State{
		MachineID: "m-01", Mode: ModeInteractive,
		WorkspaceRoot: "/ws/root", StartedAt: NowStamp(),
	}
}

func writeStateFile(t *testing.T, dir string, v any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func validJob(wi string) Job {
	return Job{
		WorkItemID: wi, CapsuleID: "cap_" + wi, Generation: 1,
		HarnessAdapter: "pi", HarnessSessionID: "sess_" + wi + "_1",
		MainAgentPID: 4242, MainAgentStartedAt: NowStamp(), Status: JobRunning,
	}
}

func TestStateLoadRejectsSemanticGarbage(t *testing.T) {
	cases := []struct {
		name string
		mut  func(st *State)
	}{
		{"empty work_item_id", func(st *State) { st.Jobs[0].WorkItemID = "" }},
		{"duplicate work_item_id", func(st *State) {
			dup := validJob("wi_ok")
			st.Jobs = append(st.Jobs, dup)
		}},
		{"empty capsule_id", func(st *State) { st.Jobs[0].CapsuleID = "" }},
		{"generation zero", func(st *State) { st.Jobs[0].Generation = 0 }},
		{"empty harness_session_id", func(st *State) { st.Jobs[0].HarnessSessionID = "" }},
		{"zero main_agent_pid", func(st *State) { st.Jobs[0].MainAgentPID = 0 }},
		{"unknown job status", func(st *State) { st.Jobs[0].Status = "zombie" }},
		{"pending empty idempotency_key", func(st *State) {
			st.PendingJobs = append(st.PendingJobs, PendingJob{WorkItemID: "wi_p", Intent: "claim", CreatedAt: NowStamp(), Status: PendingIntentPersisted})
		}},
		{"pending unknown intent", func(st *State) {
			st.PendingJobs = append(st.PendingJobs, PendingJob{WorkItemID: "wi_p", IdempotencyKey: "k", Intent: "steal", CreatedAt: NowStamp(), Status: PendingIntentPersisted})
		}},
		{"pending unknown status", func(st *State) {
			st.PendingJobs = append(st.PendingJobs, PendingJob{WorkItemID: "wi_p", IdempotencyKey: "k", Intent: "claim", CreatedAt: NowStamp(), Status: "half_way"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			st := validHeaderState()
			st.Jobs = []Job{validJob("wi_ok")}
			c.mut(st)
			writeStateFile(t, dir, st)
			store := NewStateStore(dir)
			if _, err := store.Load(); err == nil {
				t.Fatalf("a semantically invalid state file must be rejected loudly: %s", c.name)
			}
		})
	}
}

func TestStateLoadAcceptsValidFile(t *testing.T) {
	dir := t.TempDir()
	st := validHeaderState()
	st.Jobs = []Job{validJob("wi_a"), validJob("wi_b")}
	st.PendingJobs = []PendingJob{{
		WorkItemID: "wi_c", IdempotencyKey: "k9", Intent: "resume",
		CreatedAt: NowStamp(), Status: PendingClaimConfirmed, Generation: 2,
		GenerationToken: "tok_g2",
	}}
	writeStateFile(t, dir, st)
	store := NewStateStore(dir)
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("a valid state file must load: %v", err)
	}
	if len(loaded.Jobs) != 2 || loaded.FindJob("wi_b") == nil || len(loaded.PendingJobs) != 1 {
		t.Fatalf("loaded state: %+v", loaded)
	}
}

// R2N-8: state-level required fields, duplicate pending intents, and
// pending/jobs generation consistency.

func validPending(wi string, status string, gen int64) PendingJob {
	return PendingJob{
		WorkItemID: wi, IdempotencyKey: "k-" + wi, Intent: "claim",
		CreatedAt: NowStamp(), Status: status, Generation: gen,
		GenerationToken: "tok_" + wi,
	}
}

func TestStateValidateRejectsStateLevelGarbage(t *testing.T) {
	cases := []struct {
		name string
		mut  func(st *State)
	}{
		{"empty machine_id", func(st *State) { st.MachineID = "" }},
		{"empty mode", func(st *State) { st.Mode = "" }},
		{"unknown mode", func(st *State) { st.Mode = "turbo" }},
		{"empty workspace_root", func(st *State) { st.WorkspaceRoot = "" }},
		{"empty started_at", func(st *State) { st.StartedAt = "" }},
		{"duplicate pending intent for one WI", func(st *State) {
			st.PendingJobs = append(st.PendingJobs, validPending("wi_dupe", PendingIntentPersisted, 0))
			st.PendingJobs = append(st.PendingJobs, validPending("wi_dupe", PendingIntentPersisted, 0))
		}},
		{"claim_confirmed without generation", func(st *State) {
			p := validPending("wi_noGen", PendingClaimConfirmed, 0)
			st.PendingJobs = append(st.PendingJobs, p)
		}},
		{"session_persisted without token", func(st *State) {
			p := validPending("wi_noTok", PendingSessionPersisted, 3)
			p.GenerationToken = ""
			st.PendingJobs = append(st.PendingJobs, p)
		}},
		{"pending generation disagrees with jobs[]", func(st *State) {
			st.Jobs = []Job{validJob("wi_mismatch")}
			st.PendingJobs = append(st.PendingJobs, validPending("wi_mismatch", PendingSessionPersisted, 9))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := validHeaderState()
			c.mut(st)
			if err := st.validate(); err == nil {
				t.Fatalf("validate must reject: %s", c.name)
			}
		})
	}
}

// TestStateValidateAcceptsConsistentPendingAndJobs: a crash-window state
// (the journal row and the formal jobs[] record coexist, same generation)
// is legal.
func TestStateValidateAcceptsConsistentPendingAndJobs(t *testing.T) {
	st := validHeaderState()
	st.Jobs = []Job{validJob("wi_win")}
	st.PendingJobs = []PendingJob{validPending("wi_win", PendingSessionPersisted, 1)}
	if err := st.validate(); err != nil {
		t.Fatalf("the consistent crash-window state must validate: %v", err)
	}
}

// TestLoadOrAdoptBackfillsOldFormat: a state file written by an older
// build (no state-level header) is ADOPTED by backfill (04 §11
// backward-compatible read) and persisted, so the strict validation never
// bricks an upgrade on its own previous file.
func TestLoadOrAdoptBackfillsOldFormat(t *testing.T) {
	dir := t.TempDir()
	old := &State{Jobs: []Job{validJob("wi_old")}}
	writeStateFile(t, dir, old)

	store := NewStateStore(dir)
	st, err := store.LoadOrAdopt(StateHeader{
		MachineID: "m-01", Mode: ModeInteractive, WorkspaceRoot: "/ws/root", StartedAt: NowStamp(),
	})
	if err != nil {
		t.Fatalf("an old-format state file must be adopted, not rejected: %v", err)
	}
	if st.MachineID != "m-01" || st.Mode != ModeInteractive || st.WorkspaceRoot != "/ws/root" || st.StartedAt == "" {
		t.Fatalf("the header must be backfilled: %+v", st)
	}
	// Persisted: a second strict Load must pass.
	if _, err := NewStateStore(dir).Load(); err != nil {
		t.Fatalf("the backfilled file must pass strict validation: %v", err)
	}
	// A fresh dir: LoadOrAdopt creates the state file immediately (the
	// file is the crash-recovery input, 04 §4.1).
	fresh := NewStateStore(t.TempDir())
	if _, err := fresh.LoadOrAdopt(StateHeader{
		MachineID: "m-02", Mode: ModeInteractive, WorkspaceRoot: "/ws/2", StartedAt: NowStamp(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fresh.Path)); err != nil {
		t.Fatalf("LoadOrAdopt must persist the fresh state file: %v", err)
	}
}

// TestUpdatePendingSupersedesAnyRowForWI (R3N-F7): the pending journal is
// ONE row per WI admission; the idempotency key is a FIELD of that row,
// not its identity. A spawn-failure leftover row (old key, advanced
// status) plus a FRESH takeover row (new key) must end up as exactly one
// row — the old code keyed the replacement by (WI, key), appended the
// second row, and the next crash made LoadOrAdopt reject the daemon's own
// state file as corrupt.
func TestUpdatePendingSupersedesAnyRowForWI(t *testing.T) {
	store := NewStateStore(t.TempDir())
	st := validHeaderState()

	// The spawn-failure leftover: an advanced row carrying the OLD key.
	leftover := PendingJob{
		WorkItemID: "wi_f7", IdempotencyKey: "pf-old-key-1", Intent: "takeover",
		CreatedAt: NowStamp(), Status: PendingSessionPersisted,
		Generation: 3, GenerationToken: "tok_g3", HarnessAdapter: "pi",
		HarnessSessionID: "sess_f7_3",
	}
	if err := store.UpdatePending(st, leftover); err != nil {
		t.Fatal(err)
	}
	// The fresh takeover: a NEW key for the same WI.
	fresh := PendingJob{
		WorkItemID: "wi_f7", IdempotencyKey: "pf-new-key-2", Intent: "takeover",
		CreatedAt: NowStamp(), Status: PendingIntentPersisted,
	}
	if err := store.UpdatePending(st, fresh); err != nil {
		t.Fatal(err)
	}

	// Reload: the state the next daemon life must be able to LOAD.
	st2, err := store.Load()
	if err != nil {
		t.Fatalf("R3N-F7: the state must load after a key change, got: %v", err)
	}
	if len(st2.PendingJobs) != 1 {
		t.Fatalf("exactly one journal row may exist per WI, got %+v", st2.PendingJobs)
	}
	if row := st2.PendingJobs[0]; row.IdempotencyKey != "pf-new-key-2" || row.Status != PendingIntentPersisted {
		t.Fatalf("the fresh row must supersede the leftover, got %+v", row)
	}

	// AppendPending keeps the same invariant (it supersedes, never dups).
	if err := store.AppendPending(st2, PendingJob{
		WorkItemID: "wi_f7", IdempotencyKey: "pf-newer-key-3", Intent: "takeover",
		CreatedAt: NowStamp(), Status: PendingIntentPersisted,
	}); err != nil {
		t.Fatal(err)
	}
	st3, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st3.PendingJobs) != 1 || st3.PendingJobs[0].IdempotencyKey != "pf-newer-key-3" {
		t.Fatalf("AppendPending must supersede too, got %+v", st3.PendingJobs)
	}
}
