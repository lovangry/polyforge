## Iron Rules (all operations must obey)

**IR1 - Work-item-gated writes**
Every write operation (`git commit`, `git push`, `gh pr create`, `Edit`/`Write` under
`.repo/`) must occur inside a claimed work-item worktree (`pf.<project>-<seq>/<repo>/`).
No env-var bypass.

**IR2 - Analyze obstacles; track blockers as work items**
When you hit an obstacle, analyze the root cause - do not route around it. If the
obstacle is a bug or cannot be resolved in the current context, create a work item to
track it (`/pf-work --goal "bug: ..."`).

**IR3 - Server/daemon unreachable -> stop**
(v1.0.0: the MCP face is gone; the connectivity surfaces are the pf CLI -> aihub
HTTP and the pf CLI -> local daemon UDS.) If either surface is unreachable,
report the error and stop. Do not fall back to improvised raw curl/HTTP calls
or route around the broken layer. Diagnose with `pf doctor`, then retry.

**IR4 - Session start: reconcile per-WI watchers**
On entering a session (and on any `/pf-status`), if any claimed, non-terminal WI lacks a
live per-WI watcher, relaunch it from its disk script (`~/.polyforge/watchers/<wi>.sh`)
per `fragments/watcher-reconcile.md` - do not let a session restart leave a live WI
unwatched.


**IR5 - Speak plainly**
When reporting to the owner, use everyday words. Lead with the answer; explain
mechanisms with analogies (chef/restaurant beats "supervision goroutine");
introduce a technical term only when it names something the owner must act on,
and gloss it on first use. A jargon wall is a bug in the report, not a sign of
rigor. If a two-line summary and a table both work, give both: table for the
eyes, plain sentence for the decision.
