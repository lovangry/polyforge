package test

// w174 (polyforge#174): tools/install.sh, run hermetically end to end.
//
// Nothing here reaches the real machine: PF_BIN installs into a scratch
// dir (never /usr/local/bin), HOME is scratch (pi settings, Claude
// settings and skills, the data dir, the config), tmux is a private
// server (TMUX_TMPDIR scratch, TMUX unset), gh is a stub serving a fake
// release, and PATH carries no real pf/pi/claude. The "released" pf is a
// wrapper that journals every invocation and execs the real test build,
// and tmux is a shim that journals and execs the real tmux — so one
// journal records the order install.sh drives them in.
//
// Pinned: the old layout (pi git package entry, copied Claude skill dir)
// migrates to the new one (pi local package naming release/current,
// Claude skill symlink); a second run changes nothing; an upgrade stops
// the restart loop FIRST, then shuts the daemon down gracefully, then
// starts the new daemon through `pf daemon ensure`; pi is never invoked.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// w174Install is one hermetic installer machine on top of w174Env.
type w174Install struct {
	*w174Env
	shim     string // gh, tmux, pi shims (first on PATH)
	rel      string // the fake release assets the gh stub serves
	src      string // the release tree `gh repo clone` copies
	bin      string // PF_BIN
	journal  string
	tagFile  string
	extraEnv []string
}

