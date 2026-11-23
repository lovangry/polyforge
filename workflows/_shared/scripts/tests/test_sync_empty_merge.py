# carries: workflows/_shared/scripts/sync.py
"""The sync's empty merge: a default branch whose tree the working branch
already carries by another route (a restored snapshot, a re-recorded
patch) — its commits' ancestry is not there — merges to a tree
byte-identical to HEAD's. That merge is never published: the branch stays
at HEAD, the merge is kept under refs/polyforge/discarded/pf-sync/ (the
same lane hold_to_run parks a kept blob's commit), and sync_one records
after == before (the settle "already contained" classification), so the
verifying gate sees a synced repo, not a merge it must publish."""
import importlib.util
import os
import unittest
from typing import Tuple

from .gitworld import GitCase, Repo, git, write

_SYNC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "sync.py")

_spec = importlib.util.spec_from_file_location("sync_entry", _SYNC)
assert _spec is not None and _spec.loader is not None
sync = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(sync)


class SyncEmptyMergeTest(GitCase):
    """One capsule repo synced through Sync.sync_one, its origin's main
    advanced by a commit whose tree is the topic's own (the content
    arrived by another route; only the ancestry is new)."""

    def prepare(self) -> Tuple[str, str, str]:
        origin = Repo(self.new_dir("origin"), {"base": "one\n"})
        app = os.path.join(self.new_dir("capsule"), "repos", "app")
        git(self.tmp, "clone", "-q", origin.dir, app)
        git(app, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
        git(app, "switch", "-q", "-c", "topic", "origin/main")
        write(app, {"work.txt": "w\n"})
        git(app, "add", "-A")
        git(app, "commit", "-qm", "capsule work on topic")
        before = git(app, "rev-parse", "HEAD")
        # the default branch's tip lands on the topic's very tree by
        # another route: same content, different commit, no ancestry in
        # common past the base — the merge of it changes nothing. The tree
        # reaches the origin through a temp branch (a push straight to the
        # checked-out main would be refused), then main itself moves to the
        # hand-built commit by update-ref.
        git(app, "push", "-q", "origin", "topic:refs/heads/tmp-tree-src")
        tree = git(origin.dir, "rev-parse", "refs/heads/tmp-tree-src^{tree}")
        parent = git(origin.dir, "rev-parse", "main")
        other = git(origin.dir, "commit-tree", tree, "-p", parent, "-m", "content by another route")
        git(origin.dir, "update-ref", "refs/heads/main", other)
        git(origin.dir, "update-ref", "-d", "refs/heads/tmp-tree-src")
        return app, before, other

    def test_a_merge_that_changes_nothing_is_discarded(self) -> None:
        app, before, other = self.prepare()
        inv = sync.Invocation("sync.py", ["--capsule", os.path.dirname(os.path.dirname(app))])
        g = sync.Git(sync.Ctx())
        r = sync.SyncRepo(name="app", branch="topic", before=before)
        s = sync.Sync(inv, g, os.path.dirname(os.path.dirname(app)), os.path.join(self.tmp, "syncing.json"))
        s.sync_one(app, r)

        # the branch stays where it was: the merge is not published
        self.assertEqual(git(app, "rev-parse", "HEAD"), before)
        # the whole repo stayed put: no merge, no working-tree drift
        self.assertEqual(git(app, "status", "--porcelain"), "")
        # the merge is kept under the discarded refs, its tree HEAD's own
        refs = [tuple(l.split()) for l in git(app, "for-each-ref", "--format=%(refname) %(objectname)",
                                                "refs/polyforge/discarded/pf-sync/").splitlines() if l]
        self.assertEqual(len(refs), 1)
        self.assertIn("refs/polyforge/discarded/pf-sync/", refs[0][0])
        kept = refs[0][1]
        self.assertNotEqual(kept, before)
        self.assertEqual(git(app, "diff", "--stat", before, kept), "")
        # settle's classification: after == before (already contained)
        self.assertEqual(r.after, before)
        # and the record says so: one discard, the kept merge under its ref
        self.assertEqual(len(r.discarded), 1)
        self.assertEqual(r.discarded[0].commit, kept)
        self.assertEqual(r.discarded[0].ref, refs[0][0])


if __name__ == "__main__":
    unittest.main()
