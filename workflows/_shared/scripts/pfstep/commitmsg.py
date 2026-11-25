"""The messages the lane writes in a task repository, in the language
the work item's own goal writes (``goal_language``'s reading of the
goal the capsule's runtime-state/work-item.json carries): the
conventional prefix — the repository's own type-enum word — is kept
verbatim, never translated; the line follows the work item's goal, and
a Chinese-goal line refuses to carry no Chinese character at all (an
English goal is left exactly as it was — mixed lines are its to write).
The repository's own commit-msg hook stays the judge of type-enum and
header length; the machine messages (sync, landing) run through the
hardened no-hooks git, never wired to a hook."""

CHINESE = "zh"
ENGLISH = "en"


def _is_cjk(ch: str) -> bool:
    """Whether ch is a Chinese character: the CJK unified ideographs,
    extension A and the compatibility ideographs."""
    cp = ord(ch)
    return 0x3400 <= cp <= 0x4DBF or 0x4E00 <= cp <= 0x9FFF or 0xF900 <= cp <= 0xFAFF


def cjk_chars(text: str) -> int:
    """The Chinese characters in text (the CJK blocks named above)."""
    return sum(1 for ch in text if _is_cjk(ch))


def goal_language(goal: str) -> str:
    """The language the work item's own goal writes: CHINESE when the
    goal carries a Chinese character (the lane works in Simplified
    Chinese), ENGLISH when it does not — the repository's history is
    never the fact it follows."""
    return CHINESE if cjk_chars(goal) > 0 else ENGLISH


def _line_problem(line: str, lang: str) -> str:
    """Why the line cannot be this goal's language ("" = it can): a
    Chinese-goal line carries no Chinese character."""
    if lang == CHINESE and cjk_chars(line) == 0:
        return "a Chinese-goal line carries no Chinese character: %r" % line
    return ""


def subject(lang: str, kind: str, line: str, tag: str = "") -> str:
    """A capsule commit's subject: ``<kind>: <line>`` — the repository's
    type-enum word kept verbatim — with the marker tag appended as
    `` (tag)`` when given, the line in the work item's goal's
    language."""
    problem = _line_problem(line, lang)
    if problem != "":
        raise ValueError(problem)
    out = "%s: %s" % (kind, line)
    return "%s (%s)" % (out, tag) if tag != "" else out


def pr_title(lang: str, slug: str, line: str) -> str:
    """The publish PR's title: ``<slug>: <one line>`` — the slug and the
    colon-space shape fixed, the one line in the work item's goal's
    language (Simplified Chinese by default: a goal that carries a
    Chinese character makes the line Chinese)."""
    problem = _line_problem(line, lang)
    if problem != "":
        raise ValueError(problem)
    return "%s: %s" % (slug, line)


def sync_message(lang: str, default_branch: str, branch: str) -> str:
    """The sync merge message in the work item's goal's language:
    byte-identical English for an English goal, Chinese for a Chinese
    one."""
    if lang == CHINESE:
        return "合并 %s 到 %s (sync)" % (default_branch, branch)
    return "Merge %s into %s (sync)" % (default_branch, branch)
