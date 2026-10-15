# init — machine install (pointer) and workspace initialization

Two levels, never mixed:

- **Machine install** is `tools/install.sh`. This fragment is a POINTER,
  not a parallel procedure: the README's install section ("Install (team
  machines — read this first)") carries the one-liner and the credential
  prerequisites, the script itself owns every step. The one thing the
  script defers to the operator is reproduced below (the `[workspace]`
  example edit).
- **Workspace init** is the agent-driven flow that maps a workspace's
  projects on this machine (and optionally gives it a `.polyforge.yaml`
  of model candidates) — this fragment's core, and the part a polyforge
  session performs with the operator.

## Machine install — the pointer

```
curl -s -H "Authorization: token $(gh auth token)" \
  https://raw.githubusercontent.com/silgrid/polyforge/main/tools/install.sh \
  | bash -s -- <your-personal-api-key>
```

Install and upgrade are the same command: run it again any time to
upgrade in place. The script installs the `pf` binary (SHA-verified),
writes the client config, installs this routing skill, and — on execution
machines — starts the local daemon. Prerequisites (a personal API key,
`gh` authenticated) and flags live in the README install section and
`tools/install.sh`; this fragment deliberately does not duplicate them.

The one operator step the script defers — editing the mapping template —
belongs to the operator, not the script:

- `~/.config/polyforge/cli.toml` ships with a COMMENTED
  `[workspace."<path>"]` template; uncomment ONE section per workspace
  with its `projects = ["..."]` list.
- The daemon refuses to start until a real mapping exists; `pf doctor`
  checks the mapping (and the whole machine) after editing.

`tools/install.sh` itself never learns about workspaces.

## Workspace init — the flow this fragment owns

Triggers: the first polyforge session in a workspace with no mapping; or
the user saying "init workspace".

1. **Scan** the workspace directories: enumerate the project directories
   the operator actually works in, not a registry's opinion.
2. **Selective server sync**: ask the OPERATOR which of the found
   projects to register on the aihub server. Only operator-named projects
   are synced — a full sync is never forced. Projects that already exist
   on the server are just named; nothing is re-registered behind the
   operator's back.
3. **Propose the cli.toml `[workspace."<this-root>"]` section** with the
   `projects` list. Nothing rewrites cli.toml behind the operator's back —
   the agent proposes the section, the operator's config carries it.
4. **Optionally write `.polyforge.yaml`** at the workspace root to pin
   model candidates per project, in the exact closed schema the loader
   enforces (any other key, a duplicate key, a type mismatch, a per-step
   key that is no step id of the installed workflows, a project key this
   workspace's cli.toml section does not list, a step-entry key other
   than `models` — the retired `harness` key included — a top-level
   `models:` section (the pre-#169 shape), or a candidate that is not
   `<harness>:<model>` refuses to load; YAML merge keys (`<<`) are
   refused — share a list with anchors, `models: &x [...]` /
   `{ models: *x }`):

   ```yaml
   # Precedence: pf submit --models > steps.<project>.<step-id>.models > bundle step model_candidates > steps.<project>.default.models > bundle defaults
   # Every candidate is <harness>:<model>, harness pi or claude (polyforge#175)
   steps:
     polyforge:                               # one entry per project of this workspace
       default: { models: ["pi:prov/model-a", "pi:prov/model-b"] }  # optional: steps without a list of their own
       fix: { models: ["pi:prov/model-c"] }   # optional: per step id (the step
                                              # with that id in every bundle)
       review: { models: ["claude:claude-opus-5-5", "pi:prov/model-a"] }  # Claude Code first, pi as fallback
   ```

   Each candidate is `<harness>:<model>`: `pi:` or `claude:`, split on
   the first `:` (the model keeps any `/` or further `:`); no prefix, an
   unknown prefix or an empty model refuses. The prefix alone decides
   where the candidate runs — a chain may mix harnesses, and a switch
   onto a candidate of another harness starts a fresh session with the
   previous transcript as read-only reference. A step with no list (or
   an empty one) runs pi's own default model.

   Precedence for a step of the claimed project: `pf submit --models`,
   then the yaml list keyed by its step id, then the workflow step's own
   `model_candidates`, then the project's `default` list, then the
   bundle's `defaults`. A project not listed keeps the bundle's. A
   declared-but-empty list governs (no switchable chain). Claude Code
   must be installed and logged in by a human on the daemon's machine
   (`claude_bin` in cli.toml is optional; the credential lives in the
   harness root's `claude-config/settings.json`; see docs/upgrade.md,
   polyforge#159, #171, #175). The aihub project registry, not this file,
   supplies the repo list.

An edit to `.polyforge.yaml` takes effect on the NEXT claim — no daemon
restart is involved; the daemon reads the yaml fresh before every claim.
An invalid file refuses the claim (DAEMON-414) before it is made: the
work item stays queued, so fix the file and resubmit.

## Verify

```
pf doctor           # machine face: config + [workspace] mapping + connectivity
cat <workspace>/.polyforge.yaml   # the model pins the daemon reads (if any)
```

The daemon-side check is a claim: the next submitted WI in this workspace
dispatches under the yaml's model candidates for its project.

## Non-goals

- No machine-setup steps here: binary, config skeleton and skills all
  come from `tools/install.sh` (the pointer above is the whole machine
  story).
- No workflow execution: this fragment is setup only — running work items
  routes to the submit verb's fragment.
- No daemon-placement change: placement keeps reading the cli.toml
  mapping; the yaml carries model candidates only.
