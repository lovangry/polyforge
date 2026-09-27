// Package schema implements the workflow.md frontmatter structure validator
// (01-workflow-skills.md §4.1, rule set WF-201 with rule ids V1–V10) and the
// step skill frontmatter validation library (§4.4). The AIHub WI-creation
// transaction calls ValidateWorkflowFrontmatter + the agents existence check;
// step skill content validation is NOT called by the creation transaction
// (02 §4.5) — it serves bundle tests and execution-side warnings.
package schema

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Budget is the three-scalar budget declaration (01 §4.1/§6.6). nil pointer =
// no upper bound for that dimension. All three scalar values flow into
// work_items.workflow_budget_baseline at WI creation (02 §4.4).
type Budget struct {
	Requests    *int64 `yaml:"requests"`
	Tokens      *int64 `yaml:"tokens"`
	WallMinutes *int64 `yaml:"wall_minutes"`
}

// OnFail is the bounded rewind declaration, allowed only on gate steps (V5).
type OnFail struct {
	Action         string  `yaml:"action"` // "rewind" | "terminate"; default terminate
	Target         string  `yaml:"target"` // required when action=rewind
	IncludeVerdict *bool   `yaml:"include_verdict"` // default true
}

// Step is one frontmatter step entry.
type Step struct {
	ID              string   `yaml:"id"`
	Kind            string   `yaml:"kind"`
	Interactive     *bool    `yaml:"interactive"` // default false
	Gate            *bool    `yaml:"gate"`        // default false
	OnFail          *OnFail  `yaml:"on_fail"`
	Budget          *Budget  `yaml:"budget"`
	ModelCandidates []string `yaml:"model_candidates"`
}

// Rewind holds the workflow-level rewind bounds (defaults 4 / 2, V6).
type Rewind struct {
	MaxTotal    *int64 `yaml:"max_total"`
	MaxPerStep  *int64 `yaml:"max_per_step"`
}

// Defaults carries the workflow-level fallback model candidates (V9).
type Defaults struct {
	ModelCandidates []string `yaml:"model_candidates"`
}

// Frontmatter is the parsed workflow.md frontmatter (structure only; the
// Markdown body is never parsed or validated by AIHub, 02 §4.5 step 2).
type Frontmatter struct {
	Name        string    `yaml:"name"`
	Description *string   `yaml:"description"`
	Budget      *Budget   `yaml:"budget"`
	Rewind      *Rewind   `yaml:"rewind"`
	Defaults    *Defaults `yaml:"defaults"`
	Steps       []Step    `yaml:"steps"`
}

// RuleError is one failed validation rule, reported back through the HTTP
// face as CORE-400 with detail.rule_id (02 §4.5).
type RuleError struct {
	RuleID string
	Msg    string
}

func (e *RuleError) Error() string { return fmt.Sprintf("WF-201/%s: %s", e.RuleID, e.Msg) }

func ruleErr(rule, format string, args ...any) *RuleError {
	return &RuleError{RuleID: rule, Msg: fmt.Sprintf(format, args...)}
}

var (
	nameRegex    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
	stepIDRegex  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	allowedKinds = map[string]bool{
		"qa": true, "spec": true, "author": true, "code": true, "gate": true, "review": true,
	}
	standardYAMLTags = map[string]bool{
		"!!map": true, "!!seq": true, "!!str": true, "!!int": true,
		"!!bool": true, "!!null": true, "!!float": true,
	}
)

var allowedTopLevelKeys = map[string]bool{
	"name": true, "description": true, "budget": true, "rewind": true,
	"defaults": true, "steps": true,
}
var allowedStepKeys = map[string]bool{
	"id": true, "kind": true, "interactive": true, "gate": true,
	"on_fail": true, "budget": true, "model_candidates": true,
}
var allowedOnFailKeys = map[string]bool{"action": true, "target": true, "include_verdict": true}
var allowedBudgetKeys = map[string]bool{"requests": true, "tokens": true, "wall_minutes": true}
var allowedRewindKeys = map[string]bool{"max_total": true, "max_per_step": true}
var allowedDefaultsKeys = map[string]bool{"model_candidates": true}

// SplitFrontmatter extracts the YAML frontmatter block from workflow.md bytes
// and returns (yamlBytes, body, ok). The file must begin with a `---` line.
func SplitFrontmatter(content []byte) (front []byte, body []byte, ok bool) {
	s := string(content)
	nl := strings.IndexByte(s, '\n')
	if nl < 0 || strings.TrimRight(s[:nl], " \t\r") != "---" {
		return nil, nil, false
	}
	rest := s[nl+1:]
	end := 0
	for {
		idx := strings.IndexByte(rest[end:], '\n')
		if idx < 0 {
			return nil, nil, false
		}
		line := strings.TrimRight(rest[end:end+idx], " \t\r")
		if line == "---" {
			return []byte(rest[:end+idx+1]), []byte(rest[end+idx+1:]), true
		}
		if !strings.HasPrefix(line, "#") && strings.TrimSpace(line) != "" && !strings.ContainsAny(line, ":") {
			return nil, nil, false
		}
		end += idx + 1
	}
}

