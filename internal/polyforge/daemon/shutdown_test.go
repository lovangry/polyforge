package daemon

// shutdown_test.go + restart wiring: the graceful shutdown sequence
// (stop_notice to every live agent, the bounded flush(shutdown) wait,
// timeout degradation, state.json on disk, the socket closing) and the
// daemon-start Restart reconciliation wiring.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// TestShutdownSequence: two live jobs, no agent flushes → the per-job
// window times out and DEGRADES (never blocks), every live agent got
// pf/stop_notice, Runtime's state.json lands with both jobs recorded,
// the socket closes and Run returns.
func TestShutdownSequence(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_s1")
	td.submitClaim(t, "wi_s2")

	start := time.Now()
	if err := td.Shutdown(context.Background()); err != nil {
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

	// polyforge#106: the pf/stop_notice delivery face is deleted with
	// the WI-Agent lane — the driver owns the step state machine, so
	// the sequence's per-job grace is the flush(shutdown) window only.

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

// TestShutdownFlushPositive: an agent that answers the notice with
// flush(shutdown) lets the sequence proceed without the degradation
// wait — the commit lands (published event) and the wait observes it.
func TestShutdownFlushPositive(t *testing.T) {
	td := newTestDaemon(t, func(c *Config) {
		c.ShutdownFlushWait = 5 * time.Second // long window: only a flush may end it fast
	})
	td.submitClaim(t, "wi_s3")
	td.waitMonitored(t, "wi_s3")
	sraw, err := td.client.Observe("wi_s3", 0)
	if err != nil {
		t.Fatalf("observe wi_s3: %v", err)
	}
	var sjv protocol.JobView
	if err := json.Unmarshal(sraw, &sjv); err != nil || sjv.SpawnN == 0 {
		t.Fatalf("wi_s3 view undecodable: %v (%s)", err, sraw)
	}

	done := make(chan error, 1)
	go func() { done <- td.Shutdown(context.Background()) }()

	// Play the agent: flush(shutdown) right after the shutdown call
	// latches. (The stop_notice delivery that used to gate this is
	// retired with the WI-Agent lane, polyforge#106.)
	time.Sleep(50 * time.Millisecond)
	if _, err := td.client.Flush("wi_s3", protocol.FlushRequest{
		WorkItem: "wi_s3", SpawnN: sjv.SpawnN, Reason: protocol.FlushShutdown,
	}); err != nil {
		t.Fatalf("flush(shutdown): %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not observe the flush within the window")
	}
}

// TestRestartWiring: a recorded job whose aihub verdict is running is
// recovered at daemon start (transcript resume through the recovery
// engine) and re-attached to a fresh monitor — the observe route
// answers for it.
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
		c.Workspace = ws
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

	// The recovered job is supervised again: the observe route answers
	// with its attempt identity.
	td.waitMonitored(t, wi)
	raw, err := td.client.Observe(wi, 0)
	if err != nil {
		t.Fatalf("observe after restart: %v", err)
	}
	var jv protocol.JobView
	if err := json.Unmarshal(raw, &jv); err != nil {
		t.Fatalf("observe undecodable: %v", err)
	}
	if jv.WorkItem != wi || jv.SpawnN == 0 || jv.Status != protocol.JobRunning {
		t.Fatalf("restarted job view = %+v", jv)
	}
}
