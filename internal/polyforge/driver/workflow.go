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
	// The step's wall-budget estimate (budget.wall_minutes, T's base).
	WallMinutes int
}

// OnFail is the gate FAIL policy: the rewind target the gate's FAIL
// verdict rewinds to (the terminate option is deleted — a gate FAIL
// always rewinds to its declared target).
type OnFail struct {
	Target string
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
			ID:                  s.ID,
			Gate:                s.Gate,
			Handoff:             s.Handoff,
			ContextPolicy:       s.ContextPolicy,
			RequiresHumanAnswer: s.RequiresHumanAnswer,
			ModelCandidates:     s.ModelCandidates,
		}
		if s.Budget != nil {
			step.WallMinutes = s.Budget.WallMinutes
		}
		if step.ContextPolicy == "" {
			step.ContextPolicy = ContextInherit // the declared default
		}
		if s.OnFail != nil {
			step.OnFail = &OnFail{Target: s.OnFail.Target}
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
	ID                  string     `yaml:"id"`
	Gate                bool       `yaml:"gate"`
	Handoff             bool       `yaml:"handoff"`
	RequiresHumanAnswer bool       `yaml:"requires_human_answer"`
	ContextPolicy       string     `yaml:"context_policy"`
	ModelCandidates     []string   `yaml:"model_candidates"`
	OnFail              *extOnFail `yaml:"on_fail"`
	Budget              *extBudget `yaml:"budget"`
}

type extBudget struct {
	WallMinutes int `yaml:"wall_minutes"`
}

type extOnFail struct {
	Target string `yaml:"target"`
}
