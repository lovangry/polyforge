package daemon

// proxy_memory_test.go: the memory proxy path (04 §5.3 whitelist rows
// memory_read/memory_write → 06 §5.1), exercised through the DAEMON FACE
// against the real Core: hProxyCall → aihub.Client.MemoryCreate /
// MemorySearch → the real server's POST /api/v1/memories and
// GET /api/v1/memories/search. The wiring existed but had NO test; the
// first run of this test caught the search half dead on arrival —
// MemorySearch issued POST /api/v1/memories/search while the Core serves
// the search face as GET with query parameters (06 §5.1), so every
// memory_read through the proxy answered 405.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

// TestProxyMemoryReadWrite: a running job's WI Agent performs one
// memory_write then one memory_read through the SO_PEERCRED-bound proxy
// (both calls from the same helper pid, which is the pid the proxy's
// peer-credential check matches). The write must land as a real memory
// row in the Core (06 §5.2 create order) and answer with the created
// entry; the read must retrieve that same entry through the server's
// search face — on the wire shape the server actually serves (the 405
// regression).
func TestProxyMemoryReadWrite(t *testing.T) {
	e := newDaemonEnv(t)
	hdir := t.TempDir()
	plan, err := json.Marshal([]map[string]any{
		{
			"op": "memory_write",
			"payload": map[string]any{
				"project":    "p1",
				"type":       "experience.debug",
				"visibility": "project",
				"title":      "proxy memory pin",
				"content":    "# proxy memory pin\nwritten through the daemon proxy",
				"tags":       []string{"proxy", "pin"},
			},
		},
		{
			"op": "memory_read",
			"payload": map[string]any{
				"project": "p1",
				"q":       "proxy memory pin",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The fake WI Agent is a re-exec of this binary (TestMain helper
	// mode): its OWN pid — recorded as main_agent_pid — originates both
	// proxy_calls.
	e.spawner.cmd = func() *exec.Cmd {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(),
			"PF_DAEMON_TEST_PROXY_HELPER=1",
			"PF_PROXY_SOCKET="+e.socket,
			"PF_PROXY_WI="+e.wi,
			"PF_PROXY_DIR="+hdir,
			"PF_PROXY_PLAN="+string(plan),
		)
		return cmd
	}

	// Job must be RUNNING for the proxy to serve it: submit + spawn.
	ctx := context.Background()
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a := e.spawner.agents[0]; a != nil && a.child != nil && a.child.Process != nil {
			a.child.Process.Kill()
		}
	})
	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()

	// Call 0: memory_write through the face.
	proxyReq(t, hdir, 0)
	st, body := proxyResp(t, hdir, 0)
	if st != 200 {
		t.Fatalf("memory_write through the proxy: %d %v", st, body)
	}
	memID, _ := body["id"].(string)
	if memID == "" {
		t.Fatalf("memory_write must answer with the created entry's id: %v", body)
	}
	// The row really landed in the Core.
	var count int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id=?1`, memID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("memory_write must create exactly one Core row for %s, got %d", memID, count)
	}

	// Call 1: memory_read retrieves the written entry through the search
	// face. A 405 here is the POST-vs-GET wire regression.
	proxyReq(t, hdir, 1)
	st, body = proxyResp(t, hdir, 1)
	if st != 200 {
		t.Fatalf("memory_read through the proxy: %d %v (the Core serves the search face as GET /api/v1/memories/search — see internal/sync/aihub MemorySearch)", st, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("memory_read must retrieve the written entry, got %v", body)
	}
	entry, _ := items[0].(map[string]any)
	if entry["id"] != memID {
		t.Fatalf("memory_read must find the entry memory_write created (%s), got %v", memID, entry)
	}
}
