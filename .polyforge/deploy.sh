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
# What it does: promote an already-released version (GitHub release assets
# aihub-linux-amd64, pf-linux-amd64, SHA256SUMS) onto the aihub docker
# container, with a verified full backup (server + off-host) taken while the
# service is stopped, a post-swap verification, and concrete rollback commands.
#
#   0 preflight   tag, assets + sha256, ssh, container, versions, disk space
#   1 baseline    read-only: integrity_check, table row counts, file counts
#   2 save old binary   docker cp <container>:/aihub -> <root>/aihub-bin-<old>-<TS>
#   3 stop + copy data  docker stop; cp -a <data> -> <root>/aihub-backup-<old>-<TS>
#   4 manifests   sha256 manifest live == server backup
#   5 off-host    rsync backup to BACKUP_ROOT, manifest + integrity + counts
#   6 stage + swap  scp assets, sha256sum -c on the server, docker cp, start
#   7 verify      version, logs, row counts, HTTP/pf smoke
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
#   BACKUP_ROOT=/data/polyforge-backups (local)   RELEASE_STAGE=/data/releases (server)
#   GH_REPO=silgrid/polyforge   AIHUB_PORT=8080   WAIT_SECS=60
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
RELEASE_STAGE="${RELEASE_STAGE:-/data/releases}"
GH_REPO="${GH_REPO:-silgrid/polyforge}"
AIHUB_PORT="${AIHUB_PORT:-8080}"
WAIT_SECS="${WAIT_SECS:-60}"
POLL_SECS="${POLL_SECS:-2}"
SRV_ROOT="$(dirname "$AIHUB_DATA")"   # where the server-side backups/binaries live
KEY_TABLES="work_items memories projects users api_keys machines"
ASSET_AIHUB="aihub-linux-amd64"
ASSET_PF="pf-linux-amd64"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/pf-deploy.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
STOPPED=""   # container stopped by this run and not yet restarted
SWAPPED=""   # new binary copied into the container
STEP="0"
OLDVER=""; TS=""; BIN_SAVED=""; SRV_BACKUP=""; LOCAL_BACKUP=""; STAGE_DIR=""

rssh() { ssh -o BatchMode=yes -o ConnectTimeout=10 "$AIHUB_HOST" "$@"; }
dk() { rssh docker "$@"; }
step() { STEP="$1"; printf '\n== step %s: %s ==\n' "$1" "$2"; }
note() { printf '   %s\n' "$*"; }

rollback_text() {
  cat <<EOF
  # stop the new service
  ssh $AIHUB_HOST docker stop $AIHUB_CONTAINER
  # put the old binary back
  ssh $AIHUB_HOST docker cp $BIN_SAVED $AIHUB_CONTAINER:/aihub
  # OPTIONAL, only if the data is damaged: restore the data dir from the backup
  ssh $AIHUB_HOST "mv $AIHUB_DATA $AIHUB_DATA.failed-$TS && cp -a $SRV_BACKUP $AIHUB_DATA"
  # start and verify
  ssh $AIHUB_HOST docker start $AIHUB_CONTAINER
  ssh $AIHUB_HOST docker exec $AIHUB_CONTAINER /aihub -version   # must print $OLDVER
EOF
}

