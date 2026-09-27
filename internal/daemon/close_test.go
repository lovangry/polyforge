package daemon

// close_test.go: APIServer.Close must drain idle accepted UDS connections
// before tearing the face down (polyforge#11 e2e round, inference 4).
// The daemon accepts UDS conns and dup's the accepted fd in
// credListener.Accept (uc.File() for SO_PEERCRED), and the server-side
// read of a pooled CLI keep-alive conn left idle would block the close —
// a real daemon shutdown with a live `pf observe`/CLI session would hang
// the same way. Close must therefore answer promptly even while such a
// conn is open and idle on the client side (no CloseIdleConnections
// rescue from the caller).

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestAPIServerClosePromptWithIdleKeepAliveConn drives one pooled client
// request over the UDS face, leaves its keep-alive conn idle (exactly a
// CLI session between two commands), and asserts Close returns fast.
func TestAPIServerClosePromptWithIdleKeepAliveConn(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AIHubURL: "http://127.0.0.1:1", APIKey: "k", MachineID: "mac_close",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: filepath.Join(dir, "ws"),
		DrainProject:  "p1",
	}
	d, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "polyforge.sock")
	api, err := NewAPIServer(d, socket)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = api.Serve() }()

	// Pooled client: a real HTTP keep-alive conn over the UDS face. The
	// dial returns the same conn to the pool for reuse.
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", socket)
	}}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://daemon/status")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon UDS face never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The conn is now IDLE in the client pool: the server side is blocked
	// reading for the next request. Deliberately NO CloseIdleConnections.

	done := make(chan error, 1)
	go func() { done <- api.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("APIServer.Close hung on an idle pooled keep-alive conn " +
			"(blocked server-side read must be drained before the close)")
	}
}
