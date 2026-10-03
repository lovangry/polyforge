package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunVersionFlag pins the --version flag on the pf CLI's real entry
// surface (Run, which cmd/pf/main.go dispatches to with os.Args[1:]).
// The goal (wi_DBOTR7Q6Kcr3) requires: a build-injected version variable
// (ldflags -X, git tag at build) and a --version flag on pf that prints
// it and exits 0. Both the single-dash and double-dash spellings must
// work (Go's flag package accepts either).
func TestRunVersionFlag(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{"double dash", []string{"--version"}},
		{"single dash", []string{"-version"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tc.argv, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("Run(%q) exited %d, want 0; --version must succeed.\nstderr:\n%s",
					tc.argv, code, stderr.String())
			}
			got := strings.TrimSpace(stdout.String())
			if got == "" {
				t.Fatal("--version printed nothing on stdout; it must print the build-injected version string")
			}
			if strings.Contains(got, "Usage") || strings.Contains(got, "polyforge v1 client CLI") {
				t.Fatalf("--version printed usage text instead of a version:\n%s", got)
			}
		})
	}
}
