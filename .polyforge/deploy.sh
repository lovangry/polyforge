#!/bin/sh
# polyforge's deploy face (polyforge#123): the project-owned, HUMAN-GATED
# counterpart to .polyforge/release.sh.
#
# Contract (the one sentence that matters):
#
#     A deploy is always triggered by a human running this script (or its
#     documented equivalent). NO automated path - no workflow step, no
#     daemon lane, no release procedure - may ever trigger a deploy. The
#     shared release dispatcher deliberately never references this file.
#
# Scope: the aihub SERVICE HOST only. Work machines are out of scope (they
# re-run tools/install.sh, one at a time - printed at the end).
#
# What it does: promote an already-released version onto the aihub docker
# service. The release (release.sh) publishes the aihub image to the PRIVATE
# GHCR repo ghcr.io/silgrid/aihub:<tag> plus the release assets
# aihub-linux-amd64, pf-linux-amd64, aihub-image.digest, SHA256SUMS. This
# script runs on the DEPLOYER machine (which holds the GitHub / GHCR
# credentials), pulls that image, verifies it, and ships it to the server with
# `docker save | ssh docker load`: the server never holds GitHub credentials
# and never builds the release image. Downtime is SECONDS: every backup step
# runs ONLINE (the service keeps running); the new image is loaded next to the
# running container; only the final `docker compose up -d --no-deps aihub`
# (container recreate) interrupts service. Images are never built by compose,
# so a recreate can no longer silently revert the binary to an older image.
#
#   0 preflight   tag, release assets + SHA256SUMS (covers the binaries and
#                 aihub-image.digest), docker pull ghcr.io/silgrid/aihub:<tag> on
#                 this machine, repo digest == aihub-image.digest, /aihub -version,
#                 ssh, container, compose file, versions, disk space
#   1 baseline    read-only: integrity_check, table row counts, file counts, sizes
#   2 save old binary   docker cp <container>:/aihub -> <root>/aihub-bin-<old>-<TS>;
#                       rollback image polyforge/aihub:<old>: if the old release has
#                       a GHCR image, ship it like the new one; otherwise (releases
#                       before the GHCR image existed, e.g. v2.2.0) build it on the
#                       server from the saved old binary
#   3 online backup     rsync memory/ git/ (pass 1) -> sqlite backup-API snapshot
#                       of aihub.db -> rsync catch-up; snapshot integrity + counts
#   4 consistency       memories rows in the snapshot == memory.md files in the backup
#   5 off-host          rsync backup to BACKUP_ROOT, manifests equal, integrity + counts
#   6 load + swap       docker save | ssh docker load (service keeps running), verify
#                       the image ID on the server == the verified local one, tag it
#                       polyforge/aihub:<tag>, back up and edit ONLY the aihub service
#                       of the compose file (image, no build:, stop_grace_period),
#                       compose config -q, compose up -d --no-deps aihub, poll HTTP,
#                       print downtime
#   7 verify      image + version, logs panic/fatal, counts >= baseline, pf smoke
#   8 rollback    concrete commands printed; final summary
#
# Usage:
#   sh .polyforge/deploy.sh <tag> [--dry-run] [--force]
#     --dry-run  steps 0-1 run for real (strictly read-only on the server),
#                steps 2-8 are printed as a plan; nothing is changed.
#     --force    allow re-deploying the running version / a downgrade.
#   There is deliberately NO --yes: the human types 'deploy'.
#
# Environment (defaults):
#   AIHUB_HOST=root@10.146.0.34   AIHUB_CONTAINER=aihub   AIHUB_DATA=/data/aihub
#   BACKUP_ROOT=/data/polyforge-backups (local)
#   GH_REPO=silgrid/polyforge   GHCR_IMAGE=ghcr.io/silgrid/aihub   AIHUB_PORT=8080   WAIT_SECS=60
#   COMPOSE_FILE=/data/aihub-app/docker-compose.yml  COMPOSE_PROJECT=aihub-app  AIHUB_SERVICE=aihub
#   DRAIN_SECS=5 (stop_grace_period)   MAX_DOWNTIME_SECS=10 (warn)   UP_WAIT_SECS=30 (fail)
set -eu

TAG=""
DRY=""
FORCE=""
for a in "$@"; do
  case "$a" in
    --dry-run) DRY=1 ;;
    --force) FORCE=1 ;;
    --yes|-y) echo "deploy: --yes is not allowed - the human gate stays (type 'deploy' at the prompt)" >&2; exit 2 ;;
    -*) echo "deploy: unknown flag $a (use --dry-run, --force)" >&2; exit 2 ;;
    *)
      if [ -n "$TAG" ]; then echo "deploy: more than one tag given" >&2; exit 2; fi
      TAG="$a" ;;
  esac
done
if [ -z "$TAG" ]; then
  echo "usage: sh .polyforge/deploy.sh <tag> [--dry-run] [--force]" >&2
  exit 2
fi
case "$TAG" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "deploy: tag must look like v<major>.<minor>.<patch> (got \"$TAG\")" >&2; exit 2 ;;
esac
case "$TAG" in
  *[!A-Za-z0-9._-]*) echo "deploy: tag has unexpected characters (got \"$TAG\")" >&2; exit 2 ;;
esac

