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
# container. Downtime is SECONDS: every backup step runs ONLINE (the service
# keeps running); only the final `docker restart` interrupts service.
#
#   0 preflight   tag, assets + sha256, ssh, container, versions, disk space
#   1 baseline    read-only: integrity_check, table row counts, file counts, sizes
#   2 save old binary   docker cp <container>:/aihub -> <root>/aihub-bin-<old>-<TS>
#   3 online backup     rsync memory/ git/ (pass 1) -> sqlite backup-API snapshot
#                       of aihub.db -> rsync catch-up; snapshot integrity + counts
#   4 consistency       memories rows in the snapshot == memory.md files in the backup
#   5 off-host          rsync backup to BACKUP_ROOT, manifests equal, integrity + counts
#   6 stage + swap      scp assets, sha256 on the server, docker cp as /aihub.new,
#                       `/aihub.new -version` == tag, atomic mv over /aihub,
#                       docker restart -t DRAIN_SECS, poll HTTP, print downtime
#   7 verify      version, logs panic/fatal, counts >= baseline, HTTP/pf smoke
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
#   DRAIN_SECS=5 (docker restart -t)   MAX_DOWNTIME_SECS=10 (warn)   UP_WAIT_SECS=30 (fail)
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
DRAIN_SECS="${DRAIN_SECS:-5}"
MAX_DOWNTIME_SECS="${MAX_DOWNTIME_SECS:-10}"
UP_WAIT_SECS="${UP_WAIT_SECS:-30}"
SRV_ROOT="$(dirname "$AIHUB_DATA")"   # where the server-side backups/binaries live
KEY_TABLES="work_items memories projects users api_keys machines"
ASSET_AIHUB="aihub-linux-amd64"
ASSET_PF="pf-linux-amd64"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/pf-deploy.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
SWAPPED=""   # the rename over /aihub was attempted (service may run the new binary)
STEP="0"
OLDVER=""; TS=""; BIN_SAVED=""; SRV_BACKUP=""; LOCAL_BACKUP=""; STAGE_DIR=""

rssh() { ssh -o BatchMode=yes -o ConnectTimeout=10 "$AIHUB_HOST" "$@"; }
dk() { rssh docker "$@"; }
step() { STEP="$1"; printf '\n== step %s: %s ==\n' "$1" "$2"; }
note() { printf '   %s\n' "$*"; }

rollback_text() {
  cat <<EOF
  # put the old binary back (same minimal-downtime pattern, a few seconds)
  ssh $AIHUB_HOST docker cp $BIN_SAVED $AIHUB_CONTAINER:/aihub.old
  ssh $AIHUB_HOST docker exec $AIHUB_CONTAINER mv /aihub.old /aihub
  ssh $AIHUB_HOST docker restart -t 5 $AIHUB_CONTAINER
  ssh $AIHUB_HOST docker exec $AIHUB_CONTAINER /aihub -version   # must print $OLDVER
  # OPTIONAL, ONLY if the data is damaged (this needs a STOP = real downtime; the backup's
  # aihub.db is the consistent snapshot):
  ssh $AIHUB_HOST "docker stop $AIHUB_CONTAINER && mv $AIHUB_DATA $AIHUB_DATA.failed-$TS && cp -a $SRV_BACKUP $AIHUB_DATA && docker start $AIHUB_CONTAINER"
EOF
}

