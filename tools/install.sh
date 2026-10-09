#!/bin/sh
# polyforge bootstrap — complete machine setup in one command.
# Installs/updates: pf binary + config + skills + daemon.
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
PKG_DIR="$HOME/.pi/agent/git/github.com/$REPO"
BIN=/usr/local/bin/pf
CFG="$HOME/.config/polyforge/cli.toml"
DAEMON_DATA="$HOME/.local/share/polyforge"
DAEMON_SOCKET="$DAEMON_DATA/daemon.sock"

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
  mkdir -p /usr/local/bin
  # stop daemon if running (graceful)
  if [ -S "$DAEMON_SOCKET" ]; then
    curl -s -X POST --unix-socket "$DAEMON_SOCKET" http://localhost/ops/shutdown 2>/dev/null
    sleep 2
  fi
  mv "$TMP/pf-linux-amd64" "$BIN" && chmod +x "$BIN"
  rm -rf "$TMP"
  echo "   pf $RUNNING → $VER installed"
fi

# ─── 2. config: create or verify ────────────────────────────────────
echo "== 2/5 config"
mkdir -p "$(dirname "$CFG")"
if [ -f "$CFG" ] && grep -q "api_key" "$CFG"; then
  echo "   config exists ($(dirname "$CFG")/$(basename "$CFG"))"
else
  cat > "$CFG" <<EOF
server = "$API"
api_key = "$KEY"
daemon_socket = "$DAEMON_SOCKET"
EOF
  chmod 600 "$CFG"
  echo "   config written"
fi

# ─── 3. skills: install or verify ───────────────────────────────────
echo "== 3/5 skills"
SKILLS_OK() { [ -f "$PKG_DIR/package.json" ] && grep -q '"skills"' "$PKG_DIR/package.json" \
  && [ -f "$PKG_DIR/skills/polyforge/SKILL.md" ]; }

if command -v pi >/dev/null 2>&1; then
  if SKILLS_OK && [ -f "$PKG_DIR/.git" ]; then
    # already installed — update to latest tag
    CURRENT=$(git -C "$PKG_DIR" describe --tags 2>/dev/null || echo "?")
    if [ "$CURRENT" != "$VER" ]; then
      pi install "git:git@github.com:$REPO.git@$VER" 2>/dev/null
      SKILLS_OK && echo "   skills $CURRENT → $VER" || echo "   WARN: skills update incomplete"
    else
      echo "   skills $VER already current"
    fi
  else
    pi install "git:git@github.com:$REPO.git@$VER" 2>/dev/null
    if SKILLS_OK; then
      echo "   skills $VER installed (restart session to load)"
    else
      echo "ERROR: skills install failed. Manual:"
      echo "  git clone -q git@github.com:$REPO.git /tmp/pf-pkg"
      echo "  cp -r /tmp/pf-pkg/skills/polyforge ~/.pi/agent/skills/"
      exit 1
    fi
  fi
elif [ -d "$HOME/.claude" ]; then
  git clone -q git@github.com:$REPO.git /tmp/pf-pkg 2>/dev/null \
    && mkdir -p "$HOME/.claude/skills" \
    && cp -r /tmp/pf-pkg/skills/polyforge "$HOME/.claude/skills/" \
    && rm -rf /tmp/pf-pkg && echo "   skills copied"
else
  echo "   no pi/claude — CLI only (skills optional)"
fi

# ─── 4. daemon: start if not running ────────────────────────────────
echo "== 4/5 daemon"
WORKFLOWS="$PKG_DIR/workflows"
if [ ! -d "$WORKFLOWS" ]; then
  echo "   SKIP: repo not cloned (skills step incomplete) — daemon needs workflows"
elif [ -S "$DAEMON_SOCKET" ] && curl -s --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | grep -q machine_id; then
  echo "   daemon already running ($(curl -s --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | python3 -c "import json,sys; print(json.load(sys.stdin).get('machine_id','?'))" 2>/dev/null))"
else
  MACHINE_ID=$(hostname -s | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/-/g')
  # The installer (not the daemon) owns workspace creation: the daemon
  # requires the dir to pre-exist and creates only capsules/ inside it.
  mkdir -p "$DAEMON_DATA/ws"
  if [ ! -f "$DAEMON_DATA/ws/.gitignore" ] || ! grep -qx 'capsules/' "$DAEMON_DATA/ws/.gitignore"; then
    printf 'capsules/\n' >> "$DAEMON_DATA/ws/.gitignore"
    echo "   added capsules/ to $DAEMON_DATA/ws/.gitignore (daemon never touches .gitignore)"
  fi
  nohup "$BIN" daemon \
    -server "$API" \
    -api-key "$KEY" \
    -machine-id "m-$MACHINE_ID" \
    -mode interactive -project polyforge \
    -data-dir "$DAEMON_DATA" \
    -socket "$DAEMON_SOCKET" \
    -workspace "$DAEMON_DATA/ws" \
    -workflows-dir "$WORKFLOWS" \
    -max-concurrent-jobs 4 \
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
  || echo "   ✗ pf queue failed — check key ($KEY)"
[ -S "$DAEMON_SOCKET" ] && curl -s --unix-socket "$DAEMON_SOCKET" http://localhost/status 2>/dev/null | grep -q machine_id \
  && echo "   ✓ daemon alive" \
  || echo "   ✗ daemon not responding"
echo ""
echo "Done. Daily use: 'pf queue' / 'pf submit <wi>' / 'pf observe <wi>'"
echo "From a pi session: just ask naturally (polyforge skill routes)."