AIHUB_HOST="${AIHUB_HOST:-root@10.146.0.34}"
AIHUB_CONTAINER="${AIHUB_CONTAINER:-aihub}"
AIHUB_DATA="${AIHUB_DATA:-/data/aihub}"
BACKUP_ROOT="${BACKUP_ROOT:-/data/polyforge-backups}"
GH_REPO="${GH_REPO:-silgrid/polyforge}"
AIHUB_PORT="${AIHUB_PORT:-8080}"
WAIT_SECS="${WAIT_SECS:-60}"
POLL_SECS="${POLL_SECS:-2}"
DRAIN_SECS="${DRAIN_SECS:-5}"
MAX_DOWNTIME_SECS="${MAX_DOWNTIME_SECS:-10}"
UP_WAIT_SECS="${UP_WAIT_SECS:-30}"
COMPOSE_FILE="${COMPOSE_FILE:-/data/aihub-app/docker-compose.yml}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-aihub-app}"
AIHUB_SERVICE="${AIHUB_SERVICE:-aihub}"
IMAGE_REPO="${IMAGE_REPO:-polyforge/aihub}"
GHCR_IMAGE="${GHCR_IMAGE:-ghcr.io/silgrid/aihub}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SRV_ROOT="$(dirname "$AIHUB_DATA")"   # where the server-side backups/binaries live
KEY_TABLES="work_items memories projects users api_keys machines"
ASSET_AIHUB="aihub-linux-amd64"
ASSET_PF="pf-linux-amd64"
ASSET_DIGEST="aihub-image.digest"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/pf-deploy.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
SWAPPED=""   # the rename over /aihub was attempted (service may run the new binary)
STEP="0"
OLDVER=""; OLDIMG=""; NEWIMG=""; COMPOSE_BAK=""; DOCKERFILE=""; TS=""; BIN_SAVED=""; SRV_BACKUP=""; LOCAL_BACKUP=""
NEW_LOCAL_ID=""; OLD_LOCAL_ID=""; OLD_HAS_GHCR=""

rssh() { ssh -o BatchMode=yes -o ConnectTimeout=10 "$AIHUB_HOST" "$@"; }
dk() { rssh docker "$@"; }
step() { STEP="$1"; printf '\n== step %s: %s ==\n' "$1" "$2"; }
note() { printf '   %s\n' "$*"; }

rollback_text() {
  cat <<EOF
  # point the aihub service back at the rollback image $OLDIMG (prepared in step 2:
  # the old GHCR image, or built from the saved old binary) and recreate it
  # (seconds of downtime):
  ssh $AIHUB_HOST "sed -i 's|image: $NEWIMG|image: $OLDIMG|' $COMPOSE_FILE"
  ssh $AIHUB_HOST docker compose -f $COMPOSE_FILE -p $COMPOSE_PROJECT up -d --no-deps $AIHUB_SERVICE
  ssh $AIHUB_HOST docker exec $AIHUB_CONTAINER /aihub -version   # must print $OLDVER
  # (the pre-deploy compose file is kept as $COMPOSE_BAK, but its image/build keys
  #  point at the stale v1.0.0 image - do NOT use it to roll back the binary)
  # OPTIONAL, ONLY if the data is damaged (this needs a STOP = real downtime; the backup's
  # aihub.db is the consistent snapshot):
  ssh $AIHUB_HOST "docker stop $AIHUB_CONTAINER && mv $AIHUB_DATA $AIHUB_DATA.failed-$TS && cp -a $SRV_BACKUP $AIHUB_DATA && docker start $AIHUB_CONTAINER"
EOF
}

die() {
  printf '\ndeploy: FAILED at step %s: %s\n' "$STEP" "$*" >&2
  if [ -z "$SWAPPED" ]; then
    echo "deploy: the service was never interrupted (never stopped, never recreated); backups and images already made are left in place." >&2
  else
    echo "deploy: the container was recreated on the new image but did not verify. Rollback commands:" >&2
    rollback_text >&2
  fi
  exit 1
}

# vcmp A B -> -1 / 0 / 1 comparing vMAJOR.MINOR.PATCH numerically.
vcmp() {
  awk -v a="$1" -v b="$2" 'BEGIN {
    sub(/^v/, "", a); sub(/^v/, "", b); sub(/[-+].*/, "", a); sub(/[-+].*/, "", b)
    n = split(a, x, "."); m = split(b, y, ".")
    for (i = 1; i <= 3; i++) { p = x[i] + 0; q = y[i] + 0
      if (p < q) { print -1; exit } if (p > q) { print 1; exit } }
    print 0 }'
}

# The sqlite probe (python3, stdlib only). args: <db> <ro|copy>
#   ro   : open the live db read-only (file:...?mode=ro).
#   copy : copy aihub.db(+wal/shm) to a scratch dir and open THAT (a stopped
#          db with a leftover -wal cannot be opened read-only in place).
# prints:  "integrity <result>"  and  "count <table> <n>" per table.
PYCHK='
import os, shutil, sqlite3, sys, tempfile
db, mode = sys.argv[1], sys.argv[2]
tmp = None
if mode == "copy":
    tmp = tempfile.mkdtemp(prefix="pf-chk-")
    for suf in ("", "-wal", "-shm"):
        if os.path.exists(db + suf):
            shutil.copy2(db + suf, os.path.join(tmp, "aihub.db" + suf))
    uri = "file:" + os.path.join(tmp, "aihub.db")
else:
    uri = "file:" + db + "?mode=ro"
try:
    c = sqlite3.connect(uri, uri=True, timeout=30)
    try:
        rows = c.execute("pragma integrity_check").fetchall()
    except sqlite3.DatabaseError as e:
        print("integrity", "ERROR: %s" % e)
        sys.exit(0)
    print("integrity", ";".join(str(r[0]) for r in rows)[:300])
    names = [r[0] for r in c.execute(
        "select name from sqlite_master where type=\"table\" order by name")]
    for n in names:
        if n.startswith("sqlite_") or n.startswith("memory_fts"):
            continue
        print("count", n, c.execute("select count(*) from \"%s\"" % n.replace("\"", "")).fetchone()[0])
finally:
    if tmp:
        shutil.rmtree(tmp, ignore_errors=True)
'
dbprobe_remote() { printf '%s\n' "$PYCHK" | rssh python3 - "$1" "$2"; }
dbprobe_local() { printf '%s\n' "$PYCHK" | python3 -I - "$1" "$2"; }

