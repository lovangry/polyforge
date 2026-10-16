package cli

// stepctx_test.go: `pf` inside a workflow step (PF_WI_ID set) — recall
// and memory show go to the daemon socket, remember writes the capsule
// outbox, every other aihub verb is refused, and nothing ever reaches
// aihub (even with an api key in the environment).

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// stepEnv answers a fake daemon serving /step/*, the env of a step
// invocation pointed at it (plus a live fake aihub that must stay
// untouched), the capsule dir and the daemon's request log.
func stepEnv(t *testing.T) (map[string]string, string, *reqRecorder, *reqRecorder) {
	t.Helper()
	capDir := t.TempDir()
	drec := &reqRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /step/context", func(w http.ResponseWriter, r *http.Request) {
		drec.record(r)
		io.WriteString(w, jsonBody(map[string]string{"work_item_id": "wi_st", "slug": "polyforge#7", "project": "polyforge", "capsule_dir": capDir}))
	})
	mux.HandleFunc("GET /step/memories/search", func(w http.ResponseWriter, r *http.Request) {
		drec.record(r)
		io.WriteString(w, `{"items":[{"id":"mem_1","type":"experience.debug","title":"sqlite busy","content_excerpt":"x"}],"total":1}`)
	})
	mux.HandleFunc("GET /step/memories/{id}", func(w http.ResponseWriter, r *http.Request) {
		drec.record(r)
		io.WriteString(w, `{"meta":{"id":"`+r.PathValue("id")+`","type":"experience.debug","title":"sqlite busy","visibility":"project"},"content":"the body\n"}`)
	})
	sock := newFakeDaemon(t, mux)
	arec := &reqRecorder{}
	srv := newFakeAihub(t, arec, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	env := map[string]string{"PF_WI_ID": "wi_st", "PF_DAEMON_SOCKET": sock, "PF_SERVER": srv.URL, "PF_API_KEY": "k_should_never_leave"}
	return env, capDir, drec, arec
}

func TestStepRecallAndShowGoThroughTheDaemon(t *testing.T) {
	env, _, drec, arec := stepEnv(t)
	// no cli.toml mapping is needed inside a step
	code, stdout, stderr := runPf(t, env, "recall", "--project", "polyforge", "--any", "--type", "experience.*", "sqlite busy")
	if code != 0 || !strings.Contains(stdout, "1 hit(s)") || !strings.Contains(stdout, "mem_1") {
		t.Fatalf("recall: exit %d %s %s", code, stdout, stderr)
	}
	q := drec.last()
	if !strings.Contains(q.Path, "match=any") || !strings.Contains(q.Path, "project=polyforge") || !strings.Contains(q.Path, "q=sqlite+busy") {
		t.Fatalf("daemon query = %s", q.Path)
	}
	code, stdout, stderr = runPf(t, env, "memory", "show", "mem_1")
	if code != 0 || !strings.Contains(stdout, "id: mem_1") || !strings.Contains(stdout, "the body") {
		t.Fatalf("memory show: exit %d %s %s", code, stdout, stderr)
	}
	if n := len(arec.all()); n != 0 {
		t.Fatalf("a step invocation reached aihub %d time(s)", n)
	}
}

func TestStepRememberWritesTheOutbox(t *testing.T) {
	env, capDir, _, arec := stepEnv(t)
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("现象→原因→解法\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runPf(t, env, "remember", "--project", "polyforge", "--type", "experience.debug", "--work-item", "polyforge#7",
		"--title", "sqlite busy under WAL", "--tags", "policy-proposal", "@"+body)
	if code != 0 || !strings.Contains(stdout, "queued "+capsule.DirMemoryOutbox+"/") {
		t.Fatalf("remember: exit %d %s %s", code, stdout, stderr)
	}
	files, _ := filepath.Glob(filepath.Join(capDir, filepath.FromSlash(capsule.DirMemoryOutbox), "*.md"))
	if len(files) != 1 {
		t.Fatalf("outbox = %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	m, err := capsule.ParseOutboxMemory(raw)
	if err != nil || m.Type != "experience.debug" || m.Title != "sqlite busy under WAL" || len(m.Tags) != 1 || m.Body != "现象→原因→解法\n" {
		t.Fatalf("outbox file = %+v, %v\n%s", m, err, raw)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--type", "doc.design", "--title", "t"}, "--type must be experience.<kind>"},
		{[]string{"--type", "experience.debug"}, "--title is required"},
		{[]string{"--type", "experience.debug", "--title", "t", "--project", "other"}, `belongs to project "polyforge"`},
		{[]string{"--type", "experience.debug", "--title", "t", "--work-item", "wi_other"}, "attributed to its own work item wi_st"},
		{[]string{"--type", "experience.debug", "--title", "t", "--visibility", "public"}, "project-visible"},
	} {
		args := append([]string{"remember", "--project", "polyforge"}, tc.args...)
		args = append(args, "body")
		if code, _, stderr := runPf(t, env, args...); code != 1 || !strings.Contains(stderr, tc.want) {
			t.Fatalf("%v: exit %d %s, want %q", tc.args, code, stderr, tc.want)
		}
	}
	if n := len(arec.all()); n != 0 {
		t.Fatalf("a step remember reached aihub %d time(s)", n)
	}
}

func TestStepRefusesOtherAihubVerbs(t *testing.T) {
	env, _, _, arec := stepEnv(t)
	for _, argv := range [][]string{
		{"file", "create", "--project", "polyforge", "--goal", "g"},
		{"queue", "--project", "polyforge"},
		{"cancel", "wi_x"},
		{"projects", "list"},
		{"users", "list"},
	} {
		code, _, stderr := runPf(t, env, argv...)
		if code != 1 || !strings.Contains(stderr, "DAEMON-403") || !strings.Contains(stderr, "refused inside a workflow step") {
			t.Fatalf("%v: exit %d %s", argv, code, stderr)
		}
	}
	if n := len(arec.all()); n != 0 {
		t.Fatalf("refused verbs reached aihub %d time(s)", n)
	}
	// outside a step the same remember goes to aihub as before
	delete(env, "PF_WI_ID")
	for k, v := range withMappingConfig(t) {
		env[k] = v
	}
	if code, _, stderr := runPf(t, env, "remember", "--project", "polyforge", "--type", "fact.note", "x"); code != 0 || len(arec.all()) != 1 {
		t.Fatalf("outside a step: exit %d %s, aihub calls %d", code, stderr, len(arec.all()))
	}
}
