## Repo Routing (task -> repo)

To decide *which repo/worktree* a task belongs to, use the `## Workspace` repo
map from Step 0:

- **Which project / repo** - match the task against each repo's one-line
  `positioning`.
- **Where to execute** - the claimed wi's worktree (`ws/capsules/<wi>/<repo>/`).
- **How to change** - each project links to `.polyforge/repo-map/<project>.md`
  (`main_modules` / `change_scenarios` / `tech_stack`); `Read` just that file.
  No link = older format, detail inlined under each repo. Neither = say
  "repo map missing - run `polyforge init`", then `codegraph_*` / `Grep`.
  Never infer internals from positioning.

A task spanning repos routes each part by the same map; if no repo clearly
fits, ask rather than guess.
