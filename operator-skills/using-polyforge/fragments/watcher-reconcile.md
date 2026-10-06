# Per-WI watcher reconcile (on-demand)

THIS FILE owns the whole per-WI watcher discipline (claim-time start AND
reconcile): every successful claim/submit/takeover on this machine must
start a per-WI watcher for that WI before doing anything else (one watcher
per WI, 3s poll, disk script under `<workspace>/.polyforge/watchers/`); session exits
kill every background-run watcher while the daemon (setsid, session-independent)
keeps the WIs running, and the disk scripts survive for re-entry.

## When
- Every session start (IR4), and any `pf ops status` run.

## Procedure
1. Live tickets: `curl --unix-socket <PF_DAEMON_SOCKET> http://localhost/status`
   → every `jobs[]` entry is a live WI on that daemon. Cross-check the hub for
   held WIs whose holder is this machine.
2. For each live WI: is there a running bg task watching it? (This session's task
   list; a fresh session has none.)
3. Missing watcher → relaunch:
   `sh <workspace>/.polyforge/watchers/<wi>.sh <wi> <daemon-socket>` via the
   harness's background-run mechanism (a named background task, e.g.
   `watch <slug>`). The script polls that WI every 3s and
   EXITS on: needs_input (wake → relay the question to the owner), the WI leaving
   the live set (wake → terminal bookkeeping), socket down, or a 30-min checkpoint.
4. Do NOT relaunch while the WI's previous question is still unanswered (alert
   storm); relaunch once answered and the WI is still live.
5. Terminal WIs: delete `<workspace>/.polyforge/watchers/<wi>.sh` remnants.
6. Duplicate watchers are harmless (idempotent scripts) - when in doubt, relaunch.

## Drain-window caveat
In drain/batch windows the daemon claims tickets the main agent did not submit;
no claim event reaches the agent. Reconcile (step 1-3) must therefore run on
EVERY wake-up during a drain window, not just at session start - each wake, diff
the daemon's live set against the session's live watchers and fill the gaps.
