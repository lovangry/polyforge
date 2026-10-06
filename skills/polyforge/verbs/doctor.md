# doctor — from pf-doctor

# pf-doctor - Workspace Health Check

## Usage

**Purpose**: Diagnose workspace, config, repo, worktree, and version health; optionally auto-fix.

**Pattern**: `/pf-doctor [--fix]`

**Required**: none

**Flags**:
- `--fix` - apply auto-fixable repairs - guarded orphan-worktree cleanup ONLY: the
  repo check reports problems but never clones or re-syncs, so missing repositories
  need a separate `polyforge init`. Leaves manual-only issues for the user
- `--stale-after=<duration>` - narrow the stale_running window for this run (server
  default 2h)
- `--uninstall` - restore the statusLine to its pre-polyforge state (undo the pf-work chain takeover)

## When to use

- "pf doctor" / "check workspace" / "diagnose" / "something's broken"
- Any pf-* tool returns connection error, config error, or "state file not found"
- First session on a new machine after `polyforge init`
- After returning from a long absence (stale worktrees, drift)

## Mechanic

Run the CLI diagnostic tool and interpret results:

```bash
polyforge doctor
# With auto-fix for fixable issues:
polyforge doctor --fix
# Remove one worktree --fix refused (read the reason first - Check 4):
polyforge doctor --fix --force-remove=<dir>
# Its work item is still active? The status must be transcribed and must match:
polyforge doctor --fix --force-remove=<dir>:<status>
```

The CLI runs 10 checks (§12.1) and prints `[ok]`, `[warn]`, or `[FAIL]` per check:

