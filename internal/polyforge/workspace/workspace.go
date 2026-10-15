// Package workspace implements the workspace-level model-candidates
// file — `.polyforge.yaml` at a workspace root — as ONE strict-schema
// loader shared by every consumer: the daemon's pre-claim read and the
// candidate-precedence compose. There is no second parser and no second
// strictness implementation.
//
// The grammar is closed:
//
//	models:                          # optional section
//	  <project-name>:                # one entry per project of this workspace
//	    default: ["id", ...]         # optional: every step of this project
//	    <step-id>: ["id", ...]       # optional: the steps with this id, in
//	                                 # every bundle (fix, code, review, ...)
//
// Model ids are non-empty strings. Any key not shown here — at any
// level — is a load error, as are duplicate keys, type mismatches, an
// empty project entry, empty model-id strings and a per-step key that is
// no step id of the installed workflows (a typo would otherwise silently
// govern nothing). The
// retired `projects:` and `workflow_defaults:` sections are therefore
// unknown keys like any other: an old-shape file fails loudly.
//
// Presence semantics: an ABSENT file and an EMPTY or comments-only file
// both answer (nil, nil) — absent-equivalent, never an error. A file
// that declares anything must parse strictly.
//
// List presence: a declared-but-empty list is a PRESENT level that
// wholly governs (yielding no switchable chain rather than a
// fall-through). Absent keys fall through per the precedence chain
// (compose.go), five levels, highest first: manual `pf submit --models`
// (per work item, every step) > models.<project>.<step-id> > bundle step
// model_candidates > models.<project>.default > bundle
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
	// Models is project name → (DefaultKey or a step id → candidate list).
	Models map[string]map[string][]string `yaml:"models"`
}

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
// (workflow.StepIDs); "" skips that one check (no workflows to judge by).
func Load(workspaceRoot, workflowsDir string) (*Config, error) {
	path := PathFor(workspaceRoot)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &Error{Path: path, Reason: fmt.Sprintf("unreadable: %v", err)}
	}
	return parse(path, raw, workflowsDir)
}

// parse strictly decodes and validates one yaml document. Split from
// Load so tests can feed bytes directly.
func parse(path string, raw []byte, workflowsDir string) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // any key outside the closed vocabulary is an error
	if err := dec.Decode(&cfg); err != nil {
		if err.Error() == "EOF" {
			// An empty (or comments-only) document: absent-equivalent.
			return nil, nil
		}
		return nil, &Error{Path: path, Reason: strings.ReplaceAll(err.Error(), "\n", "; ")}
	}
	if cfg.Models == nil {
		return nil, nil // a file declaring nothing governs nothing
	}
	var steps map[string]bool
	if workflowsDir != "" {
		var err error
		if steps, err = workflow.StepIDs(workflowsDir); err != nil {
			return nil, &Error{Path: path, Reason: fmt.Sprintf("the installed workflows are unreadable, so the step-id keys cannot be checked: %v", err)}
		}
	}
	if err := validate(path, &cfg, steps, workflowsDir); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate enforces the content rules the yaml decoder cannot express:
// non-empty model ids and the per-project key set (DefaultKey or a step
// id in steps; every unknown key is named, sorted — or, when the
// workflows dir holds no readable bundle at all, that fact).
func validate(path string, cfg *Config, steps map[string]bool, workflowsDir string) error {
	var unknown []string
	for project, keys := range cfg.Models {
		if len(keys) == 0 {
			return &Error{Path: path, Reason: fmt.Sprintf("models.%s: entry is empty — declare %s or a step-id list, or remove it", project, DefaultKey)}
		}
		for key, list := range keys {
			if key != DefaultKey && steps != nil && !steps[key] {
				unknown = append(unknown, fmt.Sprintf("models.%s.%s", project, key))
			}
			if list == nil {
				return &Error{Path: path, Reason: fmt.Sprintf("models.%s.%s: must be a list of model ids", project, key)}
			}
			for i, id := range list {
				if strings.TrimSpace(id) == "" {
					return &Error{Path: path, Reason: fmt.Sprintf("models.%s.%s[%d]: model ids must be non-empty strings", project, key, i)}
				}
			}
		}
	}
	if len(unknown) > 0 && len(steps) == 0 {
		return &Error{Path: path, Reason: fmt.Sprintf("no workflows found in %s, so no per-step key can be checked", workflowsDir)}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return &Error{Path: path, Reason: fmt.Sprintf("unknown key(s) %s: a key is %q or a step id of the installed workflows", strings.Join(unknown, ", "), DefaultKey)}
	}
	return nil
}
