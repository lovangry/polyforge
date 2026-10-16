package daemon

// stepmem_test.go: the step agent's daemon-mediated memory read face —
// the caller's work item comes from its peer (never the request), every
// search is scoped to that work item's project, another project's memory
// is not found, and the routes pass the step-origin guard as reads while
// refusing callers that are no step process.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// stepGet issues one GET over the daemon socket and answers status + body.
func stepGet(t *testing.T, sock, path string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	resp, err := c.Get("http://daemon" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// stepDaemon is a test daemon whose /step/* callers all resolve to wi.
func stepDaemon(t *testing.T, wi string) *testDaemon {
	t.Helper()
	td := newTestDaemon(t, nil)
	td.submitClaim(t, wi)
	td.stepOriginOf = func(*http.Request) (string, bool) { return wi, true }
	return td
}

func TestStepMemorySearchScopedToTheJobsProject(t *testing.T) {
	td := stepDaemon(t, "wi_sm1")
	var mu sync.Mutex
	var paths []string
	td.hub.mu.Lock()
	td.hub.memDo = func(method, path string, body, out any) error {
		mu.Lock()
		paths = append(paths, method+" "+path)
		mu.Unlock()
		return json.Unmarshal([]byte(`{"items":[{"id":"mem_1","type":"experience.debug","title":"t"}]}`), out)
	}
	td.hub.mu.Unlock()

	code, body := stepGet(t, td.sock, "/step/memories/search?q=sqlite+busy&match=any&type=experience.*&limit=5")
	if code != 200 || !strings.Contains(body, `"mem_1"`) {
		t.Fatalf("search = %d %s", code, body)
	}
	mu.Lock()
	got := paths[len(paths)-1]
	mu.Unlock()
	u, _ := url.Parse(strings.TrimPrefix(got, "GET "))
	q := u.Query()
	if u.Path != "/api/v1/memories/search" || q.Get("project") != "proj" || q.Get("q") != "sqlite busy" || q.Get("match") != "any" || q.Get("type") != "experience.*" || q.Get("limit") != "5" {
		t.Fatalf("forwarded %s", got)
	}
	// a caller naming another project is refused, nothing forwarded
	n := len(paths)
	if code, body := stepGet(t, td.sock, "/step/memories/search?q=x&project=other"); code != 403 || !strings.Contains(body, "DAEMON-403") {
		t.Fatalf("other project = %d %s", code, body)
	}
	if code, _ := stepGet(t, td.sock, "/step/memories/search?q=x&project=proj"); code != 200 {
		t.Fatalf("own project named = %d", code)
	}
	if code, _ := stepGet(t, td.sock, "/step/memories/search"); code != 400 {
		t.Fatalf("no query = %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != n+1 {
		t.Fatalf("refused searches were forwarded: %v", paths[n:])
	}
}

func TestStepMemoryShowOwnProjectOnly(t *testing.T) {
	td := stepDaemon(t, "wi_sm2")
	td.hub.mu.Lock()
	td.hub.memDo = func(method, path string, body, out any) error {
		switch path {
		case "/api/v1/projects/proj":
			return json.Unmarshal([]byte(`{"id":"prj_own","name":"proj"}`), out)
		case "/api/v1/memories/mem_own":
			return json.Unmarshal([]byte(`{"id":"mem_own","project_id":"prj_own","type":"experience.env","title":"ok"}`), out)
		case "/api/v1/memories/mem_other":
			return json.Unmarshal([]byte(`{"id":"mem_other","project_id":"prj_other","type":"experience.env"}`), out)
		}
		return fmt.Errorf("unexpected %s %s", method, path)
	}
	td.hub.memContent = func(id string) ([]byte, error) { return []byte("body of " + id), nil }
	td.hub.mu.Unlock()
	code, body := stepGet(t, td.sock, "/step/memories/mem_own")
	var doc struct {
		Meta    map[string]any `json:"meta"`
		Content string         `json:"content"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &doc) != nil || doc.Meta["id"] != "mem_own" || doc.Content != "body of mem_own" {
		t.Fatalf("own memory = %d %s", code, body)
	}
	if code, body := stepGet(t, td.sock, "/step/memories/mem_other"); code != 404 || !strings.Contains(body, "MEM-404") {
		t.Fatalf("other project's memory = %d %s", code, body)
	}
}

func TestStepContextAnswersTheCallersJob(t *testing.T) {
	td := stepDaemon(t, "wi_sm3")
	code, body := stepGet(t, td.sock, "/step/context")
	var sc stepJob
	if code != 200 || json.Unmarshal([]byte(body), &sc) != nil || sc.WorkItemID != "wi_sm3" || sc.Project != "proj" || sc.CapsuleDir == "" {
		t.Fatalf("context = %d %s", code, body)
	}
	td.stepOriginOf = func(*http.Request) (string, bool) { return "wi_gone", true }
	if code, _ := stepGet(t, td.sock, "/step/context"); code != 404 {
		t.Fatalf("a step of no live job = %d, want 404", code)
	}
}

// TestStepMemoryHelperProcess is not a test: re-executed as a child, it
// GETs one /step path over the socket named in its environment.
func TestStepMemoryHelperProcess(t *testing.T) {
	sock := os.Getenv("STEPMEM_SOCK")
	if sock == "" {
		t.Skip("helper process only")
	}
	code, body := stepGet(t, sock, os.Getenv("STEPMEM_PATH"))
	fmt.Printf("STEPMEM %d %s\n", code, body)
}

// TestStepRoutesPassTheGuardForStepsOnly dials the real UDS face from a
// child process: with the step markers of this daemon (the kernel's peer
// credential names the child) the read passes the step-origin guard and
// resolves to the child's work item; without them it is refused.
func TestStepRoutesPassTheGuardForStepsOnly(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_sm4")
	run := func(env ...string) string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStepMemoryHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), append([]string{"STEPMEM_SOCK=" + td.sock, "STEPMEM_PATH=/step/context"}, env...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := run("PF_WI_ID=wi_sm4", "PF_DAEMON_ID=m-test"); !strings.Contains(out, "STEPMEM 200") || !strings.Contains(out, `"work_item_id":"wi_sm4"`) {
		t.Fatalf("step child: %s", out)
	}
	if out := run(); !strings.Contains(out, "STEPMEM 403") || !strings.Contains(out, "serves step agents only") {
		t.Fatalf("non-step child: %s", out)
	}
	if out := run("PF_WI_ID=wi_sm4", "PF_DAEMON_ID=m-other"); !strings.Contains(out, "STEPMEM 403") {
		t.Fatalf("another daemon's step child: %s", out)
	}
}
