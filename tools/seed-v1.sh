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

# --- reconciliation.json: the machine-readable counterpart of the human
# report. Written into $OUT on success AND on every failure path (failures
# before --out is known get a throwaway dir). The JSON is assembled ONLY
# from values the script has already captured — step exit codes, grep -c
# counts, the re-run outputs $WI2/$MEM2 — no command is re-executed to
# build it; steps not reached stay empty and are emitted as null/"not_run".
EXP_RC=""         # step 1 rc (≤3 tolerated; rc 3 = exported, worth eyes)
R_EXP_WI=""       # exported work-item records (grep -c of the jsonl)
R_EXP_MEM=""      # exported memory records    (grep -c of the jsonl)
WI1_RC=""         # step 2 rc
MEM1_RC=""        # step 3 rc
R_WI_TARGET=""     # below: parsed from the already-captured $WI2
R_WI_IMP=""
R_WI_SKIP=""
R_MEM_TARGET=""   # below: parsed from the already-captured $MEM2
R_MEM_ACTIVE=""
R_MEM_REDACTED=""
R_MEM_UNCHANGED=""
R_MEM_REBUILT=""
R_MEM_FAILS=""    # "FAIL" lines in the $MEM2 verification section
R_RERUN="not_run"  # step 4: ok | failed | not_run
R_WI_ZERO=""      # true | false | "" (assertion not reached)
R_MEM_IDENT=""

jstr() { # emit a JSON string (null when empty)
  if [ -n "$1" ]; then
    printf '"%s"' "$(printf '%s' "$1" | tr '\n\r\t' '   ' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"
  else
    printf 'null'
  fi
}

jnum() { # emit a JSON number (null when empty or non-numeric)
  case "$1" in
    ''|*[!0-9]*) printf 'null' ;;
    *) printf '%s' "$1" ;;
  esac
}

jbool() { # emit true/false (null when the assertion was not reached)
  case "$1" in
    true|false) printf '%s' "$1" ;;
    *) printf 'null' ;;
  esac
}

write_reconciliation() {
  # $1 = true|false (overall ok), $2 = failure reason ("" = none)
  _exp="not_run"
  if [ -n "$EXP_RC" ]; then
    if [ "$EXP_RC" -le 3 ]; then _exp="ok"; else _exp="failed"; fi
  fi
  _wi="not_run"
  if [ -n "$WI1_RC" ]; then
    if [ "$WI1_RC" -eq 0 ]; then _wi="ok"; else _wi="failed"; fi
  fi
  _mem="not_run"
  if [ -n "$MEM1_RC" ]; then
    if [ "$MEM1_RC" -eq 0 ]; then _mem="ok"; else _mem="failed"; fi
  fi
  {
    printf '{\n'
    printf '  "ok": %s,\n' "$1"
    printf '  "generated_at": "%s",\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    printf '  "export_dir": %s,\n' "$(jstr "$OUT")"
    printf '  "failure_reason": %s,\n' "$(jstr "$2")"
    printf '  "steps": {\n'
    printf '    "export": {\n'
    printf '      "status": "%s",\n' "$_exp"
    printf '      "rc": %s,\n' "$(jnum "$EXP_RC")"
    printf '      "exported_work_items": %s,\n' "$(jnum "$R_EXP_WI")"
    printf '      "exported_memories": %s\n' "$(jnum "$R_EXP_MEM")"
    printf '    },\n'
    printf '    "migrate_work_items": {\n'
    printf '      "status": "%s",\n' "$_wi"
    printf '      "rc": %s,\n' "$(jnum "$WI1_RC")"
    printf '      "target_project": %s,\n' "$(jstr "$R_WI_TARGET")"
    printf '      "rerun_imported": %s,\n' "$(jnum "$R_WI_IMP")"
    printf '      "rerun_skipped": %s\n' "$(jnum "$R_WI_SKIP")"
    printf '    },\n'
    printf '    "migrate_memories": {\n'
    printf '      "status": "%s",\n' "$_mem"
    printf '      "rc": %s,\n' "$(jnum "$MEM1_RC")"
    printf '      "target_project": %s,\n' "$(jstr "$R_MEM_TARGET")"
    printf '      "active": %s,\n' "$(jnum "$R_MEM_ACTIVE")"
    printf '      "redacted": %s,\n' "$(jnum "$R_MEM_REDACTED")"
    printf '      "rerun_replaced_byte_identical": %s,\n' "$(jnum "$R_MEM_UNCHANGED")"
    printf '      "rerun_rebuilt": %s,\n' "$(jnum "$R_MEM_REBUILT")"
    printf '      "verification_failures": %s\n' "$(jnum "$R_MEM_FAILS")"
    printf '    },\n'
    printf '    "idempotent_rerun": {\n'
    printf '      "status": "%s",\n' "$R_RERUN"
    printf '      "wi_zero_delta": %s,\n' "$(jbool "$R_WI_ZERO")"
    printf '      "mem_byte_identical": %s\n' "$(jbool "$R_MEM_IDENT")"
    printf '    }\n'
    printf '  }\n'
    printf '}\n'
  } > "$OUT/reconciliation.json"
}

