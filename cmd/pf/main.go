// Command pf is the Polyforge vNext work-machine binary: the merged
// deployment unit (00-architecture.md §3.2 / 07-deployment-pilot.md §3)
// carrying BOTH faces in one file —
//   - the 12 one-shot routing CLI verbs (file/queue/cancel/recall direct
//     to the AIHub HTTP face; submit/observe/answer/batch-confirm/
//     pause/resume/wrap/ops to the local daemon unix-socket face), the
//     supporting WI faces (deps, budget) and the management face
//     (users/api-keys/projects/backup/rotate-secret), dispatched by
//     internal/cli.Run; and
//   - the long-running `daemon` subcommand (daemon.go), the per-machine
//     WI Job executor, dispatched below BEFORE the verb dispatcher.
//
// The binary name stays `pf` until the cutover rename ruling fires.
//
// Design authority: docs/design/polyforge-vnext/00-architecture.md §3.2
// (deployment-unit merge), §5.1 (routing split), 02-aihub-core.md §5.1
// (aihub endpoints), 04-local-runtime.md §5.1 (daemon operations).
// Wire contract: api/openapi.yaml.
package main

import (
	"os"

	"github.com/silgrid/polyforge/internal/cli"
)

func main() {
	// `pf daemon` — the long-running subcommand. Intercepted before the
	// CLI verb dispatcher because it is NOT a one-shot verb: it owns its
	// flag set, its env handling and a process-exit error path (fatal →
	// os.Exit(1)). Its graceful-shutdown epilogue RETURNS: main must
	// return IMMEDIATELY after runDaemon — falling through to the CLI
	// verb dispatcher would print `unknown command "daemon"` and exit
	// 2, breaking the exit-0 guarantee a supervisor (systemd
	// Restart=on-failure) reads as a clean stop (internal/cli.Run maps
	// Run errors onto exit codes 0/1/2). Nothing was deployed in the
	// field before the merge, so the subcommand is the only daemon entry
	// — no deprecation stub for a standalone pf-daemon binary.
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		runDaemon(os.Args[2:])
		return // graceful shutdown landed: process ends with the zero status
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
