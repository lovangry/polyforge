// Package workspace implements the workspace-level per-step settings
// file — `.polyforge.yaml` at a workspace root — as ONE strict-schema
// loader shared by every consumer: the daemon's pre-claim read, pf doctor
// and the candidate-precedence compose. There is no second parser and no
// second strictness implementation.
//
// The grammar is closed:
//
//	steps:                           # optional section
//	  <project-name>:                # one entry per project of this workspace
//	    default: { models: [...] }   # optional: every step of this project
//	    <step-id>: { models: [...] } # optional: the steps with this id, in
//	                                 # every bundle (fix, code, review, ...)
//
// A step entry's only key is `models` (a list of non-empty model ids).
// Any key not shown here — at any level — is a load error, as are
// duplicate keys, type mismatches, an empty project entry, empty model-id
// strings and a per-step key that is no step id of the installed
// workflows (a typo would otherwise silently govern nothing). The retired
// top-level `models:` section (the pre-#169 shape) refuses with a
// migration message; `projects:` / `workflow_defaults:` are unknown keys
// like any other.
//
// Presence semantics: an ABSENT file and an EMPTY or comments-only file
// both answer (nil, nil) — absent-equivalent, never an error. A file
// that declares anything must parse strictly.
//
// List presence: a declared-but-empty list is a PRESENT level that
// wholly governs (yielding no switchable chain rather than a
// fall-through). Absent keys fall through per the precedence chain
// (compose.go), five levels, highest first: manual `pf submit --models`
// (per work item, every step) > steps.<project>.<step-id>.models > bundle
// step model_candidates > steps.<project>.default.models > bundle
// defaults.model_candidates.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/silgrid/polyforge/internal/common/workflow"
	"gopkg.in/yaml.v3"
)

// FileName is the workspace-level defaults file every consumer reads.
const FileName = ".polyforge.yaml"

// DefaultKey is the per-project key whose list governs every step of
// the project that has neither a list of its own step id nor a bundle
// step-level model_candidates list.
const DefaultKey = "default"

// Config is the parsed `.polyforge.yaml`.
type Config struct {
	// Models is project name → (DefaultKey or a step id → candidate
	// list): steps.<project>.<key>.models, flattened for the compose.
	Models map[string]map[string][]string
}

// LegacyModelsReason is the refusal of a file still using the retired
// top-level `models:` section (no backward compatibility).
const LegacyModelsReason = "top-level `models:` was replaced by `steps:` (polyforge#169) — move models.<project>.<key>: [...] to steps.<project>.<key>.models: [...] (see docs/upgrade.md)"

// Error is the typed load failure: the file that refused and the
// reason. Every consumer surfaces it verbatim — the file name and the
// offending key/reason are the contract.
type Error struct {
	Path   string
	Reason string
}

func (e *Error) Error() string { return e.Path + ": " + e.Reason }

// PathFor answers the yaml path of one workspace root.
func PathFor(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, FileName)
}

// Load reads and strictly validates `<workspaceRoot>/.polyforge.yaml`.
// An absent file answers (nil, nil); an empty or comments-only file
// answers (nil, nil) too (absent-equivalent). Anything else parses
// strictly or returns a typed *Error naming the file and the reason.
// Every per-step key must be a step id of the bundles under workflowsDir
// (workflow.StepIDs; "" skips that one check), and every project key one
// of projects — the projects cli.toml maps to this workspace (nil skips
// that check).
func Load(workspaceRoot, workflowsDir string, projects []string) (*Config, error) {
	path := PathFor(workspaceRoot)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &Error{Path: path, Reason: fmt.Sprintf("unreadable: %v", err)}
	}
	cfg, reason := parse(raw, projects)
	if reason != "" {
		return nil, &Error{Path: path, Reason: reason}
	}
	if cfg == nil || workflowsDir == "" {
		return cfg, nil
	}
	steps, err := workflow.StepIDs(workflowsDir)
	if err != nil {
		return nil, &Error{Path: path, Reason: fmt.Sprintf("the installed workflows are unreadable, so the step-id keys cannot be checked: %v", err)}
	}
	if reason := checkStepIDs(cfg, steps, workflowsDir); reason != "" {
		return nil, &Error{Path: path, Reason: reason}
	}
	return cfg, nil
}

