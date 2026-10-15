// Package workflow is the minimal workflow-bundle frontmatter read surface
// shared by pf: the model-candidate and harness fields that drive model
// and harness selection.
//
// Design authority: docs/design/polyforge-vnext/01-workflow-skills.md.
package workflow

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Minimal frontmatter read surface (the F4 lane: candidate-chain
// consumption): the fields that drive model selection — workflow-level
// defaults.model_candidates and the per-step model_candidates override,
// keyed by step id. Nothing is validated here; the driver's LoadWorkflow
// is the one structural check of a bundle. Unknown keys are ignored (a
// retired `kind:` in an old bundle snapshot included).

// Defaults carries the workflow-level fallback model candidates and
// harness (polyforge#159; "" = not declared).
type Defaults struct {
	ModelCandidates []string `yaml:"model_candidates"`
	Harness         string   `yaml:"harness"`
}

// FrontmatterStep is one frontmatter step entry, reduced to the fields
// the read surface needs: the id (the workspace yaml's per-step model
// lists are keyed by it), the per-step candidate override and the
// per-step harness (polyforge#159; "" = not declared).
type FrontmatterStep struct {
	ID              string   `yaml:"id"`
	ModelCandidates []string `yaml:"model_candidates"`
	Harness         string   `yaml:"harness"`
}

// Frontmatter is the parsed workflow.md frontmatter (structure reduced to
// the minimal read surface; the Markdown body is never parsed here).
type Frontmatter struct {
	Defaults *Defaults         `yaml:"defaults"`
	Steps    []FrontmatterStep `yaml:"steps"`
}

// SplitFrontmatter splits a Markdown document into its YAML frontmatter
// block and the body that follows the closing fence. The file must begin
// with a `---` line; ok=false when it does not (a frontmatter-less
// workflow.md is legal input to this reader — the candidate chain is then
// simply absent).
func SplitFrontmatter(content []byte) (front, body []byte, ok bool) {
	s := string(content)
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return nil, nil, false
	}
	if strings.TrimRight(s[:nl], " \r") != "---" {
		return nil, nil, false
	}
	rest := s[nl+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, nil, false
	}
	block := rest[:end+1]
	// a closing fence may carry trailing spaces / CRLF noise
	tail := rest[end+4:]
	after := ""
	if nl2 := strings.IndexByte(tail, '\n'); nl2 >= 0 {
		tail, after = tail[:nl2], tail[nl2+1:]
	}
	if strings.TrimRight(tail, " \r") != "" {
		return nil, nil, false // not a fence close (e.g. ---- separator in the body)
	}
	return []byte(block), []byte(after), true
}

// ReadFrontmatter reads one workflow.md and parses its frontmatter with
// the minimal surface above. A missing file, a file with no frontmatter
// block, or an empty one answers (nil, nil) — "no candidate chain" is a
// typed absent condition, never an error. A present-but-torn YAML block
// answers (nil, err): the caller decides whether that is fatal.
func ReadFrontmatter(path string) (*Frontmatter, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	front, _, ok := SplitFrontmatter(raw)
	if !ok || len(strings.TrimSpace(string(front))) == 0 {
		return nil, nil
	}
	var fm Frontmatter
	if err := yaml.Unmarshal(front, &fm); err != nil {
		return nil, err
	}
	return &fm, nil
}

// StepIDs answers the set of step ids declared by the installed bundles
// under workflowsDir (every <workflowsDir>/<bundle>/workflow.md) — the
// legal per-step keys of the workspace yaml's model lists. A bundle
// whose frontmatter is absent or torn contributes nothing (its own
// claim surfaces the defect); an unreadable directory is an error.
func StepIDs(workflowsDir string) (map[string]bool, error) {
	ents, err := os.ReadDir(workflowsDir)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		fm, _ := ReadFrontmatter(filepath.Join(workflowsDir, e.Name(), "workflow.md"))
		if fm == nil {
			continue
		}
		for _, s := range fm.Steps {
			if s.ID != "" {
				ids[s.ID] = true
			}
		}
	}
	return ids, nil
}
