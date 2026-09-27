// Command pf-daemon is the per-machine Polyforge Daemon: the single WI Job
// executor for Interactive/Batch/Drain, supervising one persistent Harness
// RPC WI Agent per work item.
// Design authority: docs/design/polyforge-vnext/04-local-runtime.md
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pf-daemon: vNext skeleton — not yet implemented (see docs/design/polyforge-vnext/04-local-runtime.md)")
	os.Exit(1)
}
