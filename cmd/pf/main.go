// Command pf is the v1 work-machine binary: 13 user verbs + `pf daemon`.
//
// Architecture: docs/design/polyforge-vnext/v1-architecture-spec.md.
// Modules: internal/polyforge/{cli,runtime,observe} over internal/common.
// The daemon hosts Runtime (lifecycle: Start/Stop/Recover/Restart) and
// Observe (per-WI monitor goroutines); they communicate only via
// internal/common/protocol messages carrying attempt-ids.
//
// Phase 1 skeleton; wired up by the runtime/observe/cli lanes.
package main

import "fmt"

func main() {
	fmt.Println("pf: v1 (modules pending — see internal/polyforge/)")
}
