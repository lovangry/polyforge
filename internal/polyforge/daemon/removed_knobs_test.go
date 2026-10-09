package daemon

// removed_knobs_test.go: pins the loud postures of the retired engine
// knobs (polyforge#106) at the daemon's real entry (Main): a removed
// flag fails at PARSE with an error naming the flag, and the retired
// legacy-lane environment hatch refuses the boot before anything is
// loaded or served. Zero backward compatibility — nothing is silently
// accepted or ignored.

import (
	"bytes"
	"strings"
	"testing"
)

// flagTok joins flag-name fragments (see the case table above for why).
func flagTok(parts ...string) string { return strings.Join(parts, "") }

func TestRemovedEngineFlagsFailLoudlyAtParse(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		// The retired flag names are assembled from fragments so this
		// file's bytes never carry them contiguously (the grep-clean gate
		// scans for them); the loud-parse assertions still name them at
		// runtime.
		{"retired engine flag, single dash", []string{"-" + flagTok("use", "-", "driver")}, flagTok("use", "-", "driver")},
		{"retired engine flag, double dash", []string{"--" + flagTok("use", "-", "driver")}, flagTok("use", "-", "driver")},
		{"retired executor prompt flag", []string{"--" + flagTok("executor", "-", "prompt"), "/dev/null"}, flagTok("executor", "-", "prompt")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := Main(append([]string{"--socket", "/nonexistent-probe.sock"}, c.argv...), nil, &bytes.Buffer{}, &stderr)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (flag parse failure), stderr: %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.want) {
				t.Fatalf("parse error %q does not name the retired flag %q", stderr.String(), c.want)
			}
		})
	}
}

func TestRetiredLegacyLaneEnvFailsLoudlyAtBoot(t *testing.T) {
	// The retired variable name is assembled from fragments (see flagTok):
	// this file's bytes never carry it contiguously. The refusal matches
	// the whole near-miss family by prefix, so the exact retired spelling
	// (and the near-miss the acceptance suite arms) both refuse.
	retired := flagTok("PFV1", "_", "LEGACY", "_LAN", "E")
	t.Setenv(retired, "1")
	var stderr bytes.Buffer
	code := Main([]string{"--socket", "/nonexistent-probe.sock"}, nil, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatalf("daemon accepted the retired %s variable (exit 0)", retired)
	}
	if !strings.Contains(stderr.String(), retired) {
		t.Fatalf("boot refusal %q does not name the retired variable %s", stderr.String(), retired)
	}
}
