package driver

// deliver_facts.go: the merge-rule facts the driver derives itself
// (polyforge#166 review B1) instead of trusting the deliver step's
// self-report, which a crash or restart mid-deliver can make wrong (a
// re-dispatch sees "Already up to date" and no longer knows that an
// earlier dispatch merged — or hand-resolved — anything).
//
//   - verified heads: when `verification` completes, the driver records
//     each task repo's HEAD in its output (verifiedHeadsKey) — never
//     step-supplied (recordVerifiedHeads overwrites the key).
//   - tests covered: a repo's pushed HEAD equals the commit verification
//     passed on, OR this dispatch's command evidence re-ran every
//     command verification recorded with exit 0, each again with exit 0
//     and started no earlier than HEAD's commit time (after the last
//     merge). Evidence of an earlier, crashed dispatch is gone, so it
//     never counts — an unprovable re-run is the human's call.
//   - hand-resolved: every first-parent commit in <verified>..HEAD must be
//     a merge whose tree equals what `git merge-tree --write-tree` makes
//     of its two parents without conflict; anything else (a conflicted or
//     differently resolved merge, a non-merge commit, a missing record)
//     counts as hand-made, whatever the step reports.
//
// autoMergeEligible is the merge rule itself, isolated so the owner's
// pending quick-fix decision is a one-line change.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// verifiedHeadsKey is the driver-written key of verification's output:
// task repo name -> HEAD sha at the moment verification completed.
const verifiedHeadsKey = "verified_heads"

// mergeFacts is what the merge rule judges one merged repo on.
type mergeFacts struct {
	hasReview               bool   // the workflow has an independent review step
	reviewPassed            bool   // ...and it is done with pass, no blocking finding
	testsCovered            bool   // see the file comment
	handResolved            string // non-empty: why the history is not a clean merge
	selfReportedConflict    bool   // the step says it resolved a conflict
	selfReportedTestsFailed bool   // the step says tests failed
}

// autoMergeEligible is the merge rule: a PR merges without a human only
// when the independent review passed with no blocking finding, the tests
// are covered on the pushed head, and no conflict was hand-resolved. The
// pre-flight (nothing uncommitted, no release residue) is the step
// contract's part. quick-fix has no review step, so it never qualifies;
// to let it auto-merge on verification alone, change the first test to
// `f.hasReview && !f.reviewPassed`.
func autoMergeEligible(f mergeFacts) (bool, string) {
	switch {
	case !f.reviewPassed:
		return false, "no independent review passed with no blocking finding"
	case f.handResolved != "":
		return false, "the delivered history has a hand-resolved or hand-made change (" + f.handResolved + ")"
	case f.selfReportedConflict:
		return false, "the step reports an LLM-resolved conflict"
	case f.selfReportedTestsFailed:
		return false, "the step reports failing tests"
	case !f.testsCovered:
		return false, "the tests are not shown to pass on the pushed head (it differs from the commit verification passed on and this dispatch did not re-run verification's commands after the last merge)"
	}
	return true, ""
}

// deliverFacts derives one merged repo's facts from the capsule.
func deliverFacts(spec ResultSpec, repo string, ev []CommandEvidence) mergeFacts {
	f := mergeFacts{hasReview: spec.HasReview, reviewPassed: spec.ReviewPassed}
	verified := spec.VerifiedHeads[repo]
	if spec.ReposDir == "" || verified == "" {
		f.handResolved = "no verification record of this repo's head"
		return f
	}
	dir := filepath.Join(spec.ReposDir, repo)
	headOut, err := repoGit(dir, "rev-parse", "HEAD")
	if err != nil {
		f.handResolved = "HEAD unreadable"
		return f
	}
	head := strings.TrimSpace(headOut)
	if head == verified {
		f.testsCovered = true
		return f
	}
	f.handResolved = handMadeHistory(dir, verified, head)
	f.testsCovered = testBarRerun(dir, spec.VerifiedCommands, ev)
	return f
}

