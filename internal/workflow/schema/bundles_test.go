package schema

// bundles_test.go mechanically validates every reference workflow bundle shipped
// under workflows/: the real frontmatter parser (ValidateWorkflowFrontmatter,
// rule set WF-201/V1–V10), the agents existence check (V7), the step skill
// frontmatter validator (§4.4), and the bundle digest computation
// (internal/workflow/digest, 01 §4.5).
//
// Placement note: this lives next to the parser it exercises (internal/
// workflow/schema), extending the existing TestFeatureFullBundle pattern from
// "one blessed bundle" to "every bundle under workflows/". The test/ directory
// is reserved for daemon-dependent e2e/fault-injection suites; this check is
// pure mechanical validation and needs no runtime.
//
// Two layers:
//  1. bundleProblems() re-derives, independently of the parser's own checks,
//     the structural invariants a reference bundle must satisfy (unique ids,
//     kind catalog, on_fail gate/target rules, budget shape, agent-file
//     correspondence, step-skill frontmatter, deterministic digest). Where the
//     parser already enforces a rule, the parser's *RuleError surfaces here
//     as a problem; the thin re-checks are the test's own, per the rule that
//     gaps are covered by the test rather than by product code.
//  2. TestMalformedBundleFixtures proves the validation FAILS on the classic
//     authoring mistakes, using in-test temp-dir bundles so the negative
//     table never depends on the checkout layout.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/workflow/digest"
)

// The step kind catalog observed in feature-full and fixed by
// 01-workflow-skills.md §4.1 (rule V8). Kept as the test's own copy so a
// parser regression that loosens the catalog is caught here too.
var bundleKindCatalog = map[string]bool{
	"qa": true, "spec": true, "author": true, "code": true, "gate": true, "review": true,
}

var bundleStepIDRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

