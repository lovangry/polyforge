package daemon

// shutdown_test.go + restart wiring: the graceful shutdown sequence
// (state.json on disk, the socket closing) and the daemon-start Restart
// reconciliation wiring.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// TestShutdownSequence: two live jobs → the sequence is fast (no per-job
// grace window), Runtime's state.json lands with both jobs recorded, the
// socket closes and Run returns.
func TestShutdownSequence(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_s1")
	td.submitClaim(t, "wi_s2")

	start := time.Now()
	if err := td.Shutdown(context.Background(), DirectShutdownCause()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("shutdown took %v (degradation path expected to be fast)", el)
	}
	select {
	case <-td.runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the shutdown sequence")
	}

	// Runtime's state.json landed with both jobs recorded (the
	// bookkeeping the next start reconciles against aihub).
	raw, err := os.ReadFile(filepath.Join(td.cfg.DataDir, "runtime-state.json"))
	if err != nil {
		t.Fatalf("state.json: %v", err)
	}
	var st struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("state.json unparsable: %v", err)
	}
	if len(st.Jobs) != 2 {
		t.Fatalf("state.json jobs = %d, want 2 (both interrupted jobs recorded)", len(st.Jobs))
	}
	for _, j := range st.Jobs {
		if _, ok := j["work_item_id"]; !ok {
			t.Fatalf("state.json job record missing work_item_id: %v", j)
		}
	}

	// The UDS face is gone.
	if dialWorks(td.sock) {
		t.Fatal("socket still accepts connections after shutdown")
	}
}

// TestRestartWiring: a recorded job whose aihub verdict is running is
// recovered at daemon start (transcript resume through the recovery
// engine) and re-attached to a fresh monitor — the observe
// lane reports it.
func TestRestartWiring(t *testing.T) {
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
	wi := "wi_r1"
	bare := mkBare(t, hubBase, wi)
	capDir := filepath.Join(ws, "capsules", wi)
	gitDir := filepath.Join(data, "git", wi+".git")
	seedCapsuleGit(t, capDir, gitDir, bare)
	session := e2eSession(wi)
	transcript := filepath.Join(capDir, "session", "session-"+apiSessionFingerprint(session)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"message_end"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
		"transcript":          transcript,
		"bundle_name":         "demo-bundle",
		"agent_pid":           0,
		"spawn_n":             1,
	})

	td := newTestDaemon(t, func(c *Config) {
		c.Mapping = mappingFor(ws)
		c.DataDir = data
		c.GitRemoteBase = hubBase
	})

	// The recovery engine re-derived the recorded agent at restart
	// (polyforge#106: the WI-Agent transcript-resume discriminator is
	// retired with the legacy cascade; the driver derivation's own
	// resume shape is pinned by the e2e journal assertions, s08/s17 —
	// here the pin is that restart reconciliation re-derives AT ALL).
	deadline := time.Now().Add(5 * time.Second)
	for {
		td.spawner.mu.Lock()
		calls := len(td.spawner.calls)
		td.spawner.mu.Unlock()
		if calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recorded job was never recovered at restart")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The recovered job is supervised again, with its attempt identity.
	td.waitMonitored(t, wi)
	if ji, ok := td.rt.Info(wi); !ok || ji.SpawnN == 0 || ji.State != runtime.StateRunning {
		t.Fatalf("restarted job = %+v (live %v)", ji, ok)
	}
}

// slowSweepAgent is a fake agent whose SECOND Terminate — the shutdown's
// final sweep, after the face is closed — takes a while.
type slowSweepAgent struct {
	fakeAgent
	n int
}

func (a *slowSweepAgent) Terminate(d time.Duration) error {
	a.mu.Lock()
	a.n++
	n := a.n
	a.mu.Unlock()
	if n >= 2 {
		time.Sleep(500 * time.Millisecond)
	}
	return a.fakeAgent.Terminate(d)
}

// repo review 2026-10-16 B-16: closing the face (shutdown step 5) makes
// Serve return ErrServerClosed; Run must still wait for the rest of the
// sequence (runtime drain, final sweep) before it returns and the
// process exits.
func TestRunReturnsOnlyAfterTheShutdownSequence(t *testing.T) {
	td := newTestDaemon(t, func(c *Config) {
		c.AgentStarter = func(ctx context.Context, in runtime.AgentStartInput) (runtime.AgentHandle, error) {
			return &slowSweepAgent{}, nil
		}
	})
	td.submitClaim(t, "wi_sweep")
	go func() { _ = td.Shutdown(context.Background(), DirectShutdownCause()) }()
	select {
	case <-td.runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
	select {
	case <-td.shutDone:
	default:
		t.Fatal("Run returned before the shutdown sequence finished")
	}
}
