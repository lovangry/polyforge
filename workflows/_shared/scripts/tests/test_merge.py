# carries: workflows/_shared/scripts/merge.py, skills/polyforge/SKILL.md
"""回归测试：merge 在执行外部合入前必须重新确认默认分支版本。"""
import importlib.util
import os
from typing import Any, Callable, cast
import unittest

MERGE_PATH = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "merge.py")

_spec = importlib.util.spec_from_file_location("merge_entry", MERGE_PATH)
assert _spec is not None and _spec.loader is not None
merge = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(merge)


class MergeDefaultBranchFreshnessTest(unittest.TestCase):
    def test_same_verified_base_allows_merge(self) -> None:
        check = cast(Callable[[str, str], str], getattr(merge, "default_branch_update_reason", None))
        self.assertIsNotNone(check)
        self.assertEqual(check("base-1", "base-1"), "")

    def test_new_default_commit_routes_back_to_sync(self) -> None:
        check = cast(Callable[[str, str], str], getattr(merge, "default_branch_update_reason", None))
        self.assertIsNotNone(check)
        reason = check("base-1", "base-2")
        self.assertIn("default branch moved since verification", reason)
        self.assertIn("sync", reason)


class MergeNoForgeLandingTest(unittest.TestCase):
    def _repo(self, **kw: Any) -> Any:
        base = dict(name="local", pr_url="", branch="polyforge/capsule", head="a" * 40,
                    published="a" * 40, dir="/tmp/local", no_forge=True, base="main")
        base.update(kw)
        return merge.MergeRepo(**base)

    class FakeGit:
        def __init__(self, push_error: bool = False) -> None:
            self.calls = []
            self.push_error = push_error

        def fetch(self, cwd: str, *branches: str) -> None:
            self.calls.append(("fetch", branches))

        def rev(self, cwd: str, ref: str) -> str:
            if ref.endswith("origin/polyforge/capsule"):
                return "a" * 40
            if ref == "HEAD":
                return "b" * 40
            if ref.endswith("origin/main"):
                return "base" * 10
            raise AssertionError(ref)

        def git(self, cwd: str, *args: str) -> str:
            self.calls.append(("git", args))
            if args[:4] == ("symbolic-ref", "--short", "-q", "HEAD"):
                return "polyforge/capsule\n"
            if args[0] == "push" and self.push_error:
                raise merge.GoError("push rejected: non-fast-forward")
            return ""

    def test_local_landing_creates_merge_commit_and_landing_shape(self) -> None:
        r = self._repo()
        g = self.FakeGit()
        inv = type("I", (), {"say": lambda self, text: None})()
        merge.Merge(inv, g, merge.Ctx()).judge_local(r)
        self.assertEqual(r.how, merge.HOW_MERGED)
        self.assertEqual(r.merge_commit, "b" * 40)
        self.assertTrue(r.by_this_run)
        calls = [c[1] for c in g.calls if c[0] == "git"]
        self.assertIn(("switch", "--detach", "refs/remotes/origin/main"), calls)
        self.assertIn(("push", "-q", "origin", "HEAD:refs/heads/main"), calls)
        self.assertEqual(calls[-1], ("switch", "--quiet", "polyforge/capsule"))
        d = merge.Details("p.json", "v.json")
        d.repos.append(r)
        rec = merge.landing_of(d)["repos"][0]
        self.assertEqual(rec["how"] if "how" in rec else r.how, "merged")
        self.assertEqual(rec["merge_commit"], "b" * 40)
        self.assertNotIn("manual_merge_required", rec)

    def test_push_failure_restores_branch_and_routes_to_sync(self) -> None:
        r = self._repo()
        g = self.FakeGit(push_error=True)
        inv = type("I", (), {"say": lambda self, text: None})()
        merge.Merge(inv, g, merge.Ctx()).judge_local(r)
        self.assertEqual(r.how, "")
        self.assertIn("sync", r.why)
        self.assertEqual([c[1] for c in g.calls if c[0] == "git"][-1],
                         ("switch", "--quiet", "polyforge/capsule"))

    def test_landing_serialization_has_no_manual_shape(self) -> None:
        r = self._repo(how=merge.HOW_MERGED, merge_commit="b" * 40)
        d = merge.Details("p.json", "v.json")
        d.repos.append(r)
        rec = merge.landing_of(d)["repos"][0]
        self.assertNotIn("manual_merge_required", rec)
        self.assertEqual(rec["merge_commit"], "b" * 40)


if __name__ == "__main__":
    unittest.main()
