#!/bin/sh
# seed-v1.sh — the v0-pilot → v1 rehearsal runner. Chains the four steps:
#
#   1. export the v0 world (READ-ONLY) with tools/v0-export
#   2. import the terminal work-item records  (pf migrate-work-items)
#   3. import the memories                   (pf migrate-memories;
#      authority files + the server's file-wins reconcile)
#   4. re-run both imports and assert the idempotent zero-delta answer
#
# The target aihub must ALREADY be running and carry the target project
# and a bootstrap admin user (the fresh-install path prints a one-time
# admin key on first boot). The source pilot directory is never written.
#
# Usage:
#   tools/seed-v1.sh --pilot-dir /root/polyforge-pilot/aihub-data \
#                    --server http://127.0.0.1:8420 --api-key <ADMIN_KEY> \
#                    --data-dir <TARGET_DATA_DIR> --project <PROJECT> \
#                    --owner <TARGET_USER_ID> \
#                    [--out <EXPORT_DIR>] [--pf <PF_BIN>] [--v0-export <EXPORT_BIN>]
#
# All parameters explicit; nothing defaults to the real pilot path.

set -eu

PILOT_DIR=""
SERVER="${PF_SERVER:-}"
API_KEY="${PF_API_KEY:-}"
DATA_DIR=""
PROJECT="${PF_PROJECT:-}"
OWNER=""
OUT=""
PF_BIN="pf"
V0EXPORT_BIN=""

while [ $# -gt 0 ]; do
  case "$1" in
    --pilot-dir)  PILOT_DIR="$2"; shift 2 ;;
    --server)     SERVER="$2"; shift 2 ;;
    --api-key)    API_KEY="$2"; shift 2 ;;
    --data-dir)   DATA_DIR="$2"; shift 2 ;;
    --project)    PROJECT="$2"; shift 2 ;;
    --owner)      OWNER="$2"; shift 2 ;;
    --out)        OUT="$2"; shift 2 ;;
    --pf)         PF_BIN="$2"; shift 2 ;;
    --v0-export)  V0EXPORT_BIN="$2"; shift 2 ;;
    *) echo "seed-v1: unknown flag $1" >&2; exit 2 ;;
  esac
done

[ -n "$PILOT_DIR" ] || { echo "seed-v1: --pilot-dir is required" >&2; exit 2; }
[ -n "$SERVER" ]    || { echo "seed-v1: --server (or PF_SERVER) is required" >&2; exit 2; }
[ -n "$API_KEY" ]   || { echo "seed-v1: --api-key (or PF_API_KEY) is required" >&2; exit 2; }
[ -n "$DATA_DIR" ]  || { echo "seed-v1: --data-dir (the TARGET aihub data dir) is required" >&2; exit 2; }
[ -n "$PROJECT" ]   || { echo "seed-v1: --project (the TARGET project) is required" >&2; exit 2; }
[ -n "$OWNER" ]     || { echo "seed-v1: --owner (the TARGET user id, e.g. the bootstrap usr_root) is required" >&2; exit 2; }
[ -f "$PILOT_DIR/aihub.db" ] || { echo "seed-v1: $PILOT_DIR/aihub.db not found" >&2; exit 2; }

if [ -n "$OUT" ]; then :; else OUT="$(mktemp -d "${TMPDIR:-/tmp}/v1-seed-export.XXXXXX")"; fi
if [ -n "$V0EXPORT_BIN" ]; then :; else V0EXPORT_BIN="$(dirname "$0")/v0-export/v0-export"; fi

# The export binary is built on demand from the worktree when absent.
if [ ! -x "$V0EXPORT_BIN" ]; then
  echo "seed-v1: building v0-export from the worktree"
  HERE="$(cd "$(dirname "$0")/.." && pwd)"
  (cd "$HERE" && GOWORK=off go build -o "$V0EXPORT_BIN" ./tools/v0-export) || {
    echo "seed-v1: cannot build tools/v0-export" >&2; exit 1; }
fi

# Global CLI flags ride BEFORE the verb name; the per-verb flags after.
GLOBAL="--server $SERVER --api-key $API_KEY"

echo "== 1/4 export the v0 world (read-only) -> $OUT"
# exit 3 = exported, but the reconciliation found something worth eyes
# (orphan files / digest mismatches); the findings are printed and the
# run continues — the importers re-verify everything anyway.
set +e
"$V0EXPORT_BIN" --db "$PILOT_DIR/aihub.db" --data-dir "$PILOT_DIR" --out "$OUT"
EXP_RC=$?
set -e
if [ "$EXP_RC" -gt 3 ]; then
  echo "seed-v1: v0 export failed (rc=$EXP_RC)" >&2; exit 1
fi

echo "== 2/4 import terminal work-item records"
"$PF_BIN" $GLOBAL migrate-work-items --from "$OUT/work-items.jsonl" \
  --project "$PROJECT" --reporter "$OWNER"

echo "== 3/4 import memories (authority files + reconcile backfill)"
"$PF_BIN" $GLOBAL migrate-memories --from-jsonl "$OUT/memory/memories.jsonl" \
  --data-dir "$DATA_DIR" --project "$PROJECT" --owner "$OWNER"

echo "== 4/4 idempotent re-run (must be zero delta)"
WI2="$("$PF_BIN" $GLOBAL migrate-work-items --from "$OUT/work-items.jsonl" \
  --project "$PROJECT" --reporter "$OWNER")"
echo "$WI2"
if echo "$WI2" | grep -Eq "imported 0, skipped [1-9][0-9]*"; then :; else
  echo "seed-v1: work-item re-run was not a pure skip (delta != 0)" >&2; exit 1
fi
MEM2="$("$PF_BIN" $GLOBAL migrate-memories --from-jsonl "$OUT/memory/memories.jsonl" \
  --data-dir "$DATA_DIR" --project "$PROJECT" --owner "$OWNER")"
echo "$MEM2"
if echo "$MEM2" | grep -Eq "re-placed byte-identical"; then :; else
  echo "seed-v1: memory re-run changed bytes (delta != 0)" >&2; exit 1
fi
if echo "$MEM2" | grep -q "FAIL"; then
  echo "seed-v1: memory re-run verification failed" >&2; exit 1
fi

echo "== reconciliation report"
echo "  export dir:          $OUT"
grep -c '^' "$OUT/work-items.jsonl" | sed 's/^/  exported work items: /'
grep -c '^' "$OUT/memory/memories.jsonl" | sed 's/^/  exported memories: /'
echo "$WI2" | grep "target project" | sed 's/^/  target work items: /'
echo "$MEM2" | grep "reconcile rebuilt" | sed 's/^/  target memories: /'
echo "seed-v1: done"
