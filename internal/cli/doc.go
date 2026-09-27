// Package cli implements the `pf` client CLI of Polyforge vNext.
//
// Routing split (00-architecture.md §5.1, the 12-verb table):
//
//	Direct to AIHub HTTP (api/openapi.yaml is the wire contract):
//	  file, queue, cancel, recall — plus the row-26 dependency face,
//	  the human budget face (rows 11/12) and the management face M1–M5.
//
//	Local daemon unix-socket face (04-local-runtime.md §5.1):
//	  submit, observe, answer, batch-confirm, pause, resume, wrap, ops.
//
// Output: human-readable by default; --json emits the server's JSON
// response as-is. Errors are rendered from the typed wire envelope
// {error:{code,reason,detail}} with the code prominent.
//
// Configuration: ~/.config/polyforge/cli.toml (flat string-keyed TOML
// subset), overridden by PF_SERVER / PF_API_KEY / PF_DAEMON_SOCKET /
// PF_USER_ID / PF_PROJECT and finally by command-line flags.
package cli
