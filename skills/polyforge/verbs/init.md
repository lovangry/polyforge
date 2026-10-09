# init — install / upgrade a machine

Installing and upgrading are the same command: the repo's
`tools/install.sh`. It installs or updates the `pf` binary, writes the
client config, installs the routing skill into the user harness, and —
on execution machines — starts the local daemon. Nothing is inlined
here: the script's own daemon step is the invocation authority, and the
binary always comes from the script.

## Prerequisites

- a personal API key from the admin (`pf_k1_…`) — never install with an
  admin key, and never write the key into a repo, message or ticket;
- `gh` authenticated (the repo is private).

## Command

```
tools/install.sh <api-key> [server-host]
```

Run it again any time to upgrade in place: the script detects the
already-installed binary and skills and updates them to the latest
release.

## What it writes

The client config lives at `~/.config/polyforge/cli.toml` with exactly
five flat string keys:

| Key | Meaning |
|---|---|
| `server` | aihub base URL |
| `api_key` | the personal key this machine authenticates with |
| `daemon_socket` | the local daemon unix socket |
| `user_id` | caller identity |
| `project` | default project |

A missing config file is not an error (built-in defaults apply); a
malformed file is.

## Verify

```
pf --version        # installed binary and its version
pf queue            # aihub reachable, key accepted (an empty queue is fine)
pf ops status       # local daemon: jobs, mode, queue counters
pf machines         # admin liveness face: this machine heartbeats
```

Start the daemon by hand only when the script's daemon step did not run:
`pf daemon` (its flags are on its help face; the script passes the
workspace and workflows dirs it already prepared).

## Non-goals

- No downloads from this fragment: binary, config and skills all come
  from `tools/install.sh`.
- No workflow execution: this fragment is machine setup only — running
  work items routes to the submit verb's fragment.
