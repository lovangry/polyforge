# carries: workflows/_shared/scripts/pfstep/repopin.py
"""回归测试：repopin 读取 capsule.json 的 repo_branches 记录。"""
import importlib.util
import json
import os
import shutil
import tempfile
import unittest
from typing import Any, Dict

HERE = os.path.dirname(os.path.abspath(__file__))

_spec = importlib.util.spec_from_file_location(
    "repopin_under_test", os.path.join(os.path.dirname(HERE), "pfstep", "repopin.py"))
assert _spec is not None and _spec.loader is not None
repopin = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(repopin)


class RepoPinTest(unittest.TestCase):
    def setUp(self) -> None:
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir, True)

    def pin_capsule(self, doc: Any) -> None:
        body = doc if isinstance(doc, str) else json.dumps(doc)
        with open(os.path.join(self.dir, "capsule.json"), "w", encoding="utf-8") as f:
            f.write(body)

    def pins(self) -> Dict[str, str]:
        pins: Dict[str, str] = repopin.pins_of(self.dir)
        return pins

    def test_a_missing_capsule_json_reads_as_no_pins(self) -> None:
        self.assertEqual(self.pins(), {})

    def test_a_capsule_without_the_fact_reads_as_no_pins(self) -> None:
        self.pin_capsule({"work_item_id": "wi1", "repos": {"app": "ab" * 20}})
        self.assertEqual(self.pins(), {})

    def test_the_pins_are_the_recorded_map(self) -> None:
        self.pin_capsule({"repo_branches": {"app": "feat-cloud", "lib": "dev-x"}})
        self.assertEqual(self.pins(), {"app": "feat-cloud", "lib": "dev-x"})

    def test_a_mis_shaped_fact_reads_as_no_pins(self) -> None:
        for bad in (["app"], "feat-cloud", {"app": 1}):
            self.pin_capsule({"repo_branches": bad})
            self.assertEqual(self.pins(), {})

    def test_an_empty_branch_is_not_a_pin(self) -> None:
        self.pin_capsule({"repo_branches": {"app": "feat-cloud", "lib": ""}})
        self.assertEqual(self.pins(), {"app": "feat-cloud"})

    def test_a_broken_capsule_json_reads_as_no_pins(self) -> None:
        self.pin_capsule("{not json")
        self.assertEqual(self.pins(), {})


if __name__ == "__main__":
    unittest.main()