die() {
  printf '\ndeploy: FAILED at step %s: %s\n' "$STEP" "$*" >&2
  if [ -n "$STOPPED" ] && [ -z "$SWAPPED" ]; then
    echo "deploy: the container was stopped by this run; starting it again on the OLD binary ($OLDVER)..." >&2
    if dk start "$AIHUB_CONTAINER" >/dev/null 2>&1; then
      STOPPED=""
      echo "deploy: container started again on the old binary. Nothing was swapped; data untouched (backup left in place: $SRV_BACKUP)." >&2
    else
      echo "deploy: COULD NOT restart the container. Start it by hand: ssh $AIHUB_HOST docker start $AIHUB_CONTAINER" >&2
    fi
  elif [ -n "$SWAPPED" ]; then
    echo "deploy: the new binary is in place but did not verify. Rollback commands:" >&2
    rollback_text >&2
  else
    echo "deploy: nothing was changed on the server." >&2
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

# ---------------------------------------------------------------- step 0
step 0 "preflight"
note "target: $TAG on $AIHUB_HOST (container $AIHUB_CONTAINER, data $AIHUB_DATA)"
for c in gh ssh scp rsync python3 sha256sum; do
  command -v "$c" >/dev/null 2>&1 || die "local tool $c not found on PATH"
done

REL="$WORK/release"
mkdir -p "$REL"
note "downloading release assets of $GH_REPO $TAG ..."
gh release download "$TAG" -R "$GH_REPO" -D "$REL" -p "$ASSET_AIHUB" -p "$ASSET_PF" -p SHA256SUMS >"$WORK/gh.log" 2>&1 \
  || die "gh release download $TAG failed: $(cat "$WORK/gh.log")"
for f in "$ASSET_AIHUB" "$ASSET_PF" SHA256SUMS; do
  [ -s "$REL/$f" ] || die "release $TAG has no asset $f"
done
(cd "$REL" && sha256sum -c SHA256SUMS) >"$WORK/sha.log" 2>&1 || die "local sha256sum -c failed: $(cat "$WORK/sha.log")"
note "assets verified locally: $(tr '\n' ' ' <"$WORK/sha.log")"

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

# ------------------------------------------------------- plan (2-8)
TS="$(date -u +%Y%m%dT%H%M%SZ)"
BIN_SAVED="$SRV_ROOT/aihub-bin-$OLDVER-$TS"
SRV_BACKUP="$SRV_ROOT/aihub-backup-$OLDVER-$TS"
LOCAL_BACKUP="$BACKUP_ROOT/aihub-backup-$OLDVER-$TS"
STAGE_DIR="$RELEASE_STAGE/$TAG"

echo
echo "== plan for steps 2-8 =="
cat <<EOF
2. save old binary   ssh $AIHUB_HOST docker cp $AIHUB_CONTAINER:/aihub $BIN_SAVED
3. stop + copy data  ssh $AIHUB_HOST docker stop $AIHUB_CONTAINER
                     ssh $AIHUB_HOST cp -a $AIHUB_DATA $SRV_BACKUP
4. manifests         sha256 (find -type f | sort) of $AIHUB_DATA vs $SRV_BACKUP must be identical
5. off-host copy     rsync $AIHUB_HOST:$SRV_BACKUP/ -> $LOCAL_BACKUP/
                     local manifest == server manifest; integrity_check ok + row counts == baseline on the local aihub.db
6. stage + swap      scp $ASSET_AIHUB $ASSET_PF SHA256SUMS -> $AIHUB_HOST:$STAGE_DIR/
                     sha256sum -c there; chmod +x; docker cp $STAGE_DIR/$ASSET_AIHUB $AIHUB_CONTAINER:/aihub; docker start $AIHUB_CONTAINER
7. verify            wait <= ${WAIT_SECS}s for 'docker exec $AIHUB_CONTAINER /aihub -version' == $TAG and container running
                     docker logs --since <start>: print error lines, FAIL on panic/fatal
                     row counts >= baseline (read-only); HTTP check http://127.0.0.1:$AIHUB_PORT/ on the host answers; local 'pf queue --limit 1'
8. rollback + summary; rollback commands for this run:
EOF
rollback_text
echo
echo "   work machines: re-run tools/install.sh <api-key> on each, one at a time"
echo

if [ -n "$DRY" ]; then
  echo "dry-run: steps 0-1 ran read-only; steps 2-8 above were NOT executed. Nothing was changed."
  exit 0
fi

# The human confirm gate: the deploy happens because a person typed
# 'deploy', never because an automation decided to.
printf "Deploy %s to %s now? This STOPS the aihub service while the data is copied (downtime), then swaps the binary. Type 'deploy' to proceed: " "$TAG" "$AIHUB_HOST"
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

# ---------------------------------------------------------------- step 3
step 3 "stop the container, copy the data dir"
dk stop "$AIHUB_CONTAINER" >"$WORK/stop.log" 2>&1 || die "docker stop failed: $(cat "$WORK/stop.log")"
STOPPED=1
note "container stopped (service is DOWN from here)"
rssh "test ! -e '$SRV_BACKUP' && cp -a '$AIHUB_DATA' '$SRV_BACKUP'" || die "cp -a $AIHUB_DATA $SRV_BACKUP failed (target exists, or disk full)"
note "data copied to $SRV_BACKUP"

# ---------------------------------------------------------------- step 4
step 4 "manifests: live vs server backup"
manifest_cmd() { printf "cd '%s' && find . -type f -exec sha256sum {} + | LC_ALL=C sort -k2" "$1"; }
rssh "$(manifest_cmd "$AIHUB_DATA")" >"$WORK/live.manifest" || die "manifest of $AIHUB_DATA failed"
rssh "$(manifest_cmd "$SRV_BACKUP")" >"$WORK/srvbak.manifest" || die "manifest of $SRV_BACKUP failed"
if ! cmp -s "$WORK/live.manifest" "$WORK/srvbak.manifest"; then
  diff "$WORK/live.manifest" "$WORK/srvbak.manifest" | head -20 >&2 || true
  die "manifest of the live data dir and the server backup DIFFER"
fi
note "manifests identical ($(wc -l <"$WORK/live.manifest" | tr -d ' ') files)"
# The service was live during step 1; the stopped copy is the authoritative
# baseline for everything below. Report (do not fail on) drift since step 1.
probe_to remote "$SRV_BACKUP/aihub.db" copy "$WORK/frozen.counts"
note "server backup db: integrity_check ok"
if counts_cmp "$WORK/base.counts" "$WORK/frozen.counts" eq >"$WORK/drift.txt"; then
  note "row counts of the stopped copy equal the step-1 baseline"
else
  note "row counts moved between step 1 (live) and the stop; the stopped copy is the baseline from here:"
  cat "$WORK/drift.txt"
fi

# ---------------------------------------------------------------- step 5
step 5 "off-host copy"
mkdir -p "$BACKUP_ROOT" || die "cannot create $BACKUP_ROOT"
[ ! -e "$LOCAL_BACKUP" ] || die "$LOCAL_BACKUP already exists"
rsync -a -e "ssh -o BatchMode=yes" "$AIHUB_HOST:$SRV_BACKUP/" "$LOCAL_BACKUP/" >"$WORK/rsync.log" 2>&1 \
  || die "rsync to $LOCAL_BACKUP failed: $(tail -5 "$WORK/rsync.log")"
(cd "$LOCAL_BACKUP" && find . -type f -exec sha256sum {} + | LC_ALL=C sort -k2) >"$WORK/local.manifest" || die "local manifest failed"
if ! cmp -s "$WORK/srvbak.manifest" "$WORK/local.manifest"; then
  diff "$WORK/srvbak.manifest" "$WORK/local.manifest" | head -20 >&2 || true
  die "local copy $LOCAL_BACKUP differs from the server backup"
fi
note "local manifest identical to the server manifest"
probe_to local "$LOCAL_BACKUP/aihub.db" copy "$WORK/local.counts"
note "local aihub.db: integrity_check ok"
counts_cmp "$WORK/frozen.counts" "$WORK/local.counts" eq || die "row counts of the local copy differ from the baseline"
note "local row counts equal the baseline"

# ---------------------------------------------------------------- step 6
step 6 "stage + swap"
rssh "mkdir -p '$STAGE_DIR'" || die "mkdir $STAGE_DIR failed"
scp -o BatchMode=yes "$REL/$ASSET_AIHUB" "$REL/$ASSET_PF" "$REL/SHA256SUMS" "$AIHUB_HOST:$STAGE_DIR/" >"$WORK/scp.log" 2>&1 \
  || die "scp to $STAGE_DIR failed: $(cat "$WORK/scp.log")"
rssh "cd '$STAGE_DIR' && sha256sum -c SHA256SUMS" >"$WORK/sha2.log" 2>&1 || die "sha256sum -c on the server failed: $(cat "$WORK/sha2.log")"
note "staged assets verified on the server: $STAGE_DIR"
rssh "chmod +x '$STAGE_DIR/$ASSET_AIHUB' '$STAGE_DIR/$ASSET_PF'" || die "chmod failed"
START_AT="$(rssh "date -u +%Y-%m-%dT%H:%M:%SZ")"
SWAPPED=1   # from the first byte copied in, failures mean "rollback", not "restart old"
dk cp "$STAGE_DIR/$ASSET_AIHUB" "$AIHUB_CONTAINER:/aihub" >"$WORK/cp2.log" 2>&1 || die "docker cp of the new binary failed: $(cat "$WORK/cp2.log")"
note "new binary copied into the container"
dk start "$AIHUB_CONTAINER" >"$WORK/start.log" 2>&1 || die "docker start failed: $(cat "$WORK/start.log")"
STOPPED=""
note "container started (service coming back up)"

# ---------------------------------------------------------------- step 7
step 7 "verify"
LAST_RUN=""; LAST_VER=""
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
code="$(rssh "curl -s -m 10 -o /dev/null -w '%{http_code}' http://127.0.0.1:$AIHUB_PORT/" 2>/dev/null || true)"
case "$code" in
  [1-5][0-9][0-9]) note "HTTP http://127.0.0.1:$AIHUB_PORT/ on the host answers: $code (any status = the server is serving)" ;;
  *) die "the API does not answer HTTP on the host port $AIHUB_PORT (curl said \"$code\")" ;;
esac
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
echo "  version:        $OLDVER -> $TAG"
echo "  old binary:     $AIHUB_HOST:$BIN_SAVED"
echo "  server backup:  $AIHUB_HOST:$SRV_BACKUP"
echo "  local backup:   $LOCAL_BACKUP"
echo "  staged release: $AIHUB_HOST:$STAGE_DIR"
echo "  baseline counts (stopped copy; files: memory/ $MEM_FILES, git/ $GIT_FILES at step 1):"
show_counts "$WORK/frozen.counts"
echo
echo "work machines: re-run tools/install.sh <api-key> on each, one at a time"
