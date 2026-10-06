## Platform adaptation (pi / Claude / no-harness machines)

v1's harness adapter set is **pi** and **Claude Code**. MCP was retired in
v1.0.0: skills are plain Markdown discovered by directory - no plugin
manifests, no per-runtime tool-name spellings - and the `pf` CLI is the only
tool surface. Adaptation between runtimes is therefore near-zero.

- **Install** (`tools/install.sh` does all of this): under pi,
  `pi install git:git@github.com:silgrid/polyforge.git@<tag>` installs the
  repo as a package whose skills pi discovers; under Claude,
  `operator-skills/*` is copied to `~/.claude/skills/`. A machine with neither
  pi nor Claude gets the CLI only (skills optional).
- **pi**: `~/.pi/agent/skills/` (user scope) is always read; a project-local
  `.agents/skills/` copy sits behind pi's `/trust` gate, default-off, so on
  its own it leaves the skills silently unavailable (aihub#606). Prefer the
  installer or a user-scope copy.
- **Claude Code**: the skill is plain Markdown in `~/.claude/skills/`, loaded
  like any other skill; every polyforge action is a `pf` shell command either
  way.
- **Other runtimes (Codex, Copilot CLI, ...)**: no v1 adapter - do not promise
  skill-driven operation there. The `pf` CLI works anywhere; the skills do
  not.

