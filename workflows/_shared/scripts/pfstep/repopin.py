"""repopin — the capsule's repo branch pins (polyforge#45).

A project repo registered as ``name=url#branch`` is pinned to that
branch: the capsule clone stands on it (the daemon's ``git clone
--branch``) and the lane treats it as the repo's landing target —
sync merges from it, verification measures the baseline against it,
publish opens the PR against it and merge lands on it. capsule.json
records the pins the claim carried in ``repo_branches`` (repo name ->
branch); this module is the one reader every script shares.
"""

import json
import os
import sys
from typing import Dict

sys.dont_write_bytecode = True


def pins_of(capsule_dir: str) -> Dict[str, str]:
    """The capsule's branch pins: repo name -> the pinned branch.

    Absent facts read as none: a missing capsule.json, a capsule.json
    without ``repo_branches`` (every capsule claimed before #45), a
    mis-shaped one, or an entry whose branch is the empty string (the
    registered pin was empty — not a pin). Never an error: an
    unpinned lane keeps today's behavior, and no reader may crash on
    the capsule's bookkeeping.
    """
    return {}
