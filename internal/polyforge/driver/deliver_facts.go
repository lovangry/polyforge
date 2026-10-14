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
//     passed on, OR this dispatch's command evidence — in recorded order —
//     ran every command of verification's test_bar for that repo, in that
//     repo, with exit 0, after the dispatch's last `git merge`/`git pull`
//     there. No timestamps (started_at stays optional and ignored).
//     Evidence of an earlier, crashed dispatch is gone, so it never
//     counts; a missing test_bar is "not shown" — the human's call.
//   - hand-resolved: every first-parent commit in <verified>..HEAD must be
//     a merge whose tree equals what `git merge-tree --write-tree` makes
//     of its two parents without conflict; anything else (a conflicted or
//     differently resolved merge, a non-merge commit, a missing record)
//     counts as hand-made, whatever the step reports.
//   - delivered head: refs/remotes/origin/<feature branch> (moved by the
//     push) is HEAD, and HEAD contains the fetched default branch — or,
//     for a PR merged earlier, the recorded merge commit merged HEAD.
//
// All of this is detection after the fact: by the time the envelope is
// judged the push and the merge have happened. A failed fact escalates to
// the human; prevention rests on the step contract and the machine's
// pre-push hook.
//
// autoMergeEligible is the merge rule itself, isolated so the owner's
// quick-fix rule (no review step) stays one visible condition.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	deliveryMismatch        string // non-empty: why the pushed/merged head is not the local HEAD
	selfReportedConflict    bool   // the step says it resolved a conflict
	selfReportedTestsFailed bool   // the step says tests failed
}

// autoMergeEligible is the merge rule: a PR merges without a human only
// when the independent review passed with no blocking finding (in a
// workflow that has one), the tests are covered on the pushed head, the
// delivered head is the local HEAD on the latest default branch, and no
// conflict was hand-resolved. The pre-flight (nothing uncommitted, no
// release residue) is the step contract's part. quick-fix has no review
// step: there verification's pass (a gate the lane cannot pass by
// without) stands in for it — owner decision, polyforge#166.
func autoMergeEligible(f mergeFacts) (bool, string) {
	switch {
	case f.hasReview && !f.reviewPassed:
		return false, "no independent review passed with no blocking finding"
	case f.handResolved != "":
		return false, "the delivered history has a hand-resolved or hand-made change (" + f.handResolved + ")"
	case f.deliveryMismatch != "":
		return false, "the delivered head cannot be tied to the local HEAD (" + f.deliveryMismatch + ")"
	case f.selfReportedConflict:
		return false, "the step reports an LLM-resolved conflict"
	case f.selfReportedTestsFailed:
		return false, "the step reports failing tests"
	case !f.testsCovered:
		return false, "the tests are not shown to pass on the pushed head (it differs from the commit verification passed on and this dispatch did not re-run verification's test_bar in that repo after its last merge)"
	}
	return true, ""
}

// deliverFacts derives one merged repo's facts from the capsule.
func deliverFacts(spec ResultSpec, r deliverRepo, branch string, ev []CommandEvidence) mergeFacts {
	f := mergeFacts{hasReview: spec.HasReview, reviewPassed: spec.ReviewPassed}
	verified := spec.VerifiedHeads[r.Name]
	if spec.ReposDir == "" || verified == "" {
		f.handResolved = "no verification record of this repo's head"
		return f
	}
	dir := filepath.Join(spec.ReposDir, r.Name)
	headOut, err := repoGit(dir, "rev-parse", "HEAD")
	if err != nil {
		f.handResolved = "HEAD unreadable"
		return f
	}
	head := strings.TrimSpace(headOut)
	f.deliveryMismatch = deliveredHeadMismatch(dir, head, branch, strings.TrimSpace(r.MergeCommit))
	if head == verified {
		f.testsCovered = true
		return f
	}
	f.handResolved = handMadeHistory(dir, verified, head)
	f.testsCovered = testBarRerun(r.Name, spec.VerifiedTestBar, evidenceEvents(ev, filepath.Dir(spec.ReposDir)))
	return f
}

