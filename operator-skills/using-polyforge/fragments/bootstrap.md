## Session Startup Scan

On session start, before responding to any user message:

**Step 0 - Repo map first.** The workspace root `CLAUDE.md` `## Workspace`
managed block is already in context - consult it *before* anything else; do
**not** re-read the file. Absent -> `Read` it, or
run `polyforge init` to write it.

Then the daemon/wi scan:

1. `pf ops status` - this machine's daemon: live jobs, mode, queue counters.
   A `dial unix` failure = daemon down (IR3): report it, or start it with
   `pf daemon` if that is your call.
2. For each live job, `pf observe <id>` - step, attempts, budget, pending
   question. Relay `needs_input` questions to the owner (the `answer` verb
   carries the reply).
3. `pf queue --ready` - the project's ready queue.
