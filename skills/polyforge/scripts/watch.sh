#!/bin/sh
# watch.sh — the session-side watch lane's fixed MECHANISM (polyforge
# wi_LMSxPTQTtz5x). Versioned with the repo at
# skills/polyforge/scripts/watch.sh and copied VERBATIM to
# <workspace>/.polyforge/watchers/<wi>.sh at arm time — the arm step
# prepends a three-line header comment ABOVE this body:
#
#   # pf-watcher wi: <work item id>
#   # pf-watcher session_id: <the arming session's harness session id>
#   # pf-watcher armed_at: <RFC3339 UTC>
#
# This body reads that header from its own file ($0); a copy without it
# refuses to run. The script stays STATELESS: the delta state is the
# probe's (<wi>.state, written by `pf watch-probe`), no --since-size-style
# flag exists, and the probe's output line is never reformatted — the log
# channel only prefixes the RFC3339 UTC timestamp.
#
# Channels (one process):
#   WAKE — polls `pf observe <wi> --json` every watch_poll (default 3s);
#          on the first observed state change it prints ONE full report —
#          the change, the probe snapshot (step, status, latest event,
#          pending question with its id), and the last
#          watch_log_tail_lines watcher-log lines — and exits 0. Exit is
#          the only wake primitive: it wakes only the owning session,
#          which tells the human, handles it, and re-runs the script.
#   LOG  — every watch_log (default 1m) appends
#          `<RFC3339 UTC> <pf watch-probe human line>` to <wi>.log.
#   HEARTBEAT — optional (watch_heartbeat, default off): prints a line to
#          stdout every cycle so the harness task log shows liveness.
#
# Terminal self-clean: when the WI is observed terminal (wrapped, failed,
# canceled — observe's terminal projection "done" included), the script
# deletes <wi>.sh, <wi>.state and <wi>.log (the log only when NOT invoked
# with --keep-log) and exits.
#
# Startup guard: an already-terminal WI refuses to arm — loud, nonzero
# exit, before any file churn (never arm-then-immediately-clean). The
# cadence is sourced ONCE via `pf watch-probe <wi> --print-cadence`; env
# and cli.toml resolution stay in the Go side, and this script passes no
# cadence flags — so cli.toml beats PF_WATCH_* even inside these loops.

set -u

