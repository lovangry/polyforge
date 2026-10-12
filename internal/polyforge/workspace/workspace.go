// Package workspace implements the workspace-level workflow defaults
// file — `.polyforge.yaml` at a workspace root (polyforge#79) — as ONE
// strict-schema loader shared by every consumer: the daemon's claim-time
// read, the candidate-precedence compose, and the `pf file create`
// project inference. There is no second parser and no second
// strictness implementation.
//
// The grammar is closed (the exact key vocabulary of spec.md's
// "invalid or schema-violating" requirement):
//
//	projects:                        # optional section
//	  <project-name>:
//	    repos:                       # REQUIRED on every project entry (may be [])
//	      - name: polyforge          # single safe path element → repos/<name>
//	        path: .repo/polyforge    # workspace-root-relative; never absolute; no ../ escape
//	workflow_defaults:               # optional section
//	  model_candidates: ["id", ...]  # non-empty model-id strings only
//	  step_kinds:                    # keys ONLY from the V8 kind vocabulary
//	    code: ["id", ...]            # {qa, spec, author, code, gate, review}
//
// Any key not shown here — at any level — is a load error, as are
// duplicate keys, type mismatches, a project entry without `repos:`,
// a repo entry without `name:`/`path:` or with an unsafe name or an
// absolute/parent-escaping `path`, empty model-id strings, and
// `step_kinds` keys outside the V8 set. A `release:` section (retired,
// never consumed) is therefore an unknown key like any other.
//
// Presence semantics: an ABSENT file and an EMPTY or comments-only
// file both answer (nil, nil) — absent-equivalent, never an error
// (the workspace simply carries no yaml layer and every consumer keeps
// today's behavior). A file that declares anything must parse strictly.
//
// List presence: a declared-but-empty list is a PRESENT level that
// wholly governs (the `repos: []` precedent — "the empty list is the
// authoritative inventory"; `model_candidates: []` yields no
// switchable chain rather than a fall-through). Absent keys fall
// through per the precedence chain (compose.go).
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the workspace-level defaults file every consumer reads.
const FileName = ".polyforge.yaml"

// KindVocabulary is the closed V8 step-kind set (the same vocabulary
// bundle validation pins): the only legal `step_kinds` keys.
var KindVocabulary = []string{"qa", "spec", "author", "code", "gate", "review"}

func validKind(k string) bool {
	for _, v := range KindVocabulary {
		if k == v {
			return true
		}
	}
	return false
}

// RepoEntry is one yaml repo entry: `name` (single safe path element —
// the clone lands at repos/<name>) and `path` (the local checkout,
// workspace-root-relative).
type RepoEntry struct {
	Name string `yaml:"name" json:"name"`
	Path string `yaml:"path" json:"path"`
}

// ProjectEntry is one project's entry: its repo inventory. `repos` is
// REQUIRED (possibly empty — the empty list wholly governs).
type ProjectEntry struct {
	Repos []RepoEntry `yaml:"repos" json:"repos"`
}

// WorkflowDefaults is the workflow-level defaults section.
type WorkflowDefaults struct {
	// ModelCandidates is the flow-default candidate list (present when
	// non-nil, even when empty).
	ModelCandidates []string `yaml:"model_candidates" json:"model_candidates,omitempty"`
	// StepKinds is the per-kind candidate override, keyed by the V8
	// kind vocabulary (present entries wholly govern their kind).
	StepKinds map[string][]string `yaml:"step_kinds" json:"step_kinds,omitempty"`
}

// Config is the parsed `.polyforge.yaml`.
type Config struct {
	// Projects is the repo-source section (present when non-nil): the
	// projects it lists use these entries INSTEAD of the aihub
	// registry inventory.
	Projects map[string]*ProjectEntry `yaml:"projects" json:"projects,omitempty"`
	// WorkflowDefaults is the model-candidates section (present when
	// non-nil).
	WorkflowDefaults *WorkflowDefaults `yaml:"workflow_defaults" json:"workflow_defaults,omitempty"`
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
func Load(workspaceRoot string) (*Config, error) {
	path := PathFor(workspaceRoot)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &Error{Path: path, Reason: fmt.Sprintf("unreadable: %v", err)}
	}
	return parse(path, raw)
}

