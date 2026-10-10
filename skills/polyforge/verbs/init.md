# init — machine install (pointer) and workspace initialization

Two levels, never mixed:

- **Machine install** is `tools/install.sh`. This fragment is a POINTER,
  not a parallel procedure: the README's install section ("Install (team
  machines — read this first)") carries the one-liner and the credential
  prerequisites, the script itself owns every step. The one thing the
  script defers to the operator is reproduced below (the `[workspace]`
  example edit).
- **Workspace init** is the agent-driven flow that gives a workspace its
  `.polyforge.yaml` — this fragment's core, and the part a polyforge
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

Workspace init (below) re-derives this section FROM the workspace yaml —
the `[workspace]` mapping is a derived artifact; `.polyforge.yaml` is the
source. `tools/install.sh` itself never learns about workspaces.

## Workspace init — the flow this fragment owns

Triggers: the first polyforge session in a workspace without
`.polyforge.yaml`; the user saying "init workspace"; or a session finding
the yaml stale versus the actual directory layout.

1. **Scan** the workspace directories: enumerate the project directories
   and their local git checkouts — the directories the operator actually
   works in, not a registry's opinion.
2. **Selective server sync**: ask the OPERATOR which of the found
   projects to register on the aihub server. Only operator-named projects
   are synced — a full sync is never forced. Projects that already exist
   on the server are just named; nothing is re-registered behind the
   operator's back. After this step the local yaml is authoritative for
   the workspace.
3. **Generate or update `.polyforge.yaml`** at the workspace root, in the
   exact closed schema the loader enforces (any other key, a duplicate
   key, a type mismatch, an unsafe name or a parent-escaping path refuses
   to load):

   ```yaml
   projects:                      # the repo-source section (optional)
     <project-name>:
       repos:                     # REQUIRED on every project entry (may be [])
         - name: polyforge        # single safe path element → repos/<name>
           path: .repo/polyforge  # the local checkout, workspace-root-relative
   workflow_defaults:             # the model-pinning section (optional)
     model_candidates: ["id"]     # the flow-default candidate list
     step_kinds:                  # per-kind pins; keys ONLY from
       code: ["id"]               # {qa, spec, author, code, gate, review}
   ```

   Each scanned project lands as a `projects` entry with its local
   checkout path(s); a project with no local checkout still gets its
   entry with `repos: []` — the empty list is the authoritative inventory
   (nothing is cloned for it).
4. **Re-derive the cli.toml `[workspace]` mapping from the yaml**: for
   the machine's `~/.config/polyforge/cli.toml`, make the
   `[workspace."<this-root>"]` section's `projects` list equal the yaml's
   `projects` keys. The yaml is the source of truth; the mapping is a
   derived artifact. Nothing rewrites cli.toml behind the operator's
   back — the agent proposes the section, the operator's config carries
   it (a session edits the config only as part of THIS init flow, never
   silently).

An edit to `.polyforge.yaml` takes effect on the NEXT claim — no daemon
restart is involved; the daemon reads the yaml fresh on every claim.

## Verify

```
pf doctor           # machine face: config + [workspace] mapping + connectivity
cat <workspace>/.polyforge.yaml   # the workspace face: the yaml the daemon reads
```

The daemon-side check is a claim: the next submitted WI in this workspace
materializes from the yaml's projects (local checkouts) and dispatches
under the yaml's workflow_defaults.

## Non-goals

- No machine-setup steps here: binary, config skeleton and skills all
  come from `tools/install.sh` (the pointer above is the whole machine
  story).
- No workflow execution: this fragment is setup only — running work items
  routes to the submit verb's fragment.
- No daemon-placement change: placement keeps reading the cli.toml
  mapping exactly as before; the yaml adds the workspace's repo sources
  and workflow defaults, not a new placement source.
