#!/bin/sh
# polyforge bootstrap — curl|bash one-liner installer.
# Usage: curl -s -H "Authorization: token $(gh auth token)" \
#   https://raw.githubusercontent.com/silgrid/polyforge/main/tools/install.sh \
#   | bash -s -- <api-key>
set -e
KEY="${1:?usage: install.sh <api-key>}"
HOST="${PF_HOST:-10.146.0.34}"
REL="http://$HOST:8090"; API="http://$HOST:8080"
PKG_DIR="$HOME/.pi/agent/git/github.com/silgrid/polyforge"
SKILLS_OK() { [ -f "$PKG_DIR/package.json" ] && grep -q '"skills"' "$PKG_DIR/package.json" \
  && [ -f "$PKG_DIR/operator-skills/polyforge/SKILL.md" ]; }

echo "== 1/4 pf binary"
mkdir -p "$HOME/bin"
LATEST=$(curl -s "$REL/latest.txt"); VER=$(echo "$LATEST" | awk '{print $1}'); SHA=$(echo "$LATEST" | awk '{print $2}')
[ -n "$VER" ] || { echo "ERROR: cannot read $REL/latest.txt (release server down?)"; exit 1; }
curl -s -o "$HOME/bin/pf" "$REL/pf-$VER" && chmod +x "$HOME/bin/pf"
GOT=$(sha256sum "$HOME/bin/pf" | awk '{print $1}')
[ "$GOT" = "$SHA" ] || { echo "ERROR: pf binary SHA mismatch ($GOT != $SHA) — removed"; rm -f "$HOME/bin/pf"; exit 1; }
echo "   pf $VER ok"

echo "== 2/4 config"
CFG_DIR="$HOME/.config/polyforge"
mkdir -p "$CFG_DIR"
cat > "$CFG_DIR/cli.toml" <<EOF
server = "$API"
api_key = "$KEY"
release_url = "$REL"
EOF
chmod 600 "$CFG_DIR/cli.toml"

echo "== 3/4 skills (pi harness)"
if command -v pi >/dev/null 2>&1; then
  pi install "git:git@github.com:silgrid/polyforge.git@$VER"
  if SKILLS_OK; then
    echo "   pi: skills installed from $VER (restart session to load)"
  elif [ -d "$PKG_DIR" ] && [ ! -f "$PKG_DIR/package.json" ]; then
    echo "ERROR: tag $VER has no pi package manifest (package.json missing)."
    echo "  The release server's latest.txt is stale or the tag predates the"
    echo "  operator-skills packaging. Fix: tag a newer commit and update"
    echo "  latest.txt. Manual fallback on this machine:"
    echo "    git clone -q git@github.com:silgrid/polyforge.git /tmp/pf-pkg"
    echo "    cp -r /tmp/pf-pkg/operator-skills/* ~/.pi/agent/skills/"
    exit 1
  else
    echo "ERROR: pi install reported success but skills are missing in $PKG_DIR."
    echo "  Manual fallback:"
    echo "    git clone -q git@github.com:silgrid/polyforge.git /tmp/pf-pkg"
    echo "    cp -r /tmp/pf-pkg/operator-skills/* ~/.pi/agent/skills/"
    exit 1
  fi
elif [ -d "$HOME/.claude" ]; then
  git clone -q git@github.com:silgrid/polyforge.git /tmp/pf-pkg \
    && cp -r /tmp/pf-pkg/operator-skills/* "$HOME/.claude/skills/" \
    && rm -rf /tmp/pf-pkg && echo "   skills copied (harness skills dir)"
else
  echo "   no pi detected — CLI-only install (skills optional)"
fi

echo "== 4/4 connectivity"
"$HOME/bin/pf" queue --project polyforge > /dev/null 2>&1 \
  && echo "   OK — pf works. (path: add ~/bin to PATH; daemon setup: see /pf-init Step 4)" \
  || { echo "   WARN: pf queue failed — check key/network (pf doctor)"; }