// parse walks one yaml document node by node (so every refusal names its
// yaml path, never a Go type) and answers the flattened config or the
// refusal reason. (nil, "") is absent-equivalent.
func parse(raw []byte, projects []string) (*Config, string) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, strings.ReplaceAll(err.Error(), "\n", "; ")
	}
	if len(doc.Content) == 0 {
		return nil, "" // empty or comments-only
	}
	root := deref(doc.Content[0])
	if isNull(root) {
		return nil, ""
	}
	if root.Kind != yaml.MappingNode {
		return nil, "the file must be a mapping with one `steps:` section"
	}
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "models" {
			return nil, LegacyModelsReason // wins over every other defect
		}
	}
	top, reason := mapping(root, "")
	if reason != "" {
		return nil, reason
	}
	for key := range top {
		if key != "steps" {
			return nil, fmt.Sprintf("unknown top-level key %q (only `steps`)", key)
		}
	}
	stepsNode := top["steps"]
	if stepsNode == nil || isNull(stepsNode) {
		return nil, "" // a file declaring nothing governs nothing
	}
	if stepsNode.Kind != yaml.MappingNode {
		return nil, "steps: must be a mapping of project → step entries"
	}
	byProject, reason := mapping(stepsNode, "steps")
	if reason != "" {
		return nil, reason
	}
	cfg := &Config{Models: make(map[string]map[string][]string, len(byProject))}
	var unmapped []string
	for project, pn := range byProject {
		if projects != nil && !contains(projects, project) {
			unmapped = append(unmapped, project)
		}
		where := "steps." + project
		if isNull(pn) || (pn.Kind == yaml.MappingNode && len(pn.Content) == 0) {
			return nil, fmt.Sprintf("%s: entry is empty — declare %s or a step id, or remove it", where, DefaultKey)
		}
		if pn.Kind != yaml.MappingNode {
			return nil, where + ": must be a mapping of step entries (`default` or a step id)"
		}
		entries, reason := mapping(pn, where)
		if reason != "" {
			return nil, reason
		}
		cfg.Models[project] = make(map[string][]string, len(entries))
		for key, en := range entries {
			list, reason := stepModels(en, where+"."+key)
			if reason != "" {
				return nil, reason
			}
			cfg.Models[project][key] = list
		}
	}
	if len(unmapped) > 0 {
		sort.Strings(unmapped)
		return nil, fmt.Sprintf("project(s) %s are not mapped to this workspace in cli.toml (it maps: %s)", strings.Join(unmapped, ", "), strings.Join(projects, ", "))
	}
	return cfg, ""
}

// stepModels reads one `{ models: [...] }` step entry at path where:
// models is the only key; its items are non-empty, unique model-id
// strings (trimmed — the same rules as a `pf submit --models` list).
func stepModels(en *yaml.Node, where string) ([]string, string) {
	en = deref(en)
	if en.Kind != yaml.MappingNode {
		hint := ""
		if en.Kind == yaml.SequenceNode {
			hint = " (the pre-#169 list form?)"
		}
		return nil, where + ": must be `{ models: [...] }`" + hint
	}
	keys, reason := mapping(en, where)
	if reason != "" {
		return nil, reason
	}
	for k := range keys {
		if k != "models" {
			return nil, fmt.Sprintf("%s.%s: not allowed (a step entry's only key is `models`)", where, k)
		}
	}
	mn := keys["models"]
	if mn == nil || mn.Kind != yaml.SequenceNode {
		return nil, where + ".models: must be a list of model ids"
	}
	list := make([]string, 0, len(mn.Content))
	seen := map[string]bool{}
	for i, item := range mn.Content {
		item = deref(item)
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
			return nil, fmt.Sprintf("%s.models[%d]: must be a model-id string", where, i)
		}
		id := strings.TrimSpace(item.Value)
		if id == "" {
			return nil, fmt.Sprintf("%s.models[%d]: model ids must be non-empty strings", where, i)
		}
		if seen[id] {
			return nil, fmt.Sprintf("%s.models[%d]: repeats %q", where, i, id)
		}
		seen[id] = true
		list = append(list, id)
	}
	return list, ""
}

// mapping answers a mapping node's key → value nodes, refusing a
// non-scalar or duplicate key (where names the mapping's yaml path).
func mapping(n *yaml.Node, where string) (map[string]*yaml.Node, string) {
	out := make(map[string]*yaml.Node, len(n.Content)/2)
	prefix := where
	if prefix != "" {
		prefix += "."
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if k.Kind != yaml.ScalarNode {
			return nil, fmt.Sprintf("%s: keys must be plain names", orTop(where))
		}
		if _, dup := out[k.Value]; dup {
			return nil, fmt.Sprintf("%s%s: duplicate key", prefix, k.Value)
		}
		out[k.Value] = deref(n.Content[i+1])
	}
	return out, ""
}

func orTop(where string) string {
	if where == "" {
		return "top level"
	}
	return where
}

// deref follows a yaml alias to its anchored node.
func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// checkStepIDs refuses a per-step key that is no step id in steps (every
// unknown key named, sorted — or, when the workflows dir holds no
// readable bundle at all, that fact).
func checkStepIDs(cfg *Config, steps map[string]bool, workflowsDir string) string {
	var unknown []string
	for project, keys := range cfg.Models {
		for key := range keys {
			if key != DefaultKey && !steps[key] {
				unknown = append(unknown, fmt.Sprintf("steps.%s.%s", project, key))
			}
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	if len(steps) == 0 {
		return fmt.Sprintf("no workflows found in %s, so no per-step key can be checked", workflowsDir)
	}
	sort.Strings(unknown)
	return fmt.Sprintf("unknown key(s) %s: a key is %q or a step id of the installed workflows", strings.Join(unknown, ", "), DefaultKey)
}
