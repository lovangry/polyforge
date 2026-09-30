# Polyforge v1

Work-item orchestration platform: an aihub server (WI store, memory, git
revision chain) and a pf work-machine binary (Runtime + Observe + CLI).

Architecture: `docs/design/polyforge-vnext/v1-architecture-spec.md`
(full design record: `v1-spec-full.md`; v0 pilot code: `/v0`, frozen).

## Layout

```
internal/
  common/       shared contracts (api types, capsule, workflow, protocol)
  aihub/        server: core (WI store) / memory / git / server (HTTP)
  polyforge/    work machine: cli / runtime (lifecycle) / observe (monitors)
cmd/aihub       server binary
cmd/pf          work-machine binary (13 verbs + `pf daemon`)
```

## Build & test

    make check     # build + vet + test, all packages
    go build ./...
    go test ./...

Before starting ANY implementation or review lane: recall the v0 lessons
ledger (`docs/design/polyforge-vnext/v0-lessons-ledger.md`, now including
the v1 pilot findings F1–F4) and declare, item by item, how your change
covers the entries it touches — the ledger only prevents re-hits if it is
actually read first.

Minimal external dependencies: pure-Go SQLite (modernc.org/sqlite), yaml.v3, goldmark, jsonschema, x/sys.
