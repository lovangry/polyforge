package cli

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"encoding/json"
)

// capturedRequest is one recorded request against a fake server.
type capturedRequest struct {
	Method string
	Path   string // path + query
	Header http.Header
	Body   string
}

// reqRecorder collects requests behind a mutex.
type reqRecorder struct {
	mu   sync.Mutex
	reqs []capturedRequest
}

func (r *reqRecorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.reqs = append(r.reqs, capturedRequest{
		Method: req.Method,
		Path:   req.URL.RequestURI(),
		Header: req.Header.Clone(),
		Body:   string(body),
	})
	r.mu.Unlock()
}

func (r *reqRecorder) all() []capturedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedRequest, len(r.reqs))
	copy(out, r.reqs)
	return out
}

func (r *reqRecorder) last() capturedRequest {
	all := r.all()
	if len(all) == 0 {
		return capturedRequest{}
	}
	return all[len(all)-1]
}

// newFakeAihub starts an httptest aihub that records every request and
// answers from the given handler (rec is filled before the handler
// runs). The returned cleanup closes the server.
func newFakeAihub(t *testing.T, rec *reqRecorder, answer func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newFakeDaemon starts a REAL unix-socket daemon serving handler; the
// returned string is the socket path for NewDaemonClient / --socket.
func newFakeDaemon(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "polyforge.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("unix listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// runPf drives the full CLI the way main does; returns the exit code.
// env pairs are applied with t.Setenv for the duration of the test.
func runPf(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	clearEnv(t)
	for k, v := range env {
		t.Setenv(k, v)
	}
	var stdout, stderr bytes.Buffer
	code := Run(args, bytes.NewReader(nil), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// jsonBody is a helper for handlers answering canned JSON.
func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// writeFileT writes a temp file and returns its path.
func writeFileT(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