// handMadeHistory answers "" when every first-parent commit in
// verified..head is a merge git reproduces without conflict, else why not.
func handMadeHistory(dir, verified, head string) string {
	if _, err := repoGit(dir, "merge-base", "--is-ancestor", verified, head); err != nil {
		return "the verified commit " + short(verified) + " is not an ancestor of HEAD"
	}
	out, err := repoGit(dir, "rev-list", "--first-parent", "--parents", verified+".."+head)
	if err != nil {
		return "history unreadable"
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return "commit " + short(f[0]) + " is not a two-parent merge"
		}
		want, err := repoGit(dir, "merge-tree", "--write-tree", f[1], f[2])
		if err != nil {
			return "merge " + short(f[0]) + " had conflicts"
		}
		got, err := repoGit(dir, "rev-parse", f[0]+"^{tree}")
		if err != nil || firstLineOf(want) != strings.TrimSpace(got) {
			return "merge " + short(f[0]) + " differs from git's own merge result"
		}
	}
	return ""
}

// testBarRerun: every verification command that exited 0 reappears in
// this dispatch's evidence with exit 0, started at or after HEAD's commit
// time. No recorded verification command means nothing to prove it with.
func testBarRerun(dir string, verified, ev []CommandEvidence) bool {
	ctOut, err := repoGit(dir, "log", "-1", "--format=%ct", "HEAD")
	if err != nil {
		return false
	}
	ct, err := strconv.ParseInt(strings.TrimSpace(ctOut), 10, 64)
	if err != nil {
		return false
	}
	after := time.Unix(ct, 0)
	need := 0
	for _, v := range verified {
		if v.ExitCode != 0 || len(v.Argv) == 0 {
			continue
		}
		need++
		found := false
		for _, e := range ev {
			if e.ExitCode == 0 && reflect.DeepEqual(e.Argv, v.Argv) && !e.StartedAt.IsZero() && !e.StartedAt.Before(after) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return need > 0
}

// checkNothingToDeliver: a skipped repo has no commit beyond its
// claim-time baseline (capsule.json repos).
func checkNothingToDeliver(reposDir, repo string) error {
	if reposDir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(reposDir), capsule.CapsuleJSON))
	var id capsule.Identity
	if err == nil {
		err = json.Unmarshal(raw, &id)
	}
	if err != nil || id.Repos[repo] == "" {
		return errors.New("its claim-time baseline is not recorded, so nothing-to-deliver cannot be confirmed")
	}
	out, err := repoGit(filepath.Join(reposDir, repo), "rev-list", "--count", id.Repos[repo]+"..HEAD")
	if err != nil {
		return fmt.Errorf("its history is unreadable: %v", err)
	}
	if n := strings.TrimSpace(out); n != "0" {
		return fmt.Errorf("it has %s commit(s) beyond its baseline", n)
	}
	return nil
}

// recordVerifiedHeads writes the capsule's task repos' HEADs into
// verification's accepted output (overwriting anything the step supplied).
func recordVerifiedHeads(out map[string]any, capsuleDir string) {
	delete(out, verifiedHeadsKey)
	reposDir := filepath.Join(capsuleDir, capsule.DirRepos)
	heads := map[string]any{}
	for _, name := range taskRepoNames(reposDir) {
		if h, err := repoGit(filepath.Join(reposDir, name), "rev-parse", "HEAD"); err == nil {
			heads[name] = strings.TrimSpace(h)
		}
	}
	if len(heads) > 0 {
		out[verifiedHeadsKey] = heads
	}
}

// verificationRecord reads verified heads and commands back from the
// verification step's output (in memory or reloaded from execution.json).
func verificationRecord(st *capsule.Step) (map[string]string, []CommandEvidence) {
	if st == nil || st.Status != capsule.StepDone {
		return nil, nil
	}
	heads := map[string]string{}
	switch h := st.Output[verifiedHeadsKey].(type) {
	case map[string]any:
		for k, v := range h {
			if s, ok := v.(string); ok {
				heads[k] = s
			}
		}
	}
	var cmds []CommandEvidence
	if raw, err := json.Marshal(st.Output["commands"]); err == nil {
		_ = json.Unmarshal(raw, &cmds)
	}
	return heads, cmds
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
