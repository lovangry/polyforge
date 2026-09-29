package capsule

// quarantine_test.go: the public Quarantine face used by the Daemon's
// 04 §7.3 rollback path — the directory is MOVED under
// <ws>/capsules/_quarantine/<wi_id>/ (single "capsules" level, 03 §2.4),
// the scene is preserved, and the marker names the cause.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuarantineMarksAndMoves(t *testing.T) {
	ws := t.TempDir()
	c, _, err := Create(ws, "wi_q01", 2, "m-01", "sha256:b")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MarkReady(); err != nil {
		t.Fatal(err)
	}
	dst, err := c.Quarantine("secret_rolled_back")
	if err != nil {
		t.Fatal(err)
	}
	// Moved out of the live capsule path, preserved exactly once under
	// _quarantine (03 §2.4 root: <ws>/capsules/_quarantine/<wi>/...).
	if _, err := os.Stat(c.Dir); !os.IsNotExist(err) {
		t.Fatalf("live capsule dir must be gone, stat err = %v", err)
	}
	wantRoot := filepath.Join(ws, "capsules", "_quarantine", "wi_q01")
	if filepath.Dir(dst) != wantRoot {
		t.Fatalf("quarantine root = %s, want under %s", dst, wantRoot)
	}
	if _, err := os.Stat(filepath.Join(dst, "capsule.json")); err != nil {
		t.Fatalf("the scene must be preserved, not deleted: %v", err)
	}
	// The marker names the cause and is local-only audit evidence.
	raw, err := os.ReadFile(filepath.Join(dst, "logs", "quarantine.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		Reason        string `json:"reason"`
		QuarantinedAt string `json:"quarantined_at"`
		Generation    int64  `json:"generation"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.Reason != "secret_rolled_back" || marker.Generation != 2 {
		t.Fatalf("marker: %s", raw)
	}
	if _, err := time.Parse(time.RFC3339Nano, marker.QuarantinedAt); err != nil {
		t.Fatalf("marker timestamp not RFC3339: %q", marker.QuarantinedAt)
	}
	// Idempotent at the layout level: a second Create for the SAME
	// generation starts clean (the old scene is gone from the live path).
	c2, res, err := Create(ws, "wi_q01", 2, "m-01", "sha256:b")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reused || res.Quarantined != "" {
		t.Fatalf("post-quarantine create must be fresh: %+v", res)
	}
	if c2.Dir != c.Dir {
		t.Fatal("the live capsule path is stable across quarantine")
	}
}
