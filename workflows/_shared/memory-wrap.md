# Work item wrap summary (this step is the workflow's last)

When this step is about to complete (outcome `completed`, a pass verdict), write exactly ONE `experience.wrap` memory for the work item, besides the step memory above (the daemon uploads it before the work item is sealed and refuses a second one):

1. Body (≤ 2 KB): 做了什么 (what was done), 关键决定 (the key decisions and why), 遗留 (follow-ups, open questions, known gaps), then a `policy-proposal memories:` list naming every memory of this work item tagged `policy-proposal` — the `[policy-proposal]` lines of `artifacts/memory-log.md` and the files under `artifacts/memory-outbox/` (including `pending/`) whose frontmatter tags carry it (title each) — or `none`.
2. `pf remember --project {{project}} --type experience.wrap --work-item '{{work_item}}' --title '{{work_item}}: <what was done, one line>' @<file>`.
