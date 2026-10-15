#!/bin/sh
# polyforge bootstrap — complete machine setup in one command.
# Installs/updates (idempotent: a second run changes nothing):
#   1. pf binary        — latest release, SHA-verified; on an upgrade the
#        daemon is shut down gracefully (its restart loop, tmux session
#        polyforge-daemon, ends with it on that clean exit), install waits
#        until both are gone, then swaps the binary. From here until step
#        4 the installer holds <data-dir>/ensure.lock, so a harness
#        session's `pf daemon ensure` in that window starts nothing (no old
#        daemon revived, no new binary against the old release)
#   2. config           — created once, NEVER rewritten
#   3. release + skills — own checkout of the release tag in
#        ~/.local/share/polyforge/release/<VER> (stable symlink release/current,
#        swapped atomically, previous version kept); the harnesses REFERENCE
#        it, nothing is copied or cloned again (polyforge#174):
#        Claude Code: ~/.claude/skills/polyforge is a symlink to
#        release/current/skills/polyforge (an old copied directory is
#        replaced) + SessionStart hook registered once in
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

# ─── 1. pf binary: install or update ───────────────────────────────
echo "== 1/5 pf binary"
# Hold the ensure lock (the flock `pf daemon ensure` takes) from here
# through the release swap in step 3: an adapter's ensure (a pi or Claude
# Code session starting, resuming, compacting) waits on it and then
# answers by the socket — it never starts a loop mid-upgrade. Released
# right before step 4's own ensure.
mkdir -p "$DAEMON_DATA"
if command -v flock >/dev/null 2>&1; then
  exec 9>>"$DAEMON_DATA/ensure.lock"
  flock -w 120 9 || echo "   WARN: another pf daemon ensure held $DAEMON_DATA/ensure.lock for 120s; continuing"
else
  echo "   WARN: flock not found: a harness session starting during this upgrade could start the daemon early"
fi
command -v gh >/dev/null 2>&1 || { echo "ERROR: gh not found"; exit 1; }
VER=$(gh release list --repo "$REPO" --limit 1 --json tagName --jq '.[0].tagName' 2>/dev/null)
[ -n "$VER" ] || { echo "ERROR: no release found"; exit 1; }

RUNNING="none"
if [ -x "$BIN" ]; then
  RUNNING=$("$BIN" --version 2>/dev/null || echo "unknown")
fi

if [ "$RUNNING" = "$VER" ]; then
  echo "   pf $VER already current, skipping"
else
  TMP=$(mktemp -d)
  gh release download "$VER" --repo "$REPO" --pattern "pf-linux-amd64" --dir "$TMP" 2>/dev/null \
    || { echo "ERROR: binary not on release $VER"; rm -rf "$TMP"; exit 1; }
  gh release download "$VER" --repo "$REPO" --pattern "SHA256SUMS" --dir "$TMP" 2>/dev/null
  if [ -f "$TMP/SHA256SUMS" ]; then
    EXP=$(grep "pf-linux-amd64" "$TMP/SHA256SUMS" | awk '{print $1}')
    GOT=$(sha256sum "$TMP/pf-linux-amd64" | awk '{print $1}')
    [ "$GOT" = "$EXP" ] || { echo "ERROR: SHA mismatch"; rm -rf "$TMP"; exit 1; }
  fi
  mkdir -p "$(dirname "$BIN")"
  # Upgrade order: stop the daemon gracefully — a clean exit also ends
  # its restart loop — and wait until the daemon AND the loop are really
  # gone: the binary is never swapped under a live daemon, and no loop is
  # left to start the old binary again. A loop left without a daemon
  # (between two restarts after a failure, or an older loop) is killed.
  # The wait must exceed the daemon's graceful ShutdownBudget (5
  # minutes), hence 330s. Step 4 starts the new daemon through
  # `pf daemon ensure`.
  if daemon_running || loop_running; then
    echo "   stopping daemon (graceful; its restart loop ends with it)"
    if [ -x "$BIN" ] && "$BIN" ops shutdown >/dev/null 2>&1; then
      :
    else
      curl -s -m 5 -X POST --unix-socket "$DAEMON_SOCKET" http://localhost/shutdown >/dev/null 2>&1 || true
    fi
    WAITED=0
    while daemon_running || loop_running; do
      if ! daemon_running && loop_running; then
        tmux_q kill-session -t "=$LOOP_SESSION" 2>/dev/null || true
        echo "   stopped a leftover restart loop (tmux session $LOOP_SESSION)"
      fi
      if [ "$WAITED" -ge 330 ]; then
        echo "ERROR: daemon did not stop within 330s (graceful shutdown budget is 300s); binary NOT replaced. Its restart loop ended with the shutdown request: once the daemon exits, this machine has NO daemon until \`pf daemon ensure\` runs (by hand, or at the next pi / Claude Code session start). Check: tail $DAEMON_DATA/daemon.log"
        rm -rf "$TMP"
        exit 1
      fi
      sleep 1
      WAITED=$((WAITED + 1))
    done
    echo "   daemon stopped"
  fi
  mv "$TMP/pf-linux-amd64" "$BIN" && chmod +x "$BIN"
  rm -rf "$TMP"
  echo "   pf $RUNNING → $VER installed"
fi

