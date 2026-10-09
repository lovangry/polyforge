package test

// v1_lane_guard_test.go: pins the legacy-lane guard machinery's remaining
// surface while the phase-4 deletion (polyforge#106) is in flight. The
// explicit-engine-flag detector is already gone (the marker replaced it);
// the hatch probe stays until unit 4b of the deletion retires the hatch
// and deletes this file outright.

import (
	"strings"
	"testing"
)

func TestLegacyLaneHatchArming(t *testing.T) {
	t.Setenv("PFV1_LEGACY_LANE", "1")
	if !legacyLaneHatch() {
		t.Fatal("hatch must arm on the exact value 1")
	}
	for _, v := range []string{"", "0", "true", "yes"} {
		t.Setenv("PFV1_LEGACY_LANE", v)
		if legacyLaneHatch() {
			t.Errorf("hatch armed on %q; only the exact value 1 arms it", v)
		}
	}
}

func TestLegacyLaneSkipMessageMentionsTheTicket(t *testing.T) {
	for _, want := range []string{"polyforge#104", "migration", "PFV1_LEGACY_LANE"} {
		if !strings.Contains(legacyLaneSkipMsg, want) {
			t.Errorf("skip message must mention %q, got: %s", want, legacyLaneSkipMsg)
		}
	}
}
