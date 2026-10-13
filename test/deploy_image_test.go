package test

// Fake-environment tests for .polyforge/deploy.sh's image-pull swap: the
// deployer machine pulls ghcr.io/silgrid/aihub:<tag>, verifies the repo
// digest against the release's aihub-image.digest, then ships the image to
// the server with docker save | ssh docker load. Everything external is a
// stub on PATH (gh, docker, ssh, scp, curl): the "server" is a local temp
// tree reached through the ssh stub, the registry is a directory. Nothing
// touches a real host, registry or GitHub.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fakeDocker = `#!/bin/sh
# fake docker. FAKE_SIDE=server (set by the ssh stub) or local.
side="${FAKE_SIDE:-local}"
S="$FAKE_W/dk-$side"; mkdir -p "$S/img"
echo "$side docker $*" >> "$FAKE_W/log-docker"
key() { printf '%s' "$1" | tr '/:@' '___'; }
fmt=""
case "$1" in
  login) cat >/dev/null; echo "docker login $*" >> "$FAKE_W/log-login"; exit 0 ;;
  pull)
    r="$FAKE_W/registry/$(key "$2")"
    [ -f "$r" ] || { echo "manifest unknown" >&2; exit 1; }
    cp "$r" "$S/img/$(key "$2")"; echo "pulled $2"; exit 0 ;;
  image)
    shift; [ "$1" = inspect ] || exit 1; shift
    if [ "$1" = "-f" ]; then fmt="$2"; shift 2; fi
    f="$S/img/$(key "$1")"; [ -f "$f" ] || exit 1
    id=$(sed -n 1p "$f"); dg=$(sed -n 3p "$f")
    case "$fmt" in
      '{{.Id}}') echo "$id" ;;
      '{{range .RepoDigests}}{{println .}}{{end}}') [ -n "$dg" ] && echo "$dg" ;;
      '{{.RepoDigests}}') echo "[$dg]" ;;
    esac
    exit 0 ;;
  run)
    shift; ep=""; img=""
    while [ $# -gt 0 ]; do case "$1" in --rm) shift ;; --entrypoint) ep="$2"; shift 2 ;; *) img="$1"; shift; break ;; esac; done
    f="$S/img/$(key "$img")"; [ -f "$f" ] || exit 1
    case "$ep" in /aihub) echo "aihub $(sed -n 2p "$f")" ;; git|curl) echo "$ep version 1" ;; esac
    exit 0 ;;
  save) shift; [ "$1" = -o ] && { cp "$S/img/$(key "$3")" "$2.body"; { echo "$3"; cat "$2.body"; } > "$2"; rm -f "$2.body"; }; exit 0 ;;
  load) ref=$(head -1 /dev/stdin >/dev/null 2>&1; true); in="$(cat)"; ref=$(printf '%s\n' "$in" | sed -n 1p)
        printf '%s\n' "$in" | sed 1d > "$S/img/$(key "$ref")"; echo "Loaded image: $ref"; exit 0 ;;
  tag) cp "$S/img/$(key "$2")" "$S/img/$(key "$3")"; exit 0 ;;
  rmi) rm -f "$S/img/$(key "$2")"; exit 0 ;;
  images) for f in "$S/img/"*; do [ -e "$f" ] && basename "$f"; done; exit 0 ;;
  inspect)
    c="$S/container"; [ -f "$c" ] || exit 1
    case "$3" in
      '{{.State.Status}}') echo running ;; '{{.State.Running}}') echo true ;;
      '{{.Config.Image}}') cat "$c" ;;
    esac; exit 0 ;;
  exec)
    c="$S/container"; f="$S/img/$(key "$(cat "$c")")"; echo "aihub $(sed -n 2p "$f")"; exit 0 ;;
  cp)
    c="$S/container"; v=$(sed -n 2p "$S/img/$(key "$(cat "$c")")")
    dest="${3}"; printf '#!/bin/sh\necho "aihub %s"\n' "$v" > "$dest"; exit 0 ;;
  logs) exit 0 ;;
  build)
    shift; t=""; while [ $# -gt 0 ]; do case "$1" in -t) t="$2"; shift 2 ;; *) ctx="$1"; shift ;; esac; done
    v=$(sh "$ctx/aihub" -version | awk '{print $NF}')
    printf 'sha256:built%s\n%s\n\n' "$v" "$v" > "$S/img/$(key "$t")"; exit 0 ;;
  compose)
    shift; f=""; while [ $# -gt 0 ]; do case "$1" in -f) f="$2"; shift 2 ;; -p) shift 2 ;; config) [ -f "$f" ]; exit $? ;; up) break ;; *) shift ;; esac; done
    img=$(sed -n 's/^    image: //p' "$f" | head -1); printf '%s\n' "$img" > "$S/container"; exit 0 ;;
esac
echo "fake docker: unhandled: $*" >&2; exit 99
`

const fakeSSH = `#!/bin/sh
while [ $# -gt 0 ]; do case "$1" in -o) shift 2 ;; *) break ;; esac; done
shift   # host
export FAKE_SIDE=server
echo "ssh $*" >> "$FAKE_W/log-ssh"
exec sh -c "$*"
`

const fakeSCP = `#!/bin/sh
while [ $# -gt 0 ]; do case "$1" in -o) shift 2 ;; *) break ;; esac; done
for last; do :; done
dst="${last#*:}"; n=$#; i=0; srcs=""
for a; do i=$((i+1)); [ $i -lt $n ] && srcs="$srcs $a"; done
echo "scp$srcs -> $dst" >> "$FAKE_W/log-ssh"
cp $srcs "$dst"
`

const fakeGH = `#!/bin/sh
echo "gh $*" >> "$FAKE_W/log-gh"
case "$1 $2" in
  "release download")
    tag="$3"; shift 3; dir=""; pats=""
    while [ $# -gt 0 ]; do case "$1" in -R) shift 2 ;; -D) dir="$2"; shift 2 ;; -p) pats="$pats $2"; shift 2 ;; *) shift ;; esac; done
    [ -d "$FAKE_W/ghrel/$tag" ] || { echo "release not found" >&2; exit 1; }
    for p in $pats; do [ -f "$FAKE_W/ghrel/$tag/$p" ] && cp "$FAKE_W/ghrel/$tag/$p" "$dir/$p"; done; exit 0 ;;
  "release view")
    [ -d "$FAKE_W/ghrel/$3" ] || { echo "release not found" >&2; exit 1; }
    ls "$FAKE_W/ghrel/$3"; exit 0 ;;
  "auth token") echo faketoken; exit 0 ;;
  "api user") echo fakeuser; exit 0 ;;
