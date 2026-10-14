package test

// Safety-rail tests for .polyforge/deploy.sh: mandatory --confirm, the
// server-side flock, and the automatic rollback with distinct exit codes.
// Same fake environment as deploy_image_test.go (stub ssh/docker/gh/scp on PATH);
// the "server" is a local temp tree, so the lock is a real flock on a real file.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func (d *deployWorld) composeText() string {
	b, _ := os.ReadFile(filepath.Join(d.w, "srv", "compose", "docker-compose.yml"))
	return string(b)
}

// assertNothingMutated: no server mutation, compose untouched, no lock/backup work.
func (d *deployWorld) assertNothingMutated(oldVer string) {
	d.t.Helper()
	if m := d.serverDockerMutations(); len(m) != 0 {
		d.t.Errorf("server docker was changed: %v", m)
	}
	if !strings.Contains(d.composeText(), "image: polyforge/aihub:"+oldVer) {
		d.t.Errorf("compose file changed:\n%s", d.composeText())
	}
	sshLog := d.sshLog()
	for _, bad := range []string{"flock", "docker cp", "rsync", "sed -i"} {
		if strings.Contains(sshLog, bad) {
			d.t.Errorf("mutating ssh call %q recorded:\n%s", bad, sshLog)
		}
	}
	if _, err := os.Stat(filepath.Join(d.w, "backups")); err == nil {
		d.t.Errorf("a local backup dir was created")
	}
}

// sshLog is the ssh call log without the read-only server tool preflight line
// (it names flock/rsync only to check that they exist).
func (d *deployWorld) sshLog() string {
	var keep []string
	for _, ln := range strings.Split(d.logOf("log-ssh"), "\n") {
		if !strings.HasPrefix(ln, "ssh for c in docker python3 rsync flock curl;") {
			keep = append(keep, ln)
		}
	}
	return strings.Join(keep, "\n")
}

func (d *deployWorld) lockFree() bool {
	return exec.Command("flock", "-n", filepath.Join(d.w, "srv", "deploy.lock"), "true").Run() == nil
}

func newReadyWorld(t *testing.T) *deployWorld {
	d := newDeployWorld(t, "v2.2.1")
	d.release("v2.2.1", true, digOld, digOld)
	d.release("v2.3.0", true, digNew, digNew)
	return d
}

func TestDeployConfirmMissingOrMismatchRefusedBeforeAnyWork(t *testing.T) {
	for name, args := range map[string][]string{
		"missing":      {"v2.3.0"},
		"mismatch":     {"v2.3.0", "--confirm", "v2.2.9"},
		"dry-mismatch": {"v2.3.0", "--dry-run", "--confirm", "v9.9.9"},
	} {
		t.Run(name, func(t *testing.T) {
			d := newReadyWorld(t)
			out, err := d.run("deploy\n", args...)
			if exitCode(err) != 1 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "--confirm") {
				t.Fatalf("want exit 1 + REFUSED, got %v\n%s", err, out)
			}
			if got := d.logOf("log-ssh") + d.logOf("log-gh") + d.logOf("log-docker"); got != "" {
				t.Errorf("network/tool work happened before the refusal:\n%s", got)
			}
			d.assertNothingMutated("v2.2.1")
		})
	}
}

func TestDeployDryRunNeedsNoConfirmAndTakesNoLock(t *testing.T) {
	d := newReadyWorld(t)
	out, err := d.run("", "v2.3.0", "--dry-run", "--confirm", "v2.3.0")
	if err != nil || !strings.Contains(out, "dry-run: steps 0-1 ran read-only") {
		t.Fatalf("dry-run with a matching --confirm failed: %v\n%s", err, out)
	}
	d.assertNothingMutated("v2.2.1")
	if strings.Contains(out, "deployed=") {
		t.Errorf("dry-run must not print a deployed= line\n%s", out)
	}
}

