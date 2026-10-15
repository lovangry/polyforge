package daemon

// ensure_test.go (polyforge#174): unit pins of `pf daemon ensure` — the
// machine id it launches with, the loop script (valid sh, the canonical
// argv quoted, the daemon immune to the loop's hangup, harness markers
// dropped) and the refusals. The end-to-end behavior (real tmux on a
// private server, real daemon, concurrency, restart) lives in
// test/w174_daemon_ensure_test.go.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureMachineID(t *testing.T) {
	dir := t.TempDir()
	got := ensureMachineID(dir)
	if !strings.HasPrefix(got, "m-") || hostSanitizeRe.MatchString(got[2:]) {
		t.Fatalf("default id %q is not m-<sanitized short hostname>", got)
	}
	if err := os.WriteFile(filepath.Join(dir, daemonIDFile), []byte("m-recorded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ensureMachineID(dir); got != "m-recorded" {
		t.Fatalf("the recorded daemon-id must win, got %q", got)
	}
}

func TestEnsureLoopScript(t *testing.T) {
	p := &ensurePlan{
		pfBin: "/opt/pf bin/pf", configPath: "/c/it's cli.toml", dataDir: "/d/data",
		socket: "/d/data/daemon.sock", workflowsDir: "/d/data/release/current/workflows",
		machineID: "m-x", home: "/home/u", timeout: time.Second,
	}
	script := p.loopScript()
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("loop script is not valid sh: %v %s\n%s", err, out, script)
	}
	for _, want := range []string{
		"nohup '/opt/pf bin/pf' 'daemon' '--config' '/c/it'\\''s cli.toml' '-machine-id' 'm-x' '-data-dir' '/d/data' '-socket' '/d/data/daemon.sock' '-workflows-dir' '/d/data/release/current/workflows' < /dev/null >> '/d/data/daemon.log' 2>&1",
		"while true; do", "sleep 5", "POLYFORGE_SUPERVISOR='tmux:polyforge-daemon'",
		"unset TMUX TMUX_PANE CLAUDECODE", "PF_WI_ID",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("loop script lacks %q:\n%s", want, script)
		}
	}
	for _, banned := range []string{"-auto", "-max-concurrent-jobs", "--harness-root"} {
		if strings.Contains(script, banned) {
			t.Fatalf("loop script forces %q:\n%s", banned, script)
		}
	}
}

func TestEnsureUsageAndRefusals(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"ensure", "--bogus"}, nil, &out, &errb); code != 2 {
		t.Fatalf("unknown flag: exit %d, want 2", code)
	}
	if code := Main([]string{"ensure", "extra"}, nil, &out, &errb); code != 2 {
		t.Fatalf("stray argument: exit %d, want 2", code)
	}
	// no mapping: refused before any tmux / process work
	cfg := filepath.Join(t.TempDir(), "cli.toml")
	sock := filepath.Join(t.TempDir(), "d.sock")
	if err := os.WriteFile(cfg, []byte("server = \"http://127.0.0.1:1\"\ndaemon_socket = \""+sock+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no tmux reachable at all
	errb.Reset()
	if code := Main([]string{"ensure", "--config", cfg, "--timeout", "1s"}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "[workspace") {
		t.Fatalf("no mapping: exit %d stderr %q, want 1 naming the mapping", code, errb.String())
	}
	// a mapping but no workflows dir: refused, nothing started
	ws := t.TempDir()
	if err := os.WriteFile(cfg, []byte("server = \"http://127.0.0.1:1\"\ndaemon_socket = \""+sock+"\"\n[workspace.\""+ws+"\"]\nprojects = [\"e2e\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := Main([]string{"ensure", "--config", cfg, "--timeout", "1s", "--workflows-dir", filepath.Join(ws, "absent")}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "workflows dir") {
		t.Fatalf("missing workflows: exit %d stderr %q", code, errb.String())
	}
}
