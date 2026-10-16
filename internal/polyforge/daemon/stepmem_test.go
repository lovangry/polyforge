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
	"strconv"
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

// TestStepMemoryHelperProcess is not a test: re-executed as a child it
// plays one role of a step process tree (STEPMEM_ROLE):
//
//	relay  — spawns a "step" child with PF_WI_ID=STEPMEM_INNER_WI
//	step   — writes its pid to STEPMEM_PIDFILE, waits for STEPMEM_GO,
//	         spawns the caller (PF_WI_ID=STEPMEM_CALLER_WI when set)
//	caller — GETs /step/context and prints the answer
//
// Every role relays its child's output.
func TestStepMemoryHelperProcess(t *testing.T) {
	sock := os.Getenv("STEPMEM_SOCK")
	if sock == "" {
		t.Skip("helper process only")
	}
	child := func(role string, env ...string) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStepMemoryHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), append([]string{"STEPMEM_ROLE=" + role}, env...)...)
		out, _ := cmd.CombinedOutput()
		fmt.Print(string(out))
	}
	switch os.Getenv("STEPMEM_ROLE") {
	case "relay":
		child("step", "PF_WI_ID="+os.Getenv("STEPMEM_INNER_WI"))
	case "step":
		_ = os.WriteFile(os.Getenv("STEPMEM_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(os.Getenv("STEPMEM_GO")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		var env []string
		if wi := os.Getenv("STEPMEM_CALLER_WI"); wi != "" {
			env = append(env, "PF_WI_ID="+wi)
		}
		child("caller", env...)
	default:
		code, body := stepGet(t, sock, "/step/context")
		fmt.Printf("STEPMEM %d %s\n", code, body)
	}
}

// stepTree runs one helper tree: role relay or step at the top, the
// journal callback receiving the top pid and the inner step pid, the
// caller's output answered.
func stepTree(t *testing.T, td *testDaemon, role string, journal func(top, step int), env ...string) string {
	t.Helper()
	dir := t.TempDir()
	pidFile, goFile := filepath.Join(dir, "pid"), filepath.Join(dir, "go")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStepMemoryHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), append([]string{"STEPMEM_SOCK=" + td.sock, "STEPMEM_ROLE=" + role,
		"STEPMEM_PIDFILE=" + pidFile, "STEPMEM_GO=" + goFile}, env...)...)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var step int
	for i := 0; i < 500 && step == 0; i++ {
		if raw, err := os.ReadFile(pidFile); err == nil {
			step, _ = strconv.Atoi(string(raw))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if journal != nil {
		journal(cmd.Process.Pid, step)
	}
	if err := os.WriteFile(goFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper tree: %v\n%s", err, out.String())
	}
	return out.String()
}

// TestStepRoutesResolveTheSpawnedStep dials the real UDS face from a
// child process tree. The work item is the one whose journaled step
// process is the caller's ancestor — never the caller's own PF_WI_ID —
// matched against any of the job's journal entries; a caller claiming
// another live work item, a tree reaching two jobs' steps, a marked
// process outside every step and an unmarked one are refused.
func TestStepRoutesResolveTheSpawnedStep(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.submitClaim(t, "wi_sm4")
	td.submitClaim(t, "wi_sm5")
	ji4, _ := td.rt.Info("wi_sm4")
	ji5, _ := td.rt.Info("wi_sm5")
	marks := []string{"PF_WI_ID=wi_sm4", "PF_DAEMON_ID=m-test"}
	// the step's entry is not the newest one (a later dispatch was
	// journaled, e.g. an in-flight spawn): it still resolves
	journalStep := func(_, step int) {
		writeJournal(t, ji4.CapsuleDir, []capsule.SpawnJournalEntry{
			{WorkItem: "wi_sm4", StepID: "fix", SpawnN: 7, PID: step},
			{WorkItem: "wi_sm4", StepID: "review", SpawnN: 8, PID: 1 << 22},
		})
	}
	if out := stepTree(t, td, "step", journalStep, marks...); !strings.Contains(out, "STEPMEM 200") || !strings.Contains(out, `"work_item_id":"wi_sm4"`) ||
		!strings.Contains(out, `"step":"fix"`) || !strings.Contains(out, `"attempt":7`) {
		t.Fatalf("step child: %s", out)
	}
	if out := stepTree(t, td, "step", journalStep, append(marks, "STEPMEM_CALLER_WI=wi_sm5")...); !strings.Contains(out, "STEPMEM 403") || !strings.Contains(out, "claims PF_WI_ID=wi_sm5") {
		t.Fatalf("a caller claiming another work item: %s", out)
	}
	// a step of wi_sm5 running inside wi_sm4's step tree: both match
	both := func(top, step int) {
		writeJournal(t, ji4.CapsuleDir, []capsule.SpawnJournalEntry{{WorkItem: "wi_sm4", StepID: "fix", SpawnN: 1, PID: top}})
		writeJournal(t, ji5.CapsuleDir, []capsule.SpawnJournalEntry{{WorkItem: "wi_sm5", StepID: "fix", SpawnN: 1, PID: step}})
	}
	if out := stepTree(t, td, "relay", both, append(marks, "STEPMEM_INNER_WI=wi_sm5")...); !strings.Contains(out, "STEPMEM 403") || !strings.Contains(out, "several work items (wi_sm4, wi_sm5)") {
		t.Fatalf("a tree reaching two steps: %s", out)
	}
	writeJournal(t, ji4.CapsuleDir, nil)
	writeJournal(t, ji5.CapsuleDir, nil)
	if out := stepTree(t, td, "step", nil, marks...); !strings.Contains(out, "STEPMEM 403") || !strings.Contains(out, "no live step process") {
		t.Fatalf("a marked process outside every step: %s", out)
	}
	if out := stepTree(t, td, "step", journalStep); !strings.Contains(out, "STEPMEM 403") || !strings.Contains(out, "serves step agents only") {
		t.Fatalf("unmarked child: %s", out)
	}
}

// TestDaemonMainRefusedInsideAStep: `pf daemon` reads the machine key;
// never inside a step.
func TestDaemonMainRefusedInsideAStep(t *testing.T) {
	t.Setenv("PF_WI_ID", "wi_x")
	t.Setenv("PF_DAEMON_ID", "m-x")
	var stderr strings.Builder
	for _, argv := range [][]string{{"--socket", filepath.Join(t.TempDir(), "s")}, {"ensure"}} {
		stderr.Reset()
		if code := Main(argv, nil, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "refused inside a workflow step") {
			t.Fatalf("Main(%v) = %d %s", argv, code, stderr.String())
		}
	}
}