esac
echo "fake gh: unhandled: $*" >&2; exit 99
`

const fakeCurl = "#!/bin/sh\necho 200\n"

type deployWorld struct {
	t    *testing.T
	w    string
	path string
	env  []string
}

func wf(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// release lays one fake GitHub release (+ optional GHCR image) into the world.
// digestInRelease is what aihub-image.digest says; registryDigest is what the
// registry's image really carries (differ to simulate tampering).
func (d *deployWorld) release(tag string, withImage bool, digestInRelease, registryDigest string) {
	t := d.t
	rel := filepath.Join(d.w, "ghrel", tag)
	wf(t, filepath.Join(rel, "aihub-linux-amd64"), "bin-aihub-"+tag+"\n", 0o644)
	wf(t, filepath.Join(rel, "pf-linux-amd64"), "bin-pf-"+tag+"\n", 0o644)
	files := []string{"aihub-linux-amd64", "pf-linux-amd64"}
	if withImage {
		wf(t, filepath.Join(rel, "aihub-image.digest"), digestInRelease+"\n", 0o644)
		files = append(files, "aihub-image.digest")
		wf(t, filepath.Join(d.w, "registry", "ghcr.io_silgrid_aihub_"+tag),
			fmt.Sprintf("sha256:id%s\n%s\n%s\n", tag, tag, registryDigest), 0o644)
	}
	cmd := exec.Command("sha256sum", files...)
	cmd.Dir = rel
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	wf(t, filepath.Join(rel, "SHA256SUMS"), string(out), 0o644)
}

func newDeployWorld(t *testing.T, oldVer string) *deployWorld {
	t.Helper()
	for _, tool := range []string{"python3", "rsync", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	w := t.TempDir()
	d := &deployWorld{t: t, w: w}
	bin := filepath.Join(w, "bin")
	wf(t, filepath.Join(bin, "docker"), fakeDocker, 0o755)
	wf(t, filepath.Join(bin, "ssh"), fakeSSH, 0o755)
	wf(t, filepath.Join(bin, "scp"), fakeSCP, 0o755)
	wf(t, filepath.Join(bin, "gh"), fakeGH, 0o755)
	wf(t, filepath.Join(bin, "curl"), fakeCurl, 0o755)
	// the "server": running container on the old image, a compose file, a real sqlite db
	sdk := filepath.Join(w, "dk-server")
	wf(t, filepath.Join(sdk, "img", "aihub-app_aihub"), fmt.Sprintf("sha256:old%s\n%s\n\n", oldVer, oldVer), 0o644)
	wf(t, filepath.Join(sdk, "container"), "aihub-app_aihub\n", 0o644)
	data := filepath.Join(w, "srv", "data", "aihub")
	if err := os.MkdirAll(filepath.Join(data, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	py := `import sqlite3,sys
