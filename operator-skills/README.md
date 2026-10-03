# operator-skills/

The full v1.0.0 operator skill pack (19 skills), mirrored from the
authoring harness (`~/.pi/agent/skills/`). This directory is the team
distribution source.

## Install onto a team machine's pi session

Copy each skill directory into the target harness's skill directory
(e.g. `~/.pi/agent/skills/`):

```sh
for d in pf-* using-polyforge polyforge; do
  cp -r "$d" ~/.pi/agent/skills/
done
```

Then `/pf-init` performs the machine install (binary → config →
connectivity → optional daemon), and the daily surface is `/polyforge`
(13 routing verbs incl. `ops`).

## Inventory

- `pf-init` — v1.0.0 machine install (rewritten for the capsule model)
- `polyforge` — intent routing to the 13 CLI verbs (ops = daemon runtime)
- `using-polyforge` — resident rules (IR1-IR4 incl. per-WI watcher
  reconcile) + on-demand fragments
- `pf-work/pf-status/pf-execute/pf-stop/...` — lifecycle verbs
- `pf-doctor/pf-help` — repair + reference

Sync note: authoring edits happen in the harness directory first
(live), then mirror here on change — keep both in step.
