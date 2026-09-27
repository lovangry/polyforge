// Command aihub is the AIHub vNext service: single-active, SQLite-backed,
// one HTTP port serving the 25 business endpoints plus the management face.
// Design authority: docs/design/polyforge-vnext/02-aihub-core.md
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "aihub: vNext skeleton — not yet implemented (see docs/design/polyforge-vnext/02-aihub-core.md)")
	os.Exit(1)
}
