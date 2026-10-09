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

func TestRemovedEngineFlagsFailLoudlyAtParse(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"retired engine flag, single dash", []string{"-use-driver"}, "use-driver"},
		{"retired engine flag, double dash", []string{"--use-driver"}, "use-driver"},
		{"retired executor prompt flag", []string{"--executor-prompt", "/dev/null"}, "executor-prompt"},
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
	t.Setenv(legacyLaneEnv, "1")
	var stderr bytes.Buffer
	code := Main([]string{"--socket", "/nonexistent-probe.sock"}, nil, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatalf("daemon accepted the retired %s variable (exit 0)", legacyLaneEnv)
	}
	if !strings.Contains(stderr.String(), legacyLaneEnv) {
		t.Fatalf("boot refusal %q does not name the retired variable %s", stderr.String(), legacyLaneEnv)
	}
}
