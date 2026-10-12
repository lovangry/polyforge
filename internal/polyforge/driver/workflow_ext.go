package driver

// workflow_ext.go: the extended workflow frontmatter read surface for
// the v2 driver lane — the fields the spawn adapter (spawn.go) needs
// that the sequencing surface (driver.go's loadWorkflow) deliberately
// does not carry: context_policy (inherit|fresh), declared inputs
// (R11: the artifact paths whose content a step's prompt must carry),
// declared outputs (path + JSON schema type — the envelope's
// declared-output duty) and the handoff duty. Parsed into the
// PARALLEL DriverWorkflow type: the shared common/workflow reader
// stays the minimal F4 candidate-chain surface, and full bundle
// VALIDATION stays aihub's job at WI creation — this is a loose,
// read-only parse that fills defaults and refuses what it cannot
// honor (unknown policies, non-capsule-relative artifact paths, gate
// steps that are not fresh).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/silgrid/polyforge/internal/common/workflow"
	"gopkg.in/yaml.v3"
)

// Context policies (the frontmatter extension vocabulary).
const (
	// ContextInherit: the step continues the previous step's work —
	// its prompt carries the producing step's handoff artifact.
	ContextInherit = "inherit"
	// ContextFresh: the step starts from a clean context — declared
	// inputs only, never a handoff.
	ContextFresh = "fresh"
)

// InputRef is one declared input artifact (R11): the capsule-relative
// path of an upstream artifact whose content the step's prompt must
// carry. The YAML surface accepts both spellings — `- artifact: path`
// and the plain scalar `- path`.
type InputRef struct {
	Artifact string `json:"artifact"`
}

// UnmarshalYAML accepts the mapping form ({artifact: path}) and the
// plain scalar form (a bare path): the read surface is deliberately
// loose about spelling and strict about content — the path rules are
// enforced after parse (ValidateDriverWorkflow).
func (r *InputRef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		r.Artifact = strings.TrimSpace(node.Value)
		return nil
	}
	var m struct {
		Artifact string `yaml:"artifact"`
	}
	if err := node.Decode(&m); err != nil {
		return fmt.Errorf("driver: input entry must be \"- artifact: <path>\" or \"- <path>\": %w", err)
	}
	r.Artifact = strings.TrimSpace(m.Artifact)
	return nil
}

// DriverStep is one step of the extended frontmatter: the sequencing
// identity plus the context/artifact surface the spawn adapter
// assembles prompts from.
type DriverStep struct {
	ID      string
	Kind    string
	Gate    bool
	Handoff bool
	// RequiresHumanAnswer mirrors `requires_human_answer: true`: the step
	// may not complete until a human answered at least one of its
	// questions in the current attempt chain (ResultSpec.HumanAnswers).
	RequiresHumanAnswer bool
	// RequiresCommandEvidence mirrors `requires_command_evidence: true`
	// (the V4 gate; implied for a step whose id is "verification").
	RequiresCommandEvidence bool
	// ContextPolicy is ContextInherit (the default) or ContextFresh.
	ContextPolicy string
	// Inputs are the declared input artifacts (R11), in declaration
	// order.
	Inputs []InputRef
	// Outputs are the declared outputs the completed step's envelope
	// must deliver (path + JSON schema type).
	Outputs []OutputSpec
	// ModelCandidates is the step's candidate chain (per-step override
	// or the workflow-level defaults).
	ModelCandidates []string
	OnFail          *OnFail
}

// DriverWorkflow is the parallel frontmatter type: the step order IS
// the declaration order (rule V3), plus the rewind bounds. It coexists
// with the sequencing Workflow (driver.go) — same workflow.md, one
// consumer each.
type DriverWorkflow struct {
	Name       string
	Steps      []DriverStep
	MaxTotal   int
	MaxPerStep int
}

// StepByID answers the step spec for id (nil when absent).
func (w *DriverWorkflow) StepByID(id string) *DriverStep {
	for i := range w.Steps {
		if w.Steps[i].ID == id {
			return &w.Steps[i]
		}
	}
	return nil
}

// LoadDriverWorkflow reads a materialized bundle's workflow.md and
// parses its frontmatter into the extended type, then validates it.
func LoadDriverWorkflow(bundleDir string) (*DriverWorkflow, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, "workflow.md"))
	if err != nil {
		return nil, fmt.Errorf("driver: bundle workflow.md unreadable: %w", err)
	}
	front, ok := workflow.SplitFrontmatter(raw)
	if !ok {
		return nil, fmt.Errorf("driver: bundle workflow.md has no frontmatter block")
	}
	wf, err := ParseDriverWorkflow(front)
	if err != nil {
		return nil, err
	}
	if err := ValidateDriverWorkflow(wf); err != nil {
		return nil, err
	}
	return wf, nil
}