# probe_to <remote|local> <db> <mode> <outfile>: runs the probe, requires
# integrity ok and every key table, leaves "table n" lines in <outfile>.
probe_to() {
  where="$1"; db="$2"; mode="$3"; out="$4"
  if ! "dbprobe_$where" "$db" "$mode" >"$out.raw" 2>"$out.err"; then
    die "sqlite probe of $where:$db failed: $(cat "$out.err")"
  fi
  integ="$(sed -n 's/^integrity //p' "$out.raw")"
  if [ "$integ" != "ok" ]; then
    die "pragma integrity_check on $where:$db is \"$integ\" (must be \"ok\") - refusing to deploy over a damaged database"
  fi
  sed -n 's/^count //p' "$out.raw" >"$out"
  for t in $KEY_TABLES; do
    grep -q "^$t " "$out" || die "key table $t missing in $where:$db"
  done
}

# counts_cmp <expected> <actual> <eq|ge> -> prints differences, returns 1 if bad
counts_cmp() {
  awk -v mode="$3" '
    NR == FNR { e[$1] = $2; next }
    { a[$1] = $2 }
    END {
      bad = 0
      for (t in e) {
        if (!(t in a)) { printf "   table %s missing (was %s)\n", t, e[t]; bad = 1; continue }
        if (mode == "eq" && a[t] != e[t]) { printf "   table %s: %s != %s\n", t, a[t], e[t]; bad = 1 }
        if (mode == "ge" && a[t] + 0 < e[t] + 0) { printf "   table %s LOST rows: %s < %s\n", t, a[t], e[t]; bad = 1 }
        if (mode == "ge" && a[t] + 0 > e[t] + 0) { printf "   note: table %s grew %s -> %s (live service wrote)\n", t, e[t], a[t] }
      }
      exit bad
    }' "$1" "$2"
}

show_counts() { sed 's/^\(.*\) \([0-9]*\)$/   \1 = \2/' "$1"; }

wait_version() { # wait_version <want>  -> 0 when running and version==want
  want="$1"; waited=0; got=""
  while :; do
    run="$(dk inspect -f '{{.State.Running}}' "$AIHUB_CONTAINER" 2>/dev/null || true)"
    if [ "$run" = "true" ]; then
      got="$(dk exec "$AIHUB_CONTAINER" /aihub -version 2>/dev/null | awk 'NF{v=$NF} END{print v}' || true)"
      if [ "$got" = "$want" ]; then return 0; fi
    fi
    if [ "$waited" -ge "$WAIT_SECS" ]; then
      LAST_RUN="$run"; LAST_VER="$got"; return 1
    fi
    sleep "$POLL_SECS"; waited=$((waited + POLL_SECS))
  done
}

# shellcheck disable=SC2016
# build_image <image> <binary-on-server> <expected-version>: LEGACY rollback
# path only - builds on the server in a fresh temp context (service keeps
# running), then verifies. The release image itself is never built here.
build_image() {
  img="$1"; bin="$2"; want="$3"
  [ -n "$DOCKERFILE" ] || die "no deploy/Dockerfile available for the legacy rollback image build"
  ctx="$SRV_ROOT/aihub-build-$TS-$$"
  rssh "mkdir -p '$ctx' && cp '$bin' '$ctx/aihub'" || die "cannot prepare build context $ctx"
  scp -o BatchMode=yes "$DOCKERFILE" "$AIHUB_HOST:$ctx/Dockerfile" >"$WORK/scpdf.log" 2>&1 || die "scp Dockerfile failed: $(cat "$WORK/scpdf.log")"
  if ! rssh docker build -t "$img" "$ctx" >"$WORK/build.log" 2>&1; then
    rssh "rm -rf '$ctx'" || true
    die "docker build $img failed: $(tail -8 "$WORK/build.log")"
  fi
  rssh "rm -rf '$ctx'" || true
  got="$(dk run --rm --entrypoint /aihub "$img" -version 2>/dev/null | awk 'NF{v=$NF} END{print v}' || true)"
  [ "$got" = "$want" ] || die "image $img: /aihub -version prints \"$got\", expected $want (service untouched)"
  dk run --rm --entrypoint git "$img" --version >/dev/null 2>&1 || die "image $img: git --version does not work"
  dk run --rm --entrypoint curl "$img" --version >/dev/null 2>&1 || die "image $img: curl --version does not work"
  note "image $img built; /aihub -version = $want; git and curl work"
}

# ghcr_login: log in to ghcr.io from the gh credentials unless already logged in.
ghcr_login() {
  if grep -q '"ghcr.io"' "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null; then return 0; fi
  gh auth token | docker login ghcr.io -u "$(gh api user -q .login)" --password-stdin >"$WORK/login.log" 2>&1 \
    || die "docker login ghcr.io failed (gh auth status?): $(tail -3 "$WORK/login.log")"
}

# fetch_release <tag> <dir> <required-digest 1|0>: download the release assets
# and verify them against SHA256SUMS (which covers the binaries and the image
# digest file). Prints nothing; sets nothing; dies on any problem.
fetch_release() {
  ft="$1"; fd="$2"; need="$3"
  mkdir -p "$fd"
  gh release download "$ft" -R "$GH_REPO" -D "$fd" -p "$ASSET_AIHUB" -p "$ASSET_PF" -p "$ASSET_DIGEST" -p SHA256SUMS >"$WORK/gh.log" 2>&1 \
    || die "gh release download $ft failed: $(cat "$WORK/gh.log")"
  for f in "$ASSET_AIHUB" "$ASSET_PF" SHA256SUMS; do
    [ -s "$fd/$f" ] || die "release $ft has no asset $f"
  done
  if [ "$need" = 1 ] && [ ! -s "$fd/$ASSET_DIGEST" ]; then
    die "release $ft has no asset $ASSET_DIGEST - it was not published with a GHCR image; nothing to deploy"
  fi
  (cd "$fd" && sha256sum -c SHA256SUMS) >"$WORK/sha.log" 2>&1 || die "sha256sum -c of release $ft failed: $(cat "$WORK/sha.log")"
  if [ -s "$fd/$ASSET_DIGEST" ]; then
    grep -q "[[:space:]]$ASSET_DIGEST\$" "$fd/SHA256SUMS" || die "release $ft: $ASSET_DIGEST is not covered by SHA256SUMS"
  fi
}

