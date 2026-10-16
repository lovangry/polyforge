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
	"strconv"
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
		"while :; do", "rc=$?", "if [ \"$rc\" -eq 0 ]", "break", "sleep 5", "POLYFORGE_SUPERVISOR='tmux:polyforge-daemon'",
		"unset TMUX TMUX_PANE PF_CHAT_ID PF_WI_ID",
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

// TestEnsureLoopRestartsFailuresAndEndsOnCleanExit runs the real loop
// script (no tmux) around a fake daemon that fails once, then exits 0:
// the failure is restarted after the pause, the clean exit ends the loop;
// every inherited CLAUDE* and the step/chat markers are gone from the
// daemon's environment.
func TestEnsureLoopRestartsFailuresAndEndsOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "pf")
	body := "#!/bin/sh\nn=$(cat " + dir + "/n 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + dir + "/n\nenv > " + dir + "/env.$n\n[ $n -ge 2 ] && exit 0\nexit 3\n"
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &ensurePlan{pfBin: fake, configPath: "/c", dataDir: dir, socket: dir + "/s", workflowsDir: dir, machineID: "m-x", home: dir}
	cmd := exec.Command("sh", "-c", p.loopScript())
	cmd.Env = append(os.Environ(), "CLAUDECODE=1", "CLAUDE_CODE_SSE_PORT=1", "CLAUDE_ANYTHING=x", "PF_WI_ID=wi_1", "PF_CHAT_ID=chat_1", "TMUX=/x,1,0")
	start := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("loop: %v %s", err, out)
	}
	if took := time.Since(start); took < 4*time.Second || took > 15*time.Second {
		t.Fatalf("loop took %s: want one ~5s restart pause, then the end on the clean exit", took)
	}
	if n, _ := os.ReadFile(filepath.Join(dir, "n")); strings.TrimSpace(string(n)) != "2" {
		t.Fatalf("daemon runs = %q, want 2 (failure restarted, clean exit final)", n)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env.1"))
	for _, banned := range []string{"CLAUDE", "PF_WI_ID=", "PF_CHAT_ID=", "TMUX="} {
		if strings.Contains(string(env), banned) {
			t.Fatalf("the daemon inherited %q:\n%s", banned, env)
		}
	}
	if !strings.Contains(string(env), "POLYFORGE_SUPERVISOR=tmux:polyforge-daemon") {
		t.Fatal("the supervision marker is missing")
	}
	log, _ := os.ReadFile(filepath.Join(dir, "daemon.log"))
	if !strings.Contains(string(log), "failed (exit 3); restarting") || !strings.Contains(string(log), "exited cleanly; the loop ends") {
		t.Fatalf("loop log:\n%s", log)
	}
}

// repo review 2026-10-16 B-14: a daemon that refuses to start for a
// cause only an operator can fix (ExitNeedsOperator) ends the loop
// instead of restarting into the same failure every few seconds.
func TestEnsureLoopEndsOnNeedsOperatorExit(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "pf")
	body := "#!/bin/sh\nn=$(cat " + dir + "/n 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + dir + "/n\nexit " + strconv.Itoa(ExitNeedsOperator) + "\n"
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &ensurePlan{pfBin: fake, configPath: "/c", dataDir: dir, socket: dir + "/s", workflowsDir: dir, machineID: "m-x", home: dir}
	cmd := exec.Command("sh", "-c", p.loopScript())
	done := make(chan error, 1)
	go func() { _, err := cmd.CombinedOutput(); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("loop: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("the loop restarted a needs-operator refusal instead of ending")
	}
	if n, _ := os.ReadFile(filepath.Join(dir, "n")); strings.TrimSpace(string(n)) != "1" {
		t.Fatalf("daemon runs = %q, want 1", n)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "daemon.log"))
	if !strings.Contains(string(log), "needs an operator fix") {
		t.Fatalf("loop log:\n%s", log)
	}
}

func TestEnsureRefusedInStepAgent(t *testing.T) {
	t.Setenv("PF_WI_ID", "wi_step")
	var out, errb bytes.Buffer
	if code := Main([]string{"ensure"}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "step agent") {
		t.Fatalf("ensure in a step agent: exit %d stderr %q, want 1 refusing", code, errb.String())
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

// TestEnsureArgvIgnoresDaemonTable (decision 2026-10-16 G2): the
// daemon reads the cli.toml [daemon] table itself, so ensure's argv is
// the fixed canonical one with or without the table; ensure still
// validates the file and refuses an invalid table naming the key.
func TestEnsureArgvIgnoresDaemonTable(t *testing.T) {
	t.Setenv("POLYFORGE_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	withTable := filepath.Join(dir, "table.toml")
	body := "server = \"http://127.0.0.1:1\"\n\n[daemon]\nmax_concurrent_jobs = 6\n" +
		"harness_root = \"/srv/harness\"\nstep_timeout = \"1h30m\"\n"
	if err := os.WriteFile(withTable, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := planEnsure(withTable, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"daemon", "--config", p.configPath, "-machine-id", p.machineID,
		"-data-dir", p.dataDir, "-socket", p.socket, "-workflows-dir", p.workflowsDir}
	if got := p.daemonArgv(); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv with [daemon]:\n got %q\nwant %q", got, want)
	}

	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[daemon]\nstep_timeout = \"soon\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Main([]string{"ensure", "--config", bad, "--timeout", "1s"}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "[daemon] step_timeout") {
		t.Fatalf("invalid [daemon]: exit %d stderr %q, want 1 naming the key", code, errb.String())
	}
	// and the daemon itself refuses the same file at boot (exit 78)
	errb.Reset()
	if code := Main([]string{"--config", bad}, nil, &out, &errb); code != ExitNeedsOperator || !strings.Contains(errb.String(), "[daemon] step_timeout") {
		t.Fatalf("daemon on an invalid [daemon]: exit %d stderr %q", code, errb.String())
	}
}
