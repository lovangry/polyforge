package driver

// workflow.go: the ONE parse of a bundle's workflow.md frontmatter. The
// driver (sequencing, supervision overrides, result duties) and the
// spawn adapter (prompt assembly) read the same Workflow value, parsed
// once in driver.New / NewStepSpawnAdapter. The read is loose (unknown
// keys ignored) and full bundle VALIDATION stays aihub's job at WI
// creation; this refuses only what the driver cannot honor (unknown
// context policies, non-capsule-relative artifact paths, gate steps
// that are not fresh) and surfaces every parse error.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/silgrid/polyforge/internal/common/workflow"
	"gopkg.in/yaml.v3"
)

const (
	defaultMaxRewindTotal   = 4
	defaultMaxRewindPerStep = 2
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
// enforced after parse (validateWorkflow).
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

// Workflow is the execution graph: the step order IS the declaration
// order (rule V3), plus the rewind bounds.
type Workflow struct {
	Steps      []StepSpec
	MaxTotal   int
	MaxPerStep int
}

// StepSpec is one workflow step: identity and order, the candidate
// chain, gate policy, context policy and the artifact surface the spawn
// adapter assembles prompts from.
type StepSpec struct {
	ID   string
	Gate bool
	// ContextPolicy is ContextInherit (the default) or ContextFresh.
	ContextPolicy   string
	ModelCandidates []string
	OnFail          *OnFail
	// Handoff marks a step that must produce a handoff artifact for the
	// next inherit step (validated on its completed results).
	Handoff bool
	// RequiresHumanAnswer mirrors `requires_human_answer: true`: the step
	// may not complete until a human answered at least one of its
	// questions in the current attempt chain (ResultSpec.HumanAnswers).
	RequiresHumanAnswer bool
	// RequiresCommandEvidence mirrors `requires_command_evidence: true`
	// (the V4 gate; implied for a step whose id is "verification").
	RequiresCommandEvidence bool
	// Inputs are the declared input artifacts (R11), in declaration
	// order.
	Inputs []InputRef
	// Outputs are the declared outputs the completed step's envelope
	// must deliver (path + JSON schema type).
	Outputs []OutputSpec
	// Step-level supervision overrides: T is budget.wall_minutes, X is
	// silence_minutes, Y is tool_minutes. Zero = the global default.
	SilenceMinutes int
	ToolMinutes    int
	WallMinutes    int
}

// OnFail is the gate FAIL policy (01 §4.1): rewind with a target, or
// terminate (the default).
type OnFail struct {
	Action         string // "rewind" | "terminate"
	Target         string
	IncludeVerdict bool
}

func (w *Workflow) indexOf(stepID string) (int, bool) {
	for i, s := range w.Steps {
		if s.ID == stepID {
			return i, true
		}
	}
	return -1, false
}

// step answers the step spec for id (nil when absent).
func (w *Workflow) step(id string) *StepSpec {
	if i, ok := w.indexOf(id); ok {
		return &w.Steps[i]
	}
	return nil
}

// LoadWorkflow reads a materialized bundle's workflow.md, parses its
// frontmatter and validates it.
func LoadWorkflow(bundleDir string) (*Workflow, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, "workflow.md"))
	if err != nil {
		return nil, fmt.Errorf("driver: bundle workflow.md unreadable: %w", err)
	}
	front, _, ok := workflow.SplitFrontmatter(raw)
	if !ok {
		return nil, fmt.Errorf("driver: bundle workflow.md has no frontmatter block")
	}
	wf, err := parseWorkflow(front)
	if err != nil {
		return nil, err
	}
	if err := validateWorkflow(wf); err != nil {
		return nil, err
	}
	return wf, nil
}

// parseWorkflow parses one frontmatter block, filling the defaults: context_policy inherit, rewind bounds
// 4/2, workflow-level default model candidates. Judging the filled
// values is validateWorkflow's job.
func parseWorkflow(front []byte) (*Workflow, error) {
	var doc extDoc
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return nil, fmt.Errorf("driver: bundle frontmatter unparsable: %w", err)
	}
	wf := &Workflow{
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
		step := StepSpec{
			ID:                      s.ID,
			Gate:                    s.Gate,
			Handoff:                 s.Handoff,
			ContextPolicy:           s.ContextPolicy,
			RequiresHumanAnswer:     s.RequiresHumanAnswer,
			RequiresCommandEvidence: s.RequiresCommandEvidence,
			Inputs:                  append([]InputRef(nil), s.Inputs...),
			ModelCandidates:         s.ModelCandidates,
			SilenceMinutes:          s.SilenceMinutes,
			ToolMinutes:             s.ToolMinutes,
		}
		if s.Budget != nil {
			step.WallMinutes = s.Budget.WallMinutes
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

// validateWorkflow refuses what the spawn adapter cannot honor:
// missing/duplicate step ids, context policies outside the closed set
// (every step carries one after parseWorkflow — the inherit
// default), gate steps that are not fresh (a gate must judge on its
// own declared inputs, never on an inherited persuasive context), and
// artifact paths that are not capsule-relative.
func validateWorkflow(wf *Workflow) error {
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

// The loose frontmatter schema (unknown keys are ignored).
type extDoc struct {
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
	Gate                    bool        `yaml:"gate"`
	Handoff                 bool        `yaml:"handoff"`
	RequiresHumanAnswer     bool        `yaml:"requires_human_answer"`
	RequiresCommandEvidence bool        `yaml:"requires_command_evidence"`
	ContextPolicy           string      `yaml:"context_policy"`
	Inputs                  []InputRef  `yaml:"inputs"`
	Outputs                 []extOutput `yaml:"outputs"`
	ModelCandidates         []string    `yaml:"model_candidates"`
	OnFail                  *extOnFail  `yaml:"on_fail"`
	SilenceMinutes          int         `yaml:"silence_minutes"`
	ToolMinutes             int         `yaml:"tool_minutes"`
	Budget                  *extBudget  `yaml:"budget"`
}

type extBudget struct {
	WallMinutes int `yaml:"wall_minutes"`
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
