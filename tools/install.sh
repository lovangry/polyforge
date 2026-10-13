#!/bin/sh
# polyforge bootstrap — complete machine setup in one command.
# Installs/updates (idempotent: a second run changes nothing):
#   1. pf binary        — latest release, SHA-verified, daemon stopped first
#   2. config           — created once, NEVER rewritten
#   3. release + skills — own checkout of the release tag in
#        ~/.local/share/polyforge/release/<VER> (stable symlink release/current,
#        swapped atomically, previous version kept); then, independently:
#        Claude Code: skill replaced wholesale + SessionStart hook registered
#        once in ~/.claude/settings.json; pi: package entry pinned to <VER>
#   4. daemon           — started with -workflows-dir release/current/workflows
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
  # stop the daemon gracefully, then wait until it is really gone: the
  # binary is never swapped under a live daemon. The wait must exceed the
  # daemon's graceful ShutdownBudget (5 minutes), hence 330s.
  if daemon_running; then
    echo "   stopping daemon (graceful)"
    if [ -x "$BIN" ] && "$BIN" ops shutdown >/dev/null 2>&1; then
      :
    else
      curl -s -m 5 -X POST --unix-socket "$DAEMON_SOCKET" http://localhost/shutdown >/dev/null 2>&1 || true
    fi
    WAITED=0
    while daemon_running; do
      if [ "$WAITED" -ge 330 ]; then
        echo "ERROR: daemon did not stop within 330s (graceful shutdown budget is 300s); binary NOT replaced. Check: tail $DAEMON_DATA/daemon.log"
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
# `~` at the start of a key expands to your home directory; keys must
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
if [ -d "$HOME/.claude" ] || command -v claude >/dev/null 2>&1; then
  CSK="$HOME/.claude/skills/polyforge"
  mkdir -p "$HOME/.claude/skills"
  if [ -d "$CSK" ] && diff -r "$SRC_SKILL" "$CSK" >/dev/null 2>&1; then
    echo "   claude skill $VER already current"
  else
    # replace wholesale: copy beside, swap, drop the old (no stale files)
    rm -rf "$CSK.new.$$" "$CSK.old.$$"
    cp -R "$SRC_SKILL" "$CSK.new.$$"
    [ -d "$CSK" ] && mv "$CSK" "$CSK.old.$$"
    mv "$CSK.new.$$" "$CSK"
    rm -rf "$CSK.old.$$"
    echo "   claude skill replaced from $VER"
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
sl = cfg.get("statusLine")
if "pf watch" not in json.dumps(sl or {}):
    print('   hint: for the progress line set in %s: "statusLine": {"type": "command", "command": "pf watch --statusline --harness-session -"}' % path)
PYEOF
fi

