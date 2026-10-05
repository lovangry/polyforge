#!/bin/sh
# polyforge bootstrap — curl|bash one-liner installer.
# Usage: curl -s http://<host>:8090/install.sh | bash -s -- <api-key>
# (skills + binary + config; execution machines additionally follow pf-init Step 4)
set -e
KEY="${1:?usage: curl -s http://<host>:8090/install.sh | bash -s -- <api-key>}"
HOST="__HOST__"   # baked at deploy time
REL="http://$HOST:8090"; API="http://$HOST:8080"

echo "== 1/4 pf binary"
mkdir -p "$HOME/bin"
LATEST=$(curl -s "$REL/latest.txt"); VER=$(echo "$LATEST" | awk '{print $1}'); SHA=$(echo "$LATEST" | awk '{print $2}')
curl -s -o "$HOME/bin/pf" "$REL/pf-$VER" && chmod +x "$HOME/bin/pf"
GOT=$(sha256sum "$HOME/bin/pf" | awk '{print $1}')
[ "$GOT" = "$SHA" ] || { echo "SHA mismatch: $GOT != $SHA"; exit 1; }
echo "   pf $VER ok"

echo "== 2/4 config"
mkdir -p "$HOME/.polyforge"
cat > "$HOME/.polyforge/config.toml" <<EOF
url = "$API"
api_key = "$KEY"
release_url = "$REL"
EOF
chmod 600 "$HOME/.polyforge/config.toml"

echo "== 3/4 skills (harness autodetect)"
if command -v pi >/dev/null 2>&1; then
  pi install "git:git@github.com:silgrid/polyforge.git@$VER" 2>/dev/null && echo "   pi: skills installed (restart session to load)" \
    || echo "   pi: package install failed — copy operator-skills/ manually"
elif [ -d "$HOME/.claude" ]; then
  git clone -q --depth 1 -b "$VER" git@github.com:silgrid/polyforge.git /tmp/pf-pkg 2>/dev/null \
    && cp -r /tmp/pf-pkg/operator-skills/* "$HOME/.claude/skills/" 2>/dev/null \
    && rm -rf /tmp/pf-pkg && echo "   claude: skills copied"
else
  echo "   no pi/claude detected — CLI-only install (skills optional)"
fi

echo "== 4/4 connectivity"
"$HOME/bin/pf" queue --project polyforge > /dev/null 2>&1 \
  && echo "   OK — pf works. (path: add ~/bin to PATH; daemon setup: see /pf-init Step 4)" \
  || echo "   WARN: pf queue failed — check key/network (pf doctor)"
