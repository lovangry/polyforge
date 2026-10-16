#!/bin/sh
# polyforge bootstrap — complete machine setup in one command.
# Installs/updates (idempotent: a second run changes nothing):
#   1. prepare          — side-effect free for the running machine: the
#        latest pf binary is downloaded, SHA-verified and staged next to
#        the installed one ($BIN.new.<pid>, executable); the release tag
#        is cloned into its own versioned dir
#        ~/.local/share/polyforge/release/<VER>. Any failure here exits
#        with pf, release/current and the running daemon untouched.
#   2. switch           — takes <data-dir>/ensure.lock (held until step 4,
#        so a harness session's `pf daemon ensure` in that window starts
#        nothing: no old daemon revived, no new binary against the old
#        release). On a binary upgrade the daemon is shut down gracefully
#        (its restart loop, tmux session polyforge-daemon, ends with it on
#        that clean exit) — REFUSED while it runs jobs (a shutdown
#        terminates their agent trees) unless PF_INSTALL_FORCE=1 — install
#        waits until both are gone, then swaps the binary by one atomic
#        rename. release/current (stable symlink) is switched atomically
#        to the new version, the previous version kept.
#   3. config + harnesses — config created once, NEVER rewritten; the
#        harnesses REFERENCE release/current, nothing is copied or cloned
#        again (polyforge#174): Claude Code: ~/.claude/skills/polyforge is
#        a symlink to release/current/skills/polyforge (an old copied
#        directory is replaced) + SessionStart hook registered once in
#        ~/.claude/settings.json; pi: one local package entry naming
#        release/current in pi's settings (replaces the old
#        git:...silgrid/polyforge.git@<ver> entry)
#   4. daemon           — `pf daemon ensure`: the restart loop in tmux runs
#        the daemon with -workflows-dir release/current/workflows; after a
#        reboot the harness adapters run the same ensure at session start
#   5. verify
#
# Usage (new machine):
#   curl -s -H "Authorization: token $(gh auth token)" \
#     https://raw.githubusercontent.com/silgrid/polyforge/main/tools/install.sh \
#     | bash -s -- <api-key> [server-host]
#
# Usage (update existing machine): same command — detects and updates.
# While the daemon runs jobs (`pf ops status`) an upgrade is refused and
# nothing changes: wait for them, pause them (`pf pause <wi>`), or
# override — their agent trees are terminated and recover at the next
# daemon start:
#   curl ... | PF_INSTALL_FORCE=1 bash -s -- <api-key> [server-host]
#
# Requires: gh authenticated (repo is private).
set -e
KEY="${1:?usage: install.sh <api-key> [server-host]}"
HOST="${2:-${PF_HOST:-10.146.0.34}}"
API="http://$HOST:8080"
REPO="silgrid/polyforge"
BIN="${PF_BIN:-/usr/local/bin/pf}"
CFG="$HOME/.config/polyforge/cli.toml"
DAEMON_DATA="$HOME/.local/share/polyforge"
DAEMON_SOCKET="$DAEMON_DATA/daemon.sock"
RELEASE_DIR="$DAEMON_DATA/release"
WORKFLOWS="$RELEASE_DIR/current/workflows"
PI_SETTINGS="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}/settings.json"
CLAUDE_SETTINGS="$HOME/.claude/settings.json"

# The tmux session hosting the daemon restart loop (pf daemon ensure).
LOOP_SESSION="polyforge-daemon"

# tmux_q: tmux on the user's default server, whatever tmux this shell
# runs inside.
tmux_q() { env -u TMUX -u TMUX_PANE tmux "$@"; }

# loop_running: the restart loop's tmux session exists.
loop_running() {
  command -v tmux >/dev/null 2>&1 && tmux_q has-session -t "=$LOOP_SESSION" 2>/dev/null
}

# daemon_running: true while a daemon answers on the socket, or still
# holds its data-dir lock (quiescing after the socket went quiet). The
# lock is an flock held for the daemon's lifetime and released by the
# kernel at exit, so a free lock means the process is really gone.
daemon_running() {
  if [ -S "$DAEMON_SOCKET" ] && curl -s -m 2 --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | grep -q machine_id; then
    return 0
  fi
  if [ -f "$DAEMON_DATA/daemon.lock" ] && command -v flock >/dev/null 2>&1; then
    flock -n "$DAEMON_DATA/daemon.lock" -c true 2>/dev/null || return 0
  fi
  return 1
}

