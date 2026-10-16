# Project memory (`pf recall` / `pf remember`)

Project `{{project}}`, work item `{{work_item}}`. Project memories are non-binding history: use what is relevant, ignore the rest, and let the step definition win on any conflict. Inside this step `pf` reaches project memory only through the daemon (you hold no aihub access; every other aihub verb is refused). A failing `pf` call never fails the step: note it in your output and go on.

**At the START of the step — recall.** Before the step's own work, run `pf recall` yourself 2–3 times, each with a DIFFERENT keyword set for this step's subject (rephrase; at least one English and one 中文 phrasing):

```
pf recall --project {{project}} --any '<keyword set 1>'
pf recall --project {{project}} --any '<keyword set 2, e.g. 中文关键词>'
```

Read the body of every relevant hit with `pf memory show <id>` before relying on it.

**At the END of the step — at most ONE memory.** Before you write the result envelope, judge whether this step produced something worth remembering under the project's memory policy:

Record (worth a memory):
{{record}}

Skip (never a memory):
{{skip}}

Nothing worth remembering is the common case: then write nothing. Otherwise write exactly one:

1. Put the body (≤ 2 KB; a pitfall as 现象 → 原因 → 解法) in a temporary file outside the capsule (`mktemp`).
2. `pf remember --project {{project}} --type experience.<kind> --work-item '{{work_item}}' --title '<one-line title>' @<file>` — `<kind>` names the lesson (debug, env, decision, convention, …). It queues the memory in `artifacts/memory-outbox/`; the daemon uploads it after this step (refusing an invalid one or a second one, and skipping a duplicate of an existing memory) and records the outcome in `artifacts/memory-log.md`. A `pf recall --any` for the lesson's keywords first is optional.

A lesson that fits no Record entry but that you judge valuable is still written: add `--tags policy-proposal` and end its body with the line `建议新增条目：<the Record entry you propose>`. Never put a secret or credential value into a memory — name where it lives instead (the daemon refuses credential-looking content).
