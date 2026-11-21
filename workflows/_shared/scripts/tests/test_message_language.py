# carries: workflows/_shared/scripts/sync.py, workflows/_shared/scripts/pfstep/commitmsg.py
"""The sync merge message's language: it follows the task repository's
own default branch — a Chinese-primary repository's sync merge commits
in Chinese, an English-primary one byte-identical to today's English —
and the reading is the default branch's own history
(``refs/remotes/origin/<default>``), never the capsule's own commits on
the working branch. (The landing half of the message language — a
no-forge squash's message is the PR title verbatim, both languages — is
pinned in test_merge_local.py.)"""
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


class SyncMessageLanguageTest(GitCase):
    """One capsule repo synced through Sync.sync_one, its origin's main
    the only fact the message's language follows."""

    def prepare(self, zh: bool, n: int = 50) -> Tuple[str, str]:
        """A capsule clone (its repos/app on a topic branch with English
        commits of its own) of an origin whose main carries n bodies in
        one language, the default branch one commit ahead of the clone —
        the repo and the topic's HEAD, what a sync run records as
        before."""
        origin = Repo(self.new_dir("origin"), {"base": "one\n"})
        for i in range(n):
            body = "第 %d 条提交：修复登录崩溃" % i if zh else "commit no. %d: fix the login crash" % i
            origin.commit(body, {"c%d.txt" % i: "x\n"})
        app = os.path.join(self.new_dir("capsule"), "repos", "app")
        git(self.tmp, "clone", "-q", origin.dir, app)
        git(app, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
        git(app, "switch", "-q", "-c", "topic", "origin/main")
        # the capsule's own commits on the working branch — English words the
        # reading must not follow: the language is the default branch's fact
        write(app, {"work.txt": "w\n"})
        git(app, "add", "-A")
        git(app, "commit", "-qm", "capsule work on topic")
        before = git(app, "rev-parse", "HEAD")
        # the default branch moves: the sync has something to merge
        origin.commit("第 51 条提交：上游前进" if zh else "upstream moves on", {"new.txt": "n\n"})
        return app, before

    def run_sync_one(self, app: str, before: str) -> None:
        inv = sync.Invocation("sync.py", ["--capsule", os.path.dirname(os.path.dirname(app))])
        g = sync.Git(sync.Ctx())
        r = sync.SyncRepo(name="app", branch="topic", before=before)
        s = sync.Sync(inv, g, os.path.dirname(os.path.dirname(app)), os.path.join(self.tmp, "syncing.json"))
        s.sync_one(app, r)

    def test_a_chinese_primary_repository_merges_in_chinese(self) -> None:
        app, before = self.prepare(zh=True)
        self.run_sync_one(app, before)
        self.assertEqual(git(app, "log", "--format=%B", "-n", "1"), "合并 main 到 topic (sync)")

    def test_an_english_primary_repository_merges_in_english(self) -> None:
        app, before = self.prepare(zh=False)
        self.run_sync_one(app, before)
        self.assertEqual(git(app, "log", "--format=%B", "-n", "1"), "Merge main into topic (sync)")


if __name__ == "__main__":
    unittest.main()