die() {
  # $1 = exit code, $2 = failure reason: leave the structured trace, then exit
  if [ -z "$OUT" ]; then
    OUT="$(mktemp -d "${TMPDIR:-/tmp}/v1-seed-export.XXXXXX")"
  fi
  write_reconciliation false "$2"
  echo "seed-v1: reconciliation report: $OUT/reconciliation.json" >&2
  exit "$1"
}

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
    *) echo "seed-v1: unknown flag $1" >&2; die 2 "unknown flag $1" ;;
  esac
done

[ -n "$PILOT_DIR" ] || { echo "seed-v1: --pilot-dir is required" >&2; die 2 "--pilot-dir is required"; }
[ -n "$SERVER" ]    || { echo "seed-v1: --server (or PF_SERVER) is required" >&2; die 2 "--server (or PF_SERVER) is required"; }
[ -n "$API_KEY" ]   || { echo "seed-v1: --api-key (or PF_API_KEY) is required" >&2; die 2 "--api-key (or PF_API_KEY) is required"; }
[ -n "$DATA_DIR" ]  || { echo "seed-v1: --data-dir (the TARGET aihub data dir) is required" >&2; die 2 "--data-dir (the TARGET aihub data dir) is required"; }
[ -n "$PROJECT" ]   || { echo "seed-v1: --project (the TARGET project) is required" >&2; die 2 "--project (the TARGET project) is required"; }
[ -n "$OWNER" ]     || { echo "seed-v1: --owner (the TARGET user id, e.g. the bootstrap usr_root) is required" >&2; die 2 "--owner (the TARGET user id, e.g. the bootstrap usr_root) is required"; }
[ -f "$PILOT_DIR/aihub.db" ] || { echo "seed-v1: $PILOT_DIR/aihub.db not found" >&2; die 2 "$PILOT_DIR/aihub.db not found"; }

if [ -n "$OUT" ]; then :; else OUT="$(mktemp -d "${TMPDIR:-/tmp}/v1-seed-export.XXXXXX")"; fi
if [ -n "$V0EXPORT_BIN" ]; then :; else V0EXPORT_BIN="$(dirname "$0")/v0-export/v0-export"; fi

# The export binary is built on demand from the worktree when absent.
if [ ! -x "$V0EXPORT_BIN" ]; then
  echo "seed-v1: building v0-export from the worktree"
  HERE="$(cd "$(dirname "$0")/.." && pwd)"
  (cd "$HERE" && GOWORK=off go build -o "$V0EXPORT_BIN" ./tools/v0-export) || {
    echo "seed-v1: cannot build tools/v0-export" >&2; die 1 "cannot build tools/v0-export"; }
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
  echo "seed-v1: v0 export failed (rc=$EXP_RC)" >&2
  die 1 "v0 export failed (rc=$EXP_RC)"
fi
# exported record counts for the machine-readable report
R_EXP_WI=$(grep -c '^' "$OUT/work-items.jsonl" 2>/dev/null || true)
R_EXP_MEM=$(grep -c '^' "$OUT/memory/memories.jsonl" 2>/dev/null || true)

echo "== 2/4 import terminal work-item records"
set +e
"$PF_BIN" $GLOBAL migrate-work-items --from "$OUT/work-items.jsonl" \
  --project "$PROJECT" --reporter "$OWNER"
WI1_RC=$?
set -e
if [ "$WI1_RC" -ne 0 ]; then
  echo "seed-v1: migrate-work-items failed (rc=$WI1_RC)" >&2
  die 1 "migrate-work-items failed (rc=$WI1_RC)"
fi

echo "== 3/4 import memories (authority files + reconcile backfill)"
set +e
"$PF_BIN" $GLOBAL migrate-memories --from-jsonl "$OUT/memory/memories.jsonl" \
  --data-dir "$DATA_DIR" --project "$PROJECT" --owner "$OWNER"
