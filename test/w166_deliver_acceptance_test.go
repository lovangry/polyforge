package test

// w166_deliver_acceptance_test.go: the `deliver` step that ends the
// delivery bundles (polyforge#166), end to end through the real daemon
// and driver. GitHub is never contacted: the task repo's origin is a
// local bare repo and `gh` is a fake first on the daemon's PATH that
// keeps PRs as files and performs `gh pr merge --merge` as a real
// --no-ff merge into the bare origin's main. The step agent is the
// scripted fakepi: its deliver actions run the step's git/gh commands
// (recorded as real command evidence the driver judges) and report the
// output JSON those commands wrote.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGHPRs is the stand-in for GitHub's PR API over the bare origin
// $FAKE_GH_ORIGIN; state lives under $FAKE_GH_STATE (prs/<n>/{head,base,
// state,url,merge} and calls.log). It ignores --json/--jq and prints the
// one value each scripted call asks for.
const fakeGHPRs = `#!/bin/sh
S="$FAKE_GH_STATE"; mkdir -p "$S/prs"
echo "gh $*" >> "$S/calls.log"
[ "$1" = pr ] || { echo "fake gh: unsupported: $*" >&2; exit 2; }
sub=$2; shift 2
head=""; state=all; base=main; target=""; method=""
while [ $# -gt 0 ]; do
  case "$1" in
    --head) head=$2; shift 2 ;;
    --state) state=$2; shift 2 ;;
    --base) base=$2; shift 2 ;;
    --json|-q|--jq|--title|--body|--body-file) shift 2 ;;
    --merge|--squash|--rebase) method=$1; shift ;;
    -*) shift ;;
    *) target=$1; shift ;;
  esac
done
case "$sub" in
  list)
    for d in "$S"/prs/*; do
      [ -d "$d" ] && [ "$(cat "$d/head")" = "$head" ] || continue
      st=$(cat "$d/state")
      case "$state" in
        open) [ "$st" = OPEN ] || continue ;;
        merged) [ "$st" = MERGED ] || continue ;;
      esac
      cat "$d/url"
    done ;;
  create)
    for d in "$S"/prs/*; do
      if [ -d "$d" ] && [ "$(cat "$d/head")" = "$head" ] && [ "$(cat "$d/state")" = OPEN ]; then
        echo "a pull request for branch \"$head\" already exists" >&2; exit 1
      fi
    done
    n=$(( $(ls "$S/prs" | wc -l) + 1 )); d="$S/prs/$n"; mkdir -p "$d"
    echo "$head" > "$d/head"; echo "$base" > "$d/base"; echo OPEN > "$d/state"
    echo "https://github.com/fake/demo/pull/$n" > "$d/url"; cat "$d/url" ;;
  merge)
    d="$S/prs/${target##*/}"
    [ "$method" = --merge ] || { echo "fake gh: refusing merge method '$method'" >&2; exit 1; }
    [ "$(cat "$d/state")" = OPEN ] || { echo "fake gh: PR not open" >&2; exit 1; }
    w=$(mktemp -d)
    git clone -q "$FAKE_GH_ORIGIN" "$w/c" || exit 1
    (cd "$w/c" && git checkout -q "$(cat "$d/base")" &&
      git merge -q --no-ff -m "Merge pull request #${target##*/} from $(cat "$d/head")" "origin/$(cat "$d/head")" &&
      git push -q origin "HEAD:refs/heads/$(cat "$d/base")" && git rev-parse HEAD > "$d/merge") || exit 1
    echo MERGED > "$d/state"; rm -rf "$w" ;;
  view)
    cat "$S/prs/${target##*/}/merge" ;;
esac
`

// deliverWorld is one scenario's fake GitHub: the bare origin (main
// seeded with a baseline), the fake gh and its state dir.
type deliverWorld struct {
	origin string
	state  string
	bin    string
}

