package daemon

// statepersist_test.go: R3N-F8 regressions —
//
//   - a persistence failure in setMode/abortDrain must leave the OLD mode
//     active (the in-memory admission state and the state file never
//     diverge; the old code mutated first and kept the new mode live while
//     returning the error);
//   - concurrent mode and job/pending saves are serialized through the
//     store-level mutex with a UNIQUE tmp file per save, so both persist
//     cleanly and the reloaded state file is valid.

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// sabotageStateFile makes the atomic save FAIL while leaving the store
// otherwise usable: the canonical path becomes a directory, so the final
// rename onto it fails (CreateTemp in the same dir still succeeds).
func sabotageStateFile(t *testing.T, d *Daemon) {
	t.Helper()
	if err := os.Remove(d.state.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d.state.Path, 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestSetModePersistenceFailureKeepsOldMode (R3N-F8): when the state file
// refuses the write, the transition must NOT stay active — the visible
// admission state rolls back to the previous mode and the error surfaces.
func TestSetModePersistenceFailureKeepsOldMode(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "pfk_x", MachineID: "m-01",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: filepath.Join(dir, "ws"),
		Adapter:       "pi", Version: "mode-fail-test",
	}
	d, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.modes.Current() != ModeInteractive {
		t.Fatalf("precondition: fresh daemon is interactive, got %s", d.modes.Current())
	}

	sabotageStateFile(t, d)
	if err := d.setMode(ModeBatch); err == nil {
		t.Fatal("the sabotaged persistence must fail the mode change")
	}
	if d.modes.Current() != ModeInteractive {
		t.Fatalf("R3N-F8: the OLD mode must stay active on a persistence failure, got %s", d.modes.Current())
	}
	if d.st.Mode != ModeInteractive {
		t.Fatalf("R3N-F8: the state record must keep the old mode too, got %s", d.st.Mode)
	}

	// abortDrain rolls back the same way: enter drain with a WORKING file,
	// sabotage, then abort — Drain must survive the failed abort.
	repairStateFile(t, d)
	if err := d.setMode(ModeDrain); err != nil {
		t.Fatal(err)
	}
	sabotageStateFile(t, d)
	if err := d.abortDrain(); err == nil {
		t.Fatal("the sabotaged persistence must fail the drain abort")
	}
	if d.modes.Current() != ModeDrain {
		t.Fatalf("R3N-F8: the failed abort must leave Drain active, got %s", d.modes.Current())
	}
	if d.st.Mode != ModeDrain {
		t.Fatalf("R3N-F8: the state record must still hold drain, got %s", d.st.Mode)
	}
}

// repairStateFile removes the sabotaging directory and re-persists the
// current in-memory state so later calls in the same test can succeed.
func repairStateFile(t *testing.T, d *Daemon) {
	t.Helper()
	if err := os.RemoveAll(d.state.Path); err != nil {
		t.Fatal(err)
	}
	if err := d.state.Save(d.st); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentModeAndJobSavesPersistCleanly (R3N-F8): mode transitions
// and job/pending bookkeeping saves racing on one StateStore must BOTH
// persist cleanly — the store-level mutex serializes each mutation+persist
// pair and every save uses a unique tmp name, so no interleaved marshal
// or shared state.json.tmp collision can corrupt the file. The reloaded
// file must validate and carry every job.
func TestConcurrentModeAndJobSavesPersistCleanly(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "pfk_x", MachineID: "m-01",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: filepath.Join(dir, "ws"),
		Adapter:       "pi", Version: "concurrent-save-test",
	}
	d, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	const jobWriters = 8
	const perWriter = 25
	var jobWG sync.WaitGroup
	var modeWG sync.WaitGroup
	stop := make(chan struct{})
	// Mode transitions: serialized among themselves (one API face serves
	// them), each racing the job writers below.
	modeMu := sync.Mutex{}
	modeWG.Add(1)
	go func() {
		defer modeWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			modeMu.Lock()
			next := ModeInteractive
			if d.modes.Current() == ModeInteractive {
				next = ModeBatch
			}
			if err := d.setMode(next); err != nil {
				t.Errorf("concurrent mode save must succeed: %v", err)
				modeMu.Unlock()
				return
			}
			if d.st.Mode != d.modes.Current() {
				t.Errorf("visible mode diverged from the persisted record: %s vs %s", d.st.Mode, d.modes.Current())
			}
			modeMu.Unlock()
		}
	}()
	// Job + pending writers on DISTINCT WIs, racing the mode saves.
	for w := 0; w < jobWriters; w++ {
		jobWG.Add(1)
		go func(w int) {
			defer jobWG.Done()
			for i := 0; i < perWriter; i++ {
				wi := "wi_c" + string(rune('a'+w))
				if err := d.state.UpsertJob(d.st, validJob(wi)); err != nil {
					t.Errorf("concurrent job save must succeed: %v", err)
					return
				}
				if err := d.state.UpdatePending(d.st, PendingJob{
					WorkItemID: wi, IdempotencyKey: "k", Intent: "claim",
					CreatedAt: NowStamp(), Status: PendingIntentPersisted,
				}); err != nil {
					t.Errorf("concurrent pending save must succeed: %v", err)
					return
				}
				if err := d.state.RemovePending(d.st, wi, "k"); err != nil {
					t.Errorf("concurrent pending removal must succeed: %v", err)
					return
				}
			}
		}(w)
	}
	jobWG.Wait() // the job writers finish on their own
	close(stop)  // then the mode toggler stops
	modeWG.Wait()

	// Every save persisted cleanly: the file reloads, validates, carries
	// every job, and no tmp residue remains.
	st2, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatalf("the racing saves must leave a loadable state: %v", err)
	}
	if len(st2.Jobs) != jobWriters {
		t.Fatalf("all %d jobs must persist, got %d", jobWriters, len(st2.Jobs))
	}
	if len(st2.PendingJobs) != 0 {
		t.Fatalf("the pending journal must be drained, got %+v", st2.PendingJobs)
	}
	left, _ := filepath.Glob(filepath.Join(cfg.DataDir, "state.json.tmp-*"))
	if len(left) != 0 {
		t.Fatalf("no tmp residue may remain after successful saves: %v", left)
	}
	if st2.Mode != ModeInteractive && st2.Mode != ModeBatch {
		t.Fatalf("the persisted mode must be a legal mode, got %s", st2.Mode)
	}
	if d.st.Mode != d.modes.Current() {
		t.Fatalf("the visible mode must match the persisted record: %s vs %s", d.st.Mode, d.modes.Current())
	}
}