// ValidateWorkflowFrontmatter parses and validates the frontmatter of the
// workflow.md inside bundleDir against the full V rule set. bundleName is the
// bundle directory name (V2: name must equal it). On success it returns the
// parsed Frontmatter; on failure a *RuleError carrying the first hit rule id.
//
// Per 02 §4.5 the creation transaction also requires agents/<id>.md existence
// (V7): call CheckAgentsExist with the returned Frontmatter.
func ValidateWorkflowFrontmatter(bundleDir, bundleName string) (*Frontmatter, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, "workflow.md"))
	if err != nil {
		return nil, ruleErr("V1", "workflow.md unreadable: %v", err)
	}
	front, _, ok := SplitFrontmatter(raw)
	if !ok {
		return nil, ruleErr("V1", "workflow.md must start with a --- delimited frontmatter block")
	}

	var node yaml.Node
	if err := yaml.Unmarshal(front, &node); err != nil {
		return nil, ruleErr("V1", "frontmatter is not valid YAML: %v", err)
	}
	if len(node.Content) == 0 {
		return nil, ruleErr("V1", "frontmatter is empty")
	}
	if err := walkNode(&node, "top", allowedTopLevelKeys); err != nil {
		return nil, err
	}

	var fm Frontmatter
	if err := node.Decode(&fm); err != nil {
		return nil, ruleErr("V1", "frontmatter does not match the data schema: %v", err)
	}

	// V2: naming, size, uniqueness, bundle-dir equality.
	if !nameRegex.MatchString(fm.Name) {
		return nil, ruleErr("V2", "name %q violates ^[a-z][a-z0-9-]{1,63}$", fm.Name)
	}
	if fm.Name != bundleName {
		return nil, ruleErr("V2", "name %q must equal bundle directory name %q", fm.Name, bundleName)
	}
	if len(fm.Steps) < 1 || len(fm.Steps) > 32 {
		return nil, ruleErr("V2", "steps must have 1..32 entries, got %d", len(fm.Steps))
	}
	seen := map[string]int{}
	for i, st := range fm.Steps {
		if !stepIDRegex.MatchString(st.ID) {
			return nil, ruleErr("V2", "step id %q violates ^[a-z][a-z0-9-]{0,63}$", st.ID)
		}
		if prev, dup := seen[st.ID]; dup {
			return nil, ruleErr("V2", "duplicate step id %q (positions %d and %d)", st.ID, prev, i)
		}
		seen[st.ID] = i
	}

	if fm.Description != nil && len(*fm.Description) > 500 {
		return nil, ruleErr("V1", "description exceeds 500 characters")
	}

	// V6: budget / rewind values must be integers > 0 (pointer nil = no bound).
	if err := validateBudget("workflow", fm.Budget); err != nil {
		return nil, err
	}
	if fm.Rewind != nil {
		if fm.Rewind.MaxTotal != nil && *fm.Rewind.MaxTotal <= 0 {
			return nil, ruleErr("V6", "rewind.max_total must be > 0")
		}
		if fm.Rewind.MaxPerStep != nil && *fm.Rewind.MaxPerStep <= 0 {
			return nil, ruleErr("V6", "rewind.max_per_step must be > 0")
		}
	}

	// V9: model candidates are non-empty string lists.
	if fm.Defaults != nil {
		if err := validateModelCandidates("defaults", fm.Defaults.ModelCandidates); err != nil {
			return nil, err
		}
	}

	// Per-step rules.
	gate := func(st Step) bool { return st.Gate != nil && *st.Gate }
	firstSpec := -1
	for i, st := range fm.Steps {
		// V8
		if !allowedKinds[st.Kind] {
			return nil, ruleErr("V8", "step %q kind %q outside {qa|spec|author|code|gate|review}", st.ID, st.Kind)
		}
		// V6 step budget
		if err := validateBudget("step "+st.ID, st.Budget); err != nil {
			return nil, err
		}
		// V9 step-level candidates (整体替换 defaults，非合并)
		if err := validateModelCandidates("step "+st.ID, st.ModelCandidates); err != nil {
			return nil, err
		}
		// V5: on_fail only on gate steps; target exists and is strictly earlier.
		if st.OnFail != nil {
			if !gate(st) {
				return nil, ruleErr("V5", "step %q carries on_fail but is not gate: true", st.ID)
			}
			action := st.OnFail.Action
			if action == "" {
				action = "terminate"
			}
			if action != "rewind" && action != "terminate" {
				return nil, ruleErr("V5", "step %q on_fail.action %q outside {rewind|terminate}", st.ID, action)
			}
			if action == "rewind" {
				pos, exists := seen[st.OnFail.Target]
				if !exists {
					return nil, ruleErr("V5", "step %q on_fail.target %q does not exist", st.ID, st.OnFail.Target)
				}
				if pos >= i {
					return nil, ruleErr("V5", "step %q on_fail.target %q is not strictly earlier in the steps array", st.ID, st.OnFail.Target)
				}
			}
		}
		if st.Kind == "spec" && firstSpec < 0 {
			firstSpec = i
		}
	}

	// V10: non-gate qa steps (spec-前追问, e.g. grill) require a spec step and
	// must be strictly earlier than all spec steps.
	for i, st := range fm.Steps {
		if st.Kind == "qa" && !gate(st) {
			if firstSpec < 0 {
				return nil, ruleErr("V10", "non-gate qa step %q requires at least one kind: spec step", st.ID)
			}
			if i > firstSpec {
				return nil, ruleErr("V10", "non-gate qa step %q must precede all kind: spec steps", st.ID)
			}
		}
	}

	return &fm, nil
}

