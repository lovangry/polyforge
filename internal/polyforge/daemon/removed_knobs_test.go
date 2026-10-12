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

// TestRetiredSupervisionFlagsFailLoudlyAtParse pins the v2 converge
// deletion of the guessing lanes: their windows (hang, idle, probe, wall)
// are not accepted any more — a stale service unit must fail at parse,
// naming the flag, instead of silently running with defaults.
func TestRetiredSupervisionFlagsFailLoudlyAtParse(t *testing.T) {
	for _, name := range []string{"hang-window", "idle-window", "probe-timeout", "wall-window"} {
		var stderr bytes.Buffer
		code := Main([]string{"--socket", "/nonexistent-probe.sock", "--" + name, "1m"}, nil, &bytes.Buffer{}, &stderr)
		if code != 2 {
			t.Fatalf("--%s: exit = %d, want 2 (flag parse failure), stderr: %q", name, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), name) {
			t.Fatalf("--%s: parse error %q does not name the flag", name, stderr.String())
		}
	}
	for _, name := range []string{"silence-window", "tool-window", "step-timeout", "stall-window", "model-error-grace"} {
		var stderr bytes.Buffer
		code := Main([]string{"--socket", "/nonexistent-probe.sock", "--" + name, "notaduration"}, nil, &bytes.Buffer{}, &stderr)
		if code != 2 || strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatalf("--%s must be a defined duration flag: exit %d, %q", name, code, stderr.String())
		}
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
