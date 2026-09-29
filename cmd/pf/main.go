// Command pf is the v1 work-machine client binary: 13 user verbs plus
// the M1-M5 management face, routed per the v1 architecture
// (docs/design/polyforge-vnext/v1-architecture-spec.md):
//
//   - direct-to-aihub verbs speak the shared HTTP contract
//     (internal/common/api) through internal/polyforge/aihubclient;
//   - execution-plane verbs speak the daemon's UDS HTTP face
//     (internal/common/protocol);
//   - `pf daemon` is a placeholder until the phase-3 daemon assembly
//     (Runtime + Observe) lands.
package main

import (
	"os"

	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