MEM1_RC=$?
set -e
if [ "$MEM1_RC" -ne 0 ]; then
  echo "seed-v1: migrate-memories failed (rc=$MEM1_RC)" >&2
  die 1 "migrate-memories failed (rc=$MEM1_RC)"
fi

echo "== 4/4 idempotent re-run (must be zero delta)"
set +e
WI2="$("$PF_BIN" $GLOBAL migrate-work-items --from "$OUT/work-items.jsonl" \
  --project "$PROJECT" --reporter "$OWNER")"
WI2_RC=$?
set -e
if [ "$WI2_RC" -ne 0 ]; then
  R_RERUN=failed
  echo "seed-v1: work-item re-run command failed (rc=$WI2_RC)" >&2
  die 1 "work-item re-run command failed (rc=$WI2_RC)"
fi
echo "$WI2"
# pull the step's result counts out of the already-captured re-run output
R_WI_TARGET=$(printf '%s\n' "$WI2" | sed -n 's/.*target project \(.*\); imported \([0-9][0-9]*\), skipped \([0-9][0-9]*\);.*/\1/p')
R_WI_IMP=$(printf '%s\n' "$WI2" | sed -n 's/.*target project \(.*\); imported \([0-9][0-9]*\), skipped \([0-9][0-9]*\);.*/\2/p')
R_WI_SKIP=$(printf '%s\n' "$WI2" | sed -n 's/.*target project \(.*\); imported \([0-9][0-9]*\), skipped \([0-9][0-9]*\);.*/\3/p')
if echo "$WI2" | grep -Eq "imported 0, skipped [1-9][0-9]*"; then
  R_WI_ZERO=true
else
  R_WI_ZERO=false; R_RERUN=failed
  echo "seed-v1: work-item re-run was not a pure skip (delta != 0)" >&2
  die 1 "work-item re-run was not a pure skip (delta != 0)"
fi
set +e
MEM2="$("$PF_BIN" $GLOBAL migrate-memories --from-jsonl "$OUT/memory/memories.jsonl" \
  --data-dir "$DATA_DIR" --project "$PROJECT" --owner "$OWNER")"
MEM2_RC=$?
set -e
if [ "$MEM2_RC" -ne 0 ]; then
  R_RERUN=failed
  echo "seed-v1: memory re-run command failed (rc=$MEM2_RC)" >&2
  die 1 "memory re-run command failed (rc=$MEM2_RC)"
fi
echo "$MEM2"
# pull the step's result counts out of the already-captured re-run output
R_MEM_TARGET=$(printf '%s\n' "$MEM2" | sed -n 's/.*target project \(.*\); active \([0-9][0-9]*\), redacted \([0-9][0-9]*\).*/\1/p')
R_MEM_ACTIVE=$(printf '%s\n' "$MEM2" | sed -n 's/.*target project \(.*\); active \([0-9][0-9]*\), redacted \([0-9][0-9]*\).*/\2/p')
R_MEM_REDACTED=$(printf '%s\n' "$MEM2" | sed -n 's/.*target project \(.*\); active \([0-9][0-9]*\), redacted \([0-9][0-9]*\).*/\3/p')
R_MEM_UNCHANGED=$(printf '%s\n' "$MEM2" | sed -n 's/.* \([0-9][0-9]*\) re-placed byte-identical.*/\1/p')
R_MEM_REBUILT=$(printf '%s\n' "$MEM2" | sed -n 's/.*rebuilt \([0-9][0-9]*\) active entr.*/\1/p')
R_MEM_FAILS=$(printf '%s\n' "$MEM2" | grep -c "FAIL" || true)
if echo "$MEM2" | grep -Eq "re-placed byte-identical"; then
  R_MEM_IDENT=true
else
  R_MEM_IDENT=false; R_RERUN=failed
  echo "seed-v1: memory re-run changed bytes (delta != 0)" >&2
  die 1 "memory re-run changed bytes (delta != 0)"
fi
if echo "$MEM2" | grep -q "FAIL"; then
  R_RERUN=failed
  echo "seed-v1: memory re-run verification failed" >&2
  die 1 "memory re-run verification failed"
fi
R_RERUN=ok

echo "== reconciliation report"
echo "  export dir:          $OUT"
grep -c '^' "$OUT/work-items.jsonl" | sed 's/^/  exported work items: /'
grep -c '^' "$OUT/memory/memories.jsonl" | sed 's/^/  exported memories: /'
echo "$WI2" | grep "target project" | sed 's/^/  target work items: /'
echo "$MEM2" | grep "reconcile rebuilt" | sed 's/^/  target memories: /'
echo "seed-v1: done"

# the machine-readable counterpart of the report above
write_reconciliation true ""
