#!/bin/sh
# polyforge's deploy face (polyforge#123): the project-owned, HUMAN-GATED
# counterpart to .polyforge/release.sh.
#
# Contract (the one sentence that matters):
#
#     A deploy is always triggered by a human running this script (or its
#     documented equivalent). NO automated path — no workflow step, no
#     daemon lane, no release procedure — may ever trigger a deploy. The
#     shared release dispatcher deliberately never references this file.
#
# What a deploy is here: promote an already-released version onto the
# running infrastructure — upgrade the aihub service binary on the service
# host, then upgrade the pf work-machine binary on each work machine, one
# machine at a time. The release (cut/tag/publish) already happened; the
# deploy only moves machines onto it.
#
# Prerequisites, checked below:
#   - the version to deploy exists in the shared publish root as
#     releases/<tag>/{aihub,pf,SHA256SUMS} (the release procedure's
#     published layout, checksum manifest included);
#   - the pf binary on THIS host works well enough to run `pf upgrade`;
#   - you are a human who has decided to deploy (this script never decides
#     anything by itself).
#
# Usage:
#   sh .polyforge/deploy.sh <tag>            # plan + confirm-gate + deploy
#   sh .polyforge/deploy.sh <tag> --dry-run  # print the plan, touch nothing
set -eu

TAG="${1:-}"
DRY=""
case "${2:-}" in
  --dry-run) DRY=1 ;;
  "") ;;
  *) echo "deploy: unknown flag $2 (use --dry-run or nothing)" >&2; exit 2 ;;
esac
if [ -z "$TAG" ]; then
  echo "usage: sh .polyforge/deploy.sh <tag> [--dry-run]" >&2
  exit 2
fi
case "$TAG" in
  v[0-9]*) ;;
  *) echo "deploy: tag must look like v<semver> (got \"$TAG\")" >&2; exit 2 ;;
esac

RELEASES_ROOT="${RELEASES_ROOT:?RELEASES_ROOT must name the shared publish root the release procedure published into}"
SRC="$RELEASES_ROOT/$TAG"
for f in aihub pf SHA256SUMS; do
  if [ ! -f "$SRC/$f" ]; then
    echo "deploy: $SRC/$f does not exist - release $TAG first (see .polyforge/release.sh), never deploy an unpublished version" >&2
    exit 1
  fi
done

echo "== deploy plan for $TAG =="
echo "1. (service host) upgrade the aihub service binary to $TAG, restart the service, watch it come back healthy."
echo "2. (each work machine, ONE at a time) run: pf upgrade   # downloads $TAG, verifies SHA256SUMS, quiesces, replaces, restarts the daemon"
echo "3. (after each machine) confirm the machine reports healthy and --version prints $TAG before touching the next one."
echo "Rollback: redeploy the previous tag's binaries the same way (per machine: pf upgrade from the previous release directory; service host: restore the previous aihub binary and restart)."
echo
if [ -n "$DRY" ]; then
  echo "dry-run: nothing executed."
  exit 0
fi

# The human confirm gate: the deploy happens because a person typed yes,
# never because an automation decided to.
printf "Deploy %s now? This upgrades the service host and every work machine. Type 'deploy' to proceed: " "$TAG"
read -r ANSWER
if [ "$ANSWER" != "deploy" ]; then
  echo "deploy: aborted by the operator (nothing was touched)."
  exit 1
fi

echo "deploy: manual sequence (this skeleton does not ssh anywhere — run each step yourself, in this order):"
echo
echo "  # --- service host (aihub) ---"
echo "  # verify the release manifest, then swap the service binary and restart:"
echo "  sh -c \"cd '$SRC' && sha256sum -c SHA256SUMS\""
echo "  install -m 0755 '$SRC/aihub' <service-bin-dir>/aihub.new && mv <service-bin-dir>/aihub.new <service-bin-dir>/aihub"
echo "  <restart the aihub service the way this host runs it>   # e.g. systemctl restart aihub"
echo "  aihub --version   # must print $TAG"
echo
echo "  # --- each work machine, ONE at a time (wait for green between machines) ---"
echo "  pf upgrade        # downloads $TAG, verifies its SHA256SUMS, quiesces, replaces, restarts the daemon"
echo "  pf --version      # must print $TAG"
echo "  pf ops status     # machine healthy before the next one"
echo
echo "  # --- rollback (any machine, including the service host) ---"
echo "  # redeploy the previous tag from the publish root the same way:"
echo "  #   service host: install the previous releases/<prev-tag>/aihub and restart."
echo "  #   work machine: pf upgrade against releases/<prev-tag>/ (or restore the previous binary and restart the daemon)."
echo
echo "deploy: follow the sequence above. This script's whole authority was the plan and your yes."
