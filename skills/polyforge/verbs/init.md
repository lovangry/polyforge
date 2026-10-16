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

Install and upgrade are the same command, `tools/install.sh`: the README's
"Install (team machines — read this first)" section carries the one-liner,
the prerequisites and what an upgrade does (busy-job refusal,
`PF_INSTALL_FORCE=1`); this fragment does not repeat them. The one step
the script leaves to the operator: uncomment ONE
`[workspace."<path>"]` section per workspace (with its
`projects = ["..."]`) in the template it writes to
`~/.config/polyforge/cli.toml` — the daemon refuses to start until a real
mapping exists — then run `pf doctor`. `tools/install.sh` itself never
learns about workspaces.

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
   model candidates per project. The README's "Per-project model
   candidates (`.polyforge.yaml`)" paragraphs (release checkout:
   `~/.local/share/polyforge/release/current/README.md`) are the one
   authority for its closed schema, the `<harness>:<model>[:<effort>]`
   candidate syntax and the precedence — read them before writing. The
   shape:

   ```yaml
   steps:
     polyforge:                               # one entry per project of this workspace
       default: { models: ["pi:prov/model-a"] }  # optional: steps without a list of their own
       review: { models: ["claude:claude-opus-5-5:high", "pi:prov/model-a"] }  # optional: per step id
   ```

   An edit takes effect on the NEXT claim — no daemon restart is
   involved. An invalid file refuses the claim (DAEMON-414) before it is
   made: the work item stays queued, so fix the file and resubmit.

5. **Lay out the workspace**: `pf init <workspace-root>` (default: the
   current directory) creates `scratch/` and writes the runtime-artifact
   convention (polyforge#177) into CLAUDE.md as a layout section inside
   the `polyforge:managed` block — appended to an existing block, or a
   new block on top. Re-running it is safe: it only rewrites its own
   section, never the human part. The convention: shared content goes to
   aihub (a design → its work item's content while open, else a
   `doc.design` memory; reviews / decisions / audits → `doc.review` /
   `doc.decision` / `doc.audit` memories; experience → `experience.*` /
   `fact.*`); locally only `.repo/`, `capsules/`, `.polyforge/`
   (polyforge-generated only) and `scratch/` (disposable, reported by
   `pf doctor` after 14 days).

## Verify

```
pf doctor           # machine face: config + [workspace] mapping + workspace layout + connectivity + supervision + harness layout
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