var bundleDigestHexRegex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// bundleProblems runs the full mechanical validation over one bundle
// directory and returns every problem found (empty slice = valid bundle).
func bundleProblems(bundleDir string) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	name := filepath.Base(filepath.Clean(bundleDir))

	// --- real frontmatter parse (V1–V10) ---
	fm, err := ValidateWorkflowFrontmatter(bundleDir, name)
	if err != nil {
		add("frontmatter validation: %v", err)
		return problems
	}

	// --- thin checks: ids, kinds, interactive, gates, on_fail, budget ---
	seen := map[string]int{}
	for i, st := range fm.Steps {
		if !bundleStepIDRegex.MatchString(st.ID) {
			add("step %q: id violates ^[a-z][a-z0-9-]{0,63}$", st.ID)
		}
		if prev, dup := seen[st.ID]; dup {
			add("step %q: duplicate id (positions %d and %d)", st.ID, prev, i)
		}
		seen[st.ID] = i
		if !bundleKindCatalog[st.Kind] {
			add("step %q: kind %q outside the catalog", st.ID, st.Kind)
		}
		// Interactive/gate are decoded into *bool by the parser: a non-boolean
		// YAML value fails the typed decode (proven by the fixture table
		// below), so reaching here with a non-nil pointer means a real bool.
		gate := st.Gate != nil && *st.Gate
		if st.OnFail != nil {
			if !gate {
				add("step %q: on_fail on a non-gate step", st.ID)
			}
			action := st.OnFail.Action
			if action == "" {
				action = "terminate"
			}
			if action != "rewind" && action != "terminate" {
				add("step %q: on_fail.action %q outside {rewind|terminate}", st.ID, action)
			}
			// V5: a rewind target must exist and sit STRICTLY earlier than
			// the failing step (precede-or-equal is not enough).
			if action == "rewind" {
				pos, exists := seen[st.OnFail.Target]
				if !exists {
					add("step %q: on_fail.target %q does not exist", st.ID, st.OnFail.Target)
				} else if pos >= i {
					add("step %q: on_fail.target %q is not strictly earlier", st.ID, st.OnFail.Target)
				}
			}
		}
	}

	// Reference bundles ship an explicit workflow-level budget: it is the
	// baseline frozen into work_items.workflow_budget_baseline at WI creation
	// (02 §4.4/§4.5), so a reference bundle without one has no baseline to
	// freeze. Assert presence plus the >0 shape (parser V6 re-checked here).
	if fm.Budget == nil {
		add("workflow-level budget missing (reference bundles must ship a baseline)")
	} else {
		if fm.Budget.Requests != nil && *fm.Budget.Requests <= 0 {
			add("budget.requests must be > 0")
		}
		if fm.Budget.Tokens != nil && *fm.Budget.Tokens <= 0 {
			add("budget.tokens must be > 0")
		}
		if fm.Budget.WallMinutes != nil && *fm.Budget.WallMinutes <= 0 {
			add("budget.wall_minutes must be > 0")
		}
		if fm.Budget.Requests == nil && fm.Budget.Tokens == nil && fm.Budget.WallMinutes == nil {
			add("budget present but declares no dimension")
		}
	}
	if fm.Rewind != nil {
		if fm.Rewind.MaxTotal != nil && *fm.Rewind.MaxTotal <= 0 {
			add("rewind.max_total must be > 0")
		}
		if fm.Rewind.MaxPerStep != nil && *fm.Rewind.MaxPerStep <= 0 {
			add("rewind.max_per_step must be > 0")
		}
	}

	// --- agents correspondence (V7): one file per step id, no orphans ---
	if err := CheckAgentsExist(bundleDir, fm); err != nil {
		add("agents existence: %v", err)
	}
	agentsDir := filepath.Join(bundleDir, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		add("agents dir unreadable: %v", err)
	} else {
		for _, e := range entries {
			if e.IsDir() {
				add("agents/: subdirectory %q not allowed", e.Name())
				continue
			}
			if !strings.HasSuffix(e.Name(), ".md") {
				add("agents/: non-.md file %q (V7)", e.Name())
				continue
			}
			id := strings.TrimSuffix(e.Name(), ".md")
			if _, ok := seen[id]; !ok {
				add("agents/: orphan file %q matches no step id", e.Name())
			}
		}
	}

	// --- step skill frontmatter (§4.4): parseable description/tools/output ---
	for _, st := range fm.Steps {
		raw, err := os.ReadFile(filepath.Join(agentsDir, st.ID+".md"))
		if err != nil {
			continue // already reported by CheckAgentsExist above
		}
		if err := ValidateStepSkill(st.ID, string(raw)); err != nil {
			add("agents/%s.md: %v", st.ID, err)
		}
	}

	// --- bundle digest (01 §4.5): computable, stable, well-formed ---
	d1, err := digest.ComputeBundleDigest(bundleDir)
	if err != nil {
		add("digest: %v", err)
	} else if !bundleDigestHexRegex.MatchString(d1) {
		add("digest %q is not 64 lowercase hex chars", d1)
	}
	d2, err := digest.ComputeBundleDigest(bundleDir)
	if err == nil && d1 != d2 {
		add("digest not deterministic: %s != %s", d1, d2)
	}

	return problems
}

