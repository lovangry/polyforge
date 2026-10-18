# version — which build is this

Read-only contract: this fragment prints the pf binary's build version
and stops — it dispatches no verb, performs no wi-lifecycle action and
edits no file.

Run `pf version` and relay the output: one line, the build-injected
version string — the same `internal/version` value the global
`pf --version` flag prints. Under `--json` it is one machine line,
`{"version":"<v>"}` — script against that shape, not the prose. The
verb needs no config and touches no face (aihub, daemon): it answers
even where `pf doctor` would fail on an unloadable config, which makes
it the right query from scripts and workflow steps alike.