// deliveredHeadMismatch ties what was pushed and merged to the local
// HEAD the facts were derived from: the push updated
// refs/remotes/origin/<branch> to HEAD; HEAD contains the fetched
// origin default branch (delivered from the latest main) — or, for a PR
// merged before (the fetched default branch already contains HEAD), the
// recorded merge commit's second parent is HEAD (checked when that
// commit is present locally).
func deliveredHeadMismatch(dir, head, branch, mergeCommit string) string {
	if branch != "" {
		tracked, err := repoGit(dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch)
		if err != nil || strings.TrimSpace(tracked) != head {
			return "origin/" + branch + " (the pushed branch) is not the local HEAD"
		}
	}
	def := ""
	if out, err := repoGit(dir, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		def = strings.TrimSpace(out)
	} else {
		for _, b := range []string{"main", "master"} {
			if _, err := repoGit(dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+b); err == nil {
				def = "refs/remotes/origin/" + b
				break
			}
		}
	}
	if def == "" {
		return "the fetched origin default branch is unknown"
	}
	if _, err := repoGit(dir, "merge-base", "--is-ancestor", def, head); err == nil {
		return ""
	}
	if _, err := repoGit(dir, "merge-base", "--is-ancestor", head, def); err != nil {
		return "HEAD does not contain " + strings.TrimPrefix(def, "refs/remotes/") + " (not delivered from the latest default branch)"
	}
	// already merged: the default branch moved past HEAD
	if mergeCommit != "" {
		if _, err := repoGit(dir, "cat-file", "-e", mergeCommit+"^{commit}"); err == nil {
			p2, err := repoGit(dir, "rev-parse", "--verify", "-q", mergeCommit+"^2")
			if err != nil || strings.TrimSpace(p2) != head {
				return "the recorded merge commit " + short(mergeCommit) + " did not merge the local HEAD"
			}
		}
	}
	return ""
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

// testBarCmd is one entry of verification's test bar: a command run
// inside repos/<Repo>.
type testBarCmd struct {
	Repo    string `json:"repo"`
	Command string `json:"command"`
}

// evEvent is one simple command of the dispatch's evidence, in recorded
// order: the repo it ran in (from the entry's cwd, `cd` and `git -C`),
// its words, and its entry's exit code.
type evEvent struct {
	repo  string
	words []string
	exit  int
	merge bool // git merge / git pull
}

// evidenceEvents flattens command evidence into simple commands in order.
func evidenceEvents(ev []CommandEvidence, capsuleRoot string) []evEvent {
	var out []evEvent
	for _, e := range ev {
		dir := capsuleRel(e.Cwd, capsuleRoot)
		var segs [][]string
		switch {
		case len(e.Argv) == 1:
			segs = shellSegments(e.Argv[0])
		case len(e.Argv) > 2 && shells[filepath.Base(e.Argv[0])] && strings.HasPrefix(e.Argv[1], "-") && strings.Contains(e.Argv[1], "c"):
			segs = shellSegments(e.Argv[2])
		default:
			segs = [][]string{e.Argv}
		}
		for _, seg := range segs {
			seg = dropRedirections(seg)
			i := 0
			for i < len(seg) && (isAssignment(seg[i]) || shellKeywords[seg[i]] || commandWrappers[filepath.Base(seg[i])]) {
				i++
			}
			if i >= len(seg) {
				continue
			}
			words := seg[i:]
			if words[0] == "cd" {
				if len(words) > 1 {
					dir = joinRel(dir, words[1], capsuleRoot)
				}
				continue
			}
			ev := evEvent{repo: repoOfRel(dir), words: words, exit: e.ExitCode}
			if filepath.Base(words[0]) == "git" {
				j := 1
				for j < len(words) && strings.HasPrefix(words[j], "-") {
					if words[j] == "-C" && j+1 < len(words) {
						ev.repo = repoOfRel(joinRel(dir, words[j+1], capsuleRoot))
					}
					switch words[j] {
					case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path", "--config-env":
						j++
					}
					j++
				}
				ev.merge = j < len(words) && (words[j] == "merge" || words[j] == "pull")
			}
			out = append(out, ev)
		}
	}
	return out
}

// capsuleRel answers p relative to the capsule root ("" = the root).
func capsuleRel(p, capsuleRoot string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(capsuleRoot, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "?"
		}
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Clean(p))
}

func joinRel(dir, arg, capsuleRoot string) string {
	if filepath.IsAbs(arg) {
		return capsuleRel(arg, capsuleRoot)
	}
	return filepath.ToSlash(filepath.Clean(filepath.Join(dir, arg)))
}

// repoOfRel answers the task repo a capsule-relative dir lies in.
func repoOfRel(dir string) string {
	parts := strings.Split(dir, "/")
	if len(parts) >= 2 && parts[0] == capsule.DirRepos {
		return parts[1]
	}
	return ""
}

// testBarRerun: every test-bar command verification named for repo
// appears in this dispatch's evidence, run in that repo with exit 0,
// in order, AFTER the dispatch's last merge in that repo (no merge in
// this dispatch: anywhere in it). Commands compare as normalised words
// (quotes stripped, whitespace collapsed; a leading `cd` sets the repo).
// No test bar for the repo means nothing to prove it with.
func testBarRerun(repo string, bar []testBarCmd, events []evEvent) bool {
	start := 0
	for i, e := range events {
		if e.merge && e.repo == repo {
			start = i + 1
		}
	}
	need := 0
	for _, tb := range bar {
		if tb.Repo != repo {
			continue
		}
		for _, seg := range shellSegments(tb.Command) {
			seg = dropRedirections(seg)
			if len(seg) == 0 || seg[0] == "cd" {
				continue
			}
			need++
			found := false
			for start < len(events) {
				e := events[start]
				start++
				if e.repo == repo && e.exit == 0 && strings.Join(e.words, " ") == strings.Join(seg, " ") {
					found = true
					break
				}
			}
			if !found {
				return false
			}
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

// verificationRecord reads the verified heads (driver-written) and the
// test bar (step-named) back from the verification step's output (in
// memory or reloaded from execution.json).
func verificationRecord(st *capsule.Step) (map[string]string, []testBarCmd) {
	if st == nil || st.Status != capsule.StepDone {
		return nil, nil
	}
	heads := map[string]string{}
	if h, ok := st.Output[verifiedHeadsKey].(map[string]any); ok {
		for k, v := range h {
			if s, ok := v.(string); ok {
				heads[k] = s
			}
		}
	}
	var bar []testBarCmd
	if raw, err := json.Marshal(st.Output["test_bar"]); err == nil {
		_ = json.Unmarshal(raw, &bar)
	}
	return heads, bar
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
