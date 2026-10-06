## Iron Rules

**IR1 - Work-item-gated writes**
Every write (`git commit`, `git push`, `gh pr create`, file edits) happens
inside a claimed work-item worktree (`ws/capsules/<wi>/<repo>/`). No env-var
bypass.

**IR2 - Analyze obstacles; track blockers as work items**
Root-cause obstacles - do not route around them. If a blocker is a bug or
unresolvable here, track it as a work item:
`pf file create --project <P> --goal "bug: ..."`.

**IR3 - Server/daemon unreachable -> stop**
If either v1 surface (pf CLI -> AIHub HTTP / daemon UDS) is unreachable,
report and stop - no raw curl, no routing around it. Diagnose via
`pf ops status` plus the polyforge skill's doctor verb
(`operator-skills/polyforge/verbs/doctor.md`), then retry.

**IR4 - Session start: reconcile per-WI watchers**
On session entry (and any status check), relaunch each claimed, non-terminal
WI's missing watcher from
`<workspace>/.polyforge/watchers/<wi>.sh` per `fragments/watcher-reconcile.md`.

**IR5 - Speak plainly**
Lead with the answer; use everyday words and analogies (chef/restaurant beats
"supervision goroutine"); gloss technical terms only when the owner must act
on them. A jargon wall is a bug. If summary and table both work, give both.

**IR6 - AIHub data plane: pf CLI only**
Never curl the AIHub server directly from the harness. All data-plane
interaction (queue, submit, observe, answer, etc.) goes through the pf
CLI — it encapsulates auth, error handling, and request tracing. Direct
curl bypasses all of these (demonstrated: polyforge#72 was created via
curl without a bundle, unclaimable). Daemon-UDS status and release-server
downloads are different surfaces, not banned.
