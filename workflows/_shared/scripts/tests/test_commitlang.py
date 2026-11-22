# carries: workflows/_shared/scripts/pfstep/commitlang.py, workflows/_shared/scripts/pfstep/commitmsg.py
"""The commit language of a task repository: its own history decides —
the last 50 bodies' Chinese-to-Latin ratio (an exact tie reading
English) — and the messages a capsule writes in it keep the repo's
conventional prefix while the line follows that reading: the sync merge
message and the publish PR title in both language forms, a capsule
subject that cannot write an English-only line into a Chinese-primary
repository and leaves an English-primary repository exactly as it
was."""
import unittest
from typing import List

from pfstep import commitlang, commitmsg

from .gitworld import GitCase, Repo


class DetectionTest(unittest.TestCase):
    """The ratio reading over commit bodies, pure."""

    def test_chinese_bodies_read_chinese(self) -> None:
        self.assertEqual(commitlang.detect(["修复登录崩溃", "调整按钮间距"] * 5), commitlang.CHINESE)

    def test_english_bodies_read_english(self) -> None:
        self.assertEqual(commitlang.detect(["fix the login crash", "tune the button spacing"] * 5), commitlang.ENGLISH)

    def test_an_exact_tie_reads_english(self) -> None:
        # two Chinese characters against two Latin letters: the tie keeps today's English
        self.assertEqual(commitlang.detect(["修复 ok"]), commitlang.ENGLISH)

    def test_nothing_to_read_is_english(self) -> None:
        self.assertEqual(commitlang.detect([]), commitlang.ENGLISH)

    def test_only_the_last_fifty_bodies_decide(self) -> None:
        older_english: List[str] = ["english body no. %d" % i for i in range(60)]
        newer_chinese: List[str] = ["第 %d 条提交：修复登录崩溃" % i for i in range(60)]
        self.assertEqual(commitlang.detect(older_english + newer_chinese), commitlang.CHINESE)
        # the mirrored history reads English: the Chinese bodies are outside the window
        self.assertEqual(commitlang.detect(newer_chinese + older_english), commitlang.ENGLISH)


class RepoLanguageTest(GitCase):
    """A real repository's history, read through git."""

    def zh_repo(self, n: int, name: str) -> Repo:
        r = Repo(self.new_dir(name))
        for i in range(n):
            r.commit("第 %d 条提交：修复登录崩溃" % i, {"f%d.txt" % i: "x\n"})
        return r

    def en_repo(self, n: int, name: str) -> Repo:
        r = Repo(self.new_dir(name))
        for i in range(n):
            r.commit("commit no. %d: fix the login crash" % i, {"f%d.txt" % i: "x\n"})
        return r

    def test_a_chinese_primary_repository_reads_chinese(self) -> None:
        r = self.zh_repo(50, "zh")
        self.assertEqual(commitlang.repo_language(r.dir), commitlang.CHINESE)

    def test_an_english_primary_repository_reads_english(self) -> None:
        r = self.en_repo(50, "en")
        self.assertEqual(commitlang.repo_language(r.dir), commitlang.ENGLISH)

    def test_the_window_is_the_most_recent_fifty(self) -> None:
        r = self.zh_repo(60, "zh-first")
        for i in range(50):
            r.commit("english tail no. %d" % i, {"e%d.txt" % i: "x\n"})
        # the most recent 50 bodies are English even though 60 Chinese ones precede them
        self.assertEqual(commitlang.repo_language(r.dir), commitlang.ENGLISH)

    def test_a_capsule_subject_follows_the_repository(self) -> None:
        zh = self.zh_repo(50, "zh")
        lang = commitlang.repo_language(zh.dir)
        self.assertEqual(commitlang.CHINESE, lang)
        subject = commitmsg.subject(lang, "test", "胶囊克隆运行仓库的 commit-msg 钩子", "polyforge#41 u1")
        self.assertEqual("test: 胶囊克隆运行仓库的 commit-msg 钩子 (polyforge#41 u1)", subject)

    def test_an_english_subject_keeps_the_same_shape(self) -> None:
        en = self.en_repo(50, "en")
        lang = commitlang.repo_language(en.dir)
        self.assertEqual(commitlang.ENGLISH, lang)
        subject = commitmsg.subject(lang, "test", "a capsule clone runs the repo's commit-msg hook", "polyforge#41 u1")
        self.assertEqual("test: a capsule clone runs the repo's commit-msg hook (polyforge#41 u1)", subject)


class MessageShapeTest(unittest.TestCase):
    """The forms both languages write, byte-exact."""

    def test_the_english_sync_message_is_byte_identical_to_todays(self) -> None:
        self.assertEqual("Merge main into polyforge/41 (sync)",
                         commitmsg.sync_message(commitlang.ENGLISH, "main", "polyforge/41"))

    def test_the_chinese_sync_message(self) -> None:
        self.assertEqual("合并 main 到 polyforge/41 (sync)",
                         commitmsg.sync_message(commitlang.CHINESE, "main", "polyforge/41"))

    def test_the_pr_title_keeps_its_shape_in_both_languages(self) -> None:
        self.assertEqual("polyforge#41: 让胶囊克隆运行仓库钩子",
                         commitmsg.pr_title(commitlang.CHINESE, "polyforge#41", "让胶囊克隆运行仓库钩子"))
        self.assertEqual("polyforge#41: run the repo's hooks from a capsule clone",
                         commitmsg.pr_title(commitlang.ENGLISH, "polyforge#41", "run the repo's hooks from a capsule clone"))

    def test_a_chinese_primary_subject_refuses_an_english_line(self) -> None:
        with self.assertRaises(ValueError):
            commitmsg.subject(commitlang.CHINESE, "test", "an English line in a Chinese repo", "polyforge#41 u1")

    def test_an_english_primary_subject_leaves_mixed_lines_alone(self) -> None:
        # an English-primary repository is unchanged: its lines are not policed
        self.assertEqual("docs: name the tokens 「过目通过」 (polyforge#41 u1)",
                         commitmsg.subject(commitlang.ENGLISH, "docs", "name the tokens 「过目通过」", "polyforge#41 u1"))


if __name__ == "__main__":
    unittest.main()
