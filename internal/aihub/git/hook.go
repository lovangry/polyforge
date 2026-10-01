package git

// The pre-receive hook installed into every bare repo. It enforces the
// v1 capsule-chain invariants (v1-spec-full §六; secret scanning is NOT
// among them — dropped by ruling C1):
//
//   - FF-only: a pushed ref must move fast-forward;
//   - no ref deletion;
//   - no gitlink (mode 160000) anywhere in a pushed commit's tree;
//   - every commit carries the Polyforge-Seq trailer (capsule.SeqTrailer)
//     and the sequence increases monotonically, continuing above the
//     previous tip;
//   - blob size ≤ MaxBlobBytes (via the GIT_PF_MAX_BLOB env the proxy
//     sets; the default matches capsule.MaxBlobBytes);
//   - the session-fingerprint push option, when both sides of the
//     comparison exist, must equal PF_EXPECTED_FINGERPRINT (v1.5 session
//     fencing backup layer, contract §1.6: the proxy derives it from the
//     session the Basic credential authenticated against, so a superseded
//     session's fingerprint can never push even with a leaked secret).
//     Same structure as the v1 attempt-id backup layer, values renamed.
//
// The hook is a bash script exec'd by the real git receive-pack; the
// server process never interprets pack data itself.

import (
	"fmt"
	"os"
	"path/filepath"
)

// hookTemplate is the hook body. Kept as a function so the blob limit
// default can be baked in (the env override still wins at runtime).
func hookTemplate(defaultMaxBlob int64) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
# aihub pre-receive (v1). Rejects the WHOLE push on any violation.
set -u
MAXBLOB="${GIT_PF_MAX_BLOB:-%d}"
TRAILER="${GIT_PF_SEQ_TRAILER:-Polyforge-Seq}"
zero=0000000000000000000000000000000000000000
reject() { echo "aihub: $*" >&2; exit 1; }

# Session fencing backup layer (v1.5 §1.6): the proxy exports
# PF_EXPECTED_FINGERPRINT — the public session fingerprint the Basic
# credential authenticated against — and git exposes the push's options
# as GIT_PUSH_OPTION_COUNT + GIT_PUSH_OPTION_0..N (only when the repo
# advertises push options; note git spells the counter env in UPPERCASE).
# Find session-fingerprint=<value> among them and compare it STRICTLY
# with the expected fingerprint.
#
# When the comparison is enforced — only when BOTH sides exist:
#   - the push carries no session-fingerprint option at all: nothing to
#     compare, the gate stays open (older clients, and plain
#     fetch/upload traffic that never reaches a hook);
#   - PF_EXPECTED_FINGERPRINT is absent — a push that did not travel
#     through the proxy, e.g. a direct local test push against the bare
#     repo — has nothing to compare against, and the gate stays open:
#     the proxy is the only party that can vouch for which session a
#     credential belongs to, and inventing an expectation here would
#     fence off exactly the local tooling that must keep working.
# A mismatch (both present, different values) rejects the WHOLE push.
poptcount="${GIT_PUSH_OPTION_COUNT:-0}"
case "$poptcount" in ''|*[!0-9]*) poptcount=0 ;; esac
sent_fp=""
if [ -n "${PF_EXPECTED_FINGERPRINT:-}" ] && [ "$poptcount" -gt 0 ]; then
  i=0
  while [ "$i" -lt "$poptcount" ]; do
    eval "opt=\"\${GIT_PUSH_OPTION_$i:-}\""
    case "$opt" in
      session-fingerprint=*) sent_fp="${opt#session-fingerprint=}" ;;
    esac
    i=$((i+1))
  done
  if [ -n "$sent_fp" ] && [ "$sent_fp" != "$PF_EXPECTED_FINGERPRINT" ]; then
    reject "session-fingerprint push option '$sent_fp' does not match the credential's session fingerprint '$PF_EXPECTED_FINGERPRINT'"
  fi
fi

while read -r old new ref; do
  [ "$new" = "$zero" ] && reject "ref deletion is not allowed ($ref)"

  if [ "$old" != "$zero" ]; then
    git merge-base --is-ancestor "$old" "$new" \
      || reject "non-fast-forward push to $ref is not allowed"
    range="$old..$new"
    prev=$(git log -1 --format="%%(trailers:key=$TRAILER,valueonly)" "$old" | tr -d '[:space:]')
    case "$prev" in ''|*[!0-9]*) prev="" ;; esac
  else
    range="$new"
    prev=""
  fi

  # gitlink: mode 160000 entries are submodule commits; the capsule chain
  # must stay a single flat history.
  for c in $(git rev-list "$range"); do
    if git ls-tree -r "$c" | grep -q '^160000 '; then
      reject "gitlink (mode 160000) in commit $c is not allowed"
    fi
  done

  # blob size limit.
  badblob=$(git rev-list --objects "$range" | awk '{print $1}' \
    | git cat-file --batch-check='%%(objecttype) %%(objectsize) %%(objectname)' \
    | awk -v max="$MAXBLOB" '$1=="blob" && $2>max {print $3" ("$2" bytes)"}')
  [ -z "$badblob" ] || reject "blob exceeds the ${MAXBLOB}-byte limit: $badblob"

  # seq trailer: present, integer, strictly increasing, above the old tip.
  for c in $(git rev-list --reverse "$range"); do
    seq=$(git log -1 --format="%%(trailers:key=$TRAILER,valueonly)" "$c" | tr -d '[:space:]')
    case "$seq" in
      ''|*[!0-9]*) reject "commit $c is missing an integer $TRAILER trailer" ;;
    esac
    if [ -n "$prev" ]; then
      [ "$seq" -gt "$prev" ] || reject "$TRAILER must increase: commit $c has $seq, previous was $prev"
    fi
    prev="$seq"
  done
done
exit 0
`, defaultMaxBlob)
}

// writeHook writes the pre-receive hook and makes it executable.
func writeHook(path string, maxBlob int64) error {
	body := hookTemplate(maxBlob)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}