| # | Check | What it tests | Auto-fix |
|---|-------|---------------|----------|
| 1 | workspace | `.polyforge.yaml` found via upward search from wsRoot | `polyforge init` |
| 2 | config | aihub endpoint/credential availability + server health (GET `/v1/health`); a `config.toml` LOAD failure prints separately on stderr and is not this check's verdict | manual |
| 3 | repos | `.repo/<name>/` exists for each project repo, and `.repo/<owner>__<repo>/` for each project's `scenario:` URL; remote URL matches `.polyforge.yaml` in both cases | `polyforge init` (**not** `--apply` - that is a no-op) |
| 4 | worktrees | `pf.*` dirs cross-checked vs the server's **full** wi list (paginated); flags orphans | `polyforge doctor --fix` |
| 5 | stale_running | per project, the ready queue's `stale_running` segment: running attempts silent past the window (server default 2h, `--stale-after` narrows it); alert-only, names holder + last-seen | manual |
| 6 | branch-upstream | task worktrees whose branch tracks a protected branch (a bare `git push` with `push.default=upstream` would land there) | manual |
| 7 | version | server's `min_client_version` reported informationally - NO comparison occurs, the local binary always reads as `dev` | - (informational) |
| 8 | claude_md | CLAUDE.md `## Workspace` block format (slim vs legacy inline) + `.polyforge/repo-map/<project>.md` present for every project | `polyforge init` |
| 9 | usage_md | `.polyforge/usage.md` still carrying rule sections the `using-polyforge` skill owns (Iron Rules / NL Routing / Memory Type Reference). That file is never regenerated, so the copy there cannot be corrected and a session sees two (aihub#294) | **manual** - `--fix` does not touch it |
| 10 | pi_integration | the pi bridge record (`~/.pi/agent/extensions/polyforge/polyforge-pi.json`) points at an available, current polyforge root | re-run the printed bootstrap / `pi/install.sh` |

The ten above run out of the binary. The statusLine, seam-check, and binary-freshness
probes below are **skill-side additions** the CLI does not run - keep them separate in
the report.

Then read the launcher's binary-status marker (the binary-freshness probe below) - one `cat`, no binary needed:

```bash
cat ~/.polyforge/binary-status.txt   # "No such file or directory" IS the ok result
```

No `2>/dev/null`, no `|| echo`: this whole check exists because the launcher used to
suppress exactly that kind of error, and `cat`'s own message already says which of the two
outcomes happened.

Then run the seam-check probe (read-only, pinned to the cached `superpowers` plugin version):

```bash
bash "${CLAUDE_PLUGIN_ROOT}/bin/pf-seam-check"
```

Treat any `[WARN]` line in its output as a `warn` on the report below (it always exits 0, so the exit code carries no signal -- parse the printed lines).

## Interpretation Guide

### Check 1 - workspace FAIL
`.polyforge.yaml` not found. Run `/pf-init` skill to scaffold.

### Check 2 - config FAIL
aihub unreachable, or no API key configured. The check assesses endpoint/credential
availability and server health - NOT `config.toml` validity: a load failure prints
separately on stderr (`polyforge: … could not be loaded`) and execution continues
with an empty config, so inspect that line yourself. Triage:
1. `cat ~/.polyforge/config.toml` - verify `[server] url` and `[auth] api_key`
2. Rerun `polyforge doctor` and read Check-2's verdict - that check itself exercises
   the endpoint+credential health path (GET `/v1/health`) with the configured auth, so
   it IS the reachability test. Never a raw curl at the aihub server (IR6: the pf CLI
   owns every aihub data-plane call).
3. If API key missing: configure `[auth] api_key` in `~/.polyforge/config.toml` or
   export `POLYFORGE_API_KEY`, then rerun. `polyforge init` does NOT prompt for a
   key - it refuses without one and only prints the `[auth] api_key` instructions.

### Check 3 - repos warn
Missing or mismatched `.repo/` dirs.
- Missing: run **`polyforge init`** to clone all repos. Not `polyforge init --apply`:
  `--apply` is deprecated and a hard no-op - it prints a deprecation line and returns
  before the clone loop, before the repo map, before CLAUDE.md. Following it looks like
  a fix and does nothing (aihub#307).
- Remote mismatch: `git -C .repo/<name> remote set-url origin <correct-url>`.
  `init` cannot do this for you - it only fetches and resets an existing checkout.
- `missing: scenario <owner>__<repo>` after upgrading the binary is expected once:
  scenario clones used to be keyed on the repo name alone, so two orgs' same-named
  scenario repos shared one directory and the second silently ran the first's step
  graph (aihub#327). `polyforge init` clones to the owner-qualified path and leaves
  the old `.repo/<repo>/` where it is - do NOT delete that directory reflexively, in
  most workspaces it is also a project repo in its own right.

### Check 4 - worktrees warn
Orphan `pf.*` directories found (no matching active wi on the server).

Each reported directory carries its work item's status, e.g.
`pf.aihub-307 [wrapped]`. `--fix` removes a directory **only** when that work
item is provably terminal (`wrapped` / `failed` / `cancelled`); anything else -
`running`, `paused`, `queued`, `blocked`, or a work item it could not read at
all - is printed with the reason and **kept**.

- Auto-remove the safe ones: `polyforge doctor --fix`
- A directory nothing could be established about - unreadable work item, no such
  work item, or a name polyforge does not produce (`pf.scratch`, `pf.aihub-307.bak`):
  `polyforge doctor --fix --force-remove=<dir>`. Being wrong here costs a stale
  directory, so naming it is enough.
- A directory whose work item is **active** (`running`/`paused`/`queued`/`blocked`)
  needs the status transcribed as well: `--force-remove=<dir>:<status>`, and it must
  equal what the server reports right then. Being wrong here costs somebody's
  uncommitted work, so `--fix` refuses the bare name and does not print a
  ready-made bypass - look the status up, or wrap the work item first. Any forced
  removal reports `[warn]`, never `[ok]`.
- If `--fix` says it KEPT something because the work item is **not** terminal,
  that is a bug report, not a nuisance. It means the active listing missed a live
  work item - the aihub#307 shape, where an unpaginated query saw only the
  server's first 50 rows and nominated 5 live worktrees (one of them `running`)
  for deletion.

### Check 7 - version (informational)
Not an upgrade detector: no version comparison occurs. A reachable version endpoint
always reports `ok` with the server's `min_client_version` and a literal `local=dev`.
Binary and plugin upgrades go through the actual launcher / installation mechanism
(`bin/polyforge-mcp.sh` and the plugin install), NOT `/pf-init` - that skill clones
and renders workspace files; it reinstalls neither binary nor plugin.

### Check 8 - claude_md warn
Two distinct warnings, both fixed by re-running `polyforge init` in the workspace:

- **"managed block is the legacy inline format"** - the block still repeats every repo's
  `stack` / `modules` / `changes` / `generated` inline. That text sits at context position
  0, is re-read on every request, and compaction cannot drop it. `polyforge init` moves it
  to `.polyforge/repo-map/<project>.md`, read on demand at routing time. Measured on the
  gmi-ws workspace: 29,650 of the block's 34,606 bytes. This never auto-fixes - say what it
  costs and let the user decide when to re-init.
- **"repo map missing …"** - the block is already slim but `.polyforge/repo-map/` is
  absent, empty, or has no file for some project. Routing then has only the one-line
  positioning, which is not enough to locate code inside a repo. Surface it rather than
  letting an agent guess; `codegraph_*` / `Grep` in the worktree are the interim fallback.

Never a FAIL: a stale block is a cost, not a broken workspace.

### Check 9 - usage_md warn
**".polyforge/usage.md still carries N rule section(s) that using-polyforge owns"** - the
workspace was created before aihub#294, when `polyforge init` wrote the Iron Rules, NL
Routing and the memory-type table into `usage.md` as well as shipping them in the skill.
`writeUsageMd` refuses to overwrite an existing `usage.md` (it is the user's file), so a
template change cannot reach this workspace and the session gets both copies - of which
only the skill's can ever be corrected. They have already drifted once.

**Report-only - `--fix` deliberately does not touch this file.** Tell the user to open
`.polyforge/usage.md` and delete the named `## ` sections; the maintained copy ships with
the skill. Automating it means inferring each section's extent from the structure of a
file the user may have edited, and review found six input classes where that destroyed
their content - three of them leaving an unterminated fence or HTML comment that swallows
the rest of the document. Doing it only when a span is byte-identical to a known template
version is the correct primitive and is left to a follow-up.

A second warning, **"unterminated code fence or HTML comment"**, means the scan could not
read to the end of the file, so it is reporting "did not look" rather than "found
nothing". Fix the markdown, then re-run.

Never a FAIL - the workspace works, it is just being told the rules twice.

### Skill-side: statusLine (pf-work chain)
Verify the wi-progress chain takeover is healthy:
- `<ws>/.claude/settings.json` `statusLine.command` contains `pf-statusline.cjs`.
- `<ws>/.claude/helpers/pf/pf-statusline.cjs` and `pf-chain-render.cjs` both exist.
- `<ws>/.claude/settings.json` `statusLine.refreshInterval` is present (a number, e.g. `3`).
- If `<ws>/.polyforge/statusline-base` exists, the saved base command is non-empty.

**Missing refreshInterval (pre-aihub#122 takeover).** Workspaces initialized before
aihub#122 have a `statusLine` block without `refreshInterval`, so the wi chain freezes
during bg-subagent `/pf-execute` runs (the watching session idles -> no message updates ->
no re-render). If `statusLine.command` contains `pf-statusline.cjs` **but**
`statusLine.refreshInterval` is absent -> report `warn` and re-run `/pf-init` to re-assert
the takeover. Its Step 4 is idempotent and writes `"refreshInterval": 3` into the block.

**Drift detection (takeover clobbered).** The takeover is last-writer-wins: a later
statusline installer (e.g. ruflo) can overwrite `statusLine.command`. If
`<ws>/.polyforge/statusline-base` EXISTS (pf took over before) **but** the current
`statusLine.command` no longer contains `pf-statusline.cjs`, the takeover was clobbered ->
report `warn` and re-assert it by re-running `/pf-init`. Its Step 4 is idempotent: it saves
the current command as the new base and re-points `statusLine` at pf-statusline, so the
clobbering statusline keeps composing underneath.

If the command points at a missing script (e.g. workspace moved) -> re-run `/pf-init` to
re-copy the scripts. If not taken over at all -> `/pf-init` installs it.

### `--uninstall` - restore the previous statusLine

```python
ws = workspace_root
settings_path = f"{ws}/.claude/settings.json"
settings = read_json(settings_path) or {}
base = read_text(f"{ws}/.polyforge/statusline-base")  # None if file absent
if base and base.strip():
    settings["statusLine"] = {"type": "command", "command": base.strip()}
else:
    settings.pop("statusLine", None)   # no prior statusline -> remove the field
write_json(settings_path, settings)
remove_if_exists(f"{ws}/.polyforge/statusline-base")
# leave .claude/helpers/pf/ scripts in place (harmless); they are unreferenced now.
notify("statusLine restored to its pre-polyforge state.")
```

The plugin hook (`pf-chain-hook.cjs`) keeps shipping with the plugin; to fully stop it,
disable the polyforge plugin. `--uninstall` only undoes the statusLine takeover.

### Skill-side: seam-check (superpowers)

`pf-seam-check` pins the 6.1.1 baseline of the cached `superpowers` plugin that pf-execute's
engine pointer hardcodes against: `subagent-driven-development` (the engine pointer itself),
`finishing-a-development-branch` (the D6 execute boundary, in
`_common/references/lifecycle-details.md` §6 since aihub#338), and
`executing-plans` (the one the router test's negative assertion checks stays absent from the
execute pointer) -- plus the `superpowers@` prefix `pf-skill-router` scans for in
`enabledPlugins`. pf-spec and pf-plan are routed header-only since aihub#478 - the Iron Rules
and the output format, no engine fragment - so they have no superpowers dependency and are out
of scope for this check. Any `[WARN]` line means the installed superpowers version drifted
from one of these assumptions. The router does NOT auto-fall-back: it selects the engine on
the `superpowers@` `enabledPlugins` boolean alone, so with superpowers enabled a stale or
missing referenced skill still receives the superpowers pointer and may silently no-op at
runtime. Treat a WARN as: re-verify the seam BY HAND against the new
superpowers version, then update the pin (`PIN_VERSION` in `bin/pf-seam-check`) and any
name it references.

### Skill-side: binary freshness (`~/.polyforge/binary-status.txt`)

`cat ~/.polyforge/binary-status.txt`. **File absent -> `ok`** - meaning no outstanding
recorded failure, NOT independently verified freshness. File present -> `warn`, and
relay its contents verbatim: it names the cause, the version running, and the version
expected. Since aihub#774 there are two writers and one meaning - "this machine is not
running the binary it should be": `bin/polyforge-mcp.sh` writes it when the plugin
cache has no binary of its own (it then starts a `polyforge` from `PATH`, which works,
which is exactly why it must be recorded); `polyforge update` clears it only after a
run where every install target (machine-level + every plugin cache) ended current.
Read it as the last recorded degraded state, not a statement about right now.

Two causes, independent of each other - fixing one does not fix the other:

- **The plugin cache has no binary and nothing usable is on `PATH`.** Put a working
  `polyforge` somewhere and run `polyforge update` - distributing one build to
  `/usr/local/bin/polyforge` and every plugin cache is that command's whole job.
- **`polyforge update` cannot reach `bins-<channel>`.** It needs a GitHub token
  (`POLYFORGE_GH_TOKEN`/`GH_TOKEN`/`GITHUB_TOKEN`, or `gh auth token` - gh must be
  **>= 2.7**), and `raw.githubusercontent.com` answers **404, not 401**, for a private
  repo, so a bad token and a missing branch are indistinguishable from the response
  alone - the command reports the HTTP status so you know which question you are stuck
  on. Check the token first, since it is the cause you can rule out.

**This check is deliberately here in the skill and not a `polyforge doctor` check in
Go, and that is not an oversight.** Every numbered CLI check (1-10) runs out of the
binary. The population this one is for is exactly the population whose binary did not
update - their `polyforge doctor` predates the check and would report nothing, and in the
worst case (no binary at all, `✘ Failed to connect` in `/mcp`) there is no `polyforge` to <!-- aitaste:allow U+2718: verbatim quote of Claude Code's own /mcp failure line -->
run. A diagnostic that ships in the artifact whose absence it diagnoses cannot fire. The
skill and the SessionStart hook travel with the plugin install, so they reach those
machines; the binary, by construction, does not. See aihub#305.

## Output (three-segment format)

```markdown
## Result
<pass/fail summary; list any FAIL items>

## Status
| field     | value           |
|-----------|-----------------|
| workspace | ok              |
| config    | ok              |
| repos     | warn (missing: ieops-v2) |
| worktrees | ok (2 active)   |
| stale_running | ok (no attempt silent past 2h) |
| branch-upstream | ok (14 task worktrees, none tracking a protected branch) |
| version   | ok (server min_client_version=1.0.0, local=dev) |
| claude_md | warn (legacy inline block, 34,606 B) |
| usage_md  | warn (3 duplicated rule sections) |
| pi_integration | ok (bridge root current) |
| statusLine| ok (chain installed, refreshInterval 3) |
| seamcheck | ok (verified against superpowers 6.1.1) |
| binary    | warn (binary-status.txt: gh not authenticated; running cccccccc) |

## Next steps
- `polyforge init` - clone missing repos (`--apply` is a deprecated no-op)
- `/pf-work` - resume work once health is green
```

## NL Triggers

- "pf doctor" / "doctor" / "check" / "diagnose" / "health check"
- "something's broken with polyforge"
- "aihub unreachable" / "can't connect" / "MCP error"
- "worktree orphan" / "stale state"
