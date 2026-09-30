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
# Modes:
#   full (default)  export + import everything; establishes/refreshes
#                   the per-record hash baseline in the state file.
#   --delta         top-up run: only records NEW or CHANGED since the
#                   last successful run are exported into the delta
#                   files and imported. --delta with no baseline state
#                   file is a hard error (never a silent full export
#                   labeled as delta). Capsule snapshot export is
#                   skipped in delta mode.
#
# Baseline: the state file (default $DATA_DIR/seed-v1.state.json,
# --state-file override) records, per record id, the sha256 of each
# emitted JSONL line from the last successful run. The exporter writes
# the candidate full hash set into the export dir; it is promoted to
# the state file ONLY after all four steps pass, so a failed delta run
# keeps its changes visible to the next delta run.
#
# Usage:
#   tools/seed-v1.sh --pilot-dir /root/polyforge-pilot/aihub-data \
#                    --server http://127.0.0.1:8420 --api-key <ADMIN_KEY> \
#                    --data-dir <TARGET_DATA_DIR> --project <PROJECT> \
#                    --owner <TARGET_USER_ID> \
#                    [--out <EXPORT_DIR>] [--pf <PF_BIN>] [--v0-export <EXPORT_BIN>] \
#                    [--delta] [--state-file <STATE>] [--accept-changed]
#
# --accept-changed passes through to pf migrate-work-items (step 2): a
# changed same-id record is applied in place (new version wins, admin
# migration face only) instead of hard-failing the run. Step 4's re-run
# never carries it: after an accepted run the plain skip-check passes
# because the rows now carry the record.
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
DELTA=0
STATE_FILE=""
ACCEPT_CHANGED=0

while [ $# -gt 0 ]; do
  case "$1" in
    --pilot-dir)    PILOT_DIR="$2"; shift 2 ;;
    --server)       SERVER="$2"; shift 2 ;;
    --api-key)      API_KEY="$2"; shift 2 ;;
    --data-dir)     DATA_DIR="$2"; shift 2 ;;
    --project)      PROJECT="$2"; shift 2 ;;
    --owner)        OWNER="$2"; shift 2 ;;
    --out)          OUT="$2"; shift 2 ;;
    --pf)           PF_BIN="$2"; shift 2 ;;
    --v0-export)    V0EXPORT_BIN="$2"; shift 2 ;;
    --delta)        DELTA=1; shift ;;
    --state-file)   STATE_FILE="$2"; shift 2 ;;
    --accept-changed) ACCEPT_CHANGED=1; shift ;;
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

# The baseline state file lives under the target data dir (persistent,
# inert to aihub: a top-level non-memory/, non-capsules/ file).
if [ -n "$STATE_FILE" ]; then :; else STATE_FILE="$DATA_DIR/seed-v1.state.json"; fi
if [ "$DELTA" -eq 1 ] && [ ! -f "$STATE_FILE" ]; then
  echo "seed-v1: --delta but no baseline state file at $STATE_FILE (run a full seed first)" >&2
  exit 2
fi

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

# jsonl_count <file>: the number of records in a JSONL export file.
jsonl_count() {
  wc -l < "$1" 2>/dev/null | tr -d ' ' || echo 0
}

# report_count <key>: an integer field of the export report.
report_count() {
  sed -n "s/.*\"$1\": *\([0-9][0-9]*\).*/\1/p" "$OUT/export-report.json" | head -n 1
}

if [ "$DELTA" -eq 1 ]; then
  echo "== 1/4 delta export the v0 world (read-only) vs $STATE_FILE -> $OUT"
else
  echo "== 1/4 export the v0 world (read-only) -> $OUT"
fi
# exit 3 = exported, but the reconciliation found something worth eyes
# (orphan files / digest mismatches); the findings are printed and the
# run continues — the importers re-verify everything anyway.
EXPORT_ARGS="--db $PILOT_DIR/aihub.db --data-dir $PILOT_DIR --out $OUT"
if [ "$DELTA" -eq 1 ]; then
  EXPORT_ARGS="$EXPORT_ARGS --delta --baseline $STATE_FILE"
fi
set +e
"$V0EXPORT_BIN" $EXPORT_ARGS
EXP_RC=$?
set -e
if [ "$EXP_RC" -gt 3 ]; then
  echo "seed-v1: v0 export failed (rc=$EXP_RC)" >&2; exit 1
fi

WI_IMPORT_FLAGS="--from $OUT/work-items.jsonl --project $PROJECT --reporter $OWNER"
if [ "$ACCEPT_CHANGED" -eq 1 ]; then
  WI_IMPORT_FLAGS="$WI_IMPORT_FLAGS --accept-changed"
fi

echo "== 2/4 import terminal work-item records"
"$PF_BIN" $GLOBAL migrate-work-items $WI_IMPORT_FLAGS

echo "== 3/4 import memories (authority files + reconcile backfill)"
"$PF_BIN" $GLOBAL migrate-memories --from-jsonl "$OUT/memory/memories.jsonl" \
  --data-dir "$DATA_DIR" --project "$PROJECT" --owner "$OWNER"

echo "== 4/4 idempotent re-run (must be zero delta)"
# The re-run never carries --accept-changed: after an accepted run the
# plain all-field skip-check passes because the rows now carry the
# records. In delta mode the assertions run against the DELTA files.
WANT_WI=$(jsonl_count "$OUT/work-items.jsonl")
WI2="$("$PF_BIN" $GLOBAL migrate-work-items --from "$OUT/work-items.jsonl" \
  --project "$PROJECT" --reporter "$OWNER")"
echo "$WI2"
if echo "$WI2" | grep -q "imported 0, skipped $WANT_WI,"; then :; else
  echo "seed-v1: work-item re-run was not a pure skip (imported 0, skipped $WANT_WI)" >&2; exit 1
fi
WANT_MEM=$(jsonl_count "$OUT/memory/memories.jsonl")
MEM2="$("$PF_BIN" $GLOBAL migrate-memories --from-jsonl "$OUT/memory/memories.jsonl" \
  --data-dir "$DATA_DIR" --project "$PROJECT" --owner "$OWNER")"
echo "$MEM2"
if [ "$WANT_MEM" -gt 0 ]; then
  if echo "$MEM2" | grep -q "re-placed byte-identical"; then :; else
    echo "seed-v1: memory re-run changed bytes (delta != 0)" >&2; exit 1
  fi
fi
if echo "$MEM2" | grep -q "FAIL"; then
  echo "seed-v1: memory re-run verification failed" >&2; exit 1
fi

# All four steps passed: promote the exporter's candidate full hash set
# to the persistent state file (the baseline of the next delta run). A
# failed run never reaches here, so its changes stay visible to the
# next delta run.
cp "$OUT/candidate-state.json" "$STATE_FILE"

echo "== reconciliation report"
echo "  export dir:          $OUT"
echo "  state file:          $STATE_FILE"
grep -c '^' "$OUT/work-items.jsonl" | sed 's/^/  exported work items: /'
grep -c '^' "$OUT/memory/memories.jsonl" | sed 's/^/  exported memories: /'
if [ "$DELTA" -eq 1 ]; then
  echo "  delta work items:    $(report_count new_work_items) new, $(report_count changed_work_items) changed"
  echo "  delta memories:     $(report_count new_memories) new, $(report_count changed_memories) changed"
  echo "  capsule snapshots:  skipped (delta mode)"
fi
echo "$WI2" | grep "target project" | sed 's/^/  target work items: /'
echo "$MEM2" | grep "reconcile rebuilt" | sed 's/^/  target memories: /'
echo "seed-v1: done"
