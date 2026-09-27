// Command pf is the Polyforge client CLI: 12 routing verbs (file/queue/
// cancel/recall direct to AIHub; the rest to the local Daemon), admin and
// deployment subcommands, and the ops surface (daemon start/stop/status,
// upgrade, doctor).
// Design authority: docs/design/polyforge-vnext/00-architecture.md §5
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pf: vNext skeleton — not yet implemented (see docs/design/polyforge-vnext/00-architecture.md)")
	os.Exit(1)
}