# ─── 2. config: create or verify (NEVER rewritten) ──────────────────
echo "== 2/5 config"
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
  cat > "$CFG" <<CFGEOF
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
# ─── 3. release checkout + skills ───────────────────────────────────
# The installer owns a checkout of the release tag, independent of any
# harness: the daemon's workflows and every harness's skill come from it.
echo "== 3/5 release + skills"
mkdir -p "$RELEASE_DIR"
REL="$RELEASE_DIR/$VER"
if [ -f "$REL/skills/polyforge/SKILL.md" ] && [ -d "$REL/workflows" ]; then
  echo "   release checkout $VER present"
else
  rm -rf "$REL.tmp.$$"
  # gh uses its own credentials (ssh or https), so a machine that only has
  # gh auth can clone the private repo. The tag is cloned shallow.
  gh repo clone "$REPO" "$REL.tmp.$$" -- --branch "$VER" --depth 1 >/dev/null 2>&1 \
    || { echo "ERROR: cannot clone $REPO at $VER (gh auth?)"; rm -rf "$REL.tmp.$$"; exit 1; }
  if [ ! -f "$REL.tmp.$$/skills/polyforge/SKILL.md" ] || [ ! -d "$REL.tmp.$$/workflows" ]; then
    echo "ERROR: $VER checkout lacks skills/polyforge or workflows"
    rm -rf "$REL.tmp.$$"
    exit 1
  fi
  rm -rf "$REL"
  mv "$REL.tmp.$$" "$REL"
  echo "   release checkout $VER fetched"
fi
# stable symlink, swapped atomically (relative target: the tree is relocatable)
PREV=$(readlink "$RELEASE_DIR/current" 2>/dev/null || true)
if [ "$PREV" = "$VER" ]; then
  echo "   release/current -> $VER already"
else
  ln -sfn "$VER" "$RELEASE_DIR/.current.tmp.$$" && mv -T "$RELEASE_DIR/.current.tmp.$$" "$RELEASE_DIR/current"
  echo "   release/current -> $VER (was ${PREV:-none})"
  # keep the new and the previous version, prune the rest
  for d in "$RELEASE_DIR"/*; do
    if [ ! -d "$d" ] || [ -L "$d" ]; then continue; fi
    n=$(basename "$d")
    [ "$n" = "$VER" ] || [ "$n" = "$PREV" ] || { rm -rf "$d"; echo "   pruned old release $n"; }
  done
fi
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
    with open(path + ".tmp", "w") as f:
        json.dump(cfg, f, indent=2); f.write("\n")
    os.replace(path + ".tmp", path)
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
with open(path + ".tmp", "w") as f:
    json.dump(cfg, f, indent=2); f.write("\n")
os.replace(path + ".tmp", path)
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
# the fix for a live daemon without its restart loop (same as pf doctor)
ENSURE_FIX="pf ops shutdown && while pf ops status >/dev/null 2>&1 || ! flock -n '$DAEMON_DATA/daemon.lock' true; do sleep 2; done; pf daemon ensure"
if [ ! -d "$WORKFLOWS" ]; then
  echo "   SKIP: $WORKFLOWS missing (release checkout incomplete) — daemon needs workflows"
elif [ -S "$DAEMON_SOCKET" ] && curl -s --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | grep -q machine_id; then
  echo "   daemon already running ($(curl -s --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | python3 -c "import json,sys; print(json.load(sys.stdin).get('machine_id','?'))" 2>/dev/null))"
  # /status does not expose -workflows-dir; read it from the daemon's argv.
  # Never auto-restart here: a live daemon may be running jobs.
  RUN_WF=$(python3 - "$DAEMON_SOCKET" <<'PYEOF' 2>/dev/null || true
import glob, sys
sock = sys.argv[1]
for f in glob.glob("/proc/[0-9]*/cmdline"):
    try:
        a = open(f, "rb").read().decode("utf-8", "replace").split("\0")
    except OSError:
        continue
    if "daemon" not in a:
        continue
    if not any(x.lstrip("-") == "socket=" + sock or (x.lstrip("-") == "socket" and a[i + 1:i + 2] == [sock])
               for i, x in enumerate(a)):
        continue
    for i, x in enumerate(a):
        if x.lstrip("-").startswith("workflows-dir="):
            print(x.split("=", 1)[1]); sys.exit(0)
        if x.lstrip("-") == "workflows-dir" and i + 1 < len(a):
            print(a[i + 1]); sys.exit(0)
PYEOF
)
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
  # The installer (not the daemon) owns workspace creation: the daemon
  # requires every mapped workspace dir to pre-exist and creates only
  # capsules/ inside each.
  mkdir -p "$DAEMON_DATA/ws"
  if [ ! -f "$DAEMON_DATA/ws/.gitignore" ] || ! grep -qx 'capsules/' "$DAEMON_DATA/ws/.gitignore"; then
    printf 'capsules/\n' >> "$DAEMON_DATA/ws/.gitignore"
    echo "   added capsules/ to $DAEMON_DATA/ws/.gitignore (daemon never touches .gitignore)"
  fi
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
"$BIN" queue --project polyforge > /dev/null 2>&1 \
  && echo "   ✓ pf queue works" \
  || echo "   ✗ pf queue failed — check api_key in $CFG"
[ -S "$DAEMON_SOCKET" ] && curl -s --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | grep -q machine_id \
  && echo "   ✓ daemon alive" \
  || echo "   ✗ daemon not responding"
echo ""
echo "Done. Daily use: 'pf queue' / 'pf submit <wi>' / 'pf watch <wi> --once'"
echo "From a pi session: just ask naturally (polyforge skill routes)."