# pull_verified <tag> <release-dir> <want-version>: pull the GHCR image on THIS
# machine, require its repo digest == the release's aihub-image.digest and
# /aihub -version == <want-version>. Sets PULLED_ID to the local image ID.
pull_verified() {
  pt="$1"; pd="$2"; pv="$3"
  pref="$GHCR_IMAGE:$pt"
  want="$(head -1 "$pd/$ASSET_DIGEST" | tr -d ' \r')"
  case "$want" in "$GHCR_IMAGE"@sha256:[0-9a-f]*) ;; *) die "$ASSET_DIGEST of $pt is not \"$GHCR_IMAGE@sha256:<hex>\" (got \"$want\")" ;; esac
  docker pull "$pref" >"$WORK/pull.log" 2>&1 || die "docker pull $pref failed: $(tail -3 "$WORK/pull.log")"
  if ! docker image inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' "$pref" 2>/dev/null | grep -qxF "$want"; then
    die "pulled $pref has repo digests [$(docker image inspect -f '{{.RepoDigests}}' "$pref" 2>/dev/null)] but the release says $want - refusing (nothing on the server was touched)"
  fi
  got="$(docker run --rm --entrypoint /aihub "$pref" -version 2>/dev/null | awk 'NF{v=$NF} END{print v}' || true)"
  [ "$got" = "$pv" ] || die "image $pref: /aihub -version prints \"$got\", expected $pv (nothing on the server was touched)"
  PULLED_ID="$(docker image inspect -f '{{.Id}}' "$pref")"
  note "pulled $pref: digest $want verified; /aihub -version = $pv; image $PULLED_ID"
}

# ship_image <tag> <local-image-id> <polyforge-image>: docker save | ssh docker load,
# verify the image ID on the server, tag <polyforge-image>, verify -version/git/curl.
ship_image() {
  st="$1"; sid="$2"; simg="$3"
  sref="$GHCR_IMAGE:$st"
  docker save -o "$WORK/image-$st.tar" "$sref" || die "docker save $sref failed"
  rssh docker load <"$WORK/image-$st.tar" >"$WORK/load.log" 2>&1 || die "docker load on $AIHUB_HOST failed: $(tail -3 "$WORK/load.log")"
  rm -f "$WORK/image-$st.tar"
  sgot="$(dk image inspect -f '{{.Id}}' "$sref" 2>/dev/null || true)"
  [ "$sgot" = "$sid" ] || die "image ID on the server is \"$sgot\", expected $sid (service untouched)"
  dk tag "$sref" "$simg" || die "docker tag $sref $simg failed on the server"
  dk rmi "$sref" >/dev/null 2>&1 || true   # drop the ghcr name: compose only ever sees local polyforge/aihub tags
  dk run --rm --entrypoint git "$simg" --version >/dev/null 2>&1 || die "image $simg: git --version does not work"
  dk run --rm --entrypoint curl "$simg" --version >/dev/null 2>&1 || die "image $simg: curl --version does not work"
  note "image $simg shipped; server image ID == $sid; git and curl work"
}

# The compose edit (python3 stdlib, line-based, scoped to the aihub service block):
# image -> new tag, drop build:, stop_grace_period, add the healthcheck if absent.
# mode diff = print a unified diff only; mode apply = write the file.
PYCOMPOSE='
import difflib, os, re, sys
path, svc, image, grace, mode = sys.argv[1:6]
src = open(path).read()
lines = src.split("\n")
start = None
for i, l in enumerate(lines):
    if re.match(r"^  " + re.escape(svc) + r":\s*$", l):
        start = i
        break
if start is None:
    sys.exit("service block \"  %s:\" not found (block-style yaml expected)" % svc)
end = len(lines)
for j in range(start + 1, len(lines)):
    l = lines[j]
    if l.strip() and not l.lstrip().startswith("#") and (len(l) - len(l.lstrip())) <= 2:
        end = j
        break
while end > start + 1 and not lines[end - 1].strip():
    end -= 1
block = lines[start + 1:end]
out, skip = [], False
has_hc = any(re.match(r"^    healthcheck:", b) for b in block)
for b in block:
    ind = len(b) - len(b.lstrip())
    if skip and (not b.strip() or ind > 4):
        continue
    skip = False
    if re.match(r"^    (build|image|stop_grace_period):", b):
        skip = True
        continue
    out.append(b)
head = ["    image: " + image, "    stop_grace_period: " + grace + "s"]
tail = [] if has_hc else [
    "    healthcheck:",
    "      # any HTTP answer proves the server is up (no unauthenticated health route)",
    "      test: [\"CMD\", \"curl\", \"-s\", \"-o\", \"/dev/null\", \"http://127.0.0.1:8080/\"]",
    "      interval: 30s", "      timeout: 3s", "      retries: 3"]
new = lines[:start + 1] + head + out + tail + lines[end:]
text = "\n".join(new)
if mode == "diff":
    d = "".join(difflib.unified_diff(src.splitlines(True), text.splitlines(True), path, path + " (new)"))
    sys.stdout.write(d if d else "(no change)\n")
else:
    open(path + ".tmp", "w").write(text)
    os.replace(path + ".tmp", path)
'
compose_edit() { printf '%s\n' "$PYCOMPOSE" | rssh python3 - "$COMPOSE_FILE" "$AIHUB_SERVICE" "$NEWIMG" "$DRAIN_SECS" "$1"; }

# ---------------------------------------------------------------- step 0
step 0 "preflight"
note "target: $TAG on $AIHUB_HOST (container $AIHUB_CONTAINER, data $AIHUB_DATA)"
for c in gh docker ssh scp rsync python3 sha256sum; do
  command -v "$c" >/dev/null 2>&1 || die "local tool $c not found on PATH"
done

