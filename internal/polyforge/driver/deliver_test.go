package driver

// deliver_test.go: the delivery binding (polyforge#166) — the command
// rule over the deliver step's recorded evidence (push only the feature
// branch, merge only with --merge, no main-push override) and the merge
// rule (auto-merge only under a passed review, passing tests and no
// LLM-resolved conflict; anything else, and every unmerged PR, owes a
// human answer).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// deliverCapsule lays <tmp>/polyforge-166/repos/demo/.git and answers the
// ReposDir, so the feature branch is polyforge/polyforge-166.
func deliverCapsule(t *testing.T) string {
	t.Helper()
	repos := filepath.Join(t.TempDir(), "polyforge-166", "repos")
	if err := os.MkdirAll(filepath.Join(repos, "demo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repos
}

func deliverSpec(repos string, reviewPassed bool, answers int) ResultSpec {
	return ResultSpec{WIID: "wi_d", StepID: deliverStepID, SpawnN: 1, Gate: true,
		RequireCommandEvidence: true, ReposDir: repos, ReviewPassed: reviewPassed, HumanAnswers: answers}
}

func ev(cmds ...string) []CommandEvidence {
	var out []CommandEvidence
	for _, c := range cmds {
		out = append(out, CommandEvidence{Argv: []string{"sh", "-c", c}, DurationMs: 5})
	}
	return out
}

const goodPush = "cd repos/demo && git push origin HEAD:refs/heads/polyforge/polyforge-166"

func deliverRes(outcome Outcome, output string, evidence []CommandEvidence) *StepResult {
	return &StepResult{WIID: "wi_d", StepID: deliverStepID, SpawnN: 1, Outcome: outcome,
		Output: json.RawMessage(output), CommandEvidence: evidence}
}

const mergedOut = `{"verdict":"pass","repos":[{"name":"demo","branch":"polyforge/polyforge-166","pr_url":"https://github.com/o/r/pull/1","merged":true,"merge_commit":"abc","tests_passed":true}]}`

func TestDeliverCommandRule(t *testing.T) {
	repos := deliverCapsule(t)
	cases := []struct {
		name string
		cmds []string
		bad  string // "" = accepted; else a substring of the rejection
	}{
		{"feature branch push", []string{goodPush, "gh pr merge https://github.com/o/r/pull/1 --merge"}, ""},
		{"argv-form push", nil, ""},
		{"git -C push", []string{"git -C repos/demo push -u origin HEAD:polyforge/polyforge-166"}, ""},
		{"push main", []string{"cd repos/demo && git push origin HEAD:refs/heads/main"}, "not the work item's feature branch"},
		{"push main by name", []string{"git push origin main"}, "not the work item's feature branch"},
		{"push without refspec", []string{"cd repos/demo; git push"}, "without an explicit refspec"},
		{"push remote only", []string{"git push origin"}, "without an explicit refspec"},
		{"other feature branch", []string{"git push origin HEAD:polyforge/other-1"}, "not the work item's feature branch"},
		{"force", []string{"git push --force origin HEAD:polyforge/polyforge-166"}, "--force"},
		{"force short", []string{"git push -f origin HEAD:polyforge/polyforge-166"}, "force/delete"},
		{"force lease", []string{"git push --force-with-lease=x origin HEAD:polyforge/polyforge-166"}, "--force-with-lease"},
		{"plus refspec", []string{"git push origin +HEAD:polyforge/polyforge-166"}, "forced refspec"},
		{"delete refspec", []string{"git push origin :polyforge/polyforge-166"}, "deleting refspec"},
		{"all", []string{"git push --all origin"}, "--all"},
		{"tags", []string{"git push --tags origin HEAD:polyforge/polyforge-166"}, "--tags"},
		{"tag refspec", []string{"git push origin refs/tags/v1.0.0"}, "not the work item's feature branch"},
		{"main override", []string{"PF_ALLOW_MAIN_PUSH=1 git push origin HEAD:polyforge/polyforge-166"}, "PF_ALLOW_MAIN_PUSH"},
		{"quoted main push in a script", []string{`bash -c "cd repos/demo && git push origin 'HEAD:main'"`}, "not the work item's feature branch"},
		{"squash merge", []string{"gh pr merge 1 --squash"}, "--squash"},
		{"rebase merge", []string{"gh pr merge 1 --merge --rebase"}, "--rebase"},
		{"admin merge", []string{"gh pr merge 1 --merge --admin"}, "--admin"},
		{"merge method missing", []string{"gh pr merge 1"}, "without --merge"},
		// command position only: words that merely mention the commands
		{"pr title mentions git push", []string{`gh pr create --title "polyforge#166: deliver does git push only to the feature branch" --body-file b.md`}, ""},
		{"echo mentions gh pr merge", []string{`echo "next: gh pr merge 1 --squash"`}, ""},
		{"grep for the override", []string{"grep -rn PF_ALLOW_MAIN_PUSH ."}, ""},
		{"if wrapper", []string{"if git push origin HEAD:main; then echo ok; fi"}, "not the work item's feature branch"},
		{"env override", []string{"env PF_ALLOW_MAIN_PUSH=1 git push origin HEAD:polyforge/polyforge-166"}, "PF_ALLOW_MAIN_PUSH"},
		{"exported override", []string{"export PF_ALLOW_MAIN_PUSH=1; git push origin HEAD:polyforge/polyforge-166"}, "PF_ALLOW_MAIN_PUSH"},
		{"git-dir option", []string{"git --git-dir repos/demo/.git push origin main"}, "not the work item's feature branch"},
		{"hooks bypass", []string{"git push --no-verify origin HEAD:polyforge/polyforge-166"}, "--no-verify"},
		{"hooksPath override", []string{"git -c core.hooksPath=/dev/null push origin HEAD:polyforge/polyforge-166"}, "core.hooksPath"},
		{"gh repo flag squash", []string{"gh -R o/r pr merge 1 --squash"}, "--squash"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evidence := ev(c.cmds...)
			if c.name == "argv-form push" {
				evidence = []CommandEvidence{{Argv: []string{"git", "-C", "repos/demo", "push", "origin", "HEAD:refs/heads/polyforge/polyforge-166"}, DurationMs: 3}}
			}
			err := ValidateStepResult(deliverRes(OutcomeCompleted, mergedOut, evidence), deliverSpec(repos, true, 0))
			switch {
			case c.bad == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad)):
				t.Fatalf("want rejection containing %q, got %v", c.bad, err)
			}
		})
	}
	// The command rule judges every delivered outcome that carries
	// evidence, not just completed ones.
	q := deliverRes(OutcomeNeedsInput, `{}`, ev("git push origin HEAD:main"))
	q.QuestionID, q.Question = "deliver-merge", "merge?"
	if err := ValidateStepResult(q, deliverSpec(repos, true, 0)); err == nil {
		t.Fatal("a needs_input deliver envelope that pushed main was accepted")
	}
}

