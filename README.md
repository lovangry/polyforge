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

## `pf recall` — memory search

Searches a project's memories on the aihub memory face (trigram-indexed
full-text search):

    pf recall [--project P] <query> [--type T] [--tags a,b] [--limit N]

`<query>` is required (positional or `--q`); `--project` defaults to the
configured project. Flags:

- `--limit N` — max results. **Default 20** when the flag is omitted
  (the CLI's product choice, sent explicitly); values must satisfy
  `1 ≤ N ≤ 200` — anything else fails loudly before a request is sent
  (`pf: recall: --limit must be 1..200`). The endpoint's own default of
  50 is a wire backstop for other callers, not this verb's contract.
- `--type T` — memory type filter, exactly three legal forms:
  - exact — `--type experience.debug` matches only that type;
  - prefix — `--type experience.*` matches every type under the
    `experience.` prefix (`experience.debug`, `experience.approach`, …;
    `fact.*` does not match `experience.*` types);
  - all — `--type *` is equivalent to omitting the flag.

  Any other `*` placement (`*.debug`, `exp*rience`, `experience*`) is
  rejected locally with no request sent. Both the GET and POST faces of
  `/api/v1/memories/search` interpret a trailing `.*` the same way.
- `--tags a,b` — comma-separated tag filter (a hit carries all of them).

Examples:

    pf recall flaky build
    pf recall flaky build --type experience.* --limit 50
    pf recall "retry ladder" --type fact.note --tags postgres,backoff
