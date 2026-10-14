// preexisting.go: the boundary of verification's pre-existing-failure
// lane (polyforge#164 S6).
//
// A test that fails the same way on the base commit is reported as
// status pre_existing and does not fail the gate. That lane must never
// excuse the tests that pin THIS change: the repro of the bug being
// fixed fails on the base by definition (an unfixed bug would pass as
// "pre-existing"), and so do the authored acceptance tests and the
// regression test. The driver therefore rejects — through the ordinary
// envelope-rejection retry lane — a completed result that classifies a
// repro check or any protected test as pre_existing, and one whose
// pre_existing check does not name the base commit it was re-run on.
package driver

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// statusPreExisting is the verification check status of a failure that
// also fails on the base commit.
const statusPreExisting = "pre_existing"

// baseSHAShape finds a commit sha (abbreviated or full) in a detail.
var baseSHAShape = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

// protectedTestsOf folds the protected tests from the done steps'
// outputs: every declared test_files path (e2e-author, repro) and the
// regression_test paths (bug-fix fix; a `waived: ...` value names none).
func protectedTestsOf(outputs map[string]map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, o := range outputs {
		if list, ok := o["test_files"].([]any); ok {
			for _, v := range list {
				if s, ok := v.(string); ok {
					add(s)
				}
			}
		}
		if rt, ok := o["regression_test"].(string); ok && !strings.HasPrefix(strings.TrimSpace(strings.ToLower(rt)), "waived") {
			for _, s := range strings.Split(rt, ",") {
				add(s)
			}
		}
	}
	return out
}

// mentionsProtected answers the protected test text names (by its path
// or its file name), "" when none.
func mentionsProtected(text string, protected []string) string {
	for _, p := range protected {
		if strings.Contains(text, p) || strings.Contains(text, path.Base(p)) {
			return p
		}
	}
	return ""
}

// checkPreExisting judges the pre_existing classifications of one
// output (a no-op for outputs without any).
func checkPreExisting(output json.RawMessage, protected []string) error {
	if len(output) == 0 {
		return nil
	}
	var probe struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
		PreExisting []any `json:"pre_existing_failures"`
	}
	if json.Unmarshal(output, &probe) != nil {
		return nil
	}
	var problems []string
	for _, c := range probe.Checks {
		if c.Status != statusPreExisting {
			continue
		}
		switch {
		case strings.Contains(strings.ToLower(c.Name), "repro"):
			problems = append(problems, fmt.Sprintf("check %q is the repro of the bug being fixed — it fails on the base by definition, so it can never be pre_existing", c.Name))
		case mentionsProtected(c.Name+" "+c.Detail, protected) != "":
			problems = append(problems, fmt.Sprintf("check %q covers the protected test %s", c.Name, mentionsProtected(c.Name+" "+c.Detail, protected)))
		case !baseSHAShape.MatchString(c.Detail):
			problems = append(problems, fmt.Sprintf("check %q is pre_existing but its detail names no base commit sha", c.Name))
		}
	}
	for _, v := range probe.PreExisting {
		s, _ := v.(string)
		if p := mentionsProtected(s, protected); p != "" {
			problems = append(problems, fmt.Sprintf("pre_existing_failures entry %q is the protected test %s", s, p))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("an invalid pre_existing classification: %s — the repro, the authored acceptance tests (test_files) and the regression test pin this change and fail on the base by design; report them as pass or fail, never pre_existing",
		strings.Join(problems, "; "))
}
