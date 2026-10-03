## Post-claim Dispatch

Claim with `requires_human_session=false`: do **not** emit three-segment output -
dispatch `/pf-execute` as a write-capable subagent. **This skill is asking**,
satisfying the harness precondition for subagent dispatch.

**A claim always walks the wi's actual graph** - `rhs` only picks the driver
(`false` dispatches; `true` paces in-session) - never whether the graph runs.
A legacy/uncomposed wi (`steps_version == 0`) has no graph; no migration happens
here - `pf-execute` diagnoses it and stops (aihub#788).

**Dispatching a wi is not claiming it: say only the outcome and acceptance criteria,
never the execution path.** `claim -> pf_commit -> push -> PR` in a prompt is followed
literally and skips the graph.