# ─── 1. prepare: nothing on this machine changes yet ───────────────
# Every step that can fail for an outside reason (gh, network, SHA, a
# release lacking its tree) runs here, before anything is stopped or
# replaced. A failure in this phase leaves pf, release/current and the
# running daemon exactly as they were.
echo "== 1/5 prepare (pf binary + release checkout; nothing is changed yet)"
STAGED=""   # the new binary, staged next to $BIN (same filesystem)
TMP=""
CLONE=""
STOPPED=0   # 1 from the shutdown request until step 4 starts the daemon
cleanup() {
  rc=$?
  [ -z "$TMP" ] || rm -rf "$TMP"
  [ -z "$STAGED" ] || rm -f "$STAGED"
  [ -z "$CLONE" ] || rm -rf "$CLONE"
  if [ "$rc" -ne 0 ] && [ "$STOPPED" = 1 ]; then
    # aborted after the daemon was asked to stop: say where the machine
    # stands (never restart from here — that is the operator's call)
    echo "ERROR: install aborted (exit $rc) AFTER the daemon was asked to shut down. State now:"
    echo "   daemon: stopped or stopping — no daemon runs until \`pf daemon ensure\` (by hand, or at the next pi / Claude Code session start)"
    echo "   pf binary $BIN: $("$BIN" --version 2>/dev/null || echo "unreadable") (upgrading $RUNNING -> $VER)"
    echo "   release/current -> $(readlink "$RELEASE_DIR/current" 2>/dev/null || echo "missing") (upgrading to $VER)"
    echo "   next: re-run this installer to finish the upgrade, or run \`pf daemon ensure\` to start the daemon as it stands; log: $DAEMON_DATA/daemon.log"
  fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP
UNCHANGED="nothing on this machine was changed: pf, release/current and the running daemon are as they were"
prep_fail() { echo "ERROR: $1; $UNCHANGED"; exit 1; }

command -v gh >/dev/null 2>&1 || prep_fail "gh not found"
VER=$(gh release list --repo "$REPO" --limit 1 --json tagName --jq '.[0].tagName' 2>/dev/null) || true
[ -n "$VER" ] || prep_fail "no release found"

RUNNING="none"
if [ -x "$BIN" ]; then
  RUNNING=$("$BIN" --version 2>/dev/null || echo "unknown")
fi

if [ "$RUNNING" = "$VER" ]; then
  echo "   pf $VER already current, skipping"
else
  TMP=$(mktemp -d)
  gh release download "$VER" --repo "$REPO" --pattern "pf-linux-amd64" --dir "$TMP" 2>/dev/null \
    || prep_fail "binary not on release $VER"
  gh release download "$VER" --repo "$REPO" --pattern "SHA256SUMS" --dir "$TMP" 2>/dev/null \
    || prep_fail "SHA256SUMS not on release $VER"
  if [ -f "$TMP/SHA256SUMS" ]; then
    EXP=$(grep "pf-linux-amd64" "$TMP/SHA256SUMS" | awk '{print $1}')
    GOT=$(sha256sum "$TMP/pf-linux-amd64" | awk '{print $1}')
    [ "$GOT" = "$EXP" ] || prep_fail "SHA mismatch on pf-linux-amd64 of $VER"
  fi
  # Stage the new binary in $BIN's own directory, already executable: the
  # swap in step 2 is then one rename(2) — `pf` is at every instant
  # either the complete old or the complete new binary, never a partial
  # or non-executable file (a harness hook or pf-chat may run it then).
  mkdir -p "$(dirname "$BIN")" || prep_fail "cannot create $(dirname "$BIN")"
  STAGED="$BIN.new.$$"
  { cp "$TMP/pf-linux-amd64" "$STAGED" && chmod 755 "$STAGED"; } \
    || prep_fail "cannot stage the new binary at $STAGED (write access to $(dirname "$BIN")?)"
  rm -rf "$TMP"; TMP=""
  echo "   pf $VER downloaded, SHA-verified, staged"
fi

# The installer owns a checkout of the release tag, independent of any
# harness: the daemon's workflows and every harness's skill come from it.
# A new version gets its own directory; release/current is not touched
# here (step 2 switches it).
mkdir -p "$RELEASE_DIR"
REL="$RELEASE_DIR/$VER"
if [ -f "$REL/skills/polyforge/SKILL.md" ] && [ -d "$REL/workflows" ]; then
  echo "   release checkout $VER present"
else
  CLONE="$REL.tmp.$$"
  rm -rf "$CLONE"
  # gh uses its own credentials (ssh or https), so a machine that only has
  # gh auth can clone the private repo. The tag is cloned shallow.
  gh repo clone "$REPO" "$CLONE" -- --branch "$VER" --depth 1 >/dev/null 2>&1 \
    || prep_fail "cannot clone $REPO at $VER (gh auth?)"
  if [ ! -f "$CLONE/skills/polyforge/SKILL.md" ] || [ ! -d "$CLONE/workflows" ]; then
    prep_fail "$VER checkout lacks skills/polyforge or workflows"
  fi
  # the version's own directory: nothing references it until step 2 —
  # unless an incomplete checkout of this version IS release/current;
  # never remove the tree the running daemon and the harnesses use
  if [ -e "$REL" ] && [ "$(readlink "$RELEASE_DIR/current" 2>/dev/null)" = "$VER" ]; then
    prep_fail "release/current points to $REL, which is incomplete (no skills/polyforge/SKILL.md or workflows/); it is in use, so install does not replace it — inspect it, remove it and re-run"
  fi
  rm -rf "$REL"
  mv "$CLONE" "$REL"
  CLONE=""
  echo "   release checkout $VER fetched"
fi

# ─── 2. switch: stop the daemon, swap the binary, switch release ────
echo "== 2/5 switch"
# Hold the ensure lock (the flock `pf daemon ensure` takes) from here
# through the release switch: an adapter's ensure (a pi or Claude Code
# session starting, resuming, compacting) waits on it and then answers by
# the socket — it never starts a loop mid-upgrade (no old daemon revived,
# no new binary against the old release). Released right before step 4's
# own ensure.
mkdir -p "$DAEMON_DATA"
if command -v flock >/dev/null 2>&1; then
  exec 9>>"$DAEMON_DATA/ensure.lock"
  flock -w 120 9 || echo "   WARN: another pf daemon ensure held $DAEMON_DATA/ensure.lock for 120s; continuing"
else
  echo "   WARN: flock not found: a harness session starting during this upgrade could start the daemon early"
fi

# busy_jobs: the daemon's jobs that a shutdown would interrupt (the
# GET /status `pf ops status` reads), one "<work item> (<status>)" per
# line. Busy: running, recovering, sealing — and any status this script
# does not know (fail closed). Not busy: paused (operator pause: no
# agent tree, the record survives the restart) and end_pending (no agent
# tree; it only retries its blocked close, at every daemon start too —
# counting it would block upgrades for as long as the hub is
# unreachable). Fails closed: only a missing socket means "nothing to
# ask" (the daemon is quiescing past its listener, or only a loop is
# left); a socket that does not answer /status with a readable job list
# (curl error, timeout, non-200, no python3) exits 2.
busy_jobs() {
  [ -S "$DAEMON_SOCKET" ] || return 0
  st=$(curl -sf -m 15 --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null) || return 2
  printf '%s' "$st" | python3 -c '
import json, sys
s = json.load(sys.stdin)
if not isinstance(s, dict) or "machine_id" not in s:
    sys.exit(2)
for j in s.get("jobs") or []:
    if j.get("status") not in ("paused", "end_pending"):
        print("%s (%s)" % (j.get("slug") or j.get("work_item_id"), j.get("status")))
' 2>/dev/null || return 2
}

if [ -n "$STAGED" ]; then
  # Upgrade order: stop the daemon gracefully — a clean exit also ends
  # its restart loop — and wait until the daemon AND the loop are really
  # gone: the binary is never swapped under a live daemon, and no loop is
  # left to start the old binary again. A loop left without a daemon
  # (between two restarts after a failure, or an older loop) is killed.
  # The wait must exceed the daemon's graceful ShutdownBudget (5
  # minutes), hence 330s. Step 4 starts the new daemon through
  # `pf daemon ensure`.
  if daemon_running || loop_running; then
    # A graceful shutdown terminates every live agent tree: never behind
    # the back of a running job unless the operator says so.
    BRC=0
    BUSY=$(busy_jobs) || BRC=$?
    if [ "${PF_INSTALL_FORCE:-}" = 1 ]; then
      [ -z "$BUSY" ] || printf '   PF_INSTALL_FORCE=1: shutting down despite running jobs:\n%s\n' "$(printf '%s\n' "$BUSY" | sed 's/^/     /')"
    elif [ "$BRC" -ne 0 ]; then
      echo "ERROR: the daemon's job list could not be read (GET /status on $DAEMON_SOCKET failed, timed out or was unreadable), so whether jobs run is unknown and the upgrade to $VER is refused; $UNCHANGED (the fetched release/$VER checkout is kept for the next run)."
      echo "   check: pf ops status. To shut it down anyway (its live agent trees are terminated), re-run with PF_INSTALL_FORCE=1."
      exit 1
    elif [ -n "$BUSY" ]; then
      echo "ERROR: the daemon is running jobs; upgrading to $VER needs a graceful shutdown, which terminates their agent trees. Upgrade refused; $UNCHANGED (the fetched release/$VER checkout is kept for the next run)."
      printf '%s\n' "$BUSY" | sed 's/^/     /'
      echo "   proceed by one of: wait until they finish (pf ops status), pause them (pf pause <wi>), or re-run with PF_INSTALL_FORCE=1 to shut them down anyway (they recover at the next daemon start)."
      exit 1
    fi
    # A job claimed between this check and the shutdown request (auto-pull,
    # a pf submit) is interrupted like a forced one and recovers at the
    # next daemon start; the window is the few ms until the request.
    echo "   stopping daemon (graceful; its restart loop ends with it)"
    # A refused/failed request (an unloadable cli.toml, a wrong socket)
    # stops nothing: fail now instead of waiting out the budget and
    # reporting a still-live daemon as stopped.
    if ! SHUT_OUT=$("$BIN" ops shutdown 2>&1) && daemon_running; then
      echo "ERROR: pf ops shutdown failed, so the daemon is still running; $UNCHANGED (the fetched release/$VER checkout is kept for the next run)."
      printf '%s\n' "$SHUT_OUT" | sed 's/^/     /'
      echo "   fix what it reports (check: pf doctor; pf ops status), then re-run this installer."
      exit 1
    fi
    STOPPED=1
    WAITED=0
    while daemon_running || loop_running; do
      if ! daemon_running && loop_running; then
        tmux_q kill-session -t "=$LOOP_SESSION" 2>/dev/null || true
        echo "   stopped a leftover restart loop (tmux session $LOOP_SESSION)"
      fi
      if [ "$WAITED" -ge 330 ]; then
        echo "ERROR: daemon did not stop within 330s (graceful shutdown budget is 300s); binary NOT replaced. Its restart loop ended with the shutdown request."
        exit 1
      fi
      sleep 1
      WAITED=$((WAITED + 1))
    done
    echo "   daemon stopped"
  fi
  # atomic: a rename within one directory
  mv -f "$STAGED" "$BIN" || {
    echo "ERROR: cannot replace $BIN"
    exit 1
  }
  STAGED=""
  echo "   pf $RUNNING → $VER installed"
fi

# stable symlink, swapped atomically (relative target: the tree is relocatable)
PREV=$(readlink "$RELEASE_DIR/current" 2>/dev/null || true)
if [ "$PREV" = "$VER" ]; then
  echo "   release/current -> $VER already"
else
  { ln -sfn "$VER" "$RELEASE_DIR/.current.tmp.$$" && mv -T "$RELEASE_DIR/.current.tmp.$$" "$RELEASE_DIR/current"; } || {
    rm -f "$RELEASE_DIR/.current.tmp.$$"
    echo "ERROR: cannot switch $RELEASE_DIR/current to $VER"
    exit 1
  }
  echo "   release/current -> $VER (was ${PREV:-none})"
  # keep the new and the previous version, prune the rest
  for d in "$RELEASE_DIR"/*; do
    if [ ! -d "$d" ] || [ -L "$d" ]; then continue; fi
    n=$(basename "$d")
    [ "$n" = "$VER" ] || [ "$n" = "$PREV" ] || { rm -rf "$d"; echo "   pruned old release $n"; }
  done
fi

# ─── 3. config + harnesses ──────────────────────────────────────────
echo "== 3/5 config + harnesses"
# config: create or verify (NEVER rewritten)
mkdir -p "$(dirname "$CFG")"
if [ -f "$CFG" ] && grep -q "api_key" "$CFG"; then
  echo "   config exists ($(dirname "$CFG")/$(basename "$CFG"))"
  if grep -q '^[[:space:]]*\[workspace' "$CFG"; then
    echo "   workspace mapping present (the daemon routes capsules by it)"
  else
    # polyforge#113: an existing config predating the workspace mapping
    # is the operator's property — install.sh NEVER rewrites it. The
    # daemon refuses to start without a real mapping, so print the
    # migration steps instead and let the operator edit.
    cat <<'MIGEOF'
   ------------------------------------------------------------------
   MIGRATION REQUIRED: this config predates the workspace mapping
   (polyforge#113). There is NO default `project` key anymore, and the
   daemon refuses to start until the config carries a real mapping.
   Edit the config and add one section per workspace:

     [workspace."<absolute-path-to-your-workspace>"]
     projects = ["your-project", "another-project"]

   Notes: `~` at the start of a key expands to your home directory;
   keys must be absolute and carry no trailing separator; the daemon
   creates nothing but `capsules/` inside each workspace. Capsules from
   a previously-bound workspace keep their homes when you re-add that
   workspace for its projects. Check the result with `pf doctor`, then
   start the daemon: `pf daemon`.
   ------------------------------------------------------------------
MIGEOF
  fi
else
  # umask 077 in a subshell: the api key never sits in a world-readable
  # file, not even until the chmod below
  ( umask 077; cat > "$CFG" ) <<CFGEOF
server = "$API"
api_key = "$KEY"
daemon_socket = "$DAEMON_SOCKET"

# Workspace mapping (polyforge#113): one section per workspace, each
# listing the projects whose capsules live under it. UNCOMMENT and edit
# ONE example to create a real mapping — until then the daemon REFUSES
# to start:
#
# [workspace."<absolute-path-to-your-workspace>"]
# projects = ["your-project", "another-project"]
#
# \`~\` at the start of a key expands to your home directory; keys must
# be absolute; the daemon creates nothing but capsules/ inside each
# workspace. Run \`pf doctor\` to check the mapping after editing.
CFGEOF
  chmod 600 "$CFG"
  echo "   config written (add a real [workspace] mapping before starting the daemon — see the commented template inside)"
fi
# --- skills: the harnesses reference release/current ---
SRC_SKILL="$RELEASE_DIR/current/skills/polyforge"

# --- Claude Code (independent of pi) ---
# The skill is a symlink to the release checkout: a release switch is the
# release/current swap above, nothing to copy (Claude Code follows
# symlinked skill dirs).
if [ -d "$HOME/.claude" ] || command -v claude >/dev/null 2>&1; then
  CSK="$HOME/.claude/skills/polyforge"
  mkdir -p "$HOME/.claude/skills"
  if [ -L "$CSK" ] && [ "$(readlink "$CSK")" = "$SRC_SKILL" ]; then
    echo "   claude skill -> release/current already"
  else
    if [ -L "$CSK" ]; then
      rm -f "$CSK"
    elif [ -e "$CSK" ]; then
      # the pre-#174 layout: a copy this installer made of a release;
      # moved out of skills/ first (a second polyforge skill dir there
      # would load twice), then dropped
      rm -rf "$HOME/.claude/.polyforge-skill-old.$$"
      mv "$CSK" "$HOME/.claude/.polyforge-skill-old.$$"
      rm -rf "$HOME/.claude/.polyforge-skill-old.$$"
      echo "   claude skill: removed the old copied directory"
    fi
    ln -s "$SRC_SKILL" "$CSK"
    echo "   claude skill -> $SRC_SKILL"
  fi
  HOOK="$CSK/hooks/claude-session-start.sh"
  [ -x "$HOOK" ] || chmod +x "$HOOK"
  # register the SessionStart hook exactly once (stable absolute path)
  python3 - "$CLAUDE_SETTINGS" "$HOOK" <<'PYEOF' || echo "   WARN: could not register the claude SessionStart hook in $CLAUDE_SETTINGS"
import json, os, shutil, sys, time
path, hook = sys.argv[1], sys.argv[2]
cfg, existed = {}, os.path.exists(path)
if existed:
    try:
        cfg = json.load(open(path))
    except ValueError as e:
        print("   %s is not valid JSON (%s); hook NOT registered" % (path, e)); sys.exit(1)
    if not isinstance(cfg, dict):
        print("   %s is not a JSON object; hook NOT registered" % path); sys.exit(1)
hooks = cfg.get("hooks")
if hooks is None:
    hooks = cfg["hooks"] = {}
ss = hooks.get("SessionStart") if isinstance(hooks, dict) else False
if ss is None:
    ss = hooks["SessionStart"] = []
if not isinstance(hooks, dict) or not isinstance(ss, list):
    print("   unexpected hooks shape in %s; hook NOT registered" % path); sys.exit(1)
has = any(isinstance(e, dict) and any(isinstance(h, dict) and h.get("command") == hook
          for h in (e.get("hooks") or [])) for e in ss)
if has:
    print("   claude SessionStart hook already registered")
else:
    if existed:
        shutil.copy2(path, "%s.bak-polyforge-%s" % (path, time.strftime("%Y%m%d%H%M%S")))
    ss.append({"hooks": [{"type": "command", "command": hook}]})
    # the file may hold credentials (env.ANTHROPIC_*): the rewrite keeps
    # its mode, a new file is 0600 — never the umask default
    tmp = path + ".tmp"
    if os.path.lexists(tmp):
        os.unlink(tmp)
    with os.fdopen(os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as f:
        json.dump(cfg, f, indent=2); f.write("\n")
    if existed:
        shutil.copymode(path, tmp)
    os.replace(tmp, path)
    print("   claude SessionStart hook registered")
PYEOF
fi

# --- pi (independent of Claude Code) ---
# One LOCAL package entry naming release/current (pi loads a local path in
# place, never copies it; the release's package.json declares the skill
# and the pf-chat extension). Written straight into pi's settings: it
# replaces any git entry of this repo (the pre-#174 `pi install
# git:...@<ver>` clone) and duplicates; other packages are untouched.
if command -v pi >/dev/null 2>&1 || [ -d "$(dirname "$PI_SETTINGS")" ]; then
  python3 - "$PI_SETTINGS" "$REPO" "$RELEASE_DIR/current" <<'PYEOF' || echo "   WARN: could not set the pi package entry in $PI_SETTINGS"
import json, os, re, shutil, sys, time
path, repo, rel = sys.argv[1:4]
cfg, existed = {}, os.path.exists(path)
if existed:
    try:
        cfg = json.load(open(path))
    except ValueError as e:
        print("   %s is not valid JSON (%s); pi package NOT set" % (path, e)); sys.exit(1)
    if not isinstance(cfg, dict):
        print("   %s is not a JSON object; pi package NOT set" % path); sys.exit(1)
pk = cfg.get("packages")
if pk is None:
    pk = []
if not isinstance(pk, list):
    print("   unexpected packages shape in %s; pi package NOT set" % path); sys.exit(1)
base, home = os.path.dirname(path), os.path.expanduser("~")
def src(e): return e if isinstance(e, str) else (e.get("source", "") if isinstance(e, dict) else "")
def local(s):
    if not s or s.startswith(("npm:", "git:", "git@", "github.com")) or "://" in s: return None
    if s == "~" or s.startswith("~/"): s = home + s[1:]
    return os.path.normpath(os.path.join(base, s))
def ours(e):
    s = src(e)
    return re.search(r"github\.com[:/]" + re.escape(repo) + r"(\.git)?(@|$)", s) is not None or local(s) == os.path.normpath(rel)
idx = [i for i, e in enumerate(pk) if ours(e)]
for i, e in enumerate(pk):
    lp = local(src(e))
    if i not in idx and lp and os.path.isfile(os.path.join(lp, "skills", "polyforge", "SKILL.md")):
        print("   WARN: pi also loads another polyforge checkout (%s): remove it from %s, or pi loads the skill and pf-chat twice" % (src(e), path))
if len(idx) == 1 and src(pk[idx[0]]) == rel:
    print("   pi package -> release/current already"); sys.exit(0)
was = [src(pk[i]) for i in idx]
# an object entry keeps its other fields (filters) with the new source
entry = rel
first = pk[idx[0]] if idx else None
if isinstance(first, dict):
    entry = dict(first); entry["source"] = rel
new = [e for i, e in enumerate(pk) if i not in idx]
new.insert(idx[0] if idx else len(new), entry)
cfg["packages"] = new
os.makedirs(base, exist_ok=True)
if existed:
    shutil.copy2(path, "%s.bak-polyforge-%s" % (path, time.strftime("%Y%m%d%H%M%S")))
# the rewrite keeps the file's mode, a new file is 0600 (never the umask default)
tmp = path + ".tmp"
if os.path.lexists(tmp):
    os.unlink(tmp)
with os.fdopen(os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as f:
    json.dump(cfg, f, indent=2); f.write("\n")
if existed:
    shutil.copymode(path, tmp)
os.replace(tmp, path)
print("   pi package -> %s%s (restart pi sessions to load)" % (rel, (" (replaced: " + ", ".join(was) + ")") if was else ""))
PYEOF
fi
# Progress display: the same judgement as `pf doctor` (reads only; a
# warn row carries the copyable snippet, a skip row names an absent
# harness — user files are never edited).
"$BIN" doctor --harness 2>/dev/null | sed -e '/^ok:/d' -e '/^$/d' -e 's/^/   /'

# ─── 4. daemon: started only through `pf daemon ensure` ────────────
echo "== 4/5 daemon"
# the binary and release/current are in place: let ensure run again
if command -v flock >/dev/null 2>&1; then
  flock -u 9 2>/dev/null || true
  exec 9>&-
fi
# from here on step 4 starts the daemon or says why it does not
STOPPED=0
# the fix for a live daemon without its restart loop (same as pf doctor)
ENSURE_FIX="pf ops shutdown && while pf ops status >/dev/null 2>&1 || ! flock -n '$DAEMON_DATA/daemon.lock' true; do sleep 2; done; pf daemon ensure"
if [ ! -d "$WORKFLOWS" ]; then
  echo "   SKIP: $WORKFLOWS missing (release checkout incomplete) — daemon needs workflows"
elif [ -S "$DAEMON_SOCKET" ] && curl -s --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | grep -q machine_id; then
  echo "   daemon already running ($(curl -s --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | python3 -c "import json,sys; print(json.load(sys.stdin).get('machine_id','?'))" 2>/dev/null))"
  # Never auto-restart here: a live daemon may be running jobs.
  RUN_WF=$(curl -s -m 5 --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | python3 -c "import json,sys; print(json.load(sys.stdin).get('workflows_dir',''))" 2>/dev/null || true)
  if [ -n "$RUN_WF" ] && [ "$RUN_WF" != "$WORKFLOWS" ]; then
    echo "   ACTION: daemon runs with -workflows-dir $RUN_WF; restart it with -workflows-dir $WORKFLOWS (pf ops shutdown, wait for jobs to drain, rerun this installer)"
  fi
  if ! loop_running; then
    echo "   ACTION: the daemon runs without its restart loop (no tmux session $LOOP_SESSION): a crash is not restarted. When no job needs it, restart it under the loop: $ENSURE_FIX"
  fi
elif ! grep -q '^[[:space:]]*\[workspace' "$CFG"; then
  # polyforge#113: no real mapping in the config — the daemon would
  # refuse to start anyway. Do not launch it; the operator adds the
  # mapping first (the migration/template guidance from step 2).
  echo "   SKIP: no [workspace] mapping in $CFG yet — the daemon refuses to start until one exists"
  echo "   add a mapping (see the migration steps above or the commented template in the config), then: pf daemon ensure"
else
  # `pf daemon ensure` owns the launch (polyforge#174): the restart loop
  # in tmux runs the canonical argv — --config, -machine-id (recorded
  # daemon-id, else m-<short hostname>), -data-dir, -socket and
  # -workflows-dir release/current/workflows. polyforge#113: the
  # mapping-bearing config is the daemon's only workspace source and
  # carries the api_key, so neither appears in argv; auto and
  # max-concurrent-jobs stay the daemon's persisted settings (audit B4).
  if ENSURE_OUT=$(POLYFORGE_DATA_DIR="$DAEMON_DATA" "$BIN" daemon ensure --config "$CFG" 2>&1); then
    printf '%s\n' "$ENSURE_OUT" | sed 's/^/   /'
    echo "   NOTE: if heartbeat fails, admin must register this daemon's id (pf ops status shows it)"
  else
    printf '%s\n' "$ENSURE_OUT" | sed 's/^/   /'
    echo "   WARN: the daemon is not answering yet (a new machine id may need admin registration)"
    echo "   check: tail $DAEMON_DATA/daemon.log"
  fi
fi
# ─── 5. verify ──────────────────────────────────────────────────────
echo "== 5/5 verify"
# pf doctor is the one health judgement (aihub key and projects, daemon
# socket, mapping, harnesses); a fail is reported, never fatal here.
"$BIN" doctor 2>&1 | sed 's/^/   /' || true
echo ""
echo "Done. Daily use: 'pf queue' / 'pf submit <wi>' / 'pf watch <wi> --once'"
echo "From a pi session: just ask naturally (polyforge skill routes)."
