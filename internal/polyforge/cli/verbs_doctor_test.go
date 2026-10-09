package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verbs_doctor_test.go: the `pf doctor` unit pins — the report shape,
// the exit codes (0 all-pass / 1 any-fail / 2 usage), each check's
// distinct failure, and the not-faked-green contract on an unloadable
// config.

// doctorDoc decodes the --json report.
type doctorDoc struct {
	OK     bool `json:"ok"`
	Checks []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"checks"`
}

func doctorStatus(t *testing.T, env map[string]string, args ...string) (int, string, doctorDoc) {
	t.Helper()
	code, stdout, stderr := runPf(t, env, args...)
	var doc doctorDoc
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &doc); err != nil {
		t.Fatalf("doctor --json is not the pinned {ok, checks} shape: %v (stdout %q stderr %q)", err, stdout, stderr)
	}
	return code, stdout, doc
}

// doctorFind answers the check with the given name.
func doctorFind(t *testing.T, doc doctorDoc, name string) (string, string) {
	t.Helper()
	for _, c := range doc.Checks {
		if c.Name == name {
			return c.Status, c.Detail
		}
	}
	t.Fatalf("no check named %q in %+v", name, doc.Checks)
	return "", ""
}

// doctorServerConfig points a scratch config at srv with a live mapped
// workspace and the given daemon socket, and answers the runPf env.
func doctorServerConfig(t *testing.T, server, apiKey, socket string, projects ...string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	xdg := filepath.Join(dir, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("server = \"" + server + "\"\napi_key = \"" + apiKey + "\"\n")
	if socket != "" {
		b.WriteString("daemon_socket = \"" + socket + "\"\n")
	}
	for _, p := range projects {
		b.WriteString("\n[workspace.\"" + filepath.Join(dir, "ws-"+p) + "\"]\nprojects = [\"" + p + "\"]\n")
		if err := os.MkdirAll(filepath.Join(dir, "ws-"+p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(xdg, "polyforge", "cli.toml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"XDG_CONFIG_HOME": xdg}
}

// liveDaemonSocket serves GET /status on a real unix socket.
func liveDaemonSocket(t *testing.T, machineID string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"machine_id":"`+machineID+`","mode":"interactive","shutting_down":false,"jobs":[],"queue":{"pending":0,"running":0,"max_concurrent":1}}`)
	})
	return newFakeDaemon(t, mux)
}

// aiSever is a fake aihub for the doctor: per-project GET /api/v1/projects/<name>.
func aiSever(t *testing.T, rec *reqRecorder, known map[string]bool, status int, body string) *httptest.Server {
	t.Helper()
	return newFakeAihub(t, rec, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects/") {
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
			if status != 0 {
				w.WriteHeader(status)
				io.WriteString(w, body)
				return
			}
			if known[name] {
				io.WriteString(w, `{"id":"prj_1","name":"`+name+`"}`)
				return
			}
			w.WriteHeader(404)
			io.WriteString(w, `{"code":"CORE-404","detail":"no such project"}`)
			return
		}
		io.WriteString(w, `{}`)
	})
}

func TestDoctorHealthyMachineAllPassExit0(t *testing.T) {
	srv := aiSever(t, &reqRecorder{}, map[string]bool{"polyforge": true}, 0, "")
	sock := liveDaemonSocket(t, "m-h")
	env := doctorServerConfig(t, srv.URL, "k_ok", sock, "polyforge")
	code, stdout, doc := doctorStatus(t, env, "--json", "doctor")
	if code != 0 {
		t.Fatalf("healthy machine: exit %d, want 0 (output %s)", code, stdout)
	}
	if !doc.OK {
		t.Fatalf("healthy machine: ok = false (%+v)", doc.Checks)
	}
	for _, c := range doc.Checks {
		if c.Status != "pass" {
			t.Fatalf("check %q = %q (%s), want pass", c.Name, c.Status, c.Detail)
		}
	}
	// Human mode: every check line prints, exit 0.
	code, stdout, _ = runPf(t, env, "doctor")
	if code != 0 || !strings.Contains(stdout, "pass") {
		t.Fatalf("human doctor: exit %d output %q", code, stdout)
	}
}

