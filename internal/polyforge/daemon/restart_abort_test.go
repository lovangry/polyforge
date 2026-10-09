package daemon

// restart_abort_test.go (#103 rewind): a restart reconciliation failure
// is a HARD daemon startup failure. The typed mid-flight-capsule-missing
// error (a recorded non-terminal WI whose capsule directory is absent at
// the active workspace — the operator has not moved it) must make Run
// return the error naming the work item: the daemon process exits
// non-zero, the face never serves, and nothing is rebuilt or persisted —
// never a silent proceed (and never a rebuild-from-remote).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

func TestRunAbortsWhenMidflightCapsuleMissing(t *testing.T) {
	setGitIdentity(t)
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	data := filepath.Join(dir, "data")
	hubBase := filepath.Join(dir, "hub")
	for _, d := range []string{ws, data, hubBase} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wi := "wi_missing"
	bare := mkBare(t, hubBase, wi)
	session := e2eSession(wi)
	// The recorded WI is mid-flight (running under our session, so the
	// fake hub's unheld-but-running verdict matches) and its capsule
	// directory is deliberately ABSENT: the operator did not move it to
	// the active workspace.
	capDir := filepath.Join(ws, "capsules", wi)
	gitDir := filepath.Join(data, "git", wi+".git")
	seedStateFile(t, data, "m-test", map[string]any{
		"work_item_id":        wi,
		"project":             "proj",
		"session_id":          session,
		"session_fingerprint": apiSessionFingerprint(session),
		"state":               "running",
		"capsule_dir":         capDir,
		"git_dir":             gitDir,
		"remote_url":          bare,
		"clean_pause":         false,
		"spawn_n":             1,
	})
	stateOnDisk, err := os.ReadFile(filepath.Join(data, "runtime-state.json"))
	if err != nil {
		t.Fatal(err)
	}

	hub := newFakeHub()
	sp := &fakeSpawner{}
	cfg := Config{
		MachineID:         "m-test",
		SocketPath:        filepath.Join(dir, "s.sock"),
		Mapping:           mappingFor(ws),
		DataDir:           data,
		GitRemoteBase:     hubBase,
		MaxConcurrent:     4,
		Hub:               hub,
		Spawner:           sp,
		ObservePoll:       10 * time.Millisecond,
		DrainInterval:     10 * time.Millisecond,
		DispatchTick:      10 * time.Millisecond,
		ShutdownFlushWait: 100 * time.Millisecond,
		ExitPoll:          10 * time.Millisecond,
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(context.Background()) }()
	select {
	case err = <-runErr:
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not fail hard within the bound: a mid-flight capsule missing at the active workspace must abort startup")
	}
	if err == nil {
		t.Fatal("Run returned nil with a mid-flight capsule missing — the daemon would keep serving")
	}
	if !strings.Contains(err.Error(), wi) {
		t.Fatalf("startup failure does not identify the affected work item %s: %v", wi, err)
	}
	if !strings.Contains(err.Error(), "mid-flight capsule missing") {
		t.Fatalf("startup failure is not the typed missing-capsule error: %v", err)
	}

	// The face never served: after the abort the listener is closed, so
	// a client dial fails.
	if client := cli.NewDaemonClient(cfg.SocketPath); client != nil {
		if _, cerr := client.Status(); cerr == nil {
			t.Fatal("the daemon face answers after the reconciliation abort — the daemon is still serving")
		}
	}

	// No rebuild-from-remote: nothing was re-materialized at the base
	// (no capsule content, no git dir), and no recovery agent was
	// spawned.
	if _, serr := os.Stat(capDir); !os.IsNotExist(serr) {
		t.Fatalf("missing capsule was re-materialized at %s", capDir)
	}
	if _, serr := os.Stat(gitDir); !os.IsNotExist(serr) {
		t.Fatalf("rebuild signal: %s was created during the failed restart", gitDir)
	}
	if calls := sp.callCount(); calls != 0 {
		t.Fatalf("the failed restart spawned %d agent(s); want none", calls)
	}

	// Nothing was persisted: state.json on disk is byte-identical to the
	// seeded record — the next start (after the manual remediation)
	// reconciles the pre-reconciliation truth afresh.
	after, err := os.ReadFile(filepath.Join(data, "runtime-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(stateOnDisk) {
		t.Fatal("the aborted startup rewrote runtime-state.json — a startup abort must persist nothing")
	}
}