func w174NewInstall(t *testing.T) *w174Install {
	t.Helper()
	e := w174NewEnv(t, true)
	// install.sh builds the release layout itself
	if err := os.RemoveAll(filepath.Join(e.data, "release")); err != nil {
		t.Fatal(err)
	}
	in := &w174Install{w174Env: e, shim: t.TempDir(), rel: t.TempDir(), src: t.TempDir(),
		bin: filepath.Join(e.home, "bin", "pf"), journal: filepath.Join(t.TempDir(), "journal"), tagFile: filepath.Join(t.TempDir(), "tag")}
	root := v1Root(t)
	cp := exec.Command("cp", "-R", filepath.Join(root, "skills"), filepath.Join(root, "workflows"), filepath.Join(root, "package.json"), in.src+"/")
	if out, err := cp.CombinedOutput(); err != nil {
		t.Fatalf("release tree: %v %s", err, out)
	}
	gh := `#!/bin/sh
# w174 stub gh: a local fake release; the tag lives in $W174_TAG_FILE
case "$1 $2" in
"release list") cat "$W174_TAG_FILE"; echo ;;  # what --jq '.[0].tagName' prints
"release download")
  dir=; prev=
  for a in "$@"; do [ "$prev" = "--dir" ] && dir=$a; prev=$a; done
  cp "$W174_REL/pf-linux-amd64" "$W174_REL/SHA256SUMS" "$dir/" ;;
"repo clone") mkdir -p "$4" && cp -R "$W174_SRC"/. "$4"/ ;;
esac
exit 0
`
	tmuxReal, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	files := map[string]string{
		"gh":   gh,
		"tmux": "#!/bin/sh\necho \"tmux $*\" >> \"$W174_JOURNAL\"\nexec " + tmuxReal + " \"$@\"\n",
		"pi":   "#!/bin/sh\necho \"pi $*\" >> \"$W174_JOURNAL\"\nexit 1\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(in.shim, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// no real pf / pi / claude on PATH: the shims, the system tools, python3
	in.path = in.shim + ":/usr/bin:/bin"
	if py, err := exec.LookPath("python3"); err == nil {
		in.path += ":" + filepath.Dir(py)
	}
	// Claude Code and pi are "set up" in this HOME
	for _, d := range []string{filepath.Join(e.home, ".claude"), filepath.Join(e.home, ".pi", "agent")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

// release publishes tag: a pf wrapper reporting tag, journaling every
// call and exec'ing the real build.
func (in *w174Install) release(t *testing.T, tag string) {
	t.Helper()
	// With W174_ENSURE_DURING_DRAIN=1, `ops shutdown` also plays a harness
	// session starting mid-upgrade: once the old daemon's socket is gone
	// it runs `pf daemon ensure` and journals its exit code.
	wrapper := fmt.Sprintf(`#!/bin/sh
echo "pf $*" >> "$W174_JOURNAL"
[ "$1" = --version ] && { echo %[1]s; exit 0; }
if [ "$1 $2" = "ops shutdown" ] && [ "${W174_ENSURE_DURING_DRAIN:-}" = 1 ]; then
  %[2]s "$@"; rc=$?
  n=0
  while curl -s -m 1 --unix-socket "$W174_SOCK" http://x/status >/dev/null 2>&1 && [ $n -lt 100 ]; do sleep 0.2; n=$((n+1)); done
  out=$(%[2]s daemon ensure --config "$W174_CFG" --timeout 3s 2>&1); erc=$?
  echo "drain-ensure rc=$erc $out" | tr '\n' ' ' >> "$W174_JOURNAL"; echo >> "$W174_JOURNAL"
  exit $rc
fi
exec %[2]s "$@"
`, tag, in.pf)
	if err := os.WriteFile(filepath.Join(in.rel, "pf-linux-amd64"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(wrapper))
	if err := os.WriteFile(filepath.Join(in.rel, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  pf-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.tagFile, []byte(tag), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run executes tools/install.sh once; answers (output, exit code).
func (in *w174Install) run(t *testing.T) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(v1Root(t), "tools", "install.sh"), "w174key", "127.0.0.1")
	cmd.Env = append(in.env(), "PF_BIN="+in.bin, "W174_REL="+in.rel, "W174_SRC="+in.src,
		"W174_JOURNAL="+in.journal, "W174_TAG_FILE="+in.tagFile, "W174_SOCK="+in.sock, "W174_CFG="+in.cfg)
	cmd.Env = append(cmd.Env, in.extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("install.sh: %v (%s)", err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

func (in *w174Install) journalLines() []string {
	raw, _ := os.ReadFile(in.journal)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func (in *w174Install) resetJournal(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(in.journal, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// journalIndex answers the first journal line index starting with prefix (-1 if none).
func journalIndex(lines []string, prefix string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

// loops counts the restart-loop sessions (exact name) on the private server.
func (in *w174Install) loops() int {
	n := 0
	for _, s := range in.sessions() {
		if s == "polyforge-daemon" {
			n++
		}
	}
	return n
}

func w174ReadFile(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestW174InstallMigratesOldLayoutThenIsIdempotent: a machine with the
// pre-#174 layout (pi git package entry pinned to a version, a copied
// Claude skill dir) gets the local package entry and the symlink, other
// pi packages untouched, the daemon started under the loop by ensure;
// a second run changes nothing.
func TestW174InstallMigratesOldLayoutThenIsIdempotent(t *testing.T) {
	in := w174NewInstall(t)
	in.release(t, "v0.0.1-w174")
	piSettings := filepath.Join(in.home, ".pi", "agent", "settings.json")
	// another polyforge checkout also listed (a dev clone): install warns
	devClone := filepath.Join(t.TempDir(), "dev-polyforge")
	if err := os.MkdirAll(filepath.Join(devClone, "skills", "polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devClone, "skills", "polyforge", "SKILL.md"), []byte("# dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPi := `{"defaultModel":"x","packages":["npm:other-pkg",{"source":"git:git@github.com:silgrid/polyforge.git@v2.7.0","prompts":[]},{"source":"git:github.com/someone/else"},"` + devClone + `"]}`
	if err := os.WriteFile(piSettings, []byte(oldPi), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSkill := filepath.Join(in.home, ".claude", "skills", "polyforge")
	if err := os.MkdirAll(oldSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldSkill, "SKILL.md"), []byte("# old copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := in.run(t)
	if code != 0 {
		t.Fatalf("install.sh: exit %d\n%s", code, out)
	}
	relCur := filepath.Join(in.data, "release", "current")
	// pi: the git entry replaced in place by the local release, others kept
	var pi struct {
		DefaultModel string `json:"defaultModel"`
		Packages     []any  `json:"packages"`
	}
	if err := json.Unmarshal([]byte(w174ReadFile(t, piSettings)), &pi); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(pi.Packages)
	want, _ := json.Marshal([]any{"npm:other-pkg", map[string]any{"source": relCur, "prompts": []any{}}, map[string]any{"source": "git:github.com/someone/else"}, devClone})
	if string(got) != string(want) || pi.DefaultModel != "x" {
		t.Fatalf("pi settings after migration: packages %s (want %s), defaultModel %q", got, want, pi.DefaultModel)
	}
	if baks, _ := filepath.Glob(piSettings + ".bak-polyforge-*"); len(baks) != 1 {
		t.Fatalf("pi settings backups = %v, want one", baks)
	}
	if !strings.Contains(out, "WARN: pi also loads another polyforge checkout ("+devClone+")") {
		t.Fatalf("no warning about the second polyforge checkout:\n%s", out)
	}
	// Claude: the copied dir replaced by a symlink, the hook registered on the stable path
	if target, err := os.Readlink(oldSkill); err != nil || target != filepath.Join(relCur, "skills", "polyforge") {
		t.Fatalf("claude skill is not the release symlink: %q %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(oldSkill, "SKILL.md")); err != nil {
		t.Fatalf("the symlinked skill does not resolve: %v", err)
	}
	if strings.Contains(w174ReadFile(t, filepath.Join(oldSkill, "SKILL.md")), "old copy") {
		t.Fatal("the old copied skill is still in place")
	}
	if left, _ := filepath.Glob(filepath.Join(in.home, ".claude", "skills", "*")); len(left) != 1 {
		t.Fatalf("skills dir = %v, want only the polyforge link (no leftover copy)", left)
	}
	hook := filepath.Join(oldSkill, "hooks", "claude-session-start.sh")
	if !strings.Contains(w174ReadFile(t, filepath.Join(in.home, ".claude", "settings.json")), hook) {
		t.Fatal("the SessionStart hook is not registered on the stable skill path")
	}
	// daemon: started by ensure under the loop
	lines := in.journalLines()
	if journalIndex(lines, "pf daemon ensure") < 0 {
		t.Fatalf("install.sh did not start the daemon through `pf daemon ensure`; journal:\n%s", strings.Join(lines, "\n"))
	}
	if i := journalIndex(lines, "pi "); i >= 0 {
		t.Fatalf("install.sh invoked pi (%q): the package is a settings entry now", lines[i])
	}
	if s := in.sessions(); len(s) != 1 || s[0] != "polyforge-daemon" {
		t.Fatalf("tmux sessions = %v", s)
	}
	pid := in.oneDaemon(t, 10*time.Second)

	// second run: nothing changes
	snap := map[string]string{}
	for _, p := range []string{piSettings, filepath.Join(in.home, ".claude", "settings.json"), in.cfg} {
		snap[p] = w174ReadFile(t, p)
	}
	in.resetJournal(t)
	out2, code := in.run(t)
	if code != 0 {
		t.Fatalf("second install.sh: exit %d\n%s", code, out2)
	}
	for p, before := range snap {
		if after := w174ReadFile(t, p); after != before {
			t.Fatalf("second run changed %s:\nbefore %s\nafter  %s", p, before, after)
		}
	}
	if baks, _ := filepath.Glob(piSettings + ".bak-polyforge-*"); len(baks) != 1 {
		t.Fatalf("second run wrote another pi settings backup: %v", baks)
	}
	if target, _ := os.Readlink(oldSkill); target != filepath.Join(relCur, "skills", "polyforge") {
		t.Fatalf("second run moved the skill link: %q", target)
	}
	for _, want := range []string{"pf v0.0.1-w174 already current", "release checkout v0.0.1-w174 present", "claude skill -> release/current already", "pi package -> release/current already", "SessionStart hook already registered", "daemon already running"} {
		if !strings.Contains(out2, want) {
			t.Fatalf("second run output lacks %q:\n%s", want, out2)
		}
	}
	lines = in.journalLines()
	for _, banned := range []string{"tmux kill-session", "tmux new-session", "pf ops shutdown"} {
		if i := journalIndex(lines, banned); i >= 0 {
			t.Fatalf("second run ran %q:\n%s", lines[i], strings.Join(lines, "\n"))
		}
	}
	if again := in.oneDaemon(t, 2*time.Second); again != pid {
		t.Fatalf("second run replaced the daemon: %d -> %d", pid, again)
	}
}

// TestW174InstallUpgradeOrderAndLock: an upgrade shuts the daemon down
// gracefully (its loop ends with the clean exit), swaps, and the new
// daemon comes from `pf daemon ensure`; a harness session's ensure during
// the drain starts nothing (install.sh holds the ensure lock); unrelated
// tmux sessions — including a similarly named one — survive.
func TestW174InstallUpgradeOrderAndLock(t *testing.T) {
	in := w174NewInstall(t)
	in.release(t, "v0.0.1-w174")
	if out, code := in.run(t); code != 0 {
		t.Fatalf("install v0.0.1: exit %d\n%s", code, out)
	}
	oldPid := in.oneDaemon(t, 10*time.Second)
	for _, name := range []string{"piweb", "polyforge-daemon2"} {
		if out, err := in.tmux("new-session", "-d", "-s", name, "sleep 600"); err != nil {
			t.Fatalf("tmux new-session %s: %v %s", name, err, out)
		}
	}

	in.release(t, "v0.0.2-w174")
	in.resetJournal(t)
	in.extraEnv = []string{"W174_ENSURE_DURING_DRAIN=1"}
	out, code := in.run(t)
	if code != 0 {
		t.Fatalf("upgrade to v0.0.2: exit %d\n%s", code, out)
	}
	lines := in.journalLines()
	shut := journalIndex(lines, "pf ops shutdown")
	drain := journalIndex(lines, "drain-ensure ")
	ensure := journalIndex(lines, "pf daemon ensure --config")
	if shut < 0 || drain < 0 || ensure < 0 || !(shut < drain && drain < ensure) {
		t.Fatalf("upgrade order must be: graceful shutdown (%d) < [mid-drain ensure (%d)] < install's ensure (%d); journal:\n%s", shut, drain, ensure, strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[drain], "drain-ensure rc=1 ") || !strings.Contains(lines[drain], "another `pf daemon ensure`") {
		t.Fatalf("an ensure during the drain must wait on install's lock and start nothing: %q", lines[drain])
	}
	if i := journalIndex(lines, "tmux kill-session"); i >= 0 {
		t.Fatalf("the loop should have ended with the clean shutdown, yet install killed a session: %q", lines[i])
	}
	if got := w174ReadFile(t, in.bin); !strings.Contains(got, "v0.0.2-w174") {
		t.Fatal("the binary was not swapped")
	}
	if cur, _ := os.Readlink(filepath.Join(in.data, "release", "current")); cur != "v0.0.2-w174" {
		t.Fatalf("release/current -> %q", cur)
	}
	newPid := in.oneDaemon(t, 10*time.Second)
	if newPid == oldPid {
		t.Fatal("the old daemon survived the upgrade")
	}
	if in.loops() != 1 {
		t.Fatalf("restart loops after the upgrade = %d (sessions %v), want 1", in.loops(), in.sessions())
	}
	// unrelated sessions survive install AND a later ensure
	if code, o, e := in.ensure(t); code != 0 {
		t.Fatalf("ensure after the upgrade: %d %s %s", code, o, e)
	}
	got := strings.Join(in.sessions(), " ")
	for _, name := range []string{"piweb", "polyforge-daemon2"} {
		if !strings.Contains(" "+got+" ", " "+name+" ") {
			t.Fatalf("unrelated tmux session %s did not survive (sessions %q)", name, got)
		}
	}
}

// TestW174InstallUpgradeFromUnsupervisedNohupDaemon: today's machines run
// a bare `nohup pf daemon` and no loop; the upgrade stops it gracefully
// and ends with exactly one loop and one (new) daemon.
func TestW174InstallUpgradeFromUnsupervisedNohupDaemon(t *testing.T) {
	in := w174NewInstall(t)
	in.release(t, "v0.0.1-w174")
	if out, code := in.run(t); code != 0 {
		t.Fatalf("install v0.0.1: exit %d\n%s", code, out)
	}
	// back to today's state: stop the loop's daemon, start a bare nohup one
	stop := exec.Command(in.pf, "--config", in.cfg, "ops", "shutdown")
	stop.Env = in.env()
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("ops shutdown: %v %s", err, out)
	}
	waitUntil(t, 30*time.Second, "the loop's daemon did not end", func() bool { return len(in.daemons()) == 0 && in.loops() == 0 })
	id := strings.TrimSpace(w174ReadFile(t, filepath.Join(in.data, "daemon-id")))
	nohup := exec.Command("sh", "-c", "nohup \"$0\" daemon --config \"$1\" -machine-id \"$2\" -data-dir \"$3\" -socket \"$4\" -workflows-dir \"$3/release/current/workflows\" >/dev/null 2>&1 &",
		in.pf, in.cfg, id, in.data, in.sock)
	nohup.Env = in.env()
	if out, err := nohup.CombinedOutput(); err != nil {
		t.Fatalf("nohup daemon: %v %s", err, out)
	}
	oldPid := in.oneDaemon(t, 10*time.Second)
	if in.loops() != 0 {
		t.Fatal("fixture: a loop exists")
	}

	in.release(t, "v0.0.2-w174")
	in.resetJournal(t)
	out, code := in.run(t)
	if code != 0 {
		t.Fatalf("upgrade: exit %d\n%s", code, out)
	}
	newPid := in.oneDaemon(t, 10*time.Second)
	if newPid == oldPid {
		t.Fatal("the nohup daemon survived the upgrade")
	}
	if in.loops() != 1 {
		t.Fatalf("restart loops = %d (sessions %v), want 1", in.loops(), in.sessions())
	}
	environ, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(newPid), "environ"))
	if !strings.Contains(string(environ), "POLYFORGE_SUPERVISOR=tmux:polyforge-daemon") {
		t.Fatal("the new daemon is not the loop's")
	}
}
