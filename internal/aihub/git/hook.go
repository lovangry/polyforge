package git

// The pre-receive hook installed into every bare repo. It enforces the
// v1 capsule-chain invariants (v1-spec-full §六; secret scanning is NOT
// among them — dropped by ruling C1):
//
//   - FF-only: a pushed ref must move fast-forward (refs/pf-wip/*
//     excepted, see below);
//   - no ref deletion (refs/pf-wip/* excepted);
//   - no gitlink (mode 160000) anywhere in a pushed commit's tree;
//   - every commit carries the Polyforge-Seq trailer (capsule.SeqTrailer)
//     and the sequence increases monotonically, continuing above the
//     previous tip;
//   - blob size ≤ MaxBlobBytes (via the GIT_PF_MAX_BLOB env the proxy
//     sets; the default matches capsule.MaxBlobBytes) — for the
//     refs/pf-task/* carrier refs the limit is MaxTaskBlobBytes instead
//     (GIT_PF_MAX_TASK_BLOB; default capsule.MaxTaskBlobBytes);
//   - the session-fingerprint push option must be present and equal
//     PF_EXPECTED_FINGERPRINT whenever the push came through the proxy,
//     which sets that variable (session fencing: the proxy
//     derives it from the session the Basic credential authenticated
//     against, so a superseded session's fingerprint can never push even
//     with a leaked secret). A direct local push with no
//     PF_EXPECTED_FINGERPRINT has nothing to compare and is not fenced.
//
// The refs/pf-task/<repo> namespace is the task-repo durability lane
// (polyforge#108): the daemon's publish boundary mirrors each task
// repo's LOCAL head there so a bounded-fail re-claim on a fresh session
// can restore it. The history under these refs is the TASK repo's own —
// it carries no Polyforge-Seq trailer and may legitimately contain
// submodules — so the chain-only seq and gitlink invariants do not
// apply to it. Deletion, fast-forward and the blob limit stay enforced
// for every ref; the carrier refs get the larger task-repo blob limit,
// because the first carrier push of a repo carries its whole upstream
// history and an upstream asset must not make it uncarriable
// (polyforge#158).
//
// The refs/pf-wip/<repo> namespace (polyforge#167) carries a snapshot
// of a task repo's UNCOMMITTED work (one daemon-made commit on top of the
// carried head) across a re-claim. It is scratch by design — every close
// supersedes it with a force push or deletes it — so it is exempt from
// deletion, fast-forward, seq and gitlink; its new objects face the
// carrier blob limit (MaxTaskBlobBytes).
//
// The hook is a bash script exec'd by the real git receive-pack; the
// server process never interprets pack data itself.

import (
	"fmt"
	"os"
	"path/filepath"
)

// hookTemplate is the hook body. Kept as a function so the blob limit
// defaults can be baked in (the env overrides still win at runtime).
func hookTemplate(defaultMaxBlob, defaultMaxTaskBlob int64) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
# aihub pre-receive (v1). Rejects the WHOLE push on any violation.
set -u
MAXBLOB="${GIT_PF_MAX_BLOB:-%d}"
MAXTASKBLOB="${GIT_PF_MAX_TASK_BLOB:-%d}"
TRAILER="${GIT_PF_SEQ_TRAILER:-Polyforge-Seq}"
zero=0000000000000000000000000000000000000000
reject() { echo "aihub: $*" >&2; exit 1; }

# Session fencing (v1.5 §1.6): the proxy exports
# PF_EXPECTED_FINGERPRINT — the public session fingerprint the Basic
# credential authenticated against — and git exposes the push's options
# as GIT_PUSH_OPTION_COUNT + GIT_PUSH_OPTION_0..N (only when the repo
# advertises push options; note git spells the counter env in UPPERCASE).
# Find session-fingerprint=<value> among them and compare it STRICTLY
# with the expected fingerprint.
#
# A push that travelled through the proxy (PF_EXPECTED_FINGERPRINT set)
# MUST carry the option and it MUST match: a missing option, like a
# mismatch, rejects the WHOLE push. A push that did not travel through
# the proxy (e.g. a direct local test push against the bare repo) has
# nothing to compare against and the gate stays open: the proxy is the
# only party that can vouch for which session a credential belongs to.
if [ -n "${PF_EXPECTED_FINGERPRINT:-}" ]; then
  poptcount="${GIT_PUSH_OPTION_COUNT:-0}"
  case "$poptcount" in ''|*[!0-9]*) poptcount=0 ;; esac
  sent_fp=""
  i=0
  while [ "$i" -lt "$poptcount" ]; do
    eval "opt=\"\${GIT_PUSH_OPTION_$i:-}\""
    case "$opt" in
      session-fingerprint=*) sent_fp="${opt#session-fingerprint=}" ;;
    esac
    i=$((i+1))
  done
  [ -n "$sent_fp" ] || reject "push carries no session-fingerprint push option (required)"
  if [ "$sent_fp" != "$PF_EXPECTED_FINGERPRINT" ]; then
    reject "session-fingerprint push option '$sent_fp' does not match the credential's session fingerprint '$PF_EXPECTED_FINGERPRINT'"
  fi
fi

while read -r old new ref; do
  # Task-repo WIP namespace (polyforge#167): a scratch snapshot of a task
  # repo's uncommitted work, superseded (non-fast-forward) or deleted at
  # every close. Deletion and force are allowed; only the objects new to
  # the repo face the carrier blob limit; no seq or gitlink invariant.
  case "$ref" in refs/pf-wip/*)
    [ "$new" = "$zero" ] && continue
    badblob=$(git rev-list --objects "$new" --not --all | awk '{print $1}' \
      | git cat-file --batch-check='%%(objecttype) %%(objectsize) %%(objectname)' \
      | awk -v max="$MAXTASKBLOB" '$1=="blob" && $2>max {print $3" ("$2" bytes)"}')
    [ -z "$badblob" ] || reject "blob exceeds the ${MAXTASKBLOB}-byte limit: $badblob"
    continue ;;
  esac

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

  # blob size limit (every ref; the carrier namespace has its own,
  # larger limit: task-repo history includes the upstream's).
  limit="$MAXBLOB"
  case "$ref" in refs/pf-task/*) limit="$MAXTASKBLOB" ;; esac
  badblob=$(git rev-list --objects "$range" | awk '{print $1}' \
    | git cat-file --batch-check='%%(objecttype) %%(objectsize) %%(objectname)' \
    | awk -v max="$limit" '$1=="blob" && $2>max {print $3" ("$2" bytes)"}')
  [ -z "$badblob" ] || reject "blob exceeds the ${limit}-byte limit: $badblob"

  # Task-repo carrier namespace: foreign (task-repo) history — the
  # chain-only gitlink and seq invariants stop here. Deletion,
  # fast-forward and the blob limit above stay for every ref.
  case "$ref" in refs/pf-task/*) continue ;; esac

  # gitlink: mode 160000 entries are submodule commits; the capsule chain
  # must stay a single flat history.
  for c in $(git rev-list "$range"); do
    if git ls-tree -r "$c" | grep -q '^160000 '; then
      reject "gitlink (mode 160000) in commit $c is not allowed"
    fi
  done

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
`, defaultMaxBlob, defaultMaxTaskBlob)
}

// writeHook writes the pre-receive hook and makes it executable.
func writeHook(path string, maxBlob, maxTaskBlob int64) error {
	body := hookTemplate(maxBlob, maxTaskBlob)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}