// TestReferenceBundles iterates every bundle under workflows/ and requires
// zero problems from the full mechanical validation, plus pairwise-distinct
// digests (two bundles that digest identically would be one bundle too many).
func TestReferenceBundles(t *testing.T) {
	root := filepath.Join("..", "..", "..", "workflows")
	if _, err := os.Stat(root); err != nil {
		t.Skip("workflows/ not present in this checkout")
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("workflows/ unreadable: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("workflows/ contains no bundles")
	}
	digests := map[string]string{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		t.Run(d.Name(), func(t *testing.T) {
			for _, p := range bundleProblems(dir) {
				t.Errorf("%s", p)
			}
			got, err := digest.ComputeBundleDigest(dir)
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if other, dup := digests[got]; dup {
				t.Errorf("bundle digest collides with %s", other)
			}
			digests[got] = d.Name()
		})
	}
}

// validDemoBundle is a self-contained minimal bundle: the negative fixture
// table below mutates copies of it, so the malformed-fixture tests never
// depend on the checkout layout.
func validDemoBundle() map[string]string {
	return map[string]string{
		"workflow.md": `---
name: demo
rewind:
  max_total: 2
  max_per_step: 2
budget: { requests: 10, tokens: 1000, wall_minutes: 5 }
steps:
  - { id: code, kind: code }
  - { id: verification, kind: gate, gate: true, on_fail: { action: rewind, target: code } }
---
# demo body
`,
		"agents/code.md":         "---\nname: code\ndescription: demo code step\ntools: [read, bash]\noutput:\n  type: object\n  required: [build_status]\n  properties:\n    build_status: { type: string }\n---\n# code\n",
		"agents/verification.md": "---\nname: verification\ndescription: demo gate step\ntools: [read, bash]\noutput:\n  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n---\n# verification\n",
	}
}

func writeBundleDir(t *testing.T, files map[string]string) string {
	t.Helper()
	// The bundle lives in a subdirectory named after its frontmatter `name`
	// (rule V2: name must equal the bundle directory name).
	dir := filepath.Join(t.TempDir(), "demo")
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestValidDemoBundle is the fixture table's control: the unmutated demo
// bundle must produce zero problems, otherwise every "expected failure"
// below is suspect.
func TestValidDemoBundle(t *testing.T) {
	dir := writeBundleDir(t, validDemoBundle())
	if problems := bundleProblems(dir); len(problems) != 0 {
		for _, p := range problems {
			t.Errorf("%s", p)
		}
	}
}

// TestMalformedBundleFixtures proves the bundle validation FAILS on the
// classic authoring mistakes: unknown kind, dangling on_fail target,
// duplicate id, missing agent file, non-parseable output schema, a
// non-boolean interactive flag, and an orphan agent file.
func TestMalformedBundleFixtures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(files map[string]string)
		want   string // required substring of at least one reported problem
	}{
		{
			name: "unknown kind",
			mutate: func(f map[string]string) {
				f["workflow.md"] = strings.Replace(f["workflow.md"], "kind: code", "kind: deploy", 1)
			},
			want: "V8",
		},
		{
			name: "dangling on_fail target",
			mutate: func(f map[string]string) {
				f["workflow.md"] = strings.Replace(f["workflow.md"], "target: code", "target: nonexistent", 1)
			},
			want: "V5",
		},
		{
			name: "on_fail target not strictly earlier",
			mutate: func(f map[string]string) {
				// Point the only gate's rewind at itself: exists, but not
				// strictly earlier — the precedence half of V5.
				f["workflow.md"] = strings.Replace(f["workflow.md"], "target: code", "target: verification", 1)
			},
			want: "V5",
		},
		{
			name: "duplicate step id",
			mutate: func(f map[string]string) {
				f["workflow.md"] = strings.Replace(f["workflow.md"], "{ id: verification, kind: gate", "{ id: code, kind: gate", 1)
			},
			want: "V2",
		},
		{
			name: "missing agent file",
			mutate: func(f map[string]string) {
				delete(f, "agents/code.md")
			},
			want: "V7",
		},
		{
			name: "non-parseable output schema",
			mutate: func(f map[string]string) {
				// .inf decodes to +Inf, which has no JSON serialization:
				// the output "schema" is not a parseable JSON Schema object.
				f["agents/code.md"] = "---\nname: code\ndescription: d\ntools: [read]\noutput: {type: object, bad: .inf}\n---\n# code\n"
			},
			want: "output is not a JSON object",
		},
		{
			name: "non-boolean interactive flag",
			mutate: func(f map[string]string) {
				f["workflow.md"] = strings.Replace(f["workflow.md"], "{ id: code, kind: code }", "{ id: code, kind: code, interactive: maybe }", 1)
			},
			want: "V1",
		},
		{
			name: "orphan agent file",
			mutate: func(f map[string]string) {
				f["agents/ghost.md"] = "---\nname: ghost\ndescription: orphan\ntools: [read]\noutput: {type: object}\n---\n# ghost\n"
			},
			want: "orphan file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := validDemoBundle()
			tc.mutate(files)
			dir := writeBundleDir(t, files)
			problems := bundleProblems(dir)
			if len(problems) == 0 {
				t.Fatalf("expected a validation problem containing %q, got none", tc.want)
			}
			for _, p := range problems {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("no reported problem contains %q; problems: %v", tc.want, problems)
		})
	}
}