die() {
  printf '\ndeploy: FAILED at step %s: %s\n' "$STEP" "$*" >&2
  if [ -z "$SWAPPED" ]; then
    echo "deploy: the service was never interrupted (no stop, no restart, binary not replaced); backups already taken are left in place." >&2
  else
    echo "deploy: the new binary was put in place but the service did not verify. Rollback commands:" >&2
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
for d in aihub.db aihub.db-wal memory git; do
  kb="$(rssh "du -sk '$AIHUB_DATA/$d' 2>/dev/null || echo 0" | awk '{print $1}')"
  note "size: $d $((${kb:-0} / 1024)) MiB"
done

# ------------------------------------------------------- plan (2-8)
TS="$(date -u +%Y%m%dT%H%M%SZ)"
BIN_SAVED="$SRV_ROOT/aihub-bin-$OLDVER-$TS"
SRV_BACKUP="$SRV_ROOT/aihub-backup-$OLDVER-$TS"
LOCAL_BACKUP="$BACKUP_ROOT/aihub-backup-$OLDVER-$TS"
STAGE_DIR="$RELEASE_STAGE/$TAG"

echo
echo "== plan for steps 2-8 (ONLINE backup; the only interruption is the final restart) =="
cat <<EOF
2. save old binary   ssh $AIHUB_HOST docker cp $AIHUB_CONTAINER:/aihub $BIN_SAVED
3. online backup     (service keeps running; NO docker stop anywhere)
                     rsync -a $AIHUB_DATA/ -> $SRV_BACKUP/   (pass 1, excluding aihub.db, -wal, -shm, service.lock)
                     python3 sqlite3 backup API: $AIHUB_DATA/aihub.db (ro) -> $SRV_BACKUP/aihub.db ; integrity_check + counts >= baseline
                     rsync -a again (catch-up, files at least as new as the DB)
4. consistency       rows in the snapshot's memories == memory.md files under $SRV_BACKUP/memory (retry up to 2x; still different -> stop, nothing changed)
5. off-host copy     rsync $AIHUB_HOST:$SRV_BACKUP/ -> $LOCAL_BACKUP/
                     server manifest == local manifest; integrity_check ok + row counts == frozen baseline on the local aihub.db
6. stage + swap      scp $ASSET_AIHUB $ASSET_PF SHA256SUMS -> $AIHUB_HOST:$STAGE_DIR/ ; sha256sum -c there; chmod +x
                     docker cp $STAGE_DIR/$ASSET_AIHUB $AIHUB_CONTAINER:/aihub.new ; docker exec $AIHUB_CONTAINER /aihub.new -version == $TAG
                     docker exec $AIHUB_CONTAINER mv /aihub.new /aihub   (atomic rename)
                     docker restart -t $DRAIN_SECS $AIHUB_CONTAINER ; poll http://127.0.0.1:$AIHUB_PORT/ every 0.2s ; print downtime (warn > ${MAX_DOWNTIME_SECS}s, fail if not up in ${UP_WAIT_SECS}s)
7. verify            wait <= ${WAIT_SECS}s for 'docker exec $AIHUB_CONTAINER /aihub -version' == $TAG ; docker logs --since <start>: print error lines, FAIL on panic/fatal
                     integrity + row counts >= frozen baseline (read-only); local 'pf queue --limit 1'
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
step 6 "stage + swap (the only interruption: one restart)"
rssh "mkdir -p '$STAGE_DIR'" || die "mkdir $STAGE_DIR failed"
scp -o BatchMode=yes "$REL/$ASSET_AIHUB" "$REL/$ASSET_PF" "$REL/SHA256SUMS" "$AIHUB_HOST:$STAGE_DIR/" >"$WORK/scp.log" 2>&1 \
  || die "scp to $STAGE_DIR failed: $(cat "$WORK/scp.log")"
rssh "cd '$STAGE_DIR' && sha256sum -c SHA256SUMS" >"$WORK/sha2.log" 2>&1 || die "sha256sum -c on the server failed: $(cat "$WORK/sha2.log")"
note "staged assets verified on the server: $STAGE_DIR"
rssh "chmod +x '$STAGE_DIR/$ASSET_AIHUB' '$STAGE_DIR/$ASSET_PF'" || die "chmod failed"
dk cp "$STAGE_DIR/$ASSET_AIHUB" "$AIHUB_CONTAINER:/aihub.new" >"$WORK/cp2.log" 2>&1 || die "docker cp of the new binary (as /aihub.new) failed: $(cat "$WORK/cp2.log")"
newv="$(dk exec "$AIHUB_CONTAINER" /aihub.new -version 2>/dev/null | awk 'NF{v=$NF} END{print v}' || true)"
[ "$newv" = "$TAG" ] || die "/aihub.new in the container reports version \"$newv\", expected $TAG (not renamed)"
note "/aihub.new reports $TAG"
START_AT="$(rssh "date -u +%Y-%m-%dT%H:%M:%SZ")"
SWAPPED=1   # from the rename on, failures mean "rollback"
dk exec "$AIHUB_CONTAINER" mv /aihub.new /aihub >"$WORK/mv.log" 2>&1 || die "mv /aihub.new /aihub failed: $(cat "$WORK/mv.log")"
note "binary renamed over /aihub (the running process is unaffected until the restart)"
# shellcheck disable=SC2016
RESTART_SNIP='
c="$1"; port="$2"; drain="$3"; limit="$4"
t0=$(date +%s.%N)
docker restart -t "$drain" "$c" </dev/null >/dev/null 2>&1 || { echo "RESTART-FAILED"; exit 3; }
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
down="$(printf '%s\n' "$RESTART_SNIP" | rssh sh -s -- "$AIHUB_CONTAINER" "$AIHUB_PORT" "$DRAIN_SECS" "$UP_WAIT_SECS")" \
  || die "restart did not complete: \"$down\" (docker restart failed, or no HTTP answer within ${UP_WAIT_SECS}s)"
note "downtime: ${down}s (docker restart -t $DRAIN_SECS until HTTP answers)"
if awk -v d="$down" -v m="$MAX_DOWNTIME_SECS" 'BEGIN{exit !(d + 0 > m + 0)}'; then
  note "WARNING: downtime ${down}s exceeded ${MAX_DOWNTIME_SECS}s"
fi

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
echo "  staged release: $AIHUB_HOST:$STAGE_DIR"
echo "  baseline counts (snapshot; files: memory/ $MEM_FILES, git/ $GIT_FILES at step 1):"
show_counts "$WORK/frozen.counts"
echo
echo "work machines: re-run tools/install.sh <api-key> on each, one at a time"
