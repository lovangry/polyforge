# Work item wrap summary (this step is the workflow's last)

When this step is about to complete (outcome `completed`, a pass verdict), write exactly ONE `experience.wrap` memory for the work item, besides the step memory above:

1. Skip it when `artifacts/memory-log.md` already lists an `experience.wrap` line: the work item has its summary (the log travels with the capsule across rewinds and re-claims).
2. Body (≤ 2 KB): 做了什么 (what was done), 关键决定 (the key decisions and why), 遗留 (follow-ups, open questions, known gaps), then a `policy-proposal memories:` list naming every `policy-proposal` line of `artifacts/memory-log.md` (id + title), or `none`.
3. `pf remember --project {{project}} --type experience.wrap --work-item '{{work_item}}' --title '{{work_item}}: <what was done, one line>' @<file>`, then append its line `<id> experience.wrap <title>` to `artifacts/memory-log.md`.
