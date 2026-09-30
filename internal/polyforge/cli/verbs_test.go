package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Direct verbs: request-shape assertions against a fake aihub (httptest).
// Each case drives the full Run path and asserts method, path/query,
// auth header and body shape on the wire.

const cannedWI = `{"id":"wi_abc","slug":"pf-42","seq":42,"project":"polyforge","goal":"the goal",
"status":"queued","priority":"normal","labels":["a"],"reporter":"usr_1","created_at":"2026-10-01T00:00:00Z",
"updated_at":"2026-10-01T00:00:00Z"}`

func TestVerbFileCreateRequestShape(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedWI)
	})
	code, _, stderr := runPf(t, map[string]string{"PF_SERVER": srv.URL, "PF_API_KEY": "k_cli"},
		"file", "create", "--project", "polyforge", "--goal", "do the thing",
		"--label", "infra", "--label", "urgent", "--priority", "high",
		"--bundle", "b1", "--attr", "team=x")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	got := rec.last()
	if got.Method != "POST" || got.Path != "/api/v1/work-items" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	if got.Header.Get("Authorization") != "Bearer k_cli" {
		t.Errorf("auth = %q", got.Header.Get("Authorization"))
	}
	for _, want := range []string{
		`"project":"polyforge"`, `"goal":"do the thing"`,
		`"labels":["infra","urgent"]`, `"priority":"high"`,
		`"bundle_name":"b1"`, `"team":"x"`,
	} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("body missing %s: %s", want, got.Body)
		}
	}
}

func TestVerbFileShowAndEditRequestShape(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedWI)
	})
	env := map[string]string{"PF_SERVER": srv.URL}

	if code, _, stderr := runPf(t, env, "file", "show", "wi_abc"); code != 0 {
		t.Fatalf("show: code=%d stderr=%s", code, stderr)
	}
	if got := rec.last(); got.Method != "GET" || got.Path != "/api/v1/work-items/wi_abc" {
		t.Errorf("show request = %s %s", got.Method, got.Path)
	}

	if code, _, stderr := runPf(t, env, "file", "edit", "wi_abc", "--priority", "low"); code != 0 {
		t.Fatalf("edit: code=%d stderr=%s", code, stderr)
	}
	got := rec.last()
	if got.Method != "PATCH" || got.Path != "/api/v1/work-items/wi_abc" {
		t.Errorf("edit request = %s %s", got.Method, got.Path)
	}
	// PATCH body carries ONLY the flags provided.
	if got.Body != `{"priority":"low"}` {
		t.Errorf("edit body = %s, want only the priority key", got.Body)
	}
}

func TestVerbQueueRequestShape(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[`+cannedWI+`],"total":1}`)
	})
	env := map[string]string{"PF_SERVER": srv.URL, "PF_PROJECT": "polyforge"}

	if code, _, stderr := runPf(t, env, "queue", "--status", "queued"); code != 0 {
		t.Fatalf("queue: code=%d stderr=%s", code, stderr)
	}
	if got := rec.last(); got.Method != "GET" || got.Path != "/api/v1/work-items?project=polyforge&status=queued" {
		t.Errorf("queue request = %s %s", got.Method, got.Path)
	}

	if code, _, stderr := runPf(t, env, "queue", "--ready"); code != 0 {
		t.Fatalf("queue --ready: code=%d stderr=%s", code, stderr)
	}
	if got := rec.last(); got.Path != "/api/v1/projects/polyforge/ready-queue" {
		t.Errorf("ready request path = %s", got.Path)
	}
}

func TestVerbCancelRequestShape(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Replace(cannedWI, `"queued"`, `"canceled"`, 1))
	})
	code, stdout, stderr := runPf(t, map[string]string{"PF_SERVER": srv.URL}, "cancel", "wi_abc")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if got := rec.last(); got.Method != "POST" || got.Path != "/api/v1/work-items/wi_abc/cancel" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	if !strings.Contains(stdout, "cancelled pf-42 (status canceled)") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestVerbRecallRequestShape(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[{"id":"mem_1","type":"experience.debug","content":"first line\nmore","project":"polyforge","visibility":"project","base_strength":3,"created_at":"t","updated_at":"t","tags":["x"]}]}`)
	})
	code, _, stderr := runPf(t, map[string]string{"PF_SERVER": srv.URL, "PF_PROJECT": "polyforge"},
		"recall", "flaky build", "--type", "experience.debug", "--tags", "x,y", "--limit", "5")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	got := rec.last()
	if got.Method != "GET" {
		t.Errorf("method = %s", got.Method)
	}
	for _, want := range []string{
		"/api/v1/memories/search?",
		"project=polyforge", "q=flaky+build",
		"type=experience.debug", "tags=x%2Cy", "limit=5",
	} {
		if !strings.Contains(got.Path, want) {
			t.Errorf("path missing %s: %s", want, got.Path)
		}
	}
}

