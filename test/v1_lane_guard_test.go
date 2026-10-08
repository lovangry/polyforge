package test

// v1_lane_guard_test.go: focused regressions for the legacy-lane guard's
// selection contract (polyforge#104, astra round-1 finding 7) — the pure
// pieces are unit-pinned here: explicit engine-flag detection (every
// Go-flag spelling) and the escape-hatch spelling rule. The end-to-end
// visibility (32 legacy scenarios SKIP, the -use-driver v2 e2e RUNS) is
// pinned by the package run itself under `go test -v`.

import (
	"strings"
	"testing"
)

func TestExplicitEngineFlagDetection(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare single dash", []string{"-use-driver"}, true},
		{"bare double dash", []string{"--use-driver"}, true},
		{"single dash false", []string{"-use-driver=false"}, true},
		{"double dash true", []string{"--use-driver=true"}, true},
		{"spelled among other flags", []string{"--hang-window", "30s", "--use-driver=false"}, true},
		{"prefix of another flag is not the flag", []string{"-use-driver-x"}, false},
		{"unrelated window flags", []string{"--hang-window", "5s", "--idle-window", "10s"}, false},
		{"empty argv", nil, false},
	}
	for _, c := range cases {
		if got := explicitEngineFlag(c.args); got != c.want {
			t.Errorf("%s: explicitEngineFlag(%q) = %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

func TestLegacyLaneHatchSpelling(t *testing.T) {
	// only the exact "1" arms the hatch — a deliberate opt-in, never an
	// ambient default ("0", "true", "yes" all stay disarmed)
	t.Setenv("PFV1_LEGACY_LANE", "1")
	if !legacyLaneHatch() {
		t.Fatal("hatch must arm on exactly \"1\"")
	}
	for _, v := range []string{"0", "true", "yes", ""} {
		t.Setenv("PFV1_LEGACY_LANE", v)
		if legacyLaneHatch() {
			t.Fatalf("hatch must NOT arm on %q", v)
		}
	}
}

func TestLegacyLaneSkipMsgCarriesDebtReference(t *testing.T) {
	// the debt must stay discoverable from the skip output itself: the
	// interim ticket id AND the migration framing
	for _, want := range []string{"polyforge#104", "migration", "PFV1_LEGACY_LANE"} {
		if !strings.Contains(legacyLaneSkipMsg, want) {
			t.Errorf("skip message must mention %q, got: %s", want, legacyLaneSkipMsg)
		}
	}
}