// CheckAgentsExist verifies V7: every step has agents/<id>.md in the bundle.
// Content is neither parsed nor validated here (02 §4.4/§4.5).
func CheckAgentsExist(bundleDir string, fm *Frontmatter) error {
	for _, st := range fm.Steps {
		p := filepath.Join(bundleDir, "agents", st.ID+".md")
		if _, err := os.Stat(p); err != nil {
			return ruleErr("V7", "agents/%s.md missing in bundle", st.ID)
		}
	}
	return nil
}

// walkNode enforces V1: pure-data YAML (no script/tag nodes) and the closed
// key sets at every mapping level. ctx selects the nesting context ("top",
// "step", "skill").
func walkNode(n *yaml.Node, ctx string, allowedKeys map[string]bool) error {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			if err := walkNode(c, ctx, allowedKeys); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		if !standardYAMLTags[n.Tag] {
			return ruleErr("V1", "non-data YAML tag %q rejected", n.Tag)
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if !standardYAMLTags[k.Tag] {
				return ruleErr("V1", "non-data YAML tag %q on key rejected", k.Tag)
			}
			if allowedKeys != nil && !allowedKeys[k.Value] {
				return ruleErr("V1", "unknown key %q rejected (closed schema)", k.Value)
			}
			childCtx, childKeys := ctx, allowedKeys
			if ctx == "skill" && k.Value == "output" {
				// output is an arbitrary JSON Schema object: open key set, tags still checked.
				childCtx, childKeys = "open", nil
			}
			switch k.Value {
			case "on_fail":
				childCtx, childKeys = "on_fail", allowedOnFailKeys
			case "budget":
				childCtx, childKeys = "budget", allowedBudgetKeys
			case "rewind":
				childCtx, childKeys = "rewind", allowedRewindKeys
			case "defaults":
				childCtx, childKeys = "defaults", allowedDefaultsKeys
			case "steps":
				childCtx, childKeys = "steps", allowedStepKeys
			}
			if err := walkNode(v, childCtx, childKeys); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if !standardYAMLTags[n.Tag] {
			return ruleErr("V1", "non-data YAML tag %q rejected", n.Tag)
		}
		for _, c := range n.Content {
			// steps sequence elements are mappings with the step key set;
			// other sequences (model_candidates) carry scalars and pass
			// their own context through unchanged.
			if err := walkNode(c, ctx, allowedKeys); err != nil {
				return err
			}
		}
	default:
		if !standardYAMLTags[n.Tag] {
			return ruleErr("V1", "non-data YAML tag %q rejected (script-like node)", n.Tag)
		}
	}
	return nil
}

func validateBudget(where string, b *Budget) error {
	if b == nil {
		return nil
	}
	check := func(name string, v *int64) error {
		if v != nil && *v <= 0 {
			return ruleErr("V6", "%s budget.%s must be > 0", where, name)
		}
		return nil
	}
	if err := check("requests", b.Requests); err != nil {
		return err
	}
	if err := check("tokens", b.Tokens); err != nil {
		return err
	}
	return check("wall_minutes", b.WallMinutes)
}

func validateModelCandidates(where string, cands []string) error {
	if cands == nil {
		return nil
	}
	if len(cands) == 0 {
		return ruleErr("V9", "%s model_candidates must be a non-empty ordered list when present", where)
	}
	for _, c := range cands {
		if strings.TrimSpace(c) == "" {
			return ruleErr("V9", "%s model_candidates contains an empty entry", where)
		}
	}
	return nil
}
