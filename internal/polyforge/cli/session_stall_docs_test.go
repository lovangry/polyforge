package cli

// session_stall_docs_test.go: polyforge#43 AC7 — the `session_stall_window`
// default's documentation carries its P99 calibration basis, and the ops
// verb's [daemon] knob enumeration names the key (#42's docs sync synced
// commands/config/uds-routes but missed the verb page). The acceptance is
// itself that document text (AC7: the default's description carries its
// P99 calibration basis), so the text is the behaviour this pins.
//
// carries: docs/reference/config.md, skills/polyforge/verbs/ops.md

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stallDocsRead answers one repo-relative doc page, whitespace-normalized
// (w371's normalization: line breaks and runs collapse to single spaces).
func stallDocsRead(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(docsSetRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}

// TestSessionStallWindowDocsPinned: the `5m` default's row in
// docs/reference/config.md states the calibration (what span the window
// must clear, the measured healthy head, the motivating wedge incidents),
// and skills/polyforge/verbs/ops.md's [daemon] knob list names
// session_stall_window.
func TestSessionStallWindowDocsPinned(t *testing.T) {
	config := stallDocsRead(t, "docs/reference/config.md")
	for _, want := range []string{
		"The `5m` default is calibrated, not guessed:",
		"no work-class signal at all",
		"inter-write P99 2.8 s",
		"25-30 min at zero output",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("docs/reference/config.md must state %q — the session_stall_window default's P99 calibration basis (polyforge#43 AC7)", want)
		}
	}
	ops := stallDocsRead(t, "skills/polyforge/verbs/ops.md")
	if !strings.Contains(ops, "session_stall_window") {
		t.Error("skills/polyforge/verbs/ops.md must name session_stall_window in the [daemon] knob enumeration — the zero-output dispatch window is a production-settable key the page lists")
	}
}