# --- self location: the fixed home <workspace>/.polyforge/watchers/ ----
self=$0
case $self in
  */*) ;;
  *) self=$(command -v -- "$self" 2>/dev/null || printf '%s' "$self") ;;
esac
script_dir=$(CDPATH= cd -- "$(dirname -- "$self")" && pwd) || {
  echo "watcher: cannot resolve the script directory" >&2
  exit 1
}
self=$script_dir/$(basename -- "$self")
watchers_dir=$(dirname -- "$script_dir")
workspace=$(dirname -- "$watchers_dir")
if [ "${PF_WATCH_WORKSPACE:-}" != "" ]; then
  workspace=$PF_WATCH_WORKSPACE
fi

# --- the arm header -----------------------------------------------------
header_field() {
  sed -n "s/^# pf-watcher $1: //p" "$self" 2>/dev/null | head -n 1
}
WI=$(header_field "wi")
SESSION_ID=$(header_field "session_id")
ARMED_AT=$(header_field "armed_at")
if [ -z "$WI" ] || [ -z "$SESSION_ID" ]; then
  echo "watcher: $self carries no pf-watcher header (wi / session_id)." >&2
  echo "  Arm by copying skills/polyforge/scripts/watch.sh VERBATIM to" >&2
  echo "  <workspace>/.polyforge/watchers/<wi>.sh and prepending the" >&2
  echo "  pf-watcher wi / session_id / armed_at header lines." >&2
  exit 1
fi

keep_log=0
for arg in "$@"; do
  case $arg in
    --keep-log) keep_log=1 ;;
    *) echo "watcher: unknown argument $arg (only --keep-log exists)" >&2; exit 2 ;;
  esac
done

log_dir=$workspace/.polyforge/watchers
log_file=$log_dir/$WI.log
state_file=$log_dir/$WI.state

pf=${PF_BIN:-pf}

# The probe resolves the workspace from its own working directory; run
# everything from the workspace root.
CDPATH= cd -- "$workspace" || {
  echo "watcher: workspace $workspace vanished" >&2
  exit 1
}

now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# observe_status greps the observe projection's JSON for the top-level
# status (the Go marshal order pins it as the first "status" occurrence).
observe_status() {
  $pf observe "$WI" --json 2>/dev/null | grep -o '"status":"[a-z_]*"' | head -n 1 | sed 's/.*:"//;s/"$//'
}

# terminal_status: the spec's terminal set (wrapped, failed, canceled)
# plus observe's terminal projection (done).
terminal_status() {
  case $1 in
    wrapped|failed|canceled|done) return 0 ;;
  esac
  return 1
}

# --- startup guard: never arm an already-terminal WI --------------------
guard_status=$(observe_status)
if [ -z "$guard_status" ]; then
  echo "watcher: cannot verify $WI — pf observe answered nothing (daemon down, or the WI is not monitored on this machine); refusing to arm" >&2
  exit 1
fi
if terminal_status "$guard_status"; then
  echo "watcher: refusing to arm $WI: the work item is already terminal (status $guard_status)" >&2
  exit 3
fi

# --- cadence: resolved once, from the Go side ---------------------------
cadence=$($pf watch-probe "$WI" --print-cadence 2>/dev/null) || {
  echo "watcher: pf watch-probe --print-cadence failed for $WI — cannot resolve the cadence" >&2
  exit 1
}
eval "$cadence"
poll_s=${watch_poll_seconds:-3}
log_s=${watch_log_seconds:-60}
hb_s=${watch_heartbeat_seconds:-0}
tail_lines=${watch_log_tail_lines:-50}

self_delete() {
  rm -f "$self" "$state_file"
  if [ "$keep_log" -ne 1 ]; then
    rm -f "$log_file"
  fi
}

# --- LOG channel (background subshell) ----------------------------------
mkdir -p "$log_dir"
(
  while :; do
    sleep "$log_s"
    ts=$(now_utc)
    line=$($pf watch-probe "$WI" 2>/dev/null)
    rc=$?
    if [ "$rc" -eq 0 ] && [ -n "$line" ]; then
      # The probe's own line, timestamp-prefixed, never reformatted.
      printf '%s %s\n' "$ts" "$line" >> "$log_file"
    else
      printf '%s probe failed (exit %d)\n' "$ts" "$rc" >> "$log_file"
    fi
  done
) &
log_pid=$!

# --- optional HEARTBEAT channel -----------------------------------------
hb_pid=""
if [ "$hb_s" -gt 0 ] 2>/dev/null; then
  (
    while :; do
      sleep "$hb_s"
      echo "$(now_utc) heartbeat: watcher $WI alive (session $SESSION_ID)"
    done
  ) &
  hb_pid=$!
fi

cleanup() {
  for p in "$log_pid" "$hb_pid"; do
    if [ -n "$p" ]; then
      kill "$p" 2>/dev/null
    fi
  done
}
trap cleanup EXIT

# --- WAKE channel (foreground) ------------------------------------------
prev_sig=""
while :; do
  view=$($pf observe "$WI" --json 2>/dev/null) || {
    sleep "$poll_s"
    continue
  }
  st=$(printf '%s\n' "$view" | grep -o '"status":"[a-z_]*"' | head -n 1 | sed 's/.*:"//;s/"$//')
  step=$(printf '%s\n' "$view" | grep -o '"current_step":"[^"]*"' | head -n 1 | sed 's/.*:"//;s/"$//')
  pending=$(printf '%s\n' "$view" | grep -o '"pending_question":{"id":"[^"]*"' | head -n 1 | sed 's/.*:"//;s/"$//')
  seq=$(printf '%s\n' "$view" | grep -o '"seq":[0-9]*' | tail -n 1 | sed 's/.*://')
  sig="$st|$step|$pending|$seq"
  if [ -n "$prev_sig" ] && [ "$sig" != "$prev_sig" ]; then
    echo "================================================================"
    echo "pf watcher wake: $WI (session $SESSION_ID, armed $ARMED_AT)"
    echo "$(now_utc): state changed"
    echo "  was: $prev_sig"
    echo "  now: status=$st step=$step pending_question=$pending latest_seq=$seq"
    echo "--- probe snapshot ---"
    $pf watch-probe "$WI" || echo "(the probe failed; this report is partial)"
    echo "--- watcher log (last $tail_lines lines) ---"
    if [ -f "$log_file" ]; then
      tail -n "$tail_lines" "$log_file"
    fi
    echo "================================================================"
    if terminal_status "$st"; then
      if [ "$keep_log" -eq 1 ]; then
        echo "$(now_utc): $WI is terminal (status $st) — the watcher self-deletes (the log is kept by --keep-log)"
      else
        echo "$(now_utc): $WI is terminal (status $st) — the watcher self-deletes"
      fi
      self_delete
    fi
    exit 0
  fi
  if terminal_status "$st"; then
    # Terminal at (or before) the baseline observation: the guard saw a
    # live work item moments ago; the item closed in between. Self-delete
    # without a wake report — there is nothing to wake anybody for.
    if [ "$keep_log" -eq 1 ]; then
      echo "$(now_utc): $WI is terminal (status $st) — the watcher self-deletes (the log is kept by --keep-log)"
    else
      echo "$(now_utc): $WI is terminal (status $st) — the watcher self-deletes"
    fi
    self_delete
    exit 0
  fi
  prev_sig=$sig
  sleep "$poll_s"
done
