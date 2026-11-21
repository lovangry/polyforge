"""The language a task repository's own commits write — the one fact the
lane's message language follows (no configuration surface: the
repository's history decides, never a switch).

``detect`` reads the last ``WINDOW`` commit bodies (history order,
oldest first) and answers ``zh`` when they carry more Chinese
characters (the CJK unified ideographs, extension A and the
compatibility block) than Latin letters; an exact tie — or nothing to
read — keeps English, the capsule's behaviour before this reading
existed. ``repo_language`` is the git side: the same reading over a
working clone's history at a ref, through the hardened ``pfstep.git``
(a read, never a hook path)."""

from typing import Sequence

from . import git

CHINESE = "zh"
ENGLISH = "en"

# the number of commit bodies the reading samples: the most recent ones
WINDOW = 50


def _is_cjk(ch: str) -> bool:
    """Whether ch is a Chinese character: the CJK unified ideographs,
    extension A and the compatibility ideographs."""
    cp = ord(ch)
    return 0x3400 <= cp <= 0x4DBF or 0x4E00 <= cp <= 0x9FFF or 0xF900 <= cp <= 0xFAFF


def cjk_chars(text: str) -> int:
    """The Chinese characters in text (the CJK blocks named above)."""
    return sum(1 for ch in text if _is_cjk(ch))


def latin_chars(text: str) -> int:
    """The Latin letters in text (ASCII letters only — digits, spaces
    and punctuation weigh nothing on either side)."""
    return sum(1 for ch in text if "a" <= ch <= "z" or "A" <= ch <= "Z")


def detect(bodies: Sequence[str]) -> str:
    """The language the bodies write: CHINESE when the last WINDOW of
    them carry more Chinese characters than Latin letters, ENGLISH
    otherwise (an exact tie reads English)."""
    recent = list(bodies)[-WINDOW:]
    cjk = sum(cjk_chars(b) for b in recent)
    latin = sum(latin_chars(b) for b in recent)
    return CHINESE if cjk > latin else ENGLISH


def repo_language(cwd: str, ref: str = "HEAD") -> str:
    """The language the repository at cwd writes its commits in: its
    most recent WINDOW bodies at ref, read through detect (history
    order, oldest first — the window keeps the newest)."""
    out = git.git(cwd, "log", "--max-count=%d" % WINDOW, "--pretty=format:%B%x1e", "--reverse", ref)
    bodies = [b for b in (part.strip() for part in out.split("\x1e")) if b != ""]
    return detect(bodies)
