---
name: using-polyforge
description: >
  Use at the start of any polyforge session and before any polyforge skill, work-item, or
  memory operation; loaded automatically at session start.
---

<!--
  MAINTAINER NOTES: references/manifest-notes.md - READ IT BEFORE EDITING THIS FILE.
  It holds the SIZE BUDGET rule, the kind/gate/authority schema, the tier rule and its
  BASELINE ratchet, the argument for which channel owns a rule, and why the on-demand
  verb is spelled the way it is. It sat in this comment until aihub#302, at 25,193 of
  this file's 26,558 bytes (94.9%, markers included). Measured before moving it: this
  file's BODY is charged only on the requests where the skill is INVOKED, never at
  session start - and this skill is designed not to be invoked, so 25 KB of it here
  bought no reach and priced every direct invocation. Both arms are in the notes.

  Four things not to get wrong without going and reading it:
    1. This file is a MANIFEST: its @include fragments are the resident payload, read at session start.
       To edit the skill, edit a FRAGMENT, not this file. An include directive pulls a
       fragment into the RESIDENT tier; an on-demand directive declares one that ships
       as a file and is NEVER injected. A directive may be followed by an attribute
       block; `when:` makes an include conditional (no `when:` = always).
    2. HARD SIZE BUDGET. The assembled payload must stay inside the two-sided band recorded
       in references/manifest-notes.md (currently ~5,987 chars across 7 resident fragments,
       gate ≤8,497), well under the harness's 10,000-character limit - above which the
       payload is silently replaced by a ~2,000-char preview. If the band goes red because
       you ADDED content, move a fragment to the on-demand tier. Do NOT raise the gate. If
       it goes red because you REMOVED content, lower the recorded gate to the new total.
    3. Every directive carries kind / gate / authority (plus resident-because, plus
       `when:` as the FIRST attribute line). references/manifest-notes.md records
       that schema and the TIER RULE: an unenforced rule must not leave the
       resident tier.
    4. Never start a line inside this comment with a bare directive verb - the parser
       strips each line before matching, so an indented example would be parsed for real.

  When invoked directly via the Skill tool (rare - this skill is auto-injected), read the
  fragments below to see the full content.
-->

@include: fragments/iron-rules.md
kind: rule
gate: none
authority: self

@include: fragments/output-format.md
kind: rule
gate: none
authority: self

@include: fragments/memory-first.md
kind: rule
gate: none
authority: self

@include: fragments/bootstrap.md
kind: rule
gate: none
authority: self

@include: fragments/nl-routing.md
kind: rule
gate: none
authority: each polyforge verb's own Triggers section
resident-because: it is the cross-verb index - a single verb's Triggers section cannot tell you WHICH verb to run, and that is the decision made before any skill is read.

@include: fragments/repo-routing.md
kind: rule
gate: none
authority: self

@include: fragments/on-demand-index.md
kind: info
authority: self

@ondemand: fragments/workflow-v2.md
kind: info
authority: self

@ondemand: fragments/post-claim-routing.md
kind: rule
gate: none
authority: self

@ondemand: fragments/memory-conventions.md
kind: rule
gate: none
authority: self

@ondemand: fragments/diagram-convention.md
kind: info
authority: self

@ondemand: fragments/platform-adaptation.md
kind: info
authority: references/codex-tools.md + references/copilot-tools.md

@ondemand: fragments/repo-detail.md
kind: info
authority: self
