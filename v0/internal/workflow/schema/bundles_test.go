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

	"github.com/silgrid/polyforge/internal/testutil"
	"github.com/silgrid/polyforge/internal/workflow/digest"
	"gopkg.in/yaml.v3"
)

// The step kind catalog observed in feature-full and fixed by
// 01-workflow-skills.md §4.1 (rule V8). Kept as the test's own copy so a
// parser regression that loosens the catalog is caught here too.
var bundleKindCatalog = map[string]bool{
	"qa": true, "spec": true, "author": true, "code": true, "gate": true, "review": true,
}

var bundleStepIDRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

var bundleDigestHexRegex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// candidateElemRegex is the element-format convention adopted by the reference
// bundles: a provider-qualified harness model id ("<provider>/<model>"),
// the same provider/id form the pi harness accepts for --model.
// 01-workflow-skills.md §12 item 4 deliberately leaves the concrete candidate
// encoding OPEN (interpretation belongs to the harness adapter contract,
// 00-architecture.md §3.6a, which pins no encoding either), so the product
// parser enforces only V9's non-empty-string rule; this test-side convention
// gate is what keeps every shipped reference bundle on one spelling: a
// lowercase provider segment and model segment, one '/' between them, no
// whitespace.
var candidateElemRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`)

// candidateProblems validates one declared model_candidates list (defaults
// or a step override) against the reference-bundle element convention:
// every entry matches candidateElemRegex and the list carries no duplicate
// (a duplicate is a pointless candidate under V9's bounded in-order switch
// and always an authoring mistake). Absence is legal and never lands here
// (V9: lists are optional; a step without an override inherits defaults
// wholesale — replace, never merge).
func candidateProblems(where string, cands []string) []string {
	var probs []string
	seen := map[string]bool{}
	for _, c := range cands {
		if !candidateElemRegex.MatchString(c) {
			probs = append(probs, fmt.Sprintf("%s model_candidates: element %q is not a provider-qualified id \"<provider>/<model>\"", where, c))
		}
		if seen[c] {
			probs = append(probs, fmt.Sprintf("%s model_candidates: duplicate element %q", where, c))
		}
		seen[c] = true
	}
	return probs
}

// --- output schema contract (fail-closed round) ---
//
// Enforcement reality, which decides the shape of every check below: there
// is NO machine JSON-Schema validator in the execution path. AIHub's creation
// transaction does not even parse agents frontmatter (02 §4.5); the runtime
// conformance check is the WI Agent's own reading of the schema (pf-execute
// §4 step 4, INV-1h: non-conforming output = infrastructure failure), backed
// by the M4/M5 mechanical evidence channels in execution.json. The layer that
// CAN mechanically guarantee something about the schemas is this test: it
// parses each output schema into a plain map and asserts the structural
// invariants the reference bundles must encode. Every invariant below is also
// expressed with standard draft-07 keywords only (required/enum/minItems/
// minLength/oneOf/contains), so wiring a real validator into the runtime later
// enforces the same contract with no schema rewrite.

// asMap / asSlice / strSlice / intVal / dig are small navigation helpers over
// the yaml-decoded schema map (map[string]any trees of any).
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func strSlice(v any) []string {
	var out []string
	for _, e := range asSlice(v) {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intVal(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func hasStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// dig walks a chain of mapping keys through a decoded schema tree, returning
// nil as soon as any step is absent or not a mapping.
func dig(v any, path ...string) any {
	cur := v
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// outputSchemaProblems validates one agent file's output schema against the
// reference-bundle output contract. raw is the whole file; frontmatter must
// already have passed ValidateStepSkill (bundleProblems calls this only after
// a clean ValidateStepSkill, and the negative table calls it on synthetic
// but structurally valid frontmatter).
//
// Rules, generic-first:
//   - every REQUIRED array property declares minItems >= 1 (an empty list is
//     an unfilled template — WF-6/WF-8 class);
//   - any schema whose verdict enum carries both pass and fail (a gate in the
//     executor's convention) carries a two-branch oneOf split: the fail branch
//     requires a non-empty fail_reason (minLength 1), the pass branch must
//     not (WF-1/WF-9 class — no verdict-vocabulary or conditionality gaps);
//   - evidence (root-cause) requires minItems >= 1 and item-level
//     kind/ref/detail (WF-2);
//   - ready_for_spec (feature-full grill) admits only [true] (WF-7 — false is
//     not a valid completion);
//   - targeted, keyed by reference-bundle step id: verification (checks
//     required + minItems, no `fail` check status under a pass verdict,
//     and a `fail`-status check REQUIRED under a fail verdict — an all-pass
//     checks array riding a fail verdict is the fix-round-2 contradiction),
//     review (no `blocking` severity under pass; a `blocking` finding must
//     exist under fail), release (pass requires non-empty version + at least
//     one artifacts_published), e2e-run (authored_titles echo required),
//     e2e-author (scenario_titles must itself be uniqueItems - the
//     producer side of the echoed pair).
func outputSchemaProblems(stepID string, raw []byte) []string {
	front, _, ok := SplitFrontmatter(raw)
	if !ok {
		return nil // ValidateStepSkill reports the missing frontmatter
	}
	var sk StepSkillFrontmatter
	if err := yaml.Unmarshal(front, &sk); err != nil {
		return nil // ValidateStepSkill reports the parse failure
	}
	out := sk.Output
	if out == nil {
		return nil // ValidateStepSkill reports the missing output
	}
	var probs []string
	add := func(format string, args ...any) {
		probs = append(probs, fmt.Sprintf("agents/%s.md output schema: "+format, append([]any{stepID}, args...)...))
	}
	props := asMap(out["properties"])

	// Generic: required arrays must be non-empty-able only via minItems >= 1.
	for _, name := range strSlice(out["required"]) {
		p := asMap(props[name])
		if t, _ := p["type"].(string); t != "array" {
			continue
		}
		if mi, ok := intVal(p["minItems"]); !ok || mi < 1 {
			add("required array property %q must declare minItems >= 1 (an empty list is an unfilled template)", name)
		}
	}

	// Gate split: verdict enum carrying both pass and fail ⇒ oneOf split
	// with a fail branch that requires a non-empty fail_reason.
	verdictEnum := strSlice(dig(props, "verdict", "enum"))
	var passBr, failBr map[string]any
	if hasStr(verdictEnum, "pass") && hasStr(verdictEnum, "fail") {
		branches := asSlice(out["oneOf"])
		if len(branches) != 2 {
			add("verdict gate must carry a two-branch oneOf split (pass branch / fail branch)")
		} else {
			for _, b := range branches {
				bp := asMap(b)
				be := strSlice(dig(bp, "properties", "verdict", "enum"))
				if len(be) == 1 && be[0] == "pass" {
					passBr = bp
				} else if len(be) == 1 && be[0] == "fail" {
					failBr = bp
				}
			}
			if passBr == nil || failBr == nil {
				add("oneOf branches must be verdict-specific: one branch with verdict enum [pass], one with [fail]")
				passBr, failBr = nil, nil
			} else {
				passReq := strSlice(passBr["required"])
				failReq := strSlice(failBr["required"])
				if !hasStr(passReq, "verdict") || !hasStr(failReq, "verdict") {
					add("both oneOf branches must require verdict")
				}
				if !hasStr(failReq, "fail_reason") {
					add("the fail branch must require fail_reason")
				}
				if hasStr(passReq, "fail_reason") {
					add("the pass branch must not require fail_reason")
				}
				if ml, ok := intVal(dig(failBr, "properties", "fail_reason", "minLength")); !ok || ml < 1 {
					add("the fail branch fail_reason must declare minLength >= 1")
				}
			}
		}
	}

	// Shape-keyed: evidence items must carry kind/ref/detail and be non-empty.
	if ev := asMap(props["evidence"]); ev != nil {
		if mi, ok := intVal(ev["minItems"]); !ok || mi < 1 {
			add("evidence must declare minItems >= 1")
		}
		itemReq := strSlice(dig(ev, "items", "required"))
		for _, k := range []string{"kind", "ref", "detail"} {
			if !hasStr(itemReq, k) {
				add("evidence items must require %q", k)
			}
		}
	}

	// Shape-keyed: ready_for_spec admits only true.
	if rfs := asMap(props["ready_for_spec"]); rfs != nil {
		rawEnum := asSlice(rfs["enum"])
		if len(rawEnum) != 1 || rawEnum[0] != true {
			add("ready_for_spec must be enum: [true] - the only valid completion asserts readiness")
		}
	}

	// Targeted rules keyed by reference-bundle step id.
	switch stepID {
	case "verification":
		if !hasStr(strSlice(out["required"]), "checks") {
			add("verification output must require checks (a verdict over zero checks is an unfilled template)")
		}
		if ck := asMap(props["checks"]); ck != nil {
			if mi, ok := intVal(ck["minItems"]); !ok || mi < 1 {
				add("verification checks must declare minItems >= 1")
			}
		}
		if passBr != nil {
			stEnum := strSlice(dig(passBr, "properties", "checks", "items", "properties", "status", "enum"))
			if len(stEnum) == 0 {
				add("verification pass branch must restrict check status (items properties status enum) and exclude fail")
			} else if hasStr(stEnum, "fail") {
				add("verification pass branch must not admit a check status of fail (a pass verdict with a failing check is self-contradictory)")
			}
		}
		if failBr != nil {
			// F2-5 (fix round 2): a fail verdict must mechanically carry at
			// least one check with status fail (draft-07 contains) — the old
			// fail branch accepted an all-pass checks array, so
			// {verdict:fail, fail_reason, checks:[{status:pass}]} validated.
			if stEnum := strSlice(dig(failBr, "properties", "checks", "contains", "properties", "status", "enum")); len(stEnum) != 1 || stEnum[0] != "fail" {
				add("verification fail branch must require checks to contain a failing check (contains with status enum exactly [fail]) — an all-pass checks array cannot ride a fail verdict")
			}
			if !hasStr(strSlice(dig(failBr, "properties", "checks", "contains", "required")), "status") {
				add("verification fail branch contains schema must require status (an item without the field would satisfy it vacuously)")
			}
		}
	case "review":
		if passBr != nil {
			sevEnum := strSlice(dig(passBr, "properties", "findings", "items", "properties", "severity", "enum"))
			if len(sevEnum) == 0 {
				add("review pass branch must restrict finding severity (items properties severity enum) and exclude blocking")
			} else if hasStr(sevEnum, "blocking") {
				add("review pass branch must not admit a blocking finding severity (a pass verdict with a blocking finding is self-contradictory)")
			}
		}
		if failBr != nil {
			if !hasStr(strSlice(dig(failBr, "properties", "findings", "contains", "properties", "severity", "enum")), "blocking") {
				add("review fail branch must require findings to contain a blocking finding (contains)")
			}
		}
	case "release":
		if passBr != nil {
			preq := strSlice(passBr["required"])
			if !hasStr(preq, "version") {
				add("release pass branch must require version (an unfilled template must not validate as a completed release)")
			}
			if !hasStr(preq, "artifacts_published") {
				add("release pass branch must require artifacts_published (an unfilled template must not validate as a completed release)")
			}
			if ap := asMap(dig(passBr, "properties", "artifacts_published")); ap != nil {
				if mi, ok := intVal(ap["minItems"]); !ok || mi < 1 {
					add("release pass branch artifacts_published must declare minItems >= 1")
				}
			} else {
				add("release pass branch must restate artifacts_published with minItems >= 1")
			}
			if v := asMap(dig(passBr, "properties", "version")); v != nil {
				if ml, ok := intVal(v["minLength"]); !ok || ml < 1 {
					add("release pass branch version must declare minLength >= 1")
				}
			} else {
				add("release pass branch must restate version with minLength >= 1")
			}
		}
	case "e2e-run":
		if !hasStr(strSlice(out["required"]), "authored_titles") {
			add("e2e-run output must require authored_titles (the verbatim echo of e2e-author's scenario_titles)")
		}
		// F2-6 (fix round 2): the echo must be uniqueItems — a duplicated
		// title cannot correspond 1:1 with the authored scenarios, which is
		// the whole point of the echo. The element-for-element equality
		// itself is not expressible in draft-07; that half lives in
		// pf-execute §4's echoed-field cross-check (executor-side), flagged
		// as a design-ruling finding for a future runtime validator.
		if at := asMap(props["authored_titles"]); at != nil {
			if uniq, ok := at["uniqueItems"].(bool); !ok || !uniq {
				add("e2e-run authored_titles must declare uniqueItems: true (a duplicated echo cannot correspond 1:1 with the authored scenarios)")
			}
		}
	case "e2e-author":
		// R3-F3: the producer side of the echoed pair must constrain the
		// SAME uniqueness the consumer demands. e2e-run echoes
		// scenario_titles verbatim under its own uniqueItems constraint,
		// so a producer schema without uniqueItems admits a duplicated
		// list that leaves the echo step with NO valid response (echoing
		// it violates uniqueItems; deduping it violates the exact-echo
		// instruction). The two sides of a schema-constrained echo pair
		// must agree.
		if !hasStr(strSlice(out["required"]), "scenario_titles") {
			add("e2e-author output must require scenario_titles (e2e-run echoes it verbatim as its authored_titles cross-check)")
		}
		if st := asMap(props["scenario_titles"]); st != nil {
			if uniq, ok := st["uniqueItems"].(bool); !ok || !uniq {
				add("e2e-author scenario_titles must declare uniqueItems: true (e2e-run must echo it verbatim under its own uniqueItems constraint - a duplicated authored list leaves the echo step with no valid response)")
			}
		}
	}
	return probs
}

// --- step wall-budget gate (reference bundles) ---
//
// The daemon watchdog derives its absolute backstop over a single agent
// instance as max(3x the step's declared wall, watchdogBackstopFloor) with
// watchdogBackstopFloor = 15 min, and when the step declares NO wall it
// assumes watchdogStepWallDefault = 5 min (internal/daemon/watchdog.go).
// A reference bundle that declares nothing therefore gets a flat 15-minute
// backstop on every step of every lane, and measured attempts (2-5 min
// healthy, 15-52 min in the slow tail, interactive steps routinely past 15)
// run straight through it: the agent is bounded-terminated by wall_backstop
// while still legitimately working.
//
// So every step of every shipped reference bundle must declare
// budget.wall_minutes, and the declarations must satisfy two relationships:
//
//   - stepWallMin <= wall <= stepWallMax. The lower bound is the watchdog's
//     assumed default, so the backstop is genuinely 3x the declared wall and
//     never clamps back to the 15-minute floor; the upper bound keeps a
//     single attempt under one hour (backstop <= 180 min).
//   - sum(step walls) <= workflow budget.wall_minutes, so one in-budget pass
//     over the whole flow cannot on its own bust the workflow-level ceiling
//     (WF-103 OVER_BUDGET).
//
// This is a reference-bundle convention gate, NOT a WF-201 rule:
// ValidateWorkflowFrontmatter stays permissive (V6 only requires > 0) so
// custom bundles may still declare nothing and ride the watchdog default.
const (
	stepWallMin = 5
	stepWallMax = 60
)

func stepWallProblems(fm *Frontmatter) []string {
	var probs []string
	add := func(format string, args ...any) {
		probs = append(probs, fmt.Sprintf(format, args...))
	}
	var sum int64
	for _, st := range fm.Steps {
		if st.Budget == nil || st.Budget.WallMinutes == nil {
			add("step %q: no budget.wall_minutes declared - the watchdog would fall back to its assumed step wall and clamp the backstop to the 15-minute floor", st.ID)
			continue
		}
		w := *st.Budget.WallMinutes
		if w < stepWallMin {
			add("step %q: budget.wall_minutes %d is below the %d-minute minimum (the backstop max(3xwall, 15min) would clamp to its floor)", st.ID, w, stepWallMin)
		} else if w > stepWallMax {
			add("step %q: budget.wall_minutes %d is above the %d-minute maximum (a single attempt must not book more than one hour)", st.ID, w, stepWallMax)
		}
		sum += w
	}
	if fm.Budget == nil || fm.Budget.WallMinutes == nil {
		if sum > 0 {
			add("workflow budget.wall_minutes missing: nothing bounds the %d-minute sum of declared step walls", sum)
		}
		return probs
	}
	if ceiling := *fm.Budget.WallMinutes; sum > ceiling {
		add("declared step walls sum to %d minutes, over the workflow budget.wall_minutes ceiling of %d (one in-budget pass must not bust the workflow budget)", sum, ceiling)
	}
	return probs
}

// bundleProblems runs the full mechanical validation over one bundle
// directory and returns every problem found (empty slice = valid bundle)
// plus advisory notices (legal-but-noteworthy states that must not fail
// a reference bundle, only surface it).
func bundleProblems(bundleDir string) (problems []string, notices []string) {
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	notice := func(format string, args ...any) {
		notices = append(notices, fmt.Sprintf(format, args...))
	}
	name := filepath.Base(filepath.Clean(bundleDir))

	// --- real frontmatter parse (V1–V10) ---
	fm, err := ValidateWorkflowFrontmatter(bundleDir, name)
	if err != nil {
		add("frontmatter validation: %v", err)
		return problems, notices
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

	// --- model candidates (V9): absent allowed (notice), present gated ---
	// Absence is LEGAL everywhere (V9: the lists are optional), but a
	// reference bundle without defaults.model_candidates degrades the
	// infra-failure machinery to a no-op — re-dispatch cannot switch
	// models, it just reuses the harness adapter's default — so it is
	// surfaced as a NOTICE, never a problem. Presence gets the element-
	// format and duplicate gate. Step-level lists REPLACE defaults (V9:
	// whole-list replacement, no merge), so each list is validated
	// standalone and a step override need not repeat defaults' entries.
	if fm.Defaults == nil || len(fm.Defaults.ModelCandidates) == 0 {
		notice("defaults.model_candidates absent: infra-failure re-dispatch will reuse the harness adapter's default model (V9)")
	} else {
		problems = append(problems, candidateProblems("defaults", fm.Defaults.ModelCandidates)...)
	}
	for _, st := range fm.Steps {
		if len(st.ModelCandidates) > 0 {
			problems = append(problems, candidateProblems("step "+st.ID, st.ModelCandidates)...)
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

	// --- step skill frontmatter (§4.4): parseable description/tools/output,
	// plus the fail-closed output-schema contract (see outputSchemaProblems) ---
	for _, st := range fm.Steps {
		raw, err := os.ReadFile(filepath.Join(agentsDir, st.ID+".md"))
		if err != nil {
			continue // already reported by CheckAgentsExist above
		}
		if err := ValidateStepSkill(st.ID, string(raw)); err != nil {
			add("agents/%s.md: %v", st.ID, err)
			continue
		}
		problems = append(problems, outputSchemaProblems(st.ID, raw)...)
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

	return problems, notices
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
			problems, notices := bundleProblems(dir)
			// Step wall-budget gate: layered on top of bundleProblems
			// because it is a reference-bundle convention, not something
			// the synthetic demo fixtures below are expected to satisfy.
			// A frontmatter parse failure is already reported above.
			if fm, err := ValidateWorkflowFrontmatter(dir, d.Name()); err == nil {
				problems = append(problems, stepWallProblems(fm)...)
			}
			for _, p := range problems {
				t.Errorf("%s", p)
			}
			// Absent candidates are legal (V9) — surfaced as notices so a
			// bundle that silently lost its defaults stays visible in -v
			// output without failing the gate.
			for _, n := range notices {
				t.Logf("notice: %s", n)
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

// i64 is the pointer helper for the *int64 budget fields.
func i64(v int64) *int64 { return &v }

// wallFlow builds a Frontmatter carrying a workflow-level wall ceiling and
// one step per entry in walls; a nil entry means "this step declares no
// budget at all", which is the exact state the four reference bundles were
// in before this gate existed.
func wallFlow(ceiling *int64, walls ...*int64) *Frontmatter {
	fm := &Frontmatter{Name: "demo"}
	if ceiling != nil {
		fm.Budget = &Budget{WallMinutes: ceiling}
	}
	for i, w := range walls {
		st := Step{ID: fmt.Sprintf("s%d", i), Kind: "code"}
		if w != nil {
			st.Budget = &Budget{WallMinutes: w}
		}
		fm.Steps = append(fm.Steps, st)
	}
	return fm
}

// TestStepWallProblemsValid is the positive control for the wall gate: a flow
// whose steps all declare a wall inside [5,60] and whose sum sits under the
// workflow ceiling must report nothing, including the exact boundary values.
func TestStepWallProblemsValid(t *testing.T) {
	fm := wallFlow(i64(180), i64(stepWallMin), i64(30), i64(stepWallMax), i64(20))
	if probs := stepWallProblems(fm); len(probs) != 0 {
		for _, p := range probs {
			t.Errorf("%s", p)
		}
	}
}

// TestStepWallProblemsNegatives proves the gate goes RED on each way a bundle
// can get the wall declarations wrong: a step declaring nothing (the original
// defect - the watchdog then assumes 5 min and the backstop collapses to its
// 15-minute floor), a wall under the minimum, a wall over the maximum, a step
// sum that busts the workflow ceiling, and declared walls with no workflow
// ceiling to bound them.
func TestStepWallProblemsNegatives(t *testing.T) {
	cases := []struct {
		name string
		fm   *Frontmatter
		want string
	}{
		{
			name: "step declares no wall at all",
			fm:   wallFlow(i64(180), i64(30), nil, i64(20)),
			want: "no budget.wall_minutes declared",
		},
		{
			name: "wall below the minimum",
			fm:   wallFlow(i64(180), i64(stepWallMin-1)),
			want: "below the 5-minute minimum",
		},
		{
			name: "wall above the maximum",
			fm:   wallFlow(i64(180), i64(stepWallMax+1)),
			want: "above the 60-minute maximum",
		},
		{
			name: "step sum busts the workflow ceiling",
			fm:   wallFlow(i64(60), i64(30), i64(30), i64(20)),
			want: "over the workflow budget.wall_minutes ceiling",
		},
		{
			name: "no workflow ceiling to bound the step walls",
			fm:   wallFlow(nil, i64(30), i64(20)),
			want: "workflow budget.wall_minutes missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probs := stepWallProblems(tc.fm)
			if len(probs) == 0 {
				t.Fatalf("expected a wall-budget problem containing %q, got none", tc.want)
			}
			for _, p := range probs {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("no reported problem contains %q; problems: %v", tc.want, probs)
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
		"agents/verification.md": "---\nname: verification\ndescription: demo gate step\ntools: [read, bash]\noutput:\n  type: object\n  required: [verdict, checks]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    checks:\n      type: array\n      minItems: 1\n      items: { type: object, required: [name, status] }\n    fail_reason: { type: string }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n        checks:\n          type: array\n          items:\n            type: object\n            properties:\n              status: { type: string, enum: [pass, skipped] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n        checks:\n          type: array\n          minItems: 1\n          contains:\n            type: object\n            required: [status]\n            properties:\n              status: { type: string, enum: [fail] }\n      required: [verdict, fail_reason]\n---\n# verification\n",
	}
}

func writeBundleDir(t *testing.T, files map[string]string) string {
	t.Helper()
	// The bundle lives in a subdirectory named after its frontmatter `name`
	// (rule V2: name must equal the bundle directory name).
	dir := filepath.Join(t.TempDir(), "demo")
	if err := testutil.WriteFileTree(dir, files); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestValidDemoBundle is the fixture table's control: the unmutated demo
// bundle must produce zero problems, otherwise every "expected failure"
// below is suspect.
func TestValidDemoBundle(t *testing.T) {
	dir := writeBundleDir(t, validDemoBundle())
	problems, _ := bundleProblems(dir)
	if len(problems) != 0 {
		for _, p := range problems {
			t.Errorf("%s", p)
		}
	}
}

// TestMalformedBundleFixtures proves the bundle validation FAILS on the
// classic authoring mistakes: unknown kind, dangling on_fail target,
// duplicate id, missing agent file, non-parseable output schema, a
// non-boolean interactive flag, an orphan agent file, a malformed
// model-candidate element, and a duplicate model-candidate entry.
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
		{
			// The one candidate-element negative fixture: an unqualified
			// model id (no provider segment) must fail the reference-bundle
			// convention gate. The product parser accepts it (V9 checks only
			// non-empty strings; the element encoding is an open item, 01
			// §12 item 4) — this test is the gate.
			name: "malformed model candidate element",
			mutate: func(f map[string]string) {
				f["workflow.md"] = strings.Replace(f["workflow.md"],
					"budget: { requests: 10, tokens: 1000, wall_minutes: 5 }",
					"defaults:\n  model_candidates: [sub2api-glm/glm-5.3, glm-5.3]\nbudget: { requests: 10, tokens: 1000, wall_minutes: 5 }", 1)
			},
			want: "provider-qualified",
		},
		{
			// Companion to the element gate: a duplicate entry is a
			// pointless candidate under V9's bounded in-order switch.
			name: "duplicate model candidate",
			mutate: func(f map[string]string) {
				f["workflow.md"] = strings.Replace(f["workflow.md"],
					"budget: { requests: 10, tokens: 1000, wall_minutes: 5 }",
					"defaults:\n  model_candidates: [sub2api-glm/glm-5.3, sub2api-glm/glm-5.3]\nbudget: { requests: 10, tokens: 1000, wall_minutes: 5 }", 1)
			},
			want: "duplicate element",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := validDemoBundle()
			tc.mutate(files)
			dir := writeBundleDir(t, files)
			problems, _ := bundleProblems(dir)
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

// mkAgent builds a structurally valid step skill (frontmatter + body) around
// one output schema, for the negative table below: ValidateStepSkill would
// accept every one of these — only outputSchemaProblems catches the contract
// violations.
func mkAgent(name, outputYAML string) string {
	return "---\nname: " + name + "\ndescription: synthetic contract fixture\ntools: [read]\noutput:\n" +
		outputYAML + "\n---\n# " + name + "\n"
}

// gateOutput is the contract-conforming gate schema the negative table mutates:
// verdict [pass, fail] with the two-branch oneOf split.
const gateOutput = `  type: object
  required: [verdict]
  properties:
    verdict: { type: string, enum: [pass, fail] }
    fail_reason: { type: string }
  oneOf:
    - properties:
        verdict: { type: string, enum: [pass] }
      required: [verdict]
    - properties:
        verdict: { type: string, enum: [fail] }
        fail_reason: { type: string, minLength: 1 }
      required: [verdict, fail_reason]`

// TestOutputSchemaContract is the positive control for the contract checker:
// the canonical gate shape above must produce zero problems, otherwise every
// expected-failure below is suspect.
func TestOutputSchemaContract(t *testing.T) {
	if probs := outputSchemaProblems("some-gate", []byte(mkAgent("some-gate", gateOutput))); len(probs) != 0 {
		for _, p := range probs {
			t.Errorf("%s", p)
		}
	}
}

// TestOutputSchemaProblemsNegatives proves the output-schema contract checks
// fire on the classic authoring mistakes: a verdict gate without the oneOf
// split, a fail branch that does not require (a non-empty) fail_reason, a
// required array without minItems, the release pass branch that still admits
// an unfilled template, a pass verdict that admits contradictory evidence
// (failing check / blocking finding), a fail verdict that admits an all-pass
// checks array (the fix-round-2 contradiction), a missing authored_titles
// echo, a ready_for_spec that still admits false, and evidence items without
// detail.
// These schemas all PASS ValidateStepSkill — the contract checker is the only
// layer that rejects them, matching the enforcement reality (no machine
// JSON-Schema validator exists in the execution path).
func TestOutputSchemaProblemsNegatives(t *testing.T) {
	cases := []struct {
		name   string
		stepID string
		output string
		want   string // required substring of at least one reported problem
	}{
		{
			name:   "verdict gate without oneOf split",
			stepID: "verification",
			output: "  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }",
			want:   "oneOf",
		},
		{
			name:   "fail branch missing fail_reason",
			stepID: "verification",
			output: "  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n      required: [verdict]",
			want:   "fail branch must require fail_reason",
		},
		{
			name:   "fail branch fail_reason without minLength",
			stepID: "verification",
			output: "  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string }\n      required: [verdict, fail_reason]",
			want:   "minLength",
		},
		{
			name:   "required array without minItems",
			stepID: "code",
			output: "  type: object\n  required: [commits]\n  properties:\n    commits:\n      type: array\n      items: { type: string }",
			want:   "minItems",
		},
		{
			name:   "release pass branch without artifacts_published",
			stepID: "release",
			output: "  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    version: { type: string, minLength: 1 }\n    artifacts_published:\n      type: array\n      items: { type: string }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n        version: { type: string, minLength: 1 }\n      required: [verdict, version]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n      required: [verdict, fail_reason]",
			want:   "artifacts_published",
		},
		{
			name:   "release pass branch artifacts_published without minItems",
			stepID: "release",
			output: "  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    version: { type: string, minLength: 1 }\n    artifacts_published:\n      type: array\n      items: { type: string }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n        version: { type: string, minLength: 1 }\n        artifacts_published:\n          type: array\n          items: { type: string }\n      required: [verdict, version, artifacts_published]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n      required: [verdict, fail_reason]",
			want:   "minItems",
		},
		{
			name:   "verification pass branch without status narrowing",
			stepID: "verification",
			output: "  type: object\n  required: [verdict, checks]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    checks:\n      type: array\n      minItems: 1\n      items: { type: object, required: [name, status] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n      required: [verdict, fail_reason]",
			want:   "restrict check status",
		},
		{
			name:   "verification pass branch admits a failing check",
			stepID: "verification",
			output: "  type: object\n  required: [verdict, checks]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    checks:\n      type: array\n      minItems: 1\n      items: { type: object, required: [name, status] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n        checks:\n          type: array\n          items:\n            type: object\n            properties:\n              status: { type: string, enum: [pass, fail, skipped] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n      required: [verdict, fail_reason]",
			want:   "self-contradictory",
		},
		{
			name:   "verification without checks required",
			stepID: "verification",
			output: "  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    checks:\n      type: array\n      minItems: 1\n      items: { type: object, required: [name, status] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n      required: [verdict, fail_reason]",
			want:   "require checks",
		},
		{
			name:   "verification fail branch without a contains-failing-check requirement",
			stepID: "verification",
			output: "  type: object\n  required: [verdict, checks]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    checks:\n      type: array\n      minItems: 1\n      items: { type: object, required: [name, status] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n      required: [verdict, fail_reason]",
			want:   "contain a failing check",
		},
		{
			name:   "verification fail branch contains admits non-fail status",
			stepID: "verification",
			output: "  type: object\n  required: [verdict, checks]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    checks:\n      type: array\n      minItems: 1\n      items: { type: object, required: [name, status] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n        checks:\n          type: array\n          minItems: 1\n          contains:\n            type: object\n            required: [status]\n            properties:\n              status: { type: string, enum: [pass, fail, skipped] }\n      required: [verdict, fail_reason]",
			want:   "contain a failing check",
		},
		{
			name:   "verification fail branch contains does not require status",
			stepID: "verification",
			output: "  type: object\n  required: [verdict, checks]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    checks:\n      type: array\n      minItems: 1\n      items: { type: object, required: [name, status] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n        checks:\n          type: array\n          minItems: 1\n          contains:\n            type: object\n            properties:\n              status: { type: string, enum: [fail] }\n      required: [verdict, fail_reason]",
			want:   "must require status",
		},
		{
			name:   "review pass branch without severity narrowing",
			stepID: "review",
			output: "  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    findings:\n      type: array\n      items:\n        type: object\n        required: [severity, detail]\n        properties:\n          severity: { type: string, enum: [blocking, non-blocking] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n      required: [verdict, fail_reason]",
			want:   "restrict finding severity",
		},
		{
			name:   "review pass branch admits a blocking finding",
			stepID: "review",
			output: "  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    findings:\n      type: array\n      items:\n        type: object\n        required: [severity, detail]\n        properties:\n          severity: { type: string, enum: [blocking, non-blocking] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n        findings:\n          type: array\n          items:\n            type: object\n            properties:\n              severity: { type: string, enum: [blocking, non-blocking] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n      required: [verdict, fail_reason]",
			want:   "self-contradictory",
		},
		{
			name:   "review fail branch without a contains-blocking requirement",
			stepID: "review",
			output: "  type: object\n  required: [verdict]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    findings:\n      type: array\n      items:\n        type: object\n        required: [severity, detail]\n        properties:\n          severity: { type: string, enum: [blocking, non-blocking] }\n  oneOf:\n    - properties:\n        verdict: { type: string, enum: [pass] }\n        findings:\n          type: array\n          items:\n            type: object\n            properties:\n              severity: { type: string, enum: [non-blocking] }\n      required: [verdict]\n    - properties:\n        verdict: { type: string, enum: [fail] }\n        fail_reason: { type: string, minLength: 1 }\n      required: [verdict, fail_reason]",
			want:   "contains",
		},
		{
			name:   "e2e-run without authored_titles",
			stepID: "e2e-run",
			output: "  type: object\n  required: [results_path, scenarios]\n  properties:\n    results_path: { type: string }\n    scenarios:\n      type: array\n      minItems: 1\n      items: { type: object, required: [title, status] }",
			want:   "authored_titles",
		},
		{
			name:   "e2e-run authored_titles without uniqueItems",
			stepID: "e2e-run",
			output: "  type: object\n  required: [results_path, scenarios, authored_titles]\n  properties:\n    results_path: { type: string }\n    authored_titles:\n      type: array\n      minItems: 1\n      items: { type: string }\n    scenarios:\n      type: array\n      minItems: 1\n      items: { type: object, required: [title, status] }",
			want:   "uniqueItems",
		},
		{
			name:   "e2e-author without scenario_titles required",
			stepID: "e2e-author",
			output: "  type: object\n  required: [scenarios_path]\n  properties:\n    scenarios_path: { type: string }\n    scenario_titles:\n      type: array\n      minItems: 1\n      uniqueItems: true\n      items: { type: string }",
			want:   "scenario_titles",
		},
		{
			name:   "e2e-author scenario_titles without uniqueItems",
			stepID: "e2e-author",
			output: "  type: object\n  required: [scenarios_path, scenario_titles]\n  properties:\n    scenarios_path: { type: string }\n    scenario_titles:\n      type: array\n      minItems: 1\n      items: { type: string }",
			want:   "uniqueItems",
		},
		{
			name:   "ready_for_spec still admits false",
			stepID: "grill",
			output: "  type: object\n  required: [qa_log, ready_for_spec]\n  properties:\n    qa_log: { type: string }\n    ready_for_spec: { type: boolean, enum: [true, false] }",
			want:   "enum: [true]",
		},
		{
			name:   "evidence items without required detail",
			stepID: "root-cause",
			output: "  type: object\n  required: [verdict, root_cause, evidence]\n  properties:\n    verdict: { type: string, enum: [pass, fail] }\n    root_cause: { type: string }\n    evidence:\n      type: array\n      minItems: 1\n      items:\n        type: object\n        required: [kind, ref]\n        properties:\n          kind: { type: string }\n          ref: { type: string }\n          detail: { type: string }",
			want:   "detail",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probs := outputSchemaProblems(tc.stepID, []byte(mkAgent(tc.stepID, tc.output)))
			if len(probs) == 0 {
				t.Fatalf("expected a contract problem containing %q, got none", tc.want)
			}
			for _, p := range probs {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("no reported problem contains %q; problems: %v", tc.want, probs)
		})
	}
}