func newDeliverWorld(t *testing.T) *deliverWorld {
	t.Helper()
	root := shortTmp("/tmp", "pfw166")
	t.Cleanup(func() { os.RemoveAll(root) })
	w := &deliverWorld{origin: filepath.Join(root, "demo.git"), state: filepath.Join(root, "gh"), bin: filepath.Join(root, "bin")}
	gitRun(t, "", "init", "-q", "--bare", "-b", "main", w.origin)
	seed := filepath.Join(root, "seed")
	gitRun(t, "", "clone", "-q", w.origin, seed)
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "-c", "user.name=t", "-c", "user.email=t@invalid", "commit", "-q", "-m", "baseline")
	gitRun(t, seed, "push", "-q", "origin", "HEAD:refs/heads/main")
	if err := os.MkdirAll(w.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.bin, "gh"), []byte(fakeGHPRs), 0o755); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *deliverWorld) daemonEnv() daemonOpt {
	return optEnv("PATH="+w.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GH_STATE="+w.state, "FAKE_GH_ORIGIN="+w.origin)
}

// prState answers PR n's state file ("" when absent).
func (w *deliverWorld) prState(n string) string {
	b, _ := os.ReadFile(filepath.Join(w.state, "prs", n, "state"))
	return strings.TrimSpace(string(b))
}

// calls counts the fake gh invocations starting with prefix.
func (w *deliverWorld) calls(prefix string) int {
	b, _ := os.ReadFile(filepath.Join(w.state, "calls.log"))
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// literalBranch spells the feature branch out in a scripted command: the
// driver judges the recorded command text, so a push names its branch
// literally (steps/deliver.md), never through a shell variable.
func literalBranch(branch string, cmds ...string) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = strings.ReplaceAll(c, "$b", branch)
	}
	return out
}

// deliverPre opens every scripted deliver command: the feature branch from
// the capsule directory name, then into the task repo.
const deliverPre = `set -e; b="polyforge/$(basename "$PWD")"; cap="$PWD"; mkdir -p artifacts; cd repos/demo; `

// deliverOut writes the deliver output JSON for one repo.
func deliverOut(fields string) string {
	return `printf '{"verdict":"pass","repos":[{"name":"demo","branch":"%s","pr_url":"%s",` + fields + `}],"delivery_note":"scripted delivery"}' "$b" "$url"` +
		` > "$cap/artifacts/deliver-output.json"`
}

// deliverPushAndPR is steps 1-5 of steps/deliver.md for one repo, idempotent:
// an already-delivered repo (local head on origin main and a merged PR)
// skips to the record; otherwise merge origin/main, push the feature
// branch (never forced), reuse or open the PR.
const deliverPushAndPR = deliverPre + `git fetch -q origin; ` +
	`url=$(gh pr list --head "$b" --state merged --json url -q '.[0].url' | head -1); ` +
	`if [ -n "$url" ] && git merge-base --is-ancestor HEAD origin/main; then exit 0; fi; ` +
	`git merge -q --no-edit origin/main; ` +
	`remote=$(git ls-remote origin "refs/heads/$b" | cut -f1); ` +
	`if [ -n "$remote" ] && ! git merge-base --is-ancestor "$remote" HEAD; then exit 3; fi; ` +
	`git push -q origin "HEAD:refs/heads/$b"; ` +
	`url=$(gh pr list --head "$b" --state open --json url -q '.[0].url' | head -1); ` +
	`[ -n "$url" ] || gh pr create --base main --head "$b" --title "w166 delivery" --body-file /dev/null`

// deliverMergeAndRecord merges the open PR (if any) with --merge and
// records it.
const deliverMergeAndRecord = deliverPre +
	`url=$(gh pr list --head "$b" --state open --json url -q '.[0].url' | head -1); ` +
	`if [ -n "$url" ]; then gh pr merge "$url" --merge; fi; ` +
	`url=$(gh pr list --head "$b" --state merged --json url -q '.[0].url' | head -1); ` +
	`mc=$(gh pr view "$url" --json mergeCommit -q .mergeCommit.oid); `