c=sqlite3.connect(sys.argv[1])
for t in "work_items memories projects users api_keys machines".split(): c.execute("create table %s(id integer)"%t)
c.commit()`
	if out, err := exec.Command("python3", "-I", "-c", py, filepath.Join(data, "aihub.db")).CombinedOutput(); err != nil {
		t.Fatalf("sqlite fixture: %v\n%s", err, out)
	}
	compose := filepath.Join(w, "srv", "compose", "docker-compose.yml")
	wf(t, compose, "services:\n  aihub:\n    image: polyforge/aihub:"+oldVer+"\n    ports:\n      - \"8080:8080\"\n  other:\n    image: busybox\n", 0o644)
	d.path = bin + ":" + os.Getenv("PATH")
	d.env = append(os.Environ(),
		"PATH="+d.path, "FAKE_W="+w, "TMPDIR="+w,
		"HOME="+filepath.Join(w, "home"), "DOCKER_CONFIG="+filepath.Join(w, "home", ".docker"),
		"AIHUB_HOST=fakehost", "AIHUB_DATA="+data,
		"BACKUP_ROOT="+filepath.Join(w, "backups"), "COMPOSE_FILE="+compose,
		"WAIT_SECS=4", "POLL_SECS=1", "UP_WAIT_SECS=5")
	return d
}

func (d *deployWorld) run(stdin string, args ...string) (string, error) {
	cmd := exec.Command("sh", append([]string{filepath.Join("..", ".polyforge", "deploy.sh")}, args...)...)
	cmd.Env = d.env
	cmd.Stdin = strings.NewReader(stdin)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return b.String(), err
}

func (d *deployWorld) logOf(name string) string {
	b, _ := os.ReadFile(filepath.Join(d.w, name))
	return string(b)
}

func (d *deployWorld) serverDockerMutations() []string {
	var m []string
	for _, ln := range strings.Split(d.logOf("log-docker"), "\n") {
		if !strings.HasPrefix(ln, "server docker ") {
			continue
		}
		for _, verb := range []string{" load", " tag ", " rmi ", " build "} {
			if strings.Contains(ln, "server docker"+verb) {
				m = append(m, ln)
			}
		}
		if strings.Contains(ln, " up -d") {
			m = append(m, ln)
		}
	}
	return m
}

const digOld = "ghcr.io/silgrid/aihub@sha256:1111111111111111111111111111111111111111111111111111111111111111"
const digNew = "ghcr.io/silgrid/aihub@sha256:2222222222222222222222222222222222222222222222222222222222222222"

func TestReleaseDeployImagePullSwap(t *testing.T) {
	d := newDeployWorld(t, "v2.2.1")
	d.release("v2.2.1", true, digOld, digOld) // rollback target has a GHCR image too
	d.release("v2.3.0", true, digNew, digNew)
	out, err := d.run("deploy\n", "v2.3.0")
	if err != nil {
		t.Fatalf("deploy failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"digest " + digNew + " verified", "rollback image: v2.2.1 has a GHCR image",
		"image polyforge/aihub:v2.3.0 shipped", "container image: polyforge/aihub:v2.3.0",
		"container running, version v2.3.0", "downtime:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "docker build") && strings.Contains(d.logOf("log-docker"), "server docker build") {
		t.Errorf("server must not build any image on the GHCR path:\n%s", d.logOf("log-docker"))
	}
	dl := d.logOf("log-docker")
	// order: local pull -> server load -> server tag -> compose up
	iPull := strings.Index(dl, "local docker pull ghcr.io/silgrid/aihub:v2.3.0")
	iLoad := strings.Index(dl, "server docker load")
	iTag := strings.Index(dl, "server docker tag ghcr.io/silgrid/aihub:v2.3.0 polyforge/aihub:v2.3.0")
	iUp := strings.Index(dl, " up -d --no-deps aihub")
	if !(iPull >= 0 && iPull < iLoad && iLoad < iTag && iTag < iUp) {
		t.Errorf("wrong order pull=%d load=%d tag=%d up=%d\n%s", iPull, iLoad, iTag, iUp, dl)
	}
	if !strings.Contains(d.logOf("log-login"), "ghcr.io") {
		t.Errorf("expected a docker login to ghcr.io from the deployer")
	}
	if strings.Contains(d.logOf("log-ssh"), "docker login") || strings.Contains(dl, "server docker login") || strings.Contains(dl, "server docker pull") {
		t.Errorf("the server must hold no credentials / never pull:\n%s", dl)
	}
	cf, _ := os.ReadFile(filepath.Join(d.w, "srv", "compose", "docker-compose.yml"))
	if !strings.Contains(string(cf), "image: polyforge/aihub:v2.3.0") || !strings.Contains(string(cf), "image: busybox") {
		t.Errorf("compose not swapped correctly:\n%s", cf)
	}
	if _, err := os.Stat(filepath.Join(d.w, "dk-server", "img", "polyforge_aihub_v2.2.1")); err != nil {
		t.Errorf("rollback image polyforge/aihub:v2.2.1 missing on the server: %v", err)
	}
}

func TestReleaseDeployLegacyRollbackBuild(t *testing.T) {
	d := newDeployWorld(t, "v2.2.0")
	d.release("v2.2.0", false, "", "") // first time: no GHCR image for the running version
	d.release("v2.3.0", true, digNew, digNew)
	out, err := d.run("deploy\n", "v2.3.0")
	if err != nil {
		t.Fatalf("deploy failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "v2.2.0 has no GHCR image") {
		t.Errorf("expected the legacy rollback note\n%s", out)
	}
	if !strings.Contains(d.logOf("log-docker"), "server docker build -t polyforge/aihub:v2.2.0") {
		t.Errorf("legacy rollback image must be built on the server from the saved old binary:\n%s", d.logOf("log-docker"))
	}
	if strings.Contains(d.logOf("log-docker"), "server docker build -t polyforge/aihub:v2.3.0") {
		t.Errorf("the NEW image must never be built on the server")
	}
}

func TestReleaseDeployDigestMismatchStopsBeforeServerChange(t *testing.T) {
	d := newDeployWorld(t, "v2.2.1")
	d.release("v2.2.1", true, digOld, digOld)
	d.release("v2.3.0", true, digNew, digOld) // registry image carries a different digest
	out, err := d.run("deploy\n", "v2.3.0")
	if err == nil {
		t.Fatalf("deploy must fail on a digest mismatch\n%s", out)
	}
	if !strings.Contains(out, "FAILED at step 0") || !strings.Contains(out, "refusing") {
		t.Errorf("expected a step-0 refusal\n%s", out)
	}
	if m := d.serverDockerMutations(); len(m) != 0 {
		t.Errorf("server was changed: %v", m)
	}
	cf, _ := os.ReadFile(filepath.Join(d.w, "srv", "compose", "docker-compose.yml"))
	if !strings.Contains(string(cf), "image: polyforge/aihub:v2.2.1") {
		t.Errorf("compose file changed:\n%s", cf)
	}
}

func TestReleaseDeployNoImageAssetFailsAtPreflight(t *testing.T) {
	d := newDeployWorld(t, "v2.2.1")
	d.release("v2.2.1", true, digOld, digOld)
	d.release("v2.3.0", false, "", "")
	out, err := d.run("deploy\n", "v2.3.0", "--dry-run")
	if err == nil || !strings.Contains(out, "has no asset aihub-image.digest") {
		t.Fatalf("expected a clean preflight failure, err=%v\n%s", err, out)
	}
	if m := d.serverDockerMutations(); len(m) != 0 {
		t.Errorf("server was changed: %v", m)
	}
}

func TestReleaseDeployDryRunIsReadOnly(t *testing.T) {
	d := newDeployWorld(t, "v2.2.1")
	d.release("v2.2.1", true, digOld, digOld)
	d.release("v2.3.0", true, digNew, digNew)
	out, err := d.run("", "v2.3.0", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry-run: steps 0-1 ran read-only") {
		t.Errorf("no dry-run marker\n%s", out)
	}
	if m := d.serverDockerMutations(); len(m) != 0 {
		t.Errorf("dry-run changed the server: %v", m)
	}
}
