#!/bin/sh
# polyforge bootstrap — curl|bash one-liner installer.
# Usage: curl -s -H "Authorization: token $(gh auth token)" \
#   https://raw.githubusercontent.com/silgrid/polyforge/main/tools/install.sh \
#   | bash -s -- <api-key> [server-host]
#
# Requires: gh authenticated (repo is private).
# Everything (script, binary, skills) comes from GitHub — no local
# release server. Binary from GitHub Releases, skills via pi install
# from the git tag.
set -e
KEY="${1:?usage: install.sh <api-key> [server-host]}"
HOST="${2:-${PF_HOST:-10.146.0.34}}"
API="http://$HOST:8080"
REPO="silgrid/polyforge"
PKG_DIR="$HOME/.pi/agent/git/github.com/$REPO"
SKILLS_OK() { [ -f "$PKG_DIR/package.json" ] && grep -q '"skills"' "$PKG_DIR/package.json" \
  && [ -f "$PKG_DIR/operator-skills/polyforge/SKILL.md" ]; }

echo "== 1/4 pf binary (GitHub Releases)"
command -v gh >/dev/null 2>&1 || { echo "ERROR: gh not found — install github.com/cli/cli first"; exit 1; }

# Latest tag with a release
VER=$(gh release list --repo "$REPO" --limit 1 --json tagName --jq '.[0].tagName' 2>/dev/null)
[ -n "$VER" ] || { echo "ERROR: no GitHub release found — create one at github.com/$REPO/releases"; exit 1; }

# Download + verify
TMP=$(mktemp -d)
gh release download "$VER" --repo "$REPO" --pattern "pf-linux-amd64" --output-dir "$TMP" 2>/dev/null \
  || { echo "ERROR: binary asset 'pf-linux-amd64' not found on release $VER"; rm -rf "$TMP"; exit 1; }
gh release download "$VER" --repo "$REPO" --pattern "SHA256SUMS" --output-dir "$TMP" 2>/dev/null

if [ -f "$TMP/SHA256SUMS" ]; then
  EXPECTED=$(grep "pf-linux-amd64" "$TMP/SHA256SUMS" | awk '{print $1}')
  GOT=$(sha256sum "$TMP/pf-linux-amd64" | awk '{print $1}')
  [ "$GOT" = "$EXPECTED" ] || { echo "ERROR: SHA mismatch ($GOT != $EXPECTED)"; rm -rf "$TMP"; exit 1; }
  echo "   SHA verified"
fi

mkdir -p /usr/local/bin
mv "$TMP/pf-linux-amd64" /usr/local/bin/pf && chmod +x /usr/local/bin/pf
rm -rf "$TMP"
echo "   pf $VER installed to /usr/local/bin/pf"

echo "== 2/4 config"
CFG_DIR="$HOME/.config/polyforge"
mkdir -p "$CFG_DIR"
cat > "$CFG_DIR/cli.toml" <<EOF
server = "$API"
api_key = "$KEY"
EOF
chmod 600 "$CFG_DIR/cli.toml"

echo "== 3/4 skills (pi harness)"
if command -v pi >/dev/null 2>&1; then
  pi install "git:git@github.com:$REPO.git@$VER"
  if SKILLS_OK; then
    echo "   pi: skills installed from $VER (restart session to load)"
  elif [ -d "$PKG_DIR" ] && [ ! -f "$PKG_DIR/package.json" ]; then
    echo "ERROR: tag $VER has no pi package manifest (package.json missing)."
    echo "  The release predates the operator-skills packaging."
    echo "  Manual fallback:"
    echo "    git clone -q git@github.com:$REPO.git /tmp/pf-pkg"
    echo "    cp -r /tmp/pf-pkg/operator-skills/* ~/.pi/agent/skills/"
    exit 1
  else
    echo "ERROR: pi install reported success but skills are missing in $PKG_DIR."
    echo "  Manual fallback:"
    echo "    git clone -q git@github.com:$REPO.git /tmp/pf-pkg"
    echo "    cp -r /tmp/pf-pkg/operator-skills/* ~/.pi/agent/skills/"
    exit 1
  fi
elif [ -d "$HOME/.claude" ]; then
  git clone -q git@github.com:$REPO.git /tmp/pf-pkg \
    && cp -r /tmp/pf-pkg/operator-skills/* "$HOME/.claude/skills/" \
    && rm -rf /tmp/pf-pkg && echo "   skills copied (harness skills dir)"
else
  echo "   no pi detected — CLI-only install (skills optional)"
fi

echo "== 4/4 connectivity"
/usr/local/bin/pf queue --project polyforge > /dev/null 2>&1 \
  && echo "   OK — pf works. (daemon setup: see /pf-init Step 4)" \
  || { echo "   WARN: pf queue failed — check key/network"; }