REL="$WORK/release"
note "downloading release assets of $GH_REPO $TAG ..."
fetch_release "$TAG" "$REL" 1
note "assets verified locally (SHA256SUMS covers $(awk '{print $2}' "$REL/SHA256SUMS" | tr '\n' ' '))"
ghcr_login
pull_verified "$TAG" "$REL" "$TAG"
NEW_LOCAL_ID="$PULLED_ID"
# deploy/Dockerfile is needed ONLY for the legacy rollback-image path (an old
# release without a GHCR image); the new image is never built here.
DOCKERFILE="$WORK/Dockerfile"
if git -C "$SCRIPT_DIR" show "$TAG:deploy/Dockerfile" >"$DOCKERFILE" 2>/dev/null && [ -s "$DOCKERFILE" ]; then
  :
elif [ -s "$SCRIPT_DIR/../deploy/Dockerfile" ]; then
  cp "$SCRIPT_DIR/../deploy/Dockerfile" "$DOCKERFILE"
else
  DOCKERFILE=""
fi

rssh true >/dev/null 2>&1 || die "ssh to $AIHUB_HOST failed (BatchMode)"
note "ssh $AIHUB_HOST: reachable"

state="$(dk inspect -f '{{.State.Status}}' "$AIHUB_CONTAINER" 2>/dev/null || true)"
[ -n "$state" ] || die "container $AIHUB_CONTAINER does not exist on $AIHUB_HOST"
[ "$state" = "running" ] || die "container $AIHUB_CONTAINER is $state, not running - fix the service first, then deploy"
OLDVER="$(dk exec "$AIHUB_CONTAINER" /aihub -version 2>/dev/null | awk 'NF{v=$NF} END{print v}' || true)"
[ -n "$OLDVER" ] || die "could not read the running version (docker exec $AIHUB_CONTAINER /aihub -version)"
note "container: running, version $OLDVER -> $TAG"
case "$OLDVER" in v[0-9]*.[0-9]*.[0-9]*) ;; *) die "running version \"$OLDVER\" is not a release tag" ;; esac
cmp="$(vcmp "$TAG" "$OLDVER")"
if [ "$TAG" = "$OLDVER" ] || [ "$cmp" = "0" ]; then
  [ -n "$FORCE" ] || die "$OLDVER is already running (use --force to redeploy it)"
  note "WARNING: redeploying the running version (--force)"
elif [ "$cmp" = "-1" ]; then
  [ -n "$FORCE" ] || die "$TAG is OLDER than the running $OLDVER - a downgrade needs --force"
  note "WARNING: downgrade $OLDVER -> $TAG (--force)"
fi

# The rollback image: the old release's GHCR image when it has one (a release
# that published aihub-image.digest), else the legacy build from the saved old
# binary (releases before the GHCR image existed, e.g. v2.2.0).
if gh release view "$OLDVER" -R "$GH_REPO" --json assets -q '.assets[].name' >"$WORK/oldassets.txt" 2>"$WORK/oldrel.err"; then
  if grep -qx "$ASSET_DIGEST" "$WORK/oldassets.txt"; then OLD_HAS_GHCR=1; fi
else
  note "WARNING: cannot read release $OLDVER of $GH_REPO ($(head -1 "$WORK/oldrel.err")); the rollback image falls back to a build from the saved old binary"
fi
if [ "$OLD_HAS_GHCR" = 1 ] && [ "$OLDVER" != "$TAG" ]; then
  note "rollback image: $OLDVER has a GHCR image - pulling and verifying it"
  fetch_release "$OLDVER" "$WORK/release-old" 1
  pull_verified "$OLDVER" "$WORK/release-old" "$OLDVER"
  OLD_LOCAL_ID="$PULLED_ID"
elif [ "$OLD_HAS_GHCR" = 1 ]; then
  OLD_LOCAL_ID="$NEW_LOCAL_ID"
  note "rollback image: same version as the target (--force); reusing the verified pull"
else
  note "rollback image: $OLDVER has no GHCR image - will build it from the saved old binary on the server (legacy path)"
  [ -n "$DOCKERFILE" ] || die "no deploy/Dockerfile found (neither at tag $TAG nor in the working tree) - needed for the legacy rollback image of $OLDVER"
fi

rssh "test -f '$COMPOSE_FILE'" || die "compose file $COMPOSE_FILE not found on $AIHUB_HOST"
rssh docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" config -q >"$WORK/cfg0.log" 2>&1 || die "docker compose config -q fails on the CURRENT $COMPOSE_FILE: $(head -3 "$WORK/cfg0.log")"
note "compose: $COMPOSE_FILE (project $COMPOSE_PROJECT, service $AIHUB_SERVICE) validates"
note "images: $(rssh "docker images '$IMAGE_REPO' --format '{{.Repository}}:{{.Tag}}'" | tr '\n' ' ')"
DATA_KB="$(rssh "du -sk '$AIHUB_DATA'" | awk '{print $1}')"
FREE_KB="$(rssh "df -Pk '$SRV_ROOT'" | awk 'NR==2{print $4}')"
case "$DATA_KB$FREE_KB" in ''|*[!0-9]*) die "could not read server disk sizes (du='$DATA_KB' df='$FREE_KB')" ;; esac
note "server: data $((DATA_KB / 1024)) MiB, free on $SRV_ROOT $((FREE_KB / 1024)) MiB (need >= 2x data = $((DATA_KB * 2 / 1024)) MiB)"
[ "$FREE_KB" -ge $((DATA_KB * 2)) ] || die "not enough free space on $SRV_ROOT of the server for a full data copy (+ margin)"
LB="$BACKUP_ROOT"
while [ ! -d "$LB" ] && [ "$LB" != "/" ]; do LB="$(dirname "$LB")"; done
LFREE_KB="$(df -Pk "$LB" | awk 'NR==2{print $4}')"
note "local: free on $LB $((LFREE_KB / 1024)) MiB (need >= data = $((DATA_KB / 1024)) MiB) for $BACKUP_ROOT"
[ "$LFREE_KB" -ge "$DATA_KB" ] || die "not enough free space locally under $BACKUP_ROOT for the off-host copy"