// TestW166DeliverFeatureFullAutoMergesIdempotently: a feature-full ticket
// whose gates all pass is delivered automatically — the feature branch
// pushed, one PR opened and merged with a merge commit into origin main.
// The first deliver dispatch dies after merging and before reporting; the
// re-dispatch runs the same procedure and reuses the branch and PR: still
// one PR, one merge.
func TestW166DeliverFeatureFullAutoMergesIdempotently(t *testing.T) {
	inst := newV1Install(t)
	inst.keepDeliver = true
	w := newDeliverWorld(t)
	wi := newWIWithBundle(t, inst, "w166: feature-full delivered automatically", "feature-full", nil)
	branch := "polyforge/" + wiPathName(t, inst, wi)
	deliver := literalBranch(branch, deliverPushAndPR, deliverMergeAndRecord+
		`printf '{"verdict":"pass","repos":[{"name":"demo","branch":"%s","pr_url":"%s","merged":true,"merge_commit":"%s","tests_passed":true}],"delivery_note":"scripted delivery"}' "$b" "$url" "$mc" > "$cap/artifacts/deliver-output.json"`)
	actions := []map[string]any{
		{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "spec", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "grill-review", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "plan", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-author", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "e2e-baseline", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "code", "outcome": "completed", "handoff": true, "name": "demo", "origin": w.origin},
		{"do": "envelope", "id": "e2e-run", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "review", "outcome": "completed", "output": map[string]any{"verdict": "pass"}},
		{"do": "envelope", "id": "verification", "outcome": "completed", "output": map[string]any{"verdict": "pass"}},
		// dies after pushing, opening AND merging, before the envelope
		{"do": "envelope", "id": "deliver", "run": deliver, "crash": "before"},
		// the re-dispatch: the same idempotent procedure
		{"do": "envelope", "id": "deliver", "outcome": "completed", "run": deliver, "output_file": "artifacts/deliver-output.json"},
	}
	inst.setPlanFull(t, wi, actions, nil)
	d := newV1Daemon(t, inst, w.daemonEnv())
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	waitWorkflowStatus(t, d, wi, "completed", 240*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	branchHead := strings.TrimSpace(gitRun(t, w.origin, "rev-parse", "refs/heads/"+branch))
	mainHead := strings.TrimSpace(gitRun(t, w.origin, "rev-parse", "refs/heads/main"))
	parents := strings.Fields(gitRun(t, w.origin, "rev-list", "--parents", "-n", "1", mainHead))
	if len(parents) != 3 || parents[2] != branchHead {
		t.Fatalf("origin main %s is not the PR merge commit of %s (parents %v)", mainHead, branchHead, parents)
	}
	if n := w.calls("gh pr create"); n != 1 {
		t.Fatalf("gh pr create ran %d times, want exactly 1 (idempotent re-dispatch)", n)
	}
	if n := w.calls("gh pr merge"); n != 1 {
		t.Fatalf("gh pr merge ran %d times, want exactly 1", n)
	}
	if n := w.calls("gh pr view"); n != 2 {
		t.Fatalf("the procedure ran to its record %d times, want 2 (the crashed dispatch and the re-dispatch)", n)
	}
	if merges := strings.Fields(gitRun(t, w.origin, "rev-list", "--merges", "refs/heads/main")); len(merges) != 1 {
		t.Fatalf("origin main carries %d merge commits, want 1", len(merges))
	}
	x := readExecution(t, d.capsuleDir(wi))
	st := x.Steps["deliver"]
	if st == nil || st.Status != "done" {
		t.Fatalf("deliver not done: %+v", st)
	}
	repos, _ := st.Output["repos"].([]any)
	if len(repos) != 1 {
		t.Fatalf("deliver output repos: %+v", st.Output)
	}
	r, _ := repos[0].(map[string]any)
	if r["branch"] != branch || r["merged"] != true || r["merge_commit"] != mainHead || r["pr_url"] != "https://github.com/fake/demo/pull/1" {
		t.Fatalf("deliver result does not record branch/PR/merge commit: %+v (want branch %s, merge %s)", r, branch, mainHead)
	}
}

// TestW166DeliverLLMResolvedConflictStopsAtHuman: origin main moves on with
// a conflicting change while the chore lane runs; deliver resolves the
// conflict, re-runs the tests, pushes and opens the PR — and may not merge
// on its own: a completed result claiming the merge is refused, the step
// asks, and only after a human answers "merge" does the merge land.
func TestW166DeliverLLMResolvedConflictStopsAtHuman(t *testing.T) {
	inst := newV1Install(t)
	inst.keepDeliver = true
	w := newDeliverWorld(t)
	const qid = "deliver-merge"
	wi := newWIWithBundle(t, inst, "w166: conflict resolved by the LLM needs a human", "chore", nil)
	branch := "polyforge/" + wiPathName(t, inst, wi)
	// main moves on: another change to the same file lands on origin main
	moveMain := `set -e; t=$(mktemp -d); git clone -q ` + w.origin + ` "$t/c"; cd "$t/c"; echo "main moved on" > work.txt; git add -A; ` +
		`git -c user.name=o -c user.email=o@invalid commit -q -m "other PR"; git push -q origin HEAD:refs/heads/main; rm -rf "$t"`
	resolve := deliverPre + `git fetch -q origin; if git merge -q --no-edit origin/main; then exit 9; fi; ` +
		`{ echo "fake agent repo work"; echo "main moved on"; } > work.txt; git add work.txt; git commit -q --no-edit; ` +
		`echo "full test bar re-run: ok"; ` +
		`git push -q origin "HEAD:refs/heads/$b"; ` +
		`url=$(gh pr create --base main --head "$b" --title "w166 conflict" --body-file /dev/null); ` +
		deliverOut(`"merged":true,"merge_commit":"not-merged-yet","tests_passed":true,"llm_resolved_conflict":true`)
	merge := deliverMergeAndRecord +
		`printf '{"verdict":"pass","repos":[{"name":"demo","branch":"%s","pr_url":"%s","merged":true,"merge_commit":"%s","tests_passed":true,"llm_resolved_conflict":true}]}' "$b" "$url" "$mc" > "$cap/artifacts/deliver-output.json"`
	actions := []map[string]any{
		{"do": "envelope", "id": "plan", "outcome": "completed", "handoff": true},
		{"do": "envelope", "id": "code", "outcome": "completed", "handoff": true, "name": "demo", "origin": w.origin},
		{"do": "envelope", "id": "review", "outcome": "completed", "output": map[string]any{"verdict": "pass"}, "run": []string{moveMain}},
		{"do": "envelope", "id": "verification", "outcome": "completed", "output": map[string]any{"verdict": "pass"}},
		// resolves, re-tests, pushes, opens the PR — then claims a merge it may not make alone
		{"do": "envelope", "id": "deliver", "outcome": "completed", "run": literalBranch(branch, resolve), "output_file": "artifacts/deliver-output.json"},
		{"do": "envelope", "id": "deliver", "outcome": "needs_input", "question_id": qid,
			"question": "PR https://github.com/fake/demo/pull/1: merging main needed an LLM-resolved conflict in work.txt. merge / don't merge / back to code?"},
		{"do": "envelope", "id": "deliver", "outcome": "completed", "run": literalBranch(branch, merge), "output_file": "artifacts/deliver-output.json"},
	}
	inst.setPlanFull(t, wi, actions, nil)
	d := newV1Daemon(t, inst, w.daemonEnv())
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	waitQuestion(t, d, wi, qid, 180*time.Second)
	rej := waitForEvent(t, d, wi, "envelope_rejected", 30*time.Second)
	if !strings.Contains(string(rej.Body), "LLM-resolved conflict") {
		t.Fatalf("the unanswered merge claim was not refused for the LLM-resolved conflict: %s", rej.Body)
	}
	branchHead := strings.TrimSpace(gitRun(t, w.origin, "rev-parse", "refs/heads/"+branch))
	if w.prState("1") != "OPEN" || w.calls("gh pr merge") != 0 {
		t.Fatalf("PR must be open and unmerged while the human question is pending (state %q)", w.prState("1"))
	}
	if strings.Contains(gitRun(t, w.origin, "branch", "--contains", branchHead), "main") {
		t.Fatal("origin main already contains the delivered branch before the human answered")
	}

	answerQuestion(t, d, wi, qid, "merge")
	waitWorkflowStatus(t, d, wi, "completed", 120*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)
	if w.prState("1") != "MERGED" {
		t.Fatalf("PR state %q after the human's merge answer", w.prState("1"))
	}
	mainHead := strings.TrimSpace(gitRun(t, w.origin, "rev-parse", "refs/heads/main"))
	parents := strings.Fields(gitRun(t, w.origin, "rev-list", "--parents", "-n", "1", mainHead))
	if len(parents) != 3 || parents[2] != branchHead {
		t.Fatalf("origin main %s is not the merge of %s (parents %v)", mainHead, branchHead, parents)
	}
}

// TestW166DeliverWithoutIndependentReviewNeedsHuman: quick-fix has no
// review step, so the merge rule never holds: a merge claim without a
// human answer is refused; the human answers "don't merge" and the lane
// completes with the PR open and origin main untouched.
func TestW166DeliverWithoutIndependentReviewNeedsHuman(t *testing.T) {
	inst := newV1Install(t)
	inst.keepDeliver = true
	w := newDeliverWorld(t)
	const qid = "deliver-merge"
	push := deliverPushAndPR + `; url=$(gh pr list --head "$b" --state open --json url -q '.[0].url' | head -1); ` +
		deliverOut(`"merged":true,"merge_commit":"x","tests_passed":true`)
	keep := deliverPre + `url=$(gh pr list --head "$b" --state open --json url -q '.[0].url' | head -1); ` +
		deliverOut(`"merged":false,"not_merged_reason":"human: don't merge yet","tests_passed":true`)
	wi := inst.newWI(t, "w166: no independent review, human decides", nil, nil)
	branch := "polyforge/" + wiPathName(t, inst, wi)
	inst.setPlanFull(t, wi,
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "name": "demo", "origin": w.origin},
			{"do": "envelope", "id": "verification", "outcome": "completed", "output": map[string]any{"verdict": "pass"}},
			{"do": "envelope", "id": "deliver", "outcome": "completed", "run": literalBranch(branch, push), "output_file": "artifacts/deliver-output.json"},
			{"do": "envelope", "id": "deliver", "outcome": "needs_input", "question_id": qid,
				"question": "PR https://github.com/fake/demo/pull/1: no independent review in quick-fix. merge / don't merge / back to code?"},
			{"do": "envelope", "id": "deliver", "outcome": "completed", "run": literalBranch(branch, keep), "output_file": "artifacts/deliver-output.json"},
		}, nil)
	d := newV1Daemon(t, inst, w.daemonEnv())
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)

	waitQuestion(t, d, wi, qid, 120*time.Second)
	rej := waitForEvent(t, d, wi, "envelope_rejected", 30*time.Second)
	if !strings.Contains(string(rej.Body), "independent review") {
		t.Fatalf("the unanswered merge claim was not refused for the missing review: %s", rej.Body)
	}
	answerQuestion(t, d, wi, qid, "don't merge")
	waitWorkflowStatus(t, d, wi, "completed", 120*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)
	if w.prState("1") != "OPEN" || w.calls("gh pr merge") != 0 {
		t.Fatalf("PR must stay open and unmerged (state %q)", w.prState("1"))
	}
	if merges := strings.TrimSpace(gitRun(t, w.origin, "rev-list", "--merges", "refs/heads/main")); merges != "" {
		t.Fatalf("origin main got a merge despite \"don't merge\": %s", merges)
	}
	gitRun(t, w.origin, "rev-parse", "--verify", "refs/heads/"+branch) // the feature branch was pushed
}
