package aihub

// aihub_client_test.go: wire-shape pins for the memory proxy forwards
// (06 §5.1) — the daemon-face test (internal/daemon proxy_memory_test)
// exercises the full path against the real Core, but the exact METHOD and
// query-string mapping deserve a direct pin too: the first wired
// MemorySearch POSTed to /api/v1/memories/search while the Core serves
// that face as GET, and only a wire-level pin makes the regression legible.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture records one request's method, path and body.
type capture struct {
	method string
	path   string
	body   string
}

func captureServer(t *testing.T, reply string, cap *capture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.method = r.Method
		cap.path = r.URL.RequestURI()
		cap.body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMemorySearchWireIsGetWithQueryParams(t *testing.T) {
	var cap capture
	srv := captureServer(t, `{"items":[]}`, &cap)
	c := New(srv.URL, "k")

	out, err := c.MemorySearch(map[string]any{
		"project": "p1", "q": "routing verbs", "type": "experience.debug",
		"tags": []any{"a", "b"}, "limit": float64(42),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("search answer must decode")
	}
	if cap.method != "GET" {
		t.Fatalf("the search face is GET (06 §5.1), got %s", cap.method)
	}
	if !strings.HasPrefix(cap.path, "/api/v1/memories/search?") {
		t.Fatalf("path = %s", cap.path)
	}
	for _, want := range []string{
		"project=p1", "q=routing+verbs", "type=experience.debug", "tags=a%2Cb", "limit=42",
	} {
		if !strings.Contains(cap.path, want) {
			t.Errorf("path %q missing %q", cap.path, want)
		}
	}
}

func TestMemoryCreateWireIsPostBody(t *testing.T) {
	var cap capture
	srv := captureServer(t, `{"id":"mem_x"}`, &cap)
	c := New(srv.URL, "k")

	out, err := c.MemoryCreate(map[string]any{
		"project": "p1", "type": "experience.debug", "visibility": "project",
		"content": "# body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["id"] != "mem_x" {
		t.Fatalf("create answer must decode: %v", out)
	}
	if cap.method != "POST" || cap.path != "/api/v1/memories" {
		t.Fatalf("create wire = %s %s", cap.method, cap.path)
	}
	for _, want := range []string{`"content":"# body"`, `"project":"p1"`} {
		if !strings.Contains(cap.body, want) {
			t.Errorf("create body %q missing %q", cap.body, want)
		}
	}
}