# --- pi (independent of Claude Code) ---
# `pi install <src>@<ref>` REPLACES an existing entry of the same repo at
# another ref (package-manager.js addSourceToSettings), so the pin check
# reads the settings entry, never the checkout.
if command -v pi >/dev/null 2>&1; then
  PI_SRC="git:git@github.com:$REPO.git"
  pi_pin() { # print "<n-entries> <refs...>" ; with "fix" dedupe to $VER
    python3 - "$PI_SETTINGS" "$REPO" "$VER" "$1" <<'PYEOF'
import json, os, re, shutil, sys, time
path, repo, ver, mode = sys.argv[1:5]
try:
    cfg = json.load(open(path))
except (OSError, ValueError):
    print("0"); sys.exit(0)
pk = cfg.get("packages") or []
def src(e): return e if isinstance(e, str) else (e.get("source", "") if isinstance(e, dict) else "")
def mine(e): return re.search(r"github\.com[:/]" + re.escape(repo) + r"(\.git)?(@|$)", src(e)) is not None
idx = [i for i, e in enumerate(pk) if mine(e)]
if mode == "fix" and len(idx) > 1:
    keep = next((i for i in idx if src(pk[i]).endswith("@" + ver)), idx[-1])
    shutil.copy2(path, "%s.bak-polyforge-%s" % (path, time.strftime("%Y%m%d%H%M%S")))
    cfg["packages"] = [e for i, e in enumerate(pk) if i not in idx or i == keep]
    with open(path + ".tmp", "w") as f:
        json.dump(cfg, f, indent=2); f.write("\n")
    os.replace(path + ".tmp", path)
    pk = cfg["packages"]; idx = [i for i, e in enumerate(pk) if mine(e)]
print("%d %s" % (len(idx), " ".join((re.search(r"@([^@/:]+)$", src(pk[i])) or [0, "-"])[1] for i in idx)))
PYEOF
  }
  if [ "$(pi_pin check)" = "1 $VER" ]; then
    echo "   pi package already pinned to $VER"
  else
    pi install "$PI_SRC@$VER" >/dev/null 2>&1 || echo "   WARN: pi install $PI_SRC@$VER failed"
    # pi replaces a same-repo entry; only a hand-made duplicate needs cleanup
    PIN=$(pi_pin fix)
    if [ "$PIN" = "1 $VER" ]; then
      echo "   pi package pinned to $VER (restart session to load)"
    else
      echo "   WARN: pi package entry not at $VER (settings: $PIN) — check $PI_SETTINGS"
    fi
  fi
fi
command -v pi >/dev/null 2>&1 || [ -d "$HOME/.claude" ] || command -v claude >/dev/null 2>&1 \
  || echo "   no pi/claude — CLI only (skills optional)"

# ─── 4. daemon: start only when a real mapping exists ────────────────
echo "== 4/5 daemon"
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
elif ! grep -q '^[[:space:]]*\[workspace' "$CFG"; then
  # polyforge#113: no real mapping in the config — the daemon would
  # refuse to start anyway. Do not launch it; the operator adds the
  # mapping first (the migration/template guidance from step 2).
  echo "   SKIP: no [workspace] mapping in $CFG yet — the daemon refuses to start until one exists"
  echo "   add a mapping (see the migration steps above or the commented template in the config), then: pf daemon"
else
  MACHINE_ID=$(hostname -s | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/-/g')
  # The installer (not the daemon) owns workspace creation: the daemon
  # requires every mapped workspace dir to pre-exist and creates only
  # capsules/ inside each.
  mkdir -p "$DAEMON_DATA/ws"
  if [ ! -f "$DAEMON_DATA/ws/.gitignore" ] || ! grep -qx 'capsules/' "$DAEMON_DATA/ws/.gitignore"; then
    printf 'capsules/\n' >> "$DAEMON_DATA/ws/.gitignore"
    echo "   added capsules/ to $DAEMON_DATA/ws/.gitignore (daemon never touches .gitignore)"
  fi
  # polyforge#113: the mapping-bearing config is the daemon's only
  # workspace source — the daemon argv carries neither a workspace nor a
  # project flag, ever. The api_key is read from the same config, so it
  # never appears in argv. The daemon's OWN persisted settings (and its
  # first-boot defaults) govern auto and max-concurrent-jobs — the
  # installer never forces operator settings on the argv (audit B4: an
  # upgrade must not wipe daemon-state.json's auto/max).
  nohup "$BIN" daemon \
    --config "$CFG" \
    -machine-id "m-$MACHINE_ID" \
    -data-dir "$DAEMON_DATA" \
    -socket "$DAEMON_SOCKET" \
    -workflows-dir "$WORKFLOWS" \
    > "$DAEMON_DATA/daemon.log" 2>&1 &
  DAEMON_PID=$!
  sleep 3
  if curl -s --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | grep -q machine_id; then
    echo "   daemon started (m-$MACHINE_ID, pid $DAEMON_PID)"
    echo "   NOTE: if heartbeat fails, admin must register m-$MACHINE_ID first"
  else
    echo "   WARN: daemon may need admin to register m-$MACHINE_ID"
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
