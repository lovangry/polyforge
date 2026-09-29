// Command aihub is the v1 server binary.
//
// Architecture: docs/design/polyforge-vnext/v1-architecture-spec.md.
// Modules: internal/aihub/{core,memory,git,server} — WI/business store
// (SQLite), FTS memory authority, bare-repo management + git-http-backend
// proxy with pre-receive hooks, HTTP surface.
//
// Phase 1 skeleton; wired up by the aihub implementation lane.
package main

import "fmt"

func main() {
	fmt.Println("aihub: v1 (modules pending — see internal/aihub/)")
}
