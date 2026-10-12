# ship — push a closed work item's sealed commits to git origin

Execution-plane verb: routed to the local daemon (the wrap→git delivery
bridge, the 13th verb). `ship` is the delivery step AFTER `wrap`: the
closed WI's task-repo commits live in the sealed capsule, and `ship` pushes
them to each inventory repo's own origin as the fixed-name branch
`polyforge/<id>`, then opens or reuses a PR when a PR credential exists.

## Triggers

- EN: ship it / push the wrapped branch to origin / deliver it / open the
  PR for the wrapped work item / push the sealed commits
- 中文: 推上去 / 交付 / 把封存提交推送到远端 / 给收尾的票开 PR / 推送分支

## Command

```
pf ship <id>
```

One argument, no flags. The daemon reads the sealed capsule on this machine
(read-only access; the write ban on the sealed scene holds) and pushes each
`repos/<name>` worktree's sealed HEAD to that repo's origin. No AIHub
credential is involved — the push uses the machine's existing git push
identity.

The inventory itself is never resolved by hand: the capsule's
`repos/<name>` worktrees were cloned at claim from the repo list the aihub
project registry serves.

## Success

Human mode: `shipped <id> → branch polyforge/<id>` plus one line per
inventory repo (`pushed`, or `reused (remote branch already at the sealed
commit)` on a repeat ship — the branch is never pushed twice, never
force-pushed) and the PR URL; `--json`: the raw answer
(`{work_item_id, branch, repos:[{name, origin_url, head, branch, pushed,
reused, pr_url, compare_url}]}`). When no PR credential is available the
answer carries a compare URL instead of a PR URL and the op STILL SUCCEEDS
— a missing PR credential is not an error; only the push credential is.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-404` | This machine holds no sealed capsule for the WI | The capsule lives on the machine that executed the WI — run `pf ship` there. |
| `DAEMON-409` | The work item is still active on this daemon, or the remote branch `polyforge/<id>` moved past the sealed commit | Active: wrap it first. Moved: ship never force-pushes over it — inspect the remote branch and reconcile by hand. |
| `DAEMON-403` | Reserved branch target (main/master) | ship structurally never pushes main/master; this id cannot be shipped as-is — bridge the commits manually. |
| `DAEMON-512` (`SHIP_PUSH_PREFLIGHT_FAILED`) | No usable local git push identity (SSH/credential helper) or the origin is unreachable | The push was never initiated. Fix the machine's git push credential or the origin reachability, then re-run — ship is idempotent. |
| `dial unix …` | Daemon not running | Nothing can be shipped without the local daemon — start it or check `--socket`. |

A successful ship is safe to repeat: a second run with the same sealed
commit reuses the branch and re-attaches the existing PR (or compare URL).
