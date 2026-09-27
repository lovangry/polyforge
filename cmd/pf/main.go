// Command pf is the Polyforge vNext client CLI: the 12 routing verbs
// (file/queue/cancel/recall direct to the AIHub HTTP face;
// submit/observe/answer/batch-confirm/pause/resume/wrap/ops to the local
// daemon unix-socket face), the supporting WI faces (deps, budget) and
// the management face (users/api-keys/projects/backup/rotate-secret).
//
// Design authority: docs/design/polyforge-vnext/00-architecture.md §5.1
// (routing split), 02-aihub-core.md §5.1 (aihub endpoints),
// 04-local-runtime.md §5.1 (daemon operations).
// Wire contract: api/openapi.yaml.
package main

import (
	"os"

	"github.com/silgrid/polyforge/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
