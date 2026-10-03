package version

import "testing"

// TestVersionDefault pins the un-injected default: a plain `go build` (no
// ldflags) must still yield a non-empty version string, so `pf --version`
// and `aihub --version` never print nothing.
func TestVersionDefault(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must never be the empty string; un-injected builds default to \"dev\"")
	}
}