// ParseDriverWorkflow parses one frontmatter block into the extended
// type, filling the defaults: context_policy inherit, rewind bounds
// 4/2, workflow-level default model candidates. Judging the filled
// values is ValidateDriverWorkflow's job.
func ParseDriverWorkflow(front []byte) (*DriverWorkflow, error) {
	var doc extDoc
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return nil, fmt.Errorf("driver: bundle frontmatter unparsable: %w", err)
	}
	wf := &DriverWorkflow{
		Name:       doc.Name,
		MaxTotal:   defaultMaxRewindTotal,
		MaxPerStep: defaultMaxRewindPerStep,
	}
	if doc.Rewind != nil {
		if doc.Rewind.MaxTotal != nil {
			wf.MaxTotal = *doc.Rewind.MaxTotal
		}
		if doc.Rewind.MaxPerStep != nil {
			wf.MaxPerStep = *doc.Rewind.MaxPerStep
		}
	}
	for _, s := range doc.Steps {
		step := DriverStep{
			ID:                      s.ID,
			Kind:                    s.Kind,
			Gate:                    s.Gate,
			Handoff:                 s.Handoff,
			ContextPolicy:           s.ContextPolicy,
			RequiresHumanAnswer:     s.RequiresHumanAnswer,
			RequiresCommandEvidence: s.RequiresCommandEvidence,
			Inputs:                  append([]InputRef(nil), s.Inputs...),
			ModelCandidates:         s.ModelCandidates,
		}
		if step.ContextPolicy == "" {
			step.ContextPolicy = ContextInherit // the declared default
		}
		for _, o := range s.Outputs {
			step.Outputs = append(step.Outputs, OutputSpec{Path: o.Path, Schema: o.Schema})
		}
		if s.OnFail != nil {
			of := &OnFail{Action: s.OnFail.Action, Target: s.OnFail.Target}
			if s.OnFail.IncludeVerdict == nil || *s.OnFail.IncludeVerdict {
				of.IncludeVerdict = true
			}
			step.OnFail = of
		}
		if step.ModelCandidates == nil && doc.Defaults != nil {
			step.ModelCandidates = doc.Defaults.ModelCandidates
		}
		wf.Steps = append(wf.Steps, step)
	}
	return wf, nil
}

// ValidateDriverWorkflow refuses what the spawn adapter cannot honor:
// missing/duplicate step ids, context policies outside the closed set
// (every step carries one after ParseDriverWorkflow — the inherit
// default), gate steps that are not fresh (a gate must judge on its
// own declared inputs, never on an inherited persuasive context), and
// artifact paths that are not capsule-relative.
func ValidateDriverWorkflow(wf *DriverWorkflow) error {
	if wf == nil || len(wf.Steps) == 0 {
		return fmt.Errorf("driver: workflow has no steps")
	}
	seen := map[string]bool{}
	for i := range wf.Steps {
		s := &wf.Steps[i]
		if s.ID == "" {
			return fmt.Errorf("driver: workflow step %d without an id", i)
		}
		if seen[s.ID] {
			return fmt.Errorf("driver: duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
		switch s.ContextPolicy {
		case ContextInherit, ContextFresh:
		default:
			return fmt.Errorf("driver: step %q has unknown context_policy %q (want %q or %q)",
				s.ID, s.ContextPolicy, ContextInherit, ContextFresh)
		}
		if s.Gate && s.ContextPolicy != ContextFresh {
			return fmt.Errorf("driver: gate step %q must run context_policy: %s (found %q)",
				s.ID, ContextFresh, s.ContextPolicy)
		}
		for _, in := range s.Inputs {
			if err := checkRelPath(in.Artifact); err != nil {
				return fmt.Errorf("driver: step %q input: %w", s.ID, err)
			}
		}
		for _, o := range s.Outputs {
			if err := checkRelPath(o.Path); err != nil {
				return fmt.Errorf("driver: step %q output: %w", s.ID, err)
			}
			if !validSchemaName(o.Schema) {
				return fmt.Errorf("driver: step %q output %q carries unknown schema %q",
					s.ID, o.Path, o.Schema)
			}
		}
	}
	return nil
}

// validSchemaName answers whether schema is a name of the closed
// output-schema set (result.go's ValidateOutputSchema vocabulary);
// "" and "any" mean unconstrained.
func validSchemaName(schema string) bool {
	switch schema {
	case "", "any", "object", "array", "string", "number", "boolean":
		return true
	}
	return false
}

// checkRelPath asserts one capsule-relative artifact path: non-empty,
// not absolute, no parent escape.
func checkRelPath(rel string) error {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return fmt.Errorf("empty artifact path")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("path %q must be capsule-relative", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return fmt.Errorf("parent escape in path %q", rel)
		}
	}
	return nil
}

// The loose frontmatter schema the extended reader parses (a superset
// of the sequencing reader's; unknown keys are ignored).
type extDoc struct {
	Name     string       `yaml:"name"`
	Defaults *extDefaults `yaml:"defaults"`
	Rewind   *extRewind   `yaml:"rewind"`
	Steps    []extStep    `yaml:"steps"`
}

type extDefaults struct {
	ModelCandidates []string `yaml:"model_candidates"`
}

type extRewind struct {
	MaxTotal   *int `yaml:"max_total"`
	MaxPerStep *int `yaml:"max_per_step"`
}

type extStep struct {
	ID                      string      `yaml:"id"`
	Kind                    string      `yaml:"kind"`
	Gate                    bool        `yaml:"gate"`
	Handoff                 bool        `yaml:"handoff"`
	RequiresHumanAnswer     bool        `yaml:"requires_human_answer"`
	RequiresCommandEvidence bool        `yaml:"requires_command_evidence"`
	ContextPolicy           string      `yaml:"context_policy"`
	Inputs                  []InputRef  `yaml:"inputs"`
	Outputs                 []extOutput `yaml:"outputs"`
	ModelCandidates         []string    `yaml:"model_candidates"`
	OnFail                  *extOnFail  `yaml:"on_fail"`
}

type extOutput struct {
	Path   string `yaml:"path"`
	Schema string `yaml:"schema"`
}

type extOnFail struct {
	Action         string `yaml:"action"`
	Target         string `yaml:"target"`
	IncludeVerdict *bool  `yaml:"include_verdict"`
}