func TestDeployLockHeldFailsFastNothingMutated(t *testing.T) {
	d := newReadyWorld(t)
	lock := filepath.Join(d.w, "srv", "deploy.lock")
	holder := exec.Command("flock", "-n", lock, "sleep", "120")
	ownGroup(holder) // flock forks `sleep`: killing flock alone orphans it
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { killGroup(holder); _ = holder.Wait() }()
	for i := 0; i < 50 && d.lockFree(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if d.lockFree() {
		t.Fatal("could not hold the fake lock")
	}
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if exitCode(err) != 3 || !strings.Contains(out, "LOCK HELD") {
		t.Fatalf("want exit 3 + LOCK HELD, got %v\n%s", err, out)
	}
	if strings.Contains(out, "deployed=") {
		t.Errorf("unexpected deployed= line\n%s", out)
	}
	if m := d.serverDockerMutations(); len(m) != 0 {
		t.Errorf("server docker was changed: %v", m)
	}
	if strings.Contains(d.sshLog(), "docker cp") || strings.Contains(d.sshLog(), "rsync") || !strings.Contains(d.composeText(), "image: polyforge/aihub:v2.2.1") {
		t.Errorf("mutation recorded despite the held lock:\n%s", d.logOf("log-ssh"))
	}
	if _, err := os.Stat(filepath.Join(d.w, "backups")); err == nil {
		t.Errorf("a local backup dir was created")
	}
}

func TestDeployHappyPathHoldsAndReleasesLock(t *testing.T) {
	d := newReadyWorld(t)
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if err != nil {
		t.Fatalf("deploy failed: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := lines[len(lines)-1]; last != "deployed=v2.3.0 previous=v2.2.1" {
		t.Errorf("last line = %q", last)
	}
	if !strings.Contains(d.logOf("log-ssh"), "flock -n") {
		t.Errorf("no lock taken:\n%s", d.logOf("log-ssh"))
	}
	if !d.lockFree() {
		t.Errorf("the lock is still held after the script exited")
	}
}

func TestDeployVerificationFailureAutoRollsBack(t *testing.T) {
	d := newReadyWorld(t)
	d.env = append(d.env, "FAKE_BAD_IMG=polyforge/aihub:v2.3.0") // new image: container never reports healthy
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if exitCode(err) != 10 {
		t.Fatalf("want exit 10, got %v\n%s", err, out)
	}
	for _, want := range []string{"ROLLED BACK", "AUTOMATIC ROLLBACK to v2.2.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := lines[len(lines)-1]; last != "deployed=ROLLED_BACK previous=v2.2.1 attempted=v2.3.0" {
		t.Errorf("last line = %q", last)
	}
	if n := strings.Count(d.logOf("log-docker"), " up -d --no-deps aihub"); n != 2 {
		t.Errorf("want 2 compose ups (new, rollback), got %d\n%s", n, d.logOf("log-docker"))
	}
	if !strings.Contains(d.composeText(), "image: polyforge/aihub:v2.2.1") || strings.Contains(d.composeText(), "aihub:v2.3.0") {
		t.Errorf("compose not rolled back:\n%s", d.composeText())
	}
	if !strings.Contains(d.logOf("log-ssh"), "sed -i 's|image: polyforge/aihub:v2.3.0|image: polyforge/aihub:v2.2.1|'") {
		t.Errorf("no compose edit back to the old image:\n%s", d.logOf("log-ssh"))
	}
	if c, _ := os.ReadFile(filepath.Join(d.w, "dk-server", "container")); strings.TrimSpace(string(c)) != "polyforge/aihub:v2.2.1" {
		t.Errorf("container is not on the old image: %q", c)
	}
	if !d.lockFree() {
		t.Errorf("the lock is still held after the script exited")
	}
}

func TestDeploySwapUpFailureAutoRollsBack(t *testing.T) {
	d := newReadyWorld(t)
	d.env = append(d.env, "FAKE_FAIL_UP_IMG=polyforge/aihub:v2.3.0") // compose up of the new image fails
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if exitCode(err) != 10 || !strings.Contains(out, "ROLLED BACK") {
		t.Fatalf("want exit 10 + ROLLED BACK, got %v\n%s", err, out)
	}
	if !strings.Contains(d.composeText(), "image: polyforge/aihub:v2.2.1") {
		t.Errorf("compose not rolled back:\n%s", d.composeText())
	}
}

func TestDeployRollbackFailureIsLoudExit11(t *testing.T) {
	d := newReadyWorld(t)
	d.env = append(d.env, "FAKE_BAD_IMG=polyforge/aihub:v2.3.0", "FAKE_FAIL_UP_IMG=polyforge/aihub:v2.2.1")
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if exitCode(err) != 11 {
		t.Fatalf("want exit 11, got %v\n%s", err, out)
	}
	for _, want := range []string{"ROLLBACK FAILED", "ACT BY HAND", "docker compose -f", "docker stop aihub", "deployed=UNKNOWN previous=v2.2.1 attempted=v2.3.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "deployed=ROLLED_BACK") {
		t.Errorf("must not claim a rollback\n%s", out)
	}
}

// ---- review fixes: lock monitoring, server tools, log scan, dry-run, cleanup

func TestDeployLockSessionDyingBeforeSwapDiesNothingSwapped(t *testing.T) {
	d := newReadyWorld(t)
	// the lock ssh session dies while the NEW image is being loaded (step 6)
	d.env = append(d.env, "FAKE_KILL_LOCK_ON_LOAD=ghcr.io/silgrid/aihub:v2.3.0")
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if exitCode(err) != 1 || !strings.Contains(out, "deploy lock session died") {
		t.Fatalf("want exit 1 + lock session died, got %v\n%s", err, out)
	}
	if strings.Contains(out, "deployed=") {
		t.Errorf("unexpected deployed= line\n%s", out)
	}
	if n := strings.Count(d.logOf("log-docker"), " up -d --no-deps aihub"); n != 0 {
		t.Errorf("a compose up ran although the lock was gone (%d)\n%s", n, d.logOf("log-docker"))
	}
	if !strings.Contains(d.composeText(), "image: polyforge/aihub:v2.2.1") || strings.Contains(d.composeText(), "aihub:v2.3.0") {
		t.Errorf("compose was edited without the lock:\n%s", d.composeText())
	}
	if c, _ := os.ReadFile(filepath.Join(d.w, "dk-server", "container")); strings.Contains(string(c), "v2.3.0") {
		t.Errorf("container was recreated on the new image: %q", c)
	}
}

func TestDeployPreflightRequiresServerTools(t *testing.T) {
	for _, tool := range []string{"rsync", "flock", "python3"} {
		t.Run(tool, func(t *testing.T) {
			d := newReadyWorld(t)
			d.env = append(d.env, "FAKE_MISSING_TOOL="+tool)
			out, err := d.run("", "v2.3.0", "--dry-run") // read-only mode too
			if exitCode(err) != 1 || !strings.Contains(out, "missing on fakehost: "+tool) {
				t.Fatalf("want exit 1 + missing %s, got %v\n%s", tool, err, out)
			}
			d.assertNothingMutated("v2.2.1")
		})
	}
}

func TestDeployLogScanOnlyGoRuntimeSignatures(t *testing.T) {
	logs := func(t *testing.T, d *deployWorld, body string) {
		p := filepath.Join(d.w, "service.log")
		wf(t, p, body, 0o644)
		d.env = append(d.env, "FAKE_LOGS_FILE="+p)
	}
	t.Run("git proxy fatal is not a crash", func(t *testing.T) {
		d := newReadyWorld(t)
		logs(t, d, "2026/10/13 git proxy: fatal: the remote end hung up unexpectedly\nfatal: repository not found\nerror: some warning\n")
		out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
		if err != nil || !strings.Contains(out, "deployed=v2.3.0 previous=v2.2.1") {
			t.Fatalf("a git fatal: line must not fail the deploy: %v\n%s", err, out)
		}
		if strings.Contains(out, "ROLLED BACK") {
			t.Errorf("rolled back on a git fatal: line\n%s", out)
		}
	})
	for name, line := range map[string]string{
		"panic":       "panic: runtime error: index out of range",
		"fatal error": "fatal error: all goroutines are asleep - deadlock!",
		"goroutine":   "goroutine 17 [running]:",
	} {
		t.Run(name+" rolls back", func(t *testing.T) {
			d := newReadyWorld(t)
			logs(t, d, "starting\n"+line+"\n")
			out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
			if exitCode(err) != 10 || !strings.Contains(out, "ROLLED BACK") {
				t.Fatalf("want exit 10 + ROLLED BACK, got %v\n%s", err, out)
			}
		})
	}
}

func TestDeployDryRunComposeEditFailureIsFatal(t *testing.T) {
	d := newReadyWorld(t)
	// the compose file validates but has no "  aihub:" service block to edit
	wf(t, filepath.Join(d.w, "srv", "compose", "docker-compose.yml"), "services:\n  web:\n    image: busybox\n", 0o644)
	out, err := d.run("", "v2.3.0", "--dry-run")
	if exitCode(err) != 1 || !strings.Contains(out, "cannot compute the compose edit") {
		t.Fatalf("want exit 1 + compose edit failure, got %v\n%s", err, out)
	}
	if strings.Contains(out, "Nothing was changed.") && strings.Contains(out, "dry-run: steps 0-1 ran") {
		t.Errorf("the dry-run claimed success\n%s", out)
	}
}

func TestDeployTarSpaceCheckedInPreflight(t *testing.T) {
	d := newReadyWorld(t)
	d.env = append(d.env, "FAKE_IMAGE_SIZE=999999999999999")
	out, err := d.run("", "v2.3.0", "--dry-run")
	if exitCode(err) != 1 || !strings.Contains(out, "not enough free space in") || !strings.Contains(out, "docker save tar") {
		t.Fatalf("want exit 1 + tar space refusal, got %v\n%s", err, out)
	}
	d.assertNothingMutated("v2.2.1")
}

func TestDeployUnexpectedExitAfterSwapRollsBackAndCleansUp(t *testing.T) {
	d := newReadyWorld(t)
	d.env = append(d.env, "FAKE_TERM_ON_LOGS=1") // SIGTERM reaches the script in step 7
	cmd := exec.Command("sh", filepath.Join("..", ".polyforge", "deploy.sh"), "v2.3.0", "--confirm", "v2.3.0")
	cmd.Env = d.env
	var b strings.Builder
	cmd.Stdout, cmd.Stderr = &b, &b
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wf(t, filepath.Join(d.w, "deploypid"), fmt.Sprint(cmd.Process.Pid), 0o644)
	err := cmd.Wait()
	out := b.String()
	if exitCode(err) != 10 || !strings.Contains(out, "unexpected exit") || !strings.Contains(out, "ROLLED BACK") {
		t.Fatalf("want exit 10 + rollback after the signal, got %v\n%s", err, out)
	}
	if !strings.Contains(d.composeText(), "image: polyforge/aihub:v2.2.1") {
		t.Errorf("compose not rolled back:\n%s", d.composeText())
	}
	if left, _ := filepath.Glob(filepath.Join(d.w, "pf-deploy.*")); len(left) != 0 {
		t.Errorf("the work dir leaked: %v", left)
	}
	if !d.lockFree() {
		t.Errorf("the lock is still held after the script exited")
	}
}
