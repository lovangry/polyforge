# polyforge

Polyforge vNext mono-repo — the platform implementation built from the
eight-doc design baseline in [`docs/design/polyforge-vnext/`](docs/design/polyforge-vnext/00-architecture.md)
(delivered by aihub#793).

Layout:

| Path | What | Design authority |
|---|---|---|
| `cmd/aihub` | AIHub service (single-active, SQLite) | `docs/design/.../02-aihub-core.md` |
| `cmd/pf-daemon` | Per-machine WI Job executor | `docs/design/.../04-local-runtime.md` |
| `cmd/pf` | Client CLI (12 verbs + admin) | `docs/design/.../00-architecture.md` §5 |
| `internal/server` | AIHub HTTP surface (25 endpoints + M1–M5) | 02 |
| `internal/domain` | Work items / generations / memory domain | 02 |
| `internal/sync` | Boundary sync engine (freeze→staging→publish) | 05 |
| `internal/runtime` | Daemon internals (jobq / observe / recovery) | 04 |
| `skills/polyforge` | Routing skill (user harness, 12 verb fragments) | 01 |
| `skills/pf-execute` | WI Agent execution skill (Daemon-injected) | 01 |
| `workflows/` | Workflow bundles (workflow.md + agents/*.md) | 01 |

The design docs are the single authority: `make check-xrefs` validates their
cross-references. Implementation artifacts (OpenAPI, JSON Schema, contract
tests, adapter argv tables) land here per the design's handoff note.
