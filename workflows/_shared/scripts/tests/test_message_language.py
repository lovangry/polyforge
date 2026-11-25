# carries: workflows/_shared/scripts/sync.py, workflows/_shared/scripts/pfstep/commitmsg.py
"""The lane's message language: it follows the work item's own goal —
read from the capsule's runtime-state/work-item.json through
``commitmsg.goal_language`` (Chinese when the goal carries a Chinese
character, English when it does not) — never the task repository's
history: a repository whose commits are all English is no reason to
write this one in English, so a Chinese goal's sync merge commits in
Chinese even in an English-primary repository, and an English goal
keeps the English bytes even in a Chinese-primary one. (The landing
half of the message language — a no-forge squash's message is the PR
title verbatim, both languages — is pinned in test_merge_local.py.)"""
import importlib.util
import json
import os
import unittest
from typing import Tuple

from pfstep import commitmsg

from .gitworld import GitCase, Repo, git, write

_SYNC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "sync.py")

_spec = importlib.util.spec_from_file_location("sync_entry", _SYNC)
assert _spec is not None and _spec.loader is not None
sync = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(sync)


class SyncMessageLanguageTest(GitCase):
    """One capsule repo synced through Sync.sync_one, the work item's
    goal the only fact the message's language follows."""

    def prepare(self, goal: str, zh: bool, n: int = 50) -> Tuple[str, str]:
        """A capsule clone (its runtime-state/work-item.json carrying
        the goal; its repos/app on a topic branch with English commits
        of its own) of an origin whose main carries n bodies in one
        language, the default branch one commit ahead of the clone —
        the repo and the topic's HEAD, what a sync run records as
        before."""
        origin = Repo(self.new_dir("origin"), {"base": "one\n"})
        for i in range(n):
            body = "第 %d 条提交：修复登录崩溃" % i if zh else "commit no. %d: fix the login crash" % i
            origin.commit(body, {"c%d.txt" % i: "x\n"})
        capsule = self.new_dir("capsule")
        app = os.path.join(capsule, "repos", "app")
        git(self.tmp, "clone", "-q", origin.dir, app)
        git(app, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
        git(app, "switch", "-q", "-c", "topic", "origin/main")
        # the capsule's own commits on the working branch — English words the
        # reading must not follow: the language is the work item's goal's fact
        write(app, {"work.txt": "w\n"})
        git(app, "add", "-A")
        git(app, "commit", "-qm", "capsule work on topic")
        before = git(app, "rev-parse", "HEAD")
        # the capsule's work item: its goal is the one fact the message's
        # language follows — the repository's history never is
        write(capsule, {"runtime-state/work-item.json": json.dumps({"id": "wi_6odSkeBxPMli", "goal": goal})})
        # the default branch moves: the sync has something to merge
        origin.commit("第 51 条提交：上游前进" if zh else "upstream moves on", {"new.txt": "n\n"})
        return app, before

    def run_sync_one(self, app: str, before: str) -> None:
        inv = sync.Invocation("sync.py", ["--capsule", os.path.dirname(os.path.dirname(app))])
        g = sync.Git(sync.Ctx())
        r = sync.SyncRepo(name="app", branch="topic", before=before)
        s = sync.Sync(inv, g, os.path.dirname(os.path.dirname(app)), os.path.join(self.tmp, "syncing.json"))
        s.sync_one(app, r)

    def test_a_chinese_goal_merges_in_chinese_in_an_english_primary_repository(self) -> None:
        app, before = self.prepare(goal="落地标题固定用简体中文：不再随仓库英文惯例漂移", zh=False)
        self.run_sync_one(app, before)
        self.assertEqual(git(app, "log", "--format=%B", "-n", "1"), "合并 main 到 topic (sync)")

    def test_an_english_goal_merges_in_english_in_a_chinese_primary_repository(self) -> None:
        app, before = self.prepare(goal="pin the landing title in Simplified Chinese", zh=True)
        self.run_sync_one(app, before)
        self.assertEqual(git(app, "log", "--format=%B", "-n", "1"), "Merge main into topic (sync)")

    def test_a_capsule_without_a_readable_work_item_refuses(self) -> None:
        app, before = self.prepare(goal="落地标题固定用简体中文", zh=True)
        os.remove(os.path.join(os.path.dirname(os.path.dirname(app)), "runtime-state", "work-item.json"))
        with self.assertRaises(sync.Refused):
            self.run_sync_one(app, before)


class MessageShapeTest(unittest.TestCase):
    """The forms both languages write, byte-exact, and the one line the
    work item's goal reads."""

    def test_the_goal_language_reads_the_work_item(self) -> None:
        self.assertEqual(commitmsg.CHINESE, commitmsg.goal_language("落地标题固定用简体中文"))
        self.assertEqual(commitmsg.CHINESE, commitmsg.goal_language("pin the title: 落地标题固定简体中文"))
        self.assertEqual(commitmsg.ENGLISH, commitmsg.goal_language("pin the landing title in Simplified Chinese"))

    def test_the_english_sync_message_is_byte_identical_to_todays(self) -> None:
        self.assertEqual("Merge main into polyforge/47 (sync)",
                         commitmsg.sync_message(commitmsg.ENGLISH, "main", "polyforge/47"))

    def test_the_chinese_sync_message(self) -> None:
        self.assertEqual("合并 main 到 polyforge/47 (sync)",
                         commitmsg.sync_message(commitmsg.CHINESE, "main", "polyforge/47"))

    def test_the_pr_title_is_chinese_for_a_chinese_goal(self) -> None:
        self.assertEqual("polyforge#47: 落地标题固定用简体中文",
                         commitmsg.pr_title(commitmsg.CHINESE, "polyforge#47", "落地标题固定用简体中文"))

    def test_the_pr_title_keeps_the_english_shape_for_an_english_goal(self) -> None:
        self.assertEqual("polyforge#47: pin the landing title in Simplified Chinese",
                         commitmsg.pr_title(commitmsg.ENGLISH, "polyforge#47", "pin the landing title in Simplified Chinese"))

    def test_a_chinese_goal_subject_keeps_the_conventional_prefix(self) -> None:
        self.assertEqual("test: 胶囊消息语言随工单 goal (polyforge#47 u1)",
                         commitmsg.subject(commitmsg.CHINESE, "test", "胶囊消息语言随工单 goal", "polyforge#47 u1"))

    def test_a_chinese_goal_refuses_an_english_only_line(self) -> None:
        with self.assertRaises(ValueError):
            commitmsg.subject(commitmsg.CHINESE, "test", "an English line for a Chinese goal", "polyforge#47 u1")

    def test_an_english_goal_leaves_mixed_lines_alone(self) -> None:
        # an English goal is unchanged: its lines are not policed
        self.assertEqual("docs: name the tokens 「过目通过」 (polyforge#47 u1)",
                         commitmsg.subject(commitmsg.ENGLISH, "docs", "name the tokens 「过目通过」", "polyforge#47 u1"))


if __name__ == "__main__":
    unittest.main()
