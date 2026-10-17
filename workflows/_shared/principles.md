First read each `repos/<repo>/AGENTS.md` (else `CLAUDE.md`): its project rules bind too and win any conflict, noted in your artifact.

1. **Simple.** Simplest mechanism, same behavior; simplifying never drops features. No concept, state, field, error code or config the requirement did not ask for; if it forces one, say so and propose changing it. One fact, one place; reuse before adding.
2. **Recoverable.** Resumable from any interruption: intent before action, atomic writes, idempotent replay. Before adding halfway-state logic, check whether restart recovery covers it. No I/O under a lock; no sleep or backoff posing as synchronization.
3. **Least privilege** (against accidents). Credentials stay out of agent env, command lines, logs and artifacts (name variables, never values). A new entry point states who can reach it.
4. **Evidence.** Verify before asserting; label guesses; back "absent" with how you searched (empty may mean unindexed). New tests fail on the old code; never relax an assertion without saying why. Docs change with code. Confirm shipped features act on real data.
5. **The owner decides behavior.** Flag any behavior change or conflict with a past ruling and ask; never apply it by default.
