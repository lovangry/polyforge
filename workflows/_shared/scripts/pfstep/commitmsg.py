"""The messages the lane writes in a task repository, in the language
the repository itself writes (``commitlang``'s reading of its history):
the conventional prefix — the repository's own type-enum word — is
kept verbatim, never translated; the line follows the repository's
language, and a Chinese-primary repository refuses a line that carries
no Chinese character at all (an English-primary repository is left
exactly as it was — mixed lines are its to write). The repository's own
commit-msg hook stays the judge of type-enum and header length; the
machine messages (sync, landing) run through the hardened no-hooks git,
never wired to a hook."""

from . import commitlang


def _line_problem(line: str, lang: str) -> str:
    """Why the line cannot be this repository's language ("" = it can)."""
    return ""


def subject(lang: str, kind: str, line: str, tag: str = "") -> str:
    """A capsule commit's subject: ``<kind>: <line>`` — the repository's
    type-enum word kept verbatim — with the marker tag appended as
    `` (tag)`` when given, the line in the repository's language."""
    return ""


def pr_title(lang: str, slug: str, line: str) -> str:
    """The publish PR's title: ``<slug>: <one line>`` — the slug and the
    colon-space shape fixed, the one line in the repository's
    language."""
    return ""


def sync_message(lang: str, default_branch: str, branch: str) -> str:
    """The sync merge message in the repository's language:
    byte-identical English in an English-primary repository, Chinese in
    a Chinese-primary one."""
    return ""
