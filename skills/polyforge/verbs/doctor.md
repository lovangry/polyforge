# doctor — the machine's single health face

CLI-meta verb: one command reports whether this machine can do its job —
the config, the `[workspace]` mapping (polyforge#113) and the three
connectivity faces (aihub server reach, local daemon socket, auth), plus
whether each harness will display polyforge progress. It
is a pure read face: nothing is mutated, nothing is auto-fixed (there is
no `--fix`; remediation is always manual, the check detail names it).

## Triggers

- EN: is this machine healthy / something's wrong / why won't the daemon
  start / check the workspace mapping / diagnose this box / pre-upgrade
  check / is aihub reachable
- 中文: 体检 / 诊断 / 这台机器健康吗 / daemon 为什么起不来 / 检查映射 /
  升级前自检

## Command

```
pf doctor [--harness] [--json]
```

`--harness` runs only the two progress-display checks (no config, no
network); `tools/install.sh` prints its rows.

Exit codes follow the repo convention: 0 every check passes, 1 any check
fails, 2 usage errors. `--json` emits one machine document:
`{"ok": <bool>, "checks": [{"name", "status": "pass"|"fail"|"skip"|"warn",
"detail"}]}`.

## The checks

| Check | What it proves | On fail |
|---|---|---|
| `config` | the config loads | the load error verbatim (an old-shape flat `project` key or a malformed mapping names itself) plus migration guidance; every config-dependent check is then `skip` — never a faked green |
| `workspace mapping` | at least one `[workspace."<path>"]` section exists | guidance to add the shape (a zero-section config is the fresh-install template) |
| `workspace dirs` | every mapped workspace pre-exists as a directory | the missing path(s), named |
| `aihub projects` | every mapped project is known to aihub (the project read face) | the project AND its workspace, named (an ambiguous inverse or duplicate surfaces through the config check, both workspaces named) |
| `server reach` | aihub answers at all (any HTTP answer counts) | the dial failure |
| `daemon socket` | the local daemon answers GET /status over its unix socket | the socket path and the dial error — start the daemon |
| `auth` | the api key gets past aihub's auth boundary (the same signed read the projects check uses) | the typed 401/403 answer — fix `api_key` |

Checks that cannot be proven (config unloadable, aihub unreachable) are
reported `skip` — a skip never pretends to be a pass.

## Progress display (presentation only — never `fail`, never moves the exit code)

| Check | `pass` | `warn` (detail carries the fix) | `skip` |
|---|---|---|---|
| `claude progress display` | `~/.claude/settings.json`'s statusLine command runs `pf watch --statusline`, directly or in a script file it names | no statusLine → the full `"statusLine"` snippet; a statusLine without pf → an append-style snippet that runs the user's command first, keeps its output, then the progress rows; unreadable/malformed settings, or a named script that could not be checked (relative, missing, or itself naming another script) → "cannot tell" | no `~/.claude` |
| `pi progress display` | pi settings list the polyforge package (or `pf-chat.ts` itself) | the `pi install …` line; malformed settings → "cannot tell" | no pi agent dir (`$PI_CODING_AGENT_DIR` or `~/.pi/agent`) |

The judgement is static: the statusLine command is read, never run, and
no user file is ever written — the user pastes the snippet.

## Workspace yaml

`pf doctor` reads cli.toml only. A workspace's `.polyforge.yaml` (per-project
model candidates) is validated by the daemon before each claim, not by
doctor: an invalid file shows up as a DAEMON-414 refusal on `pf submit`.

## Success

`ok: true` and every check `pass` (exit 0). Human mode prints one line
per check with its status and detail; `--json` prints the document only.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| exit 1, `config` fail | old-shape or malformed config | Follow the printed migration steps: replace the flat `project` key with a `[workspace."<path>"]` section (`projects = [...]`), then re-run. |
| exit 1, `workspace mapping` fail | zero sections (fresh template) | Add a real mapping to the config — the daemon refuses to start without one. |
| exit 1, `workspace dirs` fail | a mapped directory is missing | Create it (the daemon never does) or fix the `[workspace]` key. |
| exit 1, `aihub projects` fail | a mapped project is unknown to aihub, or the project read fails | File/register the project on aihub or fix the mapping; the detail names the project and its workspace. |
| exit 1, `server reach` fail | aihub unreachable | Check `--server` / the config's `server` and whether the aihub host is up. |
| exit 1, `daemon socket` fail | no live daemon | Start it (`pf daemon`); the config's `[workspace]` mapping must be real first. |
| exit 1, `auth` fail | the credential is rejected | Fix the api key (config `api_key` / `PF_API_KEY`). |
| exit 2 | usage error (unknown flag) | Fix the invocation. |

A `dial unix …` inside the daemon-socket check's detail means the daemon
is not running on this machine — it is a finding, not a crash of the
doctor itself.