# ---------------------------------------------------------------- step 1
step 1 "baseline (read-only)"
probe_to remote "$AIHUB_DATA/aihub.db" ro "$WORK/base.counts"
note "integrity_check: ok"
note "row counts:"
show_counts "$WORK/base.counts"
MEM_FILES="$(rssh "find '$AIHUB_DATA/memory' -type f | wc -l" | tr -d ' ')"
GIT_FILES="$(rssh "find '$AIHUB_DATA/git' -type f | wc -l" | tr -d ' ')"
note "files: memory/ $MEM_FILES, git/ $GIT_FILES"
for d in aihub.db aihub.db-wal memory git; do
  kb="$(rssh "du -sk '$AIHUB_DATA/$d' 2>/dev/null || echo 0" | awk '{print $1}')"
  note "size: $d $((${kb:-0} / 1024)) MiB"
done

# ------------------------------------------------------- plan (2-8)
TS="$(date -u +%Y%m%dT%H%M%SZ)"
BIN_SAVED="$SRV_ROOT/aihub-bin-$OLDVER-$TS"
SRV_BACKUP="$SRV_ROOT/aihub-backup-$OLDVER-$TS"
LOCAL_BACKUP="$BACKUP_ROOT/aihub-backup-$OLDVER-$TS"
OLDIMG="$IMAGE_REPO:$OLDVER"
NEWIMG="$IMAGE_REPO:$TAG"
COMPOSE_BAK="$COMPOSE_FILE.bak-$TS"
if [ -n "$OLD_HAS_GHCR" ]; then
  OLD_PLAN="docker save $GHCR_IMAGE:$OLDVER | ssh docker load, verify image ID, tag $OLDIMG"
else
  OLD_PLAN="legacy: docker build from the saved old binary + deploy/Dockerfile (no GHCR image for $OLDVER)"
fi

echo
echo "== plan for steps 2-8 (ONLINE backup; the only interruption is one container recreate) =="
cat <<EOF
2. save old binary   ssh $AIHUB_HOST docker cp $AIHUB_CONTAINER:/aihub $BIN_SAVED
                     rollback image $OLDIMG (if absent on the server): $OLD_PLAN; verify -version == $OLDVER
3. online backup     (service keeps running; NO docker stop anywhere)
                     rsync -a $AIHUB_DATA/ -> $SRV_BACKUP/   (pass 1, excluding aihub.db, -wal, -shm, service.lock)
                     python3 sqlite3 backup API: $AIHUB_DATA/aihub.db (ro) -> $SRV_BACKUP/aihub.db ; integrity_check + counts >= baseline
                     rsync -a again (catch-up, files at least as new as the DB)
4. consistency       rows in the snapshot's memories == memory.md files under $SRV_BACKUP/memory (retry up to 2x; still different -> stop, nothing changed)
5. off-host copy     rsync $AIHUB_HOST:$SRV_BACKUP/ -> $LOCAL_BACKUP/
                     server manifest == local manifest; integrity_check ok + row counts == frozen baseline on the local aihub.db
6. load + swap       (already verified in step 0 on this machine: $GHCR_IMAGE:$TAG repo digest == $ASSET_DIGEST, -version == $TAG)
                     docker save $GHCR_IMAGE:$TAG | ssh $AIHUB_HOST docker load  (service keeps running); server image ID == $NEW_LOCAL_ID
                     docker tag on the server -> $NEWIMG ; verify git, curl in it
                     back up $COMPOSE_FILE -> $COMPOSE_BAK ; edit ONLY service $AIHUB_SERVICE (planned diff below) ; docker compose config -q (failure -> backup restored, stop)
                     docker compose -f $COMPOSE_FILE -p $COMPOSE_PROJECT up -d --no-deps $AIHUB_SERVICE ; poll http://127.0.0.1:$AIHUB_PORT/ every 0.2s ; print downtime (warn > ${MAX_DOWNTIME_SECS}s, fail if not up in ${UP_WAIT_SECS}s)
7. verify            container image == $NEWIMG ; wait <= ${WAIT_SECS}s for 'docker exec $AIHUB_CONTAINER /aihub -version' == $TAG ; docker logs --since <start>: print error lines, FAIL on panic/fatal
                     integrity + row counts >= frozen baseline (read-only); local 'pf queue --limit 1'
8. rollback + summary; rollback commands for this run:
EOF
rollback_text
echo
echo "planned compose change (read-only preview of $COMPOSE_FILE):"
compose_edit diff 2>&1 | sed 's/^/   | /' || true
echo
echo "   work machines: re-run tools/install.sh <api-key> on each, one at a time"
echo

if [ -n "$DRY" ]; then
  echo "dry-run: steps 0-1 ran read-only (the image pull is local to this machine); steps 2-8 above were NOT executed. Nothing was changed."
  exit 0
fi

# The human confirm gate: the deploy happens because a person typed
# 'deploy', never because an automation decided to.
printf "Deploy %s to %s now? The backup runs online; the service restarts once on the new binary (seconds of downtime). Type 'deploy' to proceed: " "$TAG" "$AIHUB_HOST"
ANSWER=""
read -r ANSWER || true
if [ "$ANSWER" != "deploy" ]; then
  echo "deploy: aborted by the operator (nothing was touched)."
  exit 1
fi

# ---------------------------------------------------------------- step 2
step 2 "save the old binary"
dk cp "$AIHUB_CONTAINER:/aihub" "$BIN_SAVED" >"$WORK/cp.log" 2>&1 || die "docker cp of the old binary failed: $(cat "$WORK/cp.log")"
rssh "test -s '$BIN_SAVED' && chmod +x '$BIN_SAVED'" || die "saved binary $BIN_SAVED is missing or empty"
saved_v="$(rssh "'$BIN_SAVED' -version" 2>/dev/null | awk 'NF{v=$NF} END{print v}' || true)"
[ "$saved_v" = "$OLDVER" ] || die "saved binary reports version \"$saved_v\", expected $OLDVER"
note "old binary saved: $BIN_SAVED ($OLDVER)"
if rssh docker image inspect "$OLDIMG" >/dev/null 2>&1; then
  got="$(dk run --rm --entrypoint /aihub "$OLDIMG" -version 2>/dev/null | awk 'NF{v=$NF} END{print v}' || true)"
  [ "$got" = "$OLDVER" ] || die "image $OLDIMG already exists but its /aihub reports \"$got\", not $OLDVER - a rollback to it would be wrong; remove/retag it by hand"
  note "rollback image $OLDIMG already exists and reports $OLDVER"