func TestDeliverMergeRule(t *testing.T) {
	repos := deliverCapsule(t)
	out := func(repo string) string {
		return `{"verdict":"pass","repos":[` + repo + `]}`
	}
	const base = `"name":"demo","branch":"polyforge/polyforge-166","pr_url":"https://github.com/o/r/pull/1"`
	cases := []struct {
		name    string
		output  string
		review  bool
		answers int
		bad     string
	}{
		{"auto merge", mergedOut, true, 0, ""},
		{"llm-resolved conflict needs a human", out(`{` + base + `,"merged":true,"merge_commit":"abc","tests_passed":true,"llm_resolved_conflict":true}`), true, 0, "LLM-resolved conflict"},
		{"llm-resolved conflict after a human answer", out(`{` + base + `,"merged":true,"merge_commit":"abc","tests_passed":true,"llm_resolved_conflict":true}`), true, 1, ""},
		{"no passed review needs a human", mergedOut, false, 0, "independent review"},
		{"no passed review after a human answer", mergedOut, false, 1, ""},
		{"failed tests need a human", out(`{` + base + `,"merged":true,"merge_commit":"abc","tests_passed":false}`), true, 0, "passing tests"},
		{"unmerged needs a human", out(`{` + base + `,"merged":false,"not_merged_reason":"human said no"}`), true, 0, "left unmerged"},
		{"unmerged after a human answer", out(`{` + base + `,"merged":false,"not_merged_reason":"human said no"}`), true, 1, ""},
		{"unmerged without reason", out(`{` + base + `,"merged":false}`), true, 1, "not_merged_reason"},
		{"merged without merge commit", out(`{` + base + `,"merged":true,"tests_passed":true}`), true, 0, "merge_commit"},
		{"merged unknown", out(`{` + base + `}`), true, 0, "merged true or false"},
		{"no pr", out(`{"name":"demo","branch":"polyforge/polyforge-166","merged":true,"merge_commit":"a","tests_passed":true}`), true, 0, "pr_url"},
		{"wrong branch", out(`{"name":"demo","branch":"main","pr_url":"u","merged":true,"merge_commit":"a","tests_passed":true}`), true, 0, "feature branch"},
		{"declared repo missing", out(`{"name":"other","skipped_reason":"nothing"}`), true, 0, `declared repo "demo"`},
		{"skipped repo", out(`{"name":"demo","skipped_reason":"no commits beyond the baseline"}`), true, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateStepResult(deliverRes(OutcomeCompleted, c.output, ev(goodPush)), deliverSpec(repos, c.review, c.answers))
			switch {
			case c.bad == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad)):
				t.Fatalf("want rejection containing %q, got %v", c.bad, err)
			}
		})
	}
	// "back to code" is a fail verdict: re-bound to gate_failed (the rewind),
	// never judged by the merge rule.
	res := deliverRes(OutcomeCompleted, `{"verdict":"fail","fail_reason":"human: back to code"}`, ev(goodPush))
	if err := ValidateStepResult(res, deliverSpec(repos, true, 1)); err != nil || res.Outcome != OutcomeGateFailed {
		t.Fatalf("back-to-code verdict: err=%v outcome=%s", err, res.Outcome)
	}
}

func TestReviewPassed(t *testing.T) {
	done := func(out map[string]any) *capsule.Step { return &capsule.Step{Status: capsule.StepDone, Output: out} }
	cases := []struct {
		name string
		st   *capsule.Step
		want bool
	}{
		{"no review step", nil, false},
		{"pass", done(map[string]any{"verdict": "pass"}), true},
		{"pass with non-blocking", done(map[string]any{"verdict": "pass", "findings": []any{map[string]any{"severity": "non-blocking"}}}), true},
		{"pass with blocking", done(map[string]any{"verdict": "pass", "findings": []any{map[string]any{"severity": "blocking"}}}), false},
		{"fail", done(map[string]any{"verdict": "fail"}), false},
		{"not done", &capsule.Step{Status: "in_progress", Output: map[string]any{"verdict": "pass"}}, false},
	}
	for _, c := range cases {
		if got := reviewPassed(c.st); got != c.want {
			t.Errorf("%s: reviewPassed = %v, want %v", c.name, got, c.want)
		}
	}
}
