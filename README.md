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

## The `pf` CLI (naming ruling)

The client CLI lives in `cmd/pf` + `internal/cli`. **Naming ruling:** the end state is
**one user-facing binary** that carries every CLI verb plus the long-running
**`daemon` subcommand** (work-machine binary, deployment-unit merge ruling —
00 §3.2 / 07 §3; one-shot verbs + `daemon` long-running in a single file). The
v1 binary name is **`pf`** (short, typed dozens of times a day by both humans
and routing-skill prompts). At **legacy retirement** the binary cuts over
to the full name **`polyforge`**, leaving a **`pf` symlink** behind so every
existing invocation, script and skill fragment keeps working — the same
scheme the platform already uses for the `pf-*` skill family. No third
name, no per-command renames: verbs are stable across the cutover.

Client-side wire notes (authorities: `api/openapi.yaml` for the AIHub face,
`internal/daemon/api.go` for the daemon unix-socket face):

- The daemon face answers mode queries via `GET /status` (`POST /mode`
  has no `get` action) and the batch wave route is `POST /batch_submit`.
- `pf pause` / `pf wrap` project into the daemon's `/jobs/{id}/...` route
  family (`/pause`, `/wrap`), and both routes are live: the daemon operations
  table (04 §5.1) documents the pause and wrap rows, `internal/daemon/api.go`
  registers both handlers (`POST /jobs/{id}/pause`, `POST /jobs/{id}/wrap`),
  and the dogfood runs exercise both — no spec gap remains here (the daemon
  generates the closing `flush_evidence`, 02 §6.4).
