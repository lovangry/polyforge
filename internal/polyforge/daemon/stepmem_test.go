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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
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
	td.stepOriginOf = func(*http.Request) (stepOrigin, string) { return stepOrigin{wi: wi, step: "fix", attempt: 2}, "" }
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
	if code != 200 || json.Unmarshal([]byte(body), &sc) != nil || sc.WorkItemID != "wi_sm3" || sc.Project != "proj" || sc.CapsuleDir == "" || sc.Step != "fix" || sc.Attempt != 2 {
		t.Fatalf("context = %d %s", code, body)
	}
	td.stepOriginOf = func(*http.Request) (stepOrigin, string) { return stepOrigin{wi: "wi_gone"}, "" }
	if code, _ := stepGet(t, td.sock, "/step/context"); code != 404 {
		t.Fatalf("a step of no live job = %d, want 404", code)
	}
}

// TestStepMemoryHelperProcess is not a test: re-executed as a child, it
// plays a step process. With STEPMEM_GO set it waits for that file (the
// parent journals this pid as the step process meanwhile), then runs the
// caller — itself again in caller mode, with STEPMEM_CALLER_WI as its
// PF_WI_ID when set — and relays its output.
func TestStepMemoryHelperProcess(t *testing.T) {
	sock := os.Getenv("STEPMEM_SOCK")
	if sock == "" {
		t.Skip("helper process only")
	}
	if goFile := os.Getenv("STEPMEM_GO"); goFile != "" {
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(goFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestStepMemoryHelperProcess$", "-test.count=1")
		env := []string{"STEPMEM_GO="}
		if wi := os.Getenv("STEPMEM_CALLER_WI"); wi != "" {
			env = append(env, "PF_WI_ID="+wi)
		}
		cmd.Env = append(os.Environ(), env...)
		out, _ := cmd.CombinedOutput()
		fmt.Print(string(out))
		return
	}
	code, body := stepGet(t, sock, "/step/context")
	fmt.Printf("STEPMEM %d %s\n", code, body)
}

// TestStepRoutesResolveTheSpawnedStep dials the real UDS face from a
// child process tree. The work item is the one whose journaled step
// process is the caller's ancestor — never the caller's own PF_WI_ID: a
// caller claiming another live work item is refused, as is a marked
// process that descends from no step of this daemon, and an unmarked one.
func TestStepRoutesResolveTheSpawnedStep(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_sm4")
	td.submitClaim(t, "wi_sm5")
	ji, _ := td.rt.Info("wi_sm4")
	run := func(journal bool, callerWI string, env ...string) string {
		goFile := filepath.Join(t.TempDir(), "go")
		cmd := exec.Command(os.Args[0], "-test.run=^TestStepMemoryHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), append([]string{"STEPMEM_SOCK=" + td.sock, "STEPMEM_GO=" + goFile, "STEPMEM_CALLER_WI=" + callerWI}, env...)...)
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if journal {
			writeJournal(t, ji.CapsuleDir, []capsule.SpawnJournalEntry{{WorkItem: "wi_sm4", StepID: "fix", SpawnN: 7, PID: cmd.Process.Pid}})
		}
		if err := os.WriteFile(goFile, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child: %v\n%s", err, out.String())
		}
		return out.String()
	}
	marks := []string{"PF_WI_ID=wi_sm4", "PF_DAEMON_ID=m-test"}
	if out := run(true, "", marks...); !strings.Contains(out, "STEPMEM 200") || !strings.Contains(out, `"work_item_id":"wi_sm4"`) ||
		!strings.Contains(out, `"step":"fix"`) || !strings.Contains(out, `"attempt":7`) {
		t.Fatalf("step child: %s", out)
	}
	if out := run(true, "wi_sm5", marks...); !strings.Contains(out, "STEPMEM 403") || !strings.Contains(out, "claims PF_WI_ID=wi_sm5") {
		t.Fatalf("a caller claiming another work item: %s", out)
	}
	writeJournal(t, ji.CapsuleDir, nil)
	if out := run(false, "", marks...); !strings.Contains(out, "STEPMEM 403") || !strings.Contains(out, "no live step process") {
		t.Fatalf("a marked process outside every step: %s", out)
	}
	if out := run(true, ""); !strings.Contains(out, "STEPMEM 403") || !strings.Contains(out, "serves step agents only") {
		t.Fatalf("unmarked child: %s", out)
	}
}

// TestDaemonMainRefusedInsideAStep: `pf daemon` reads the machine key;
// never inside a step.
func TestDaemonMainRefusedInsideAStep(t *testing.T) {
	t.Setenv("PF_WI_ID", "wi_x")
	t.Setenv("PF_DAEMON_ID", "m-x")
	var stderr strings.Builder
	if code := Main([]string{"--socket", filepath.Join(t.TempDir(), "s")}, nil, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "refused inside a workflow step") {
		t.Fatalf("Main = %d %s", code, stderr.String())
	}
}
