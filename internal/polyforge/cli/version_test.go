package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/version"
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

// TestRunVersionVerb pins the `version` verb (wi_gixPL0tarfnV): the
// build string on the dispatch, the same source and output shapes as
// the --version flag path (TestRunVersionFlag), listed by `pf help`,
// and an arity error (exit 1, the doctor convention) on extra
// positionals. NoConfigLoad means it answers with no config at all.
func TestRunVersionVerb(t *testing.T) {
	// Success: exit 0, stdout is exactly the version string (one line),
	// no usage text.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"version"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("pf version exited %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != version.Version {
		t.Fatalf("pf version printed %q, want the build-injected %q", got, version.Version)
	}
	if strings.Contains(stdout.String(), "Usage") || strings.Contains(stdout.String(), "pf v") {
		t.Fatalf("pf version printed usage text instead of a version:\n%s", stdout.String())
	}

	// --json: the same JSON line the --version flag path prints.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"--json", "version"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("pf --json version exited %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != fmt.Sprintf("{\"version\":%q}", version.Version) {
		t.Fatalf("pf --json version printed %q, want the flag path's JSON shape", got)
	}

	// `pf help` lists the verb (the WI's acceptance: reachable without
	// knowing the flag), under a Diagnostics group line.
	help := new(bytes.Buffer)
	if code := Run([]string{"help"}, nil, help, &stderr); code != 0 {
		t.Fatalf("pf help exited %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if count := strings.Count(help.String(), "version"); count < 2 {
		t.Fatalf("pf help output lists `version` %d time(s), want at least\n2 (the verb row and the --version global flag):\n%s", count, help.String())
	}

	// Arity: extra positionals are a local validation error, exit 1 (not
	// an unknown-command 2), with the doctor-convention wording.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"version", "wi_extra"}, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("pf version wi_extra exited %d, want 1; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "version: unexpected arguments") {
		t.Fatalf("stderr = %q, want the arity error wording", stderr.String())
	}
}