elif [ -n "$OLD_HAS_GHCR" ]; then
  ship_image "$OLDVER" "$OLD_LOCAL_ID" "$OLDIMG"
  got="$(dk run --rm --entrypoint /aihub "$OLDIMG" -version 2>/dev/null | awk 'NF{v=$NF} END{print v}' || true)"
  [ "$got" = "$OLDVER" ] || die "rollback image $OLDIMG reports \"$got\", expected $OLDVER"
else
  build_image "$OLDIMG" "$BIN_SAVED" "$OLDVER"
fi

# ---------------------------------------------------------------- step 3
step 3 "online backup (service keeps running)"
# sqlite online snapshot via the backup API (consistent even under WAL writes).
# shellcheck disable=SC2016
PYSNAP='
import os, sqlite3, sys
src, dst = sys.argv[1], sys.argv[2]
for suf in ("", "-wal", "-shm"):
    if os.path.exists(dst + suf):
        os.remove(dst + suf)
s = sqlite3.connect("file:" + src + "?mode=ro", uri=True, timeout=30)
d = sqlite3.connect(dst)
s.backup(d)
d.execute("pragma journal_mode=delete")
d.close()
s.close()
'
rsync_pass() {
  rssh rsync -a --exclude=/aihub.db --exclude=/aihub.db-wal --exclude=/aihub.db-shm --exclude=/service.lock "$AIHUB_DATA/" "$SRV_BACKUP/" >"$WORK/rs.log" 2>&1 \
    || die "rsync $AIHUB_DATA -> $SRV_BACKUP failed: $(tail -5 "$WORK/rs.log")"
}
snapshot() {
  printf '%s\n' "$PYSNAP" | rssh python3 - "$AIHUB_DATA/aihub.db" "$SRV_BACKUP/aihub.db" >"$WORK/snap.log" 2>&1 \
    || die "sqlite backup of aihub.db failed: $(tail -5 "$WORK/snap.log")"
  probe_to remote "$SRV_BACKUP/aihub.db" copy "$WORK/frozen.counts"
  counts_cmp "$WORK/base.counts" "$WORK/frozen.counts" ge >"$WORK/drift.txt" || { cat "$WORK/drift.txt" >&2; die "snapshot has FEWER rows than the step-1 baseline"; }
}
rssh "test ! -e '$SRV_BACKUP' && mkdir -p '$SRV_BACKUP'" || die "cannot create $SRV_BACKUP (exists?)"
rsync_pass; note "rsync pass 1 (bulk) done"
snapshot;   note "sqlite snapshot taken; snapshot integrity_check ok"
rsync_pass; note "rsync pass 2 (catch-up) done"
cat "$WORK/drift.txt"
note "snapshot row counts (frozen baseline from here):"
show_counts "$WORK/frozen.counts"

# ---------------------------------------------------------------- step 4
step 4 "consistency: memories rows vs memory files"
# Each memories row is exactly one <data>/memory/<project>/<id>/memory.md (or the
# same under _redacted/ for redacted rows): internal/aihub/memory/store.go.
tries=0
while :; do
  rows="$(awk '$1 == "memories" {print $2}' "$WORK/frozen.counts")"
  files="$(rssh "find '$SRV_BACKUP/memory' -type f -name memory.md | wc -l" | tr -d ' ')"
  note "snapshot memories rows: $rows ; memory.md files in the backup: $files"
  [ "$rows" = "$files" ] && break
  tries=$((tries + 1))
  [ "$tries" -le 2 ] || die "memories rows ($rows) != memory files ($files) after 2 retries - memory is being written during the backup or files are missing; re-run (nothing was changed)"
  note "mismatch: re-snapshot + catch-up (retry $tries)"
  snapshot; rsync_pass
done
note "consistent"

# ---------------------------------------------------------------- step 5
step 5 "off-host copy"
mkdir -p "$BACKUP_ROOT" || die "cannot create $BACKUP_ROOT"
[ ! -e "$LOCAL_BACKUP" ] || die "$LOCAL_BACKUP already exists"
manifest_cmd() { printf "cd '%s' && find . -type f -exec sha256sum {} + | LC_ALL=C sort -k2" "$1"; }
rssh "$(manifest_cmd "$SRV_BACKUP")" >"$WORK/srvbak.manifest" || die "manifest of $SRV_BACKUP failed"
rsync -a -e "ssh -o BatchMode=yes" "$AIHUB_HOST:$SRV_BACKUP/" "$LOCAL_BACKUP/" >"$WORK/rsync.log" 2>&1 \
  || die "rsync to $LOCAL_BACKUP failed: $(tail -5 "$WORK/rsync.log")"
(cd "$LOCAL_BACKUP" && find . -type f -exec sha256sum {} + | LC_ALL=C sort -k2) >"$WORK/local.manifest" || die "local manifest failed"
if ! cmp -s "$WORK/srvbak.manifest" "$WORK/local.manifest"; then
  diff "$WORK/srvbak.manifest" "$WORK/local.manifest" | head -20 >&2 || true
  die "local copy $LOCAL_BACKUP differs from the server backup"
fi
note "server and local manifests identical ($(wc -l <"$WORK/local.manifest" | tr -d ' ') files)"
probe_to local "$LOCAL_BACKUP/aihub.db" copy "$WORK/local.counts"
note "local aihub.db: integrity_check ok"
counts_cmp "$WORK/frozen.counts" "$WORK/local.counts" eq || die "row counts of the local copy differ from the baseline"
note "local row counts equal the frozen baseline"

