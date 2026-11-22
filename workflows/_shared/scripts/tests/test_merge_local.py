# carries: workflows/_shared/scripts/merge.py
"""Real-Git landing contracts: refs, owner bytes, recovery and landing output."""
import os
import json
import subprocess
import sys
from typing import Any, cast

from .gitworld import GitCase, Repo, git, write
from .test_merge import merge


class LocalLandingTest(GitCase):
    def fixture(self, bare: bool = False, two_worktrees: bool = False) -> Any:
        owner = Repo(self.new_dir("owner"), {"edit": "one\n" + "middle\n" * 30 + "last\n", "other": "keep\n"})
        if two_worktrees:
            owner.git("branch", "-m", "master")
            owner.git("worktree", "add", "-b", "feat-knowledge", os.path.join(self.tmp, "feature"))
            base = "feat-knowledge"
        else:
            base = "main"
        source = owner.dir
        if bare:
            source = os.path.join(self.tmp, "bare.git")
            git(self.tmp, "clone", "--bare", owner.dir, source)
        capsule = os.path.join(self.tmp, "capsule")
        git(self.tmp, "clone", source, capsule)
        git(capsule, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/" + base)
        git(capsule, "switch", "-c", "topic", "origin/" + base)
        self.owner, self.capsule, self.source, self.base = owner, capsule, source, base
        self.before = git(source, "rev-parse", "refs/heads/" + base)
        return owner

    def publish(self, files: dict[str, str], title: str = "app: the change") -> Any:
        write(self.capsule, files)
        git(self.capsule, "add", "-A")
        git(self.capsule, "commit", "-qm", "verified")
        head = git(self.capsule, "rev-parse", "HEAD")
        git(self.capsule, "push", "origin", "HEAD:refs/heads/topic")
        return merge.MergeRepo(name="app", pr_url="", branch="topic", head=head,
                               published=head, dir=self.capsule, no_forge=True,
                               title=title,
                               verified_base=self.before)

    def land(self, r: Any, expected: int = 0) -> dict[str, Any]:
        inv = type("I", (), {"say": lambda self, text: None})()
        ctx = merge.Ctx()
        details = merge.Details("", "")
        details.repos.append(r)
        result = merge.Merge(inv, merge.Git(ctx), ctx).judge(details)
        self.assertEqual(result, expected, r.why)
        if expected == 0:
            self.assertEqual(r.how, "squash")
            self.assertEqual(git(self.source, "rev-parse", "refs/heads/" + self.base), r.merge_commit)
            self.assertEqual(git(self.source, "rev-parse", r.merge_commit + "^"), self.before)
        return cast(dict[str, Any], merge.landing_of(details)["repos"][0])

    def test_clean(self) -> None:
        self.fixture()
        r = self.publish({"new": "verified\n"})
        rec = self.land(r)
        self.assertTrue(rec["worktree_synced"])
        self.assertEqual(git(self.owner.dir, "status", "--porcelain"), "")
        self.assertEqual(git(self.capsule, "branch", "--show-current"), "topic")

    def test_dirty_nonoverlap(self) -> None:
        owner = self.fixture()
        r = self.publish({"new": "verified\n"})
        write(owner.dir, {"other": "owner bytes\n", "untracked": "precious\n"})
        self.land(r)
        self.assertEqual(git(owner.dir, "diff", "--", "other").splitlines()[-1], "+owner bytes")
        with open(os.path.join(owner.dir, "untracked")) as f:
            self.assertEqual(f.read(), "precious\n")
        self.assertEqual(git(owner.dir, "stash", "list"), "")

    def test_dirty_overlap_stash_pop(self) -> None:
        owner = self.fixture()
        r = self.publish({"edit": "verified\n" + "middle\n" * 30 + "last\n"})
        write(owner.dir, {"edit": "one\n" + "middle\n" * 30 + "owner\n", "untracked": "precious\n"})
        self.land(r)
        with open(os.path.join(owner.dir, "edit")) as f:
            self.assertEqual(f.read(), "verified\n" + "middle\n" * 30 + "owner\n")
        self.assertEqual(git(owner.dir, "stash", "list"), "")
        self.assertTrue(r.stash)

    def test_stash_pop_conflict(self) -> None:
        owner = self.fixture()
        r = self.publish({"edit": "verified\n"})
        write(owner.dir, {"edit": "owner\n", "untracked": "precious\n"})
        rec = self.land(r)
        self.assertTrue(rec["stash_conflict"])
        self.assertEqual(git(owner.dir, "rev-parse", rec["stash"]), git(owner.dir, "rev-parse", "refs/stash"))
        self.assertEqual(git(owner.dir, "show", rec["stash"] + ":edit"), "owner")
        self.assertEqual(git(owner.dir, "ls-files", "-u"), "")
        with open(os.path.join(owner.dir, "edit")) as f:
            self.assertNotIn("<<<<<<<", f.read())

    def test_content_conflict_aborts(self) -> None:
        owner = self.fixture()
        owner.commit("base moved before verification", {"edit": "base change\n"})
        r = self.publish({"edit": "topic change\n"})
        r.verified_base = owner.head()
        before = owner.head()
        self.land(r, expected=1)
        self.assertEqual(owner.head(), before)
        self.assertEqual(git(owner.dir, "status", "--porcelain"), "")
        self.assertIn("sync", r.why)

    def test_two_worktrees(self) -> None:
        owner = self.fixture(two_worktrees=True)
        master = owner.head()
        r = self.publish({"new": "verified\n"})
        rec = self.land(r)
        self.assertFalse(rec["worktree_synced"])
        self.assertEqual(owner.head(), master)
        self.assertEqual(owner.git("branch", "--show-current"), "master")
        self.assertEqual(git(os.path.join(self.tmp, "feature"), "branch", "--show-current"), "feat-knowledge")
        self.assertEqual(len(owner.git("worktree", "list", "--porcelain").split("worktree ")) - 1, 2)

    def test_nothing_published(self) -> None:
        repo = Repo(os.path.join(self.tmp, "repos", "app"))
        head = repo.head()
        write(self.tmp, {
            "publish.json": json.dumps({"repos": [{"name": "app", "branch": "", "pr_url": "",
                                                      "head": head, "no_forge": True,
                                                      "skipped_reason": "nothing to publish"}]}),
            "verified.json": json.dumps({"schema": "pf-step-result/1", "heads": {"app": head}}),
        })
        out = os.path.join(self.tmp, "result.json")
        landing = os.path.join(self.tmp, "landing.json")
        # No origin exists: even attempting to resolve or fetch it would fail.
        proc = subprocess.run([sys.executable, cast(str, merge.__file__),
                               "--capsule", self.tmp, "--publish", os.path.join(self.tmp, "publish.json"),
                               "--verified", os.path.join(self.tmp, "verified.json"),
                               "--landing-out", landing, "--json", "--out", out],
                              capture_output=True, text=True)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        with open(out) as f:
            self.assertEqual(json.load(f)["verdict"], "pass")
        with open(landing) as f:
            rec = json.load(f)["repos"][0]
        self.assertEqual(rec["how"], "nothing")
        self.assertEqual(rec["merge_commit"], "")
        self.assertEqual(repo.head(), head)

    def test_head_already_on_baseline(self) -> None:
        """A no-forge head already contained by origin's baseline is recorded
        as landed by the containment judgement — how: merged, by_this_run
        honestly absent, the verified head itself the landed commit — and no
        second, empty merge is made (the old record claimed by_this_run: true
        with the baseline tip as its merge commit)."""
        owner = self.fixture()
        r = self.publish({"new": "verified\n"})
        # A hand (or a crashed earlier run) already landed the head on the
        # baseline: a real merge commit whose second parent is the head.
        owner.git("merge", "--no-ff", "-m", "hand landing", r.head)
        landed = owner.head()
        r.verified_base = landed  # verification snapshotted the already-landed base
        inv = type("I", (), {"say": lambda self, text: None})()
        ctx = merge.Ctx()
        details = merge.Details("", "")
        details.repos.append(r)
        result = merge.Merge(inv, merge.Git(ctx), ctx).judge(details)
        self.assertEqual(result, 0, r.why)
        rec = cast(dict[str, Any], merge.landing_of(details)["repos"][0])
        self.assertEqual(rec["how"], "merged")
        self.assertNotIn("by_this_run", rec)
        self.assertEqual(rec["merge_commit"], r.head)
        self.assertFalse(rec["worktree_synced"])
        self.assertEqual(owner.head(), landed)
        self.assertEqual(git(self.owner.dir, "status", "--porcelain"), "")

    def test_the_landing_message_is_the_pr_title_in_chinese(self) -> None:
        """The no-forge squash's message is the PR title verbatim: a
        Chinese-primary repository's title lands as its Chinese words
        (polyforge#41 u3)."""
        self.fixture()
        r = self.publish({"new": "验证过的修改\n"}, title="app: 让胶囊克隆运行仓库的钩子")
        self.land(r)
        self.assertEqual(git(self.source, "log", "--format=%B", "-n", "1"),
                         "app: 让胶囊克隆运行仓库的钩子")

    def test_the_landing_message_is_the_pr_title_in_english(self) -> None:
        """The mirrored pin: an English-primary repository's landing keeps
        today's exact bytes — the title verbatim (polyforge#41 u3)."""
        self.fixture()
        r = self.publish({"new": "a verified change\n"},
                         title="app: run the repo's hooks from a capsule clone")
        self.land(r)
        self.assertEqual(git(self.source, "log", "--format=%B", "-n", "1"),
                         "app: run the repo's hooks from a capsule clone")

    def test_a_no_forge_entry_without_a_title_is_refused(self) -> None:
        repo = Repo(os.path.join(self.tmp, "repos", "app"))
        head = repo.head()
        write(self.tmp, {
            "publish.json": json.dumps({"repos": [{"name": "app", "branch": "topic", "pr_url": "",
                                                      "head": head, "no_forge": True}]}),
            "verified.json": json.dumps({"schema": "pf-step-result/1", "heads": {"app": head}}),
        })
        out = os.path.join(self.tmp, "result.json")
        landing = os.path.join(self.tmp, "landing.json")
        proc = subprocess.run([sys.executable, cast(str, merge.__file__),
                               "--capsule", self.tmp, "--publish", os.path.join(self.tmp, "publish.json"),
                               "--verified", os.path.join(self.tmp, "verified.json"),
                               "--landing-out", landing, "--json", "--out", out],
                              capture_output=True, text=True)
        self.assertEqual(proc.returncode, 2, proc.stdout + proc.stderr)
        self.assertIn("title", proc.stderr)
        self.assertFalse(os.path.exists(landing))


    def test_bare_origin(self) -> None:
        self.fixture(bare=True)
        r = self.publish({"new": "verified\n"})
        self.land(r)
        self.assertEqual(git(self.capsule, "branch", "--show-current"), "topic")
