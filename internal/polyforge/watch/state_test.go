package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatePathShape pins the contract path: <workspace>/.polyforge/watchers/<wi>.state.
func TestStatePathShape(t *testing.T) {
	got := StatePath("/srv/ws", "wi_abc")
	if got != filepath.Join("/srv/ws", ".polyforge", "watchers", "wi_abc.state") {
		t.Fatalf("StatePath = %q", got)
	}
}

// TestLoadStateMissingIsFresh: a missing state file is the fresh arm.
func TestLoadStateMissingIsFresh(t *testing.T) {
	st, err := LoadState(filepath.Join(t.TempDir(), "absent.state"))
	if err != nil {
		t.Fatalf("missing state must be the fresh arm, not an error: %v", err)
	}
	if st.TranscriptPath != "" || st.Size != 0 {
		t.Fatalf("fresh state must be zero: %+v", st)
	}
}

// TestSaveLoadRoundtrip: the probe's read-before-write contract.
func TestSaveLoadRoundtrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "dir", "wi_x.state")
	want := State{TranscriptPath: "/tmp/live.jsonl", Size: 4242, UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := SaveState(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("roundtrip broken: %+v != %+v", got, want)
	}
	// No temp residue.
	ents, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp residue after save: %s", e.Name())
		}
	}
}

// TestLoadStateCorruptIsLoud: a corrupt state file refuses, naming the
// file — silent misconfiguration is worse than a wrong delta.
func TestLoadStateCorruptIsLoud(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wi_x.state")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadState(p)
	if err == nil || !strings.Contains(err.Error(), "corrupt") || !strings.Contains(err.Error(), p) {
		t.Fatalf("corrupt state must refuse loudly naming the file: %v", err)
	}
}
