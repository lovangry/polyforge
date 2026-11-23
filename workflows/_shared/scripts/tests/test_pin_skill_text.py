# carries: workflows/_shared/skills/publish.md, workflows/_shared/skills/merge.md
"""The skills teach the repo branch pin (polyforge#45): publish.md resolves
`<default>` from the capsule's pin ahead of origin/HEAD — a pinned lane that
opened its PR against the server-advertised default would die at merge — and
merge.md names the pin as a pinned repo's landing target. Compared with
whitespace folded, as the lane's other text contracts are."""

import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
WORKFLOWS = os.path.dirname(os.path.dirname(os.path.dirname(HERE)))
PUBLISH_SKILL = os.path.join(WORKFLOWS, "_shared", "skills", "publish.md")
MERGE_SKILL = os.path.join(WORKFLOWS, "_shared", "skills", "merge.md")


def folded(path: str) -> str:
    with open(path, encoding="utf-8") as f:
        return " ".join(f.read().split())


class PinSkillTextTest(unittest.TestCase):
    def test_publish_resolves_default_from_the_pin_first(self) -> None:
        skill = folded(PUBLISH_SKILL)
        # the <default> resolution names the pin (capsule.json's repo_branches)
        self.assertIn("`<default>` is the repo's branch pin (capsule.json's `repo_branches`)", skill)
        self.assertIn("when the capsule records one", skill)
        # the origin/HEAD reading survives for unpinned repos, verbatim
        self.assertIn("`git symbolic-ref --short refs/remotes/origin/HEAD` less `origin/` (else `main`)", skill)
        # and the pin is resolved AHEAD of it: a lane that read origin/HEAD
        # first would open the PR against the wrong branch
        self.assertLess(skill.index("repo's branch pin (capsule.json's"),
                        skill.index("`git symbolic-ref --short refs/remotes/origin/HEAD`"))

    def test_merge_names_the_pin_as_the_landing_target(self) -> None:
        skill = folded(MERGE_SKILL)
        self.assertIn("a pinned repo's landing target is its pin", skill)
        # and the landing rules still hold for everything unpinned
        self.assertIn("an unpinned repo lands on the default branch, exactly as before", skill)


if __name__ == "__main__":
    unittest.main()
