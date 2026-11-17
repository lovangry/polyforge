# carries: workflows/_shared/scripts/merge.py, skills/polyforge/SKILL.md
"""回归测试：merge 在执行外部合入前必须重新确认默认分支版本。"""
import importlib.util
import os
from typing import Callable, cast
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


class MergeNoForgeManualLandingTest(unittest.TestCase):
    """本地/内网仓库（origin 不是 GitHub、无 gh）时 publish 不开 PR：merge 不得因此 FAIL，
    只校验 origin 分支停在 verified head，landing 记 manual_merge_required 且不造 merge commit。"""

    def _repo(self, **kw):
        base = dict(name="lantay-client", pr_url="", branch="polyforge/cpm-for-legal-400",
                    head="a" * 40, published="a" * 40, dir="/tmp/no-such-capsule/repos/lantay-client")
        base.update(kw)
        return merge.MergeRepo(**base)

    def test_manual_repo_yields_manual_landing_record(self) -> None:
        build = getattr(merge, "landing_of", None)
        self.assertIsNotNone(build)
        d = merge.Details("p.json", "v.json")
        r = self._repo()
        r.no_forge = True
        r.how = merge.HOW_MANUAL
        d.repos.append(r)
        rec = build(d)
        self.assertEqual(rec["state"], "landed")
        self.assertEqual(rec["repos"][0]["pr"], "")
        self.assertEqual(rec["repos"][0]["light_verified_head"], r.head)
        self.assertIs(rec["repos"][0].get("manual_merge_required"), True)
        self.assertNotIn("merge_commit", rec["repos"][0])

    def test_no_forge_repo_is_marked_in_details(self) -> None:
        r = self._repo()
        r.no_forge = True
        self.assertIs(r.to_dict().get("no_pr"), True)
        r.no_forge = False
        self.assertNotIn("no_pr", r.to_dict())

    def test_manual_constant_is_a_distinct_how(self) -> None:
        self.assertEqual(getattr(merge, "HOW_MANUAL", None), "manual")
        self.assertNotEqual(merge.HOW_MANUAL, merge.HOW_MERGED)


if __name__ == "__main__":
    unittest.main()
