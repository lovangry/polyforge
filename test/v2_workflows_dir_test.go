package test

// v2_workflows_dir_test.go: the workflows directory a scenario's daemon
// runs. The shipped bundles declare `requires_human_answer: true` on
// their human-confirmation steps (the driver's human-gate guarantee,
// polyforge#138), which an unattended scripted plan (completed grill with
// no question) can never satisfy. Scenarios that are not about the gate
// therefore run a copy with that one frontmatter flag stripped; scenarios
// that are (TestV2HumanGateShippedBundle) set inst.keepHumanGates.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var humanGateFlag = regexp.MustCompile(`,\s*requires_human_answer:\s*true`)

// workflowsPath answers the --workflows-dir the daemon should use.
func (u *v1Install) workflowsPath() string {
	shipped := filepath.Join(u.root, "workflows")
	if u.keepHumanGates {
		return shipped
	}
	if u.strippedWF == "" {
		u.strippedWF = strippedWorkflowsDir(u.t, shipped)
	}
	return u.strippedWF
}

// strippedWorkflowsDir copies the shipped workflows tree into a test temp
// dir with the requires_human_answer flag removed from every workflow.md,
// for daemons started by harnesses other than v1Install (w113/w079, v3).
func strippedWorkflowsDir(t testing.TB, shipped string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "workflows")
	err := filepath.Walk(shipped, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(shipped, p)
		out := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.HasSuffix(p, "workflow.md") {
			raw = humanGateFlag.ReplaceAll(raw, nil)
		}
		return os.WriteFile(out, raw, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("stripped workflows copy: %v", err)
	}
	return dst
}
