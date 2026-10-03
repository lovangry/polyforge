# Per-WI watcher reconcile (on-demand)

The pf-work skill owns the claim-time discipline ("Per-WI watcher: 领取后必起").
This file owns the RECONCILE: session exits kill every bg_run watcher; the daemon
(setsid, session-independent) keeps the WIs running; the disk scripts survive.

## When
- Every session start (IR4), and any `/pf-status` run.

## Procedure
1. Live tickets: `curl --unix-socket <PF_DAEMON_SOCKET> http://localhost/status`
   → every `jobs[]` entry is a live WI on that daemon. Cross-check the hub for
   held WIs whose holder is this machine.
2. For each live WI: is there a running bg task watching it? (This session's task
   list; a fresh session has none.)
3. Missing watcher → relaunch:
   `sh ~/.polyforge/watchers/<wi>.sh <wi> <daemon-socket>` via bg_run
   (isAgent:false, name `watch <slug>`). The script polls that WI every 3s and
   EXITS on: needs_input (wake → relay the question to the owner), the WI leaving
   the live set (wake → terminal bookkeeping), socket down, or a 30-min checkpoint.
4. Do NOT relaunch while the WI's previous question is still unanswered (alert
   storm); relaunch once answered and the WI is still live.
5. Terminal WIs: delete `~/.polyforge/watchers/<wi>.sh` remnants.
6. Duplicate watchers are harmless (idempotent scripts) - when in doubt, relaunch.

## Drain-window caveat
In drain/batch windows the daemon claims tickets the main agent did not submit;
no claim event reaches the agent. Reconcile (step 1-3) must therefore run on
EVERY wake-up during a drain window, not just at session start - each wake, diff
the daemon's live set against the session's live watchers and fill the gaps.