# ---------------------------------------------------------------- step 6
step 6 "load image, edit compose, swap (the only interruption: one container recreate)"
ship_image "$TAG" "$NEW_LOCAL_ID" "$NEWIMG"
dk run --rm --entrypoint /aihub "$NEWIMG" -version 2>/dev/null | awk 'NF{v=$NF} END{print v}' | grep -qx "$TAG" || die "image $NEWIMG on the server does not report $TAG (service untouched)"
# (b) compose: back up, edit ONLY the aihub service, validate, show the diff.
rssh "cp -p '$COMPOSE_FILE' '$COMPOSE_BAK'" || die "cannot back up $COMPOSE_FILE"
note "compose backup: $COMPOSE_BAK"
compose_edit diff >"$WORK/compose.diff" || die "cannot compute the compose edit: $(cat "$WORK/compose.diff")"
sed 's/^/   | /' "$WORK/compose.diff"
compose_edit apply >"$WORK/compose.log" 2>&1 || { rssh "cp -p '$COMPOSE_BAK' '$COMPOSE_FILE'" || true; die "compose edit failed (backup restored): $(cat "$WORK/compose.log")"; }
if ! rssh docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" config -q >"$WORK/cfg.log" 2>&1; then
  rssh "cp -p '$COMPOSE_BAK' '$COMPOSE_FILE'" || echo "deploy: COULD NOT restore $COMPOSE_BAK -> $COMPOSE_FILE, do it by hand" >&2
  die "docker compose config -q rejected the edited file (backup restored, service untouched): $(head -5 "$WORK/cfg.log")"
fi
note "compose file edited and validated (docker compose config -q ok)"
# (c) swap: recreate only the aihub service on the new image, time until HTTP answers.
START_AT="$(rssh "date -u +%Y-%m-%dT%H:%M:%SZ")"
SWAPPED=1   # from here on a failure means "rollback"
# shellcheck disable=SC2016
RESTART_SNIP='
cf="$1"; proj="$2"; svc="$3"; port="$4"; limit="$5"
t0=$(date +%s.%N)
docker compose -f "$cf" -p "$proj" up -d --no-deps "$svc" </dev/null >/dev/null 2>&1 || { echo "UP-FAILED"; exit 3; }
while :; do
  code=$(curl -s -m 1 -o /dev/null -w "%{http_code}" "http://127.0.0.1:$port/" </dev/null 2>/dev/null || true)
  case "$code" in [1-5][0-9][0-9]) break ;; esac
  now=$(date +%s.%N)
  if awk "BEGIN{exit !($now - $t0 > $limit)}"; then echo "NOT-UP"; exit 4; fi
  sleep 0.2
done
t1=$(date +%s.%N)
awk "BEGIN{printf \"%.1f\\n\", $t1 - $t0}"
'
down=""
down="$(printf '%s\n' "$RESTART_SNIP" | rssh sh -s -- "$COMPOSE_FILE" "$COMPOSE_PROJECT" "$AIHUB_SERVICE" "$AIHUB_PORT" "$UP_WAIT_SECS")" \
  || die "swap did not complete: \"$down\" (docker compose up failed, or no HTTP answer within ${UP_WAIT_SECS}s)"
note "downtime: ${down}s (compose up -d --no-deps $AIHUB_SERVICE until HTTP answers; old container drained <= ${DRAIN_SECS}s)"
if awk -v d="$down" -v m="$MAX_DOWNTIME_SECS" 'BEGIN{exit !(d + 0 > m + 0)}'; then
  note "WARNING: downtime ${down}s exceeded ${MAX_DOWNTIME_SECS}s"
fi

# ---------------------------------------------------------------- step 7
step 7 "verify"
LAST_RUN=""; LAST_VER=""
img_now="$(dk inspect -f '{{.Config.Image}}' "$AIHUB_CONTAINER" 2>/dev/null || true)"
[ "$img_now" = "$NEWIMG" ] || die "container image is \"$img_now\", expected $NEWIMG"
note "container image: $img_now"
wait_version "$TAG" || die "after ${WAIT_SECS}s: container running=$LAST_RUN, version=\"$LAST_VER\" (wanted $TAG)"
note "container running, version $TAG"
dk logs --since "$START_AT" "$AIHUB_CONTAINER" >"$WORK/logs.txt" 2>&1 || true
if grep -Ei 'error' "$WORK/logs.txt" >"$WORK/logs.err"; then
  note "log lines mentioning error since $START_AT:"
  head -20 "$WORK/logs.err" | sed 's/^/     /'
else
  note "no error lines in the logs since $START_AT"
fi
if grep -Ei 'panic|fatal' "$WORK/logs.txt" >"$WORK/logs.bad"; then
  head -20 "$WORK/logs.bad" | sed 's/^/     /' >&2
  die "panic/fatal in the service log since the restart"
fi
probe_to remote "$AIHUB_DATA/aihub.db" ro "$WORK/after.counts"
note "integrity_check ok after the swap"
counts_cmp "$WORK/frozen.counts" "$WORK/after.counts" ge || die "row counts after the swap are BELOW the baseline"
note "row counts >= baseline"
note "HTTP answered during the restart poll (any status = serving)"
if command -v pf >/dev/null 2>&1; then
  if pf queue --limit 1 >"$WORK/pf.out" 2>&1; then
    note "local 'pf queue --limit 1': ok ($(head -1 "$WORK/pf.out"))"
  else
    note "WARNING: local 'pf queue --limit 1' failed (maybe pf is not configured for this host): $(head -1 "$WORK/pf.out")"
  fi
fi

# ---------------------------------------------------------------- step 8
step 8 "rollback + summary"
echo "rollback commands for this run (not executed):"
rollback_text
echo
echo "== summary =="
echo "  version:        $OLDVER -> $TAG   (downtime ${down}s)"
echo "  old binary:     $AIHUB_HOST:$BIN_SAVED"
echo "  server backup:  $AIHUB_HOST:$SRV_BACKUP"
echo "  local backup:   $LOCAL_BACKUP"
echo "  baseline counts (snapshot; files: memory/ $MEM_FILES, git/ $GIT_FILES at step 1):"
show_counts "$WORK/frozen.counts"
echo
echo "work machines: re-run tools/install.sh <api-key> on each, one at a time"
