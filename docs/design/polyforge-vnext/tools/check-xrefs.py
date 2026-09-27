#!/usr/bin/env python3
"""Mechanical cross-reference checker for the Polyforge vNext design doc set
(docs/design/polyforge-vnext/*.md).

Anchor convention (plan step 17, gen-2): every numbered heading (levels 2-4)
MUST carry an explicit, machine-parseable anchor marker of the form
``<!-- §X.Y -->`` appended at the end of the heading line, where ``X.Y`` is
the heading's own leading section number. Every cross-reference MUST use the
unified inline form `` `NN-name.md#§X.Y` `` (shorthand `` `05` §2.9 `` and
split forms are disallowed and detected as errors).

For every cross-reference this script verifies:

  1. The target file `NN-name.md` exists in this directory.
  2. The target file contains a numbered heading that carries the explicit
     anchor marker ``<!-- §X.Y -->`` with exactly the referenced section
     number. A heading that merely starts with the number but lacks the
     marker is treated as a missing anchor (the convention is violated).
  3. Every anchor marker in every file is consistent with its heading's own
     leading number (a marker that disagrees with its heading is an error).

Exit code is 0 iff there are zero violations. Run from anywhere; the doc
directory is resolved relative to this script's location.
"""
import re
import sys
from pathlib import Path

DOC_DIR = Path(__file__).resolve().parent.parent
DOC_FILES = sorted(p for p in DOC_DIR.glob("*.md"))

HEADING_RE = re.compile(r"^(#{2,4}) (\d+(?:\.\d+)*[a-z]?)\.?\s+(.*)$")
ANCHOR_RE = re.compile(r"<!--\s*§(\d+(?:\.\d+)*[a-z]?)\s*-->\s*$")
REF_INLINE_RE = re.compile(r"`(\d{2}-[a-z0-9-]+\.md)#§(\d+(?:\.\d+)*[a-z]?)`")
# Disallowed legacy forms: shorthand `NN` §X.Y and split `NN-name.md` §X.Y
LEGACY_SHORTHAND_RE = re.compile(r"`(\d{2})`\s*§(\d+(?:\.\d+)*[a-z]?)")
LEGACY_SPLIT_RE = re.compile(r"`(\d{2}-[a-z0-9-]+\.md)`\s*§(\d+(?:\.\d+)*[a-z]?)")
LEGACY_PLAIN_RE = re.compile(r"(?<![\w.`#§])(0[0-7])\s*§(\d+(?:\.\d+)*[a-z]?)")


def load_anchors():
    """file name -> {section_number: heading_line_no}; plus marker mismatches."""
    anchors = {}
    mismatches = []
    for f in DOC_FILES:
        table = {}
        for i, line in enumerate(f.read_text(encoding="utf-8").splitlines(), 1):
            m = HEADING_RE.match(line)
            if not m:
                continue
            num = m.group(2)
            am = ANCHOR_RE.search(line)
            if am is None:
                mismatches.append((f.name, i, num, None))
                continue
            if am.group(1) != num:
                mismatches.append((f.name, i, num, am.group(1)))
                continue
            table[num] = i
        anchors[f.name] = table
    return anchors, mismatches


def main():
    anchors, mismatches = load_anchors()
    errors = []
    for fname, line, num, marked in mismatches:
        if marked is None:
            errors.append((fname, f"heading §{num} (line {line})",
                            "numbered heading lacks explicit `<!-- §X.Y -->` anchor marker"))
        else:
            errors.append((fname, f"heading §{num} (line {line})",
                           f"anchor marker says §{marked} but heading number is {num}"))

    total = 0
    for f in DOC_FILES:
        text = f.read_text(encoding="utf-8")
        for m in LEGACY_SHORTHAND_RE.finditer(text):
            errors.append((f.name, m.group(0), "legacy shorthand form; use `NN-name.md#§X.Y`"))
        for m in LEGACY_SPLIT_RE.finditer(text):
            errors.append((f.name, m.group(0), "legacy split form; use `NN-name.md#§X.Y`"))
        for m in LEGACY_PLAIN_RE.finditer(text):
            errors.append((f.name, m.group(0), "legacy plain form; use `NN-name.md#§X.Y`"))
        for m in REF_INLINE_RE.finditer(text):
            total += 1
            target_file, sect = m.group(1), m.group(2)
            if target_file not in anchors:
                errors.append((f.name, m.group(0), f"target file {target_file} does not exist"))
                continue
            if sect not in anchors[target_file]:
                errors.append((f.name, m.group(0),
                               f"no explicit anchor §{sect} in {target_file}"))

    if errors:
        print(f"FAIL: {len(errors)} violation(s)\n")
        for src, ref, reason in errors[:80]:
            print(f"  {src}: {ref}  ->  {reason}")
        if len(errors) > 80:
            print(f"  ... and {len(errors) - 80} more")
        return 1
    print(f"OK: {total} cross-reference(s) resolved against {sum(len(a) for a in anchors.values())} "
          f"explicit anchor marker(s) across {len(DOC_FILES)} documents, 0 violations.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