func TestVerbSubmitPrecheckRefusesTerminal(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Replace(cannedWI, `"queued"`, `"wrapped"`, 1))
	})
	code, _, stderr := runPf(t, map[string]string{"PF_SERVER": srv.URL, "PF_DAEMON_SOCKET": "/nonexistent/x.sock"},
		"submit", "wi_abc")
	if code != 1 {
		t.Fatalf("terminal precheck must exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "terminal (wrapped)") {
		t.Errorf("stderr = %q", stderr)
	}
	// No claim may be attempted after the refusal: the only aihub request
	// is the precheck read.
	if n := len(rec.all()); n != 1 {
		t.Errorf("aihub requests = %d, want 1 (precheck only)", n)
	}
}

func TestAdminVerbsRequestShapes(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/users"):
			io.WriteString(w, `{"id":"usr_1","display_name":"A","email":"a@x","role":"writer","user_type":"human","created_at":"t"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/users/"):
			io.WriteString(w, `{"id":"usr_1","display_name":"B","role":"admin","user_type":"human","created_at":"t"}`)
		case strings.HasSuffix(r.URL.Path, "/api-keys"):
			io.WriteString(w, `{"id":"key_1","key":"pfk_plain","user_id":"usr_1"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/api-keys/"):
			io.WriteString(w, `{"revoked":true}`)
		case r.URL.Path == "/api/v1/projects":
			io.WriteString(w, `{"id":"prj_1","name":"polyforge","visible":true,"created_at":"t","updated_at":"t"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
			io.WriteString(w, `{"id":"prj_1","name":"polyforge","visible":true,"members":[],"created_at":"t","updated_at":"t"}`)
		case strings.HasSuffix(r.URL.Path, "/backup"):
			io.WriteString(w, `{"backup":"/var/backups/snap.db"}`)
		case strings.HasSuffix(r.URL.Path, "/rotate-secret"):
			io.WriteString(w, `{"rotated":true}`)
		}
	})
	env := map[string]string{"PF_SERVER": srv.URL}

	if code, _, stderr := runPf(t, env, "users", "create", "--display-name", "A", "--user-type", "machine"); code != 0 {
		t.Fatalf("users create: %d %s", code, stderr)
	}
	if got := rec.last(); got.Method != "POST" || got.Path != "/api/v1/users" ||
		!strings.Contains(got.Body, `"user_type":"machine"`) {
		t.Errorf("users create = %s %s %s", got.Method, got.Path, got.Body)
	}

	if code, _, stderr := runPf(t, env, "users", "patch", "usr_1", "--role", "admin"); code != 0 {
		t.Fatalf("users patch: %d %s", code, stderr)
	}
	if got := rec.last(); got.Method != "PATCH" || got.Path != "/api/v1/users/usr_1" ||
		got.Body != `{"role":"admin"}` {
		t.Errorf("users patch = %s %s %s", got.Method, got.Path, got.Body)
	}

	if code, _, stderr := runPf(t, env, "api-keys", "create", "--user", "usr_1", "--project-scope", "prj_1"); code != 0 {
		t.Fatalf("api-keys create: %d %s", code, stderr)
	}
	if got := rec.last(); got.Path != "/api/v1/api-keys" ||
		!strings.Contains(got.Body, `"user_id":"usr_1"`) || !strings.Contains(got.Body, `"project_id":"prj_1"`) {
		t.Errorf("api-keys create = %s %s", got.Method, got.Path)
	}

	if code, _, stderr := runPf(t, env, "api-keys", "revoke", "key_1"); code != 0 {
		t.Fatalf("api-keys revoke: %d %s", code, stderr)
	}
	if got := rec.last(); got.Method != "DELETE" || got.Path != "/api/v1/api-keys/key_1" {
		t.Errorf("api-keys revoke = %s %s", got.Method, got.Path)
	}

	if code, _, stderr := runPf(t, env, "projects", "create", "--name", "polyforge"); code != 0 {
		t.Fatalf("projects create: %d %s", code, stderr)
	}
	if got := rec.last(); got.Method != "POST" || got.Path != "/api/v1/projects" ||
		!strings.Contains(got.Body, `"name":"polyforge"`) {
		t.Errorf("projects create = %s %s %s", got.Method, got.Path, got.Body)
	}

	if code, _, stderr := runPf(t, env, "projects", "members", "add", "prj_1", "--user", "usr_1", "--role", "maintainer"); code != 0 {
		t.Fatalf("members add: %d %s", code, stderr)
	}
	if got := rec.last(); got.Method != "PATCH" || got.Path != "/api/v1/projects/prj_1" ||
		!strings.Contains(got.Body, `"members":[{"user_id":"usr_1","role":"maintainer"}]`) {
		t.Errorf("members add = %s %s %s", got.Method, got.Path, got.Body)
	}

	if code, _, stderr := runPf(t, env, "projects", "members", "remove", "prj_1", "--user", "usr_1", "--user", "usr_2"); code != 0 {
		t.Fatalf("members remove: %d %s", code, stderr)
	}
	if got := rec.last(); !strings.Contains(got.Body, `"remove_members":["usr_1","usr_2"]`) {
		t.Errorf("members remove body = %s", got.Body)
	}

	// `projects patch` with NO --visible flag must NOT carry the key.
	if code, _, stderr := runPf(t, env, "projects", "patch", "prj_1"); code != 0 {
		t.Fatalf("projects patch: %d %s", code, stderr)
	}
	if got := rec.last(); got.Body != "{}" {
		t.Errorf("projects patch body = %s, want {}", got.Body)
	}

	// -repos is the explicit full replacement of the task-repo
	// inventory (pilot item: the wire face was already there, the CLI
	// flag was not). Omitted → key absent; spelled → the full list.
	if code, _, stderr := runPf(t, env, "projects", "patch", "prj_1",
		"--repos", "demo-src=file:///x/demo.git,other=https://h/o.git"); code != 0 {
		t.Fatalf("projects patch --repos: %d %s", code, stderr)
	}
	if got := rec.last(); !strings.Contains(got.Body,
		`"repos":[{"name":"demo-src","url":"file:///x/demo.git"},{"name":"other","url":"https://h/o.git"}]`) {
		t.Errorf("projects patch --repos body = %s", got.Body)
	}
	// the explicit clear: -repos "" carries an empty list.
	if code, _, stderr := runPf(t, env, "projects", "patch", "prj_1", "--repos", ""); code != 0 {
		t.Fatalf("projects patch --repos clear: %d %s", code, stderr)
	}
	if got := rec.last(); !strings.Contains(got.Body, `"repos":[]`) {
		t.Errorf("projects patch --repos clear body = %s", got.Body)
	}
	// a malformed entry is refused client-side, never silently dropped.
	if code, _, _ := runPf(t, env, "projects", "patch", "prj_1", "--repos", "no-url-here"); code == 0 {
		t.Fatal("malformed -repos entry must fail")
	}

	if code, _, stderr := runPf(t, env, "backup"); code != 0 {
		t.Fatalf("backup: %d %s", code, stderr)
	}
	if got := rec.last(); got.Method != "POST" || got.Path != "/api/v1/admin/backup" {
		t.Errorf("backup = %s %s", got.Method, got.Path)
	}

	if code, _, stderr := runPf(t, env, "rotate-secret", "--mode", "grace", "--grace-seconds", "300"); code != 0 {
		t.Fatalf("rotate-secret: %d %s", code, stderr)
	}
	if got := rec.last(); got.Path != "/api/v1/service/rotate-secret" ||
		!strings.Contains(got.Body, `"mode":"grace"`) || !strings.Contains(got.Body, `"grace_seconds":300`) {
		t.Errorf("rotate-secret = %s %s %s", got.Method, got.Path, got.Body)
	}
}

// TestFlagBeatsEnvEndToEnd: --server on the command line wins over the
// PF_SERVER environment value on a real verb call (level 3 of the
// config chain; levels 0-2 are pinned in config_test.go).
func TestFlagBeatsEnvEndToEnd(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedWI)
	})
	env := map[string]string{"PF_SERVER": "http://env-loses.invalid:1", "PF_API_KEY": "k_env"}
	code, _, stderr := runPf(t, env, "--server", srv.URL, "--api-key", "k_flag", "file", "show", "wi_abc")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	// The flag server received the request; the env server never could.
	if got := rec.last(); got.Header.Get("Authorization") != "Bearer k_flag" {
		t.Errorf("auth = %q, want the flag api key", got.Header.Get("Authorization"))
	}
}

// TestJSONPassthrough: --json emits the server body verbatim.
func TestJSONPassthrough(t *testing.T) {
	const body = `{"id":"wi_abc","slug":"pf-42","seq":42,"extra_unknown_field":  {"kept": true}}`
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	})
	code, stdout, stderr := runPf(t, map[string]string{"PF_SERVER": srv.URL}, "--json", "file", "show", "wi_abc")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != body {
		t.Errorf("--json passthrough altered the body:\n got: %s\nwant: %s", stdout, body)
	}
}

// TestDaemonVerbJSONPassthrough: the UDS face too.
func TestDaemonVerbJSONPassthrough(t *testing.T) {
	var aihubRec reqRecorder
	aihub := newFakeAihub(t, &aihubRec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, cannedWI)
	})
	d := newDaemonFixture(t)
	const body = `{"accepted":true,"work_item_id":"wi_abc"}`
	d.answer("POST", "/jobs/wi_abc/claim", body)
	code, stdout, stderr := runPf(t, map[string]string{
		"PF_SERVER":        aihub.URL,
		"PF_DAEMON_SOCKET": d.sock,
	}, "--json", "submit", "wi_abc")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != body {
		t.Errorf("--json passthrough altered the body: %s", stdout)
	}
}

func TestVerbRememberRequestShape(t *testing.T) {
	var rec reqRecorder
	srv := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"mem_new","type":"experience.debug","visibility":"project","created_at":"t","updated_at":"t"}`)
	})
	code, stdout, stderr := runPf(t, map[string]string{"PF_SERVER": srv.URL, "PF_API_KEY": "k_cli", "PF_PROJECT": "polyforge"},
		"remember", "--type", "experience.debug", "--title", "ladders",
		"--tags", "infra,flaky", "--work-item", "wi_abc",
		"the lesson about bounded retry ladders")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	got := rec.last()
	if got.Method != "POST" || got.Path != "/api/v1/memories" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	for _, want := range []string{
		`"project":"polyforge"`, `"type":"experience.debug"`,
		`"title":"ladders"`, `"visibility":"project"`,
		`"tags":["infra","flaky"]`, `"work_item_id":"wi_abc"`,
		`"content":"the lesson about bounded retry ladders"`,
	} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("body missing %s: %s", want, got.Body)
		}
	}
	if !strings.Contains(stdout, "stored mem_new") {
		t.Errorf("stdout = %q", stdout)
	}

	// Validation: a missing --type is a client-side refusal (no wire call).
	srv2 := newFakeAihub(t, &rec, func(w http.ResponseWriter, r *http.Request) {})
	if code, _, _ := runPf(t, map[string]string{"PF_SERVER": srv2.URL, "PF_PROJECT": "polyforge"},
		"remember", "content only"); code != 1 {
		t.Fatalf("missing --type: code=%d, want 1", code)
	}
}
