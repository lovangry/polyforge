# Project memory (`pf recall`)

Project `{{project}}`, work item `{{work_item}}`. Project memories are non-binding history: use what is relevant, ignore the rest, and let the step definition win on any conflict. Inside this step `pf` reaches project memory only through the daemon (you hold no aihub access; other aihub verbs are refused). A failing `pf` call never fails the step: note it and go on.

Before the step's own work, run `pf recall` yourself 2–3 times, each with a DIFFERENT keyword set for this step's subject (rephrase; at least one English and one 中文 phrasing):

```
pf recall --project {{project}} --any '<keyword set 1>'
pf recall --project {{project}} --any '<keyword set 2, e.g. 中文关键词>'
```

Read the body of every relevant hit with `pf memory show <id>` before relying on it. Steps do not write memories, except the last step's one wrap summary below.