func TestDoctorMappingChecksFailDistinctly(t *testing.T) {
	t.Run("mapping absent", func(t *testing.T) {
		srv := aiSever(t, &reqRecorder{}, nil, 0, "")
		env := doctorServerConfig(t, srv.URL, "k", "")
		code, _, doc := doctorStatus(t, env, "--json", "doctor")
		if code != 1 || doc.OK {
			t.Fatalf("exit %d ok %v", code, doc.OK)
		}
		st, detail := doctorFind(t, doc, "workspace mapping")
		if st != "fail" || !strings.Contains(detail, "[workspace") {
			t.Fatalf("mapping check = %s %q", st, detail)
		}
	})
	t.Run("workspace dir missing", func(t *testing.T) {
		srv := aiSever(t, &reqRecorder{}, map[string]bool{"polyforge": true}, 0, "")
		dir := t.TempDir()
		xdg := filepath.Join(dir, "xdg")
		if err := os.MkdirAll(filepath.Join(xdg, "polyforge"), 0o755); err != nil {
			t.Fatal(err)
		}
		missing := filepath.Join(dir, "gone-ws")
		cfg := "server = \"" + srv.URL + "\"\napi_key = \"k\"\n\n[workspace.\"" + missing + "\"]\nprojects = [\"polyforge\"]\n"
		if err := os.WriteFile(filepath.Join(xdg, "polyforge", "cli.toml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, doc := doctorStatus(t, map[string]string{"XDG_CONFIG_HOME": xdg}, "--json", "doctor")
		if code != 1 || doc.OK {
			t.Fatalf("exit %d ok %v", code, doc.OK)
		}
		st, detail := doctorFind(t, doc, "workspace dirs")
		if st != "fail" || !strings.Contains(detail, "gone-ws") {
			t.Fatalf("dirs check = %s %q", st, detail)
		}
	})
	t.Run("project unknown to aihub", func(t *testing.T) {
		srv := aiSever(t, &reqRecorder{}, map[string]bool{}, 0, "")
		env := doctorServerConfig(t, srv.URL, "k", "", "polyforge")
		code, stdout, doc := doctorStatus(t, env, "--json", "doctor")
		if code != 1 || doc.OK {
			t.Fatalf("exit %d ok %v (output %s)", code, doc.OK, stdout)
		}
		st, detail := doctorFind(t, doc, "aihub projects")
		if st != "fail" || !strings.Contains(detail, "polyforge") {
			t.Fatalf("projects check = %s %q", st, detail)
		}
		if !strings.Contains(detail, "ws-polyforge") {
			t.Fatalf("projects check detail %q must name the project's workspace", detail)
		}
	})
	t.Run("auth rejected", func(t *testing.T) {
		srv := aiSever(t, &reqRecorder{}, nil, 401, `{"code":"AUTH-401","detail":"bad key"}`)
		env := doctorServerConfig(t, srv.URL, "k_bad", "", "polyforge")
		code, _, doc := doctorStatus(t, env, "--json", "doctor")
		if code != 1 || doc.OK {
			t.Fatalf("exit %d ok %v", code, doc.OK)
		}
		st, detail := doctorFind(t, doc, "auth")
		if st != "fail" {
			t.Fatalf("auth check = %s %q", st, detail)
		}
	})
}

func TestDoctorConnectivityFailsDistinctly(t *testing.T) {
	t.Run("server unreachable", func(t *testing.T) {
		env := doctorServerConfig(t, deadAihubURL(t), "k", "", "polyforge")
		code, _, doc := doctorStatus(t, env, "--json", "doctor")
		if code != 1 || doc.OK {
			t.Fatalf("exit %d ok %v", code, doc.OK)
		}
		st, _ := doctorFind(t, doc, "server reach")
		if st != "fail" {
			t.Fatalf("server check not failing: %+v", doc.Checks)
		}
		// The aihub-flavored checks must not fake green behind a dead server.
		for _, name := range []string{"aihub projects", "auth"} {
			if st, _ := doctorFind(t, doc, name); st != "skip" && st != "fail" {
				t.Fatalf("%s = %s behind an unreachable server (faked green?)", name, st)
			}
		}
	})
	t.Run("daemon socket dead", func(t *testing.T) {
		srv := aiSever(t, &reqRecorder{}, map[string]bool{"polyforge": true}, 0, "")
		dead := filepath.Join(t.TempDir(), "dead.sock")
		env := doctorServerConfig(t, srv.URL, "k", dead, "polyforge")
		code, _, doc := doctorStatus(t, env, "--json", "doctor")
		if code != 1 || doc.OK {
			t.Fatalf("exit %d ok %v", code, doc.OK)
		}
		st, detail := doctorFind(t, doc, "daemon socket")
		if st != "fail" || !strings.Contains(detail, dead) {
			t.Fatalf("socket check = %s %q", st, detail)
		}
	})
}

func TestDoctorUnloadableConfigDoesNotFakeGreen(t *testing.T) {
	dir := t.TempDir()
	xdg := filepath.Join(dir, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldShape := "server = \"http://x:1\"\napi_key = \"k\"\nproject = \"e2e\"\n"
	if err := os.WriteFile(filepath.Join(xdg, "polyforge", "cli.toml"), []byte(oldShape), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"XDG_CONFIG_HOME": xdg}
	code, stdout, doc := doctorStatus(t, env, "--json", "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1 (output %s)", code, stdout)
	}
	if doc.OK {
		t.Fatalf("ok = true on an old-shape config")
	}
	st, detail := doctorFind(t, doc, "config")
	if st != "fail" || !strings.Contains(detail, "project") {
		t.Fatalf("config check = %s %q", st, detail)
	}
	skips := 0
	for _, c := range doc.Checks {
		if c.Status == "skip" {
			skips++
		}
		if c.Status == "pass" {
			t.Fatalf("check %q faked pass on an unloadable config", c.Name)
		}
	}
	if skips == 0 {
		t.Fatalf("no config-dependent check reported skip (%+v)", doc.Checks)
	}
}

func TestDoctorUsageErrorExit2(t *testing.T) {
	code, _, _ := runPf(t, map[string]string{}, "doctor", "--bogus")
	if code != 2 {
		t.Fatalf("doctor --bogus: exit %d, want 2", code)
	}
}