// parse strictly decodes and validates one yaml document. Split from
// Load so tests (and the inference seam) can feed bytes directly.
func parse(path string, raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // any key outside the closed vocabulary is an error
	if err := dec.Decode(&cfg); err != nil {
		if err.Error() == "EOF" {
			// An empty (or comments-only) document: absent-equivalent.
			return nil, nil
		}
		return nil, &Error{Path: path, Reason: sumReason(err)}
	}
	if cfg.Projects == nil && cfg.WorkflowDefaults == nil {
		return nil, nil // a file declaring nothing governs nothing
	}
	if err := validate(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// sumReason flattens the yaml decoder's error (possibly a list of
// problems, one per line) into the reason string; duplicate-key and
// unknown-field errors name their key.
func sumReason(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", "; ")
}

// validate enforces the content rules the yaml decoder cannot express:
// required fields, safe names, workspace-root-relative paths, non-empty
// model ids, and the closed step_kinds key set.
func validate(path string, cfg *Config) error {
	for name, proj := range cfg.Projects {
		if proj == nil {
			return &Error{Path: path, Reason: fmt.Sprintf("project %q: entry is empty — repos is required (declare it, possibly as [])", name)}
		}
		if proj.Repos == nil {
			return &Error{Path: path, Reason: fmt.Sprintf("project %q: repos is required (declare it, possibly as [])", name)}
		}
		seen := map[string]bool{}
		for _, re := range proj.Repos {
			if !safeRepoName(re.Name) {
				return &Error{Path: path, Reason: fmt.Sprintf("project %q: repo name %q is not a single safe path element", name, re.Name)}
			}
			if seen[re.Name] {
				return &Error{Path: path, Reason: fmt.Sprintf("project %q: repo %q is listed twice — remove the duplicate entry", name, re.Name)}
			}
			seen[re.Name] = true
			if err := checkRelCheckoutPath(re.Path); err != nil {
				return &Error{Path: path, Reason: fmt.Sprintf("project %q: repo %q: %v", name, re.Name, err)}
			}
		}
	}
	wd := cfg.WorkflowDefaults
	if wd == nil {
		return nil
	}
	for i, id := range wd.ModelCandidates {
		if strings.TrimSpace(id) == "" {
			return &Error{Path: path, Reason: fmt.Sprintf("workflow_defaults.model_candidates[%d]: model ids must be non-empty strings", i)}
		}
	}
	for kind, list := range wd.StepKinds {
		if !validKind(kind) {
			return &Error{Path: path, Reason: fmt.Sprintf("step_kinds key %q is outside the workflow kind vocabulary (%s)", kind, strings.Join(KindVocabulary, ", "))}
		}
		for i, id := range list {
			if strings.TrimSpace(id) == "" {
				return &Error{Path: path, Reason: fmt.Sprintf("workflow_defaults.step_kinds.%s[%d]: model ids must be non-empty strings", kind, i)}
			}
		}
	}
	return nil
}

// safeRepoName keeps a yaml repo name inside repos/: one path element,
// no separators, no dot-escape, no control bytes (the same rule the
// task-repo inventory applies to registry-served names).
func safeRepoName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

// checkRelCheckoutPath enforces the local-checkout path rules: non-empty,
// workspace-root-relative (never absolute), no parent escape.
func checkRelCheckoutPath(rel string) error {
	if strings.TrimSpace(rel) == "" {
		return fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("path %q must be workspace-root-relative, never absolute", rel)
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return fmt.Errorf("path %q escapes the workspace root (..)", rel)
		}
	}
	return nil
}

// ResolveRepoSource answers the clone source of one yaml repo entry:
// the workspace root joined with the entry's workspace-root-relative
// `path`. This IS the string handed to the existing `git clone` seam.
func ResolveRepoSource(workspaceRoot string, re RepoEntry) string {
	return filepath.Join(workspaceRoot, filepath.FromSlash(re.Path))
}
