// Package workflow is the minimal workflow-bundle frontmatter read surface
// shared by pf: the model-candidate fields that drive model selection.
//
// Design authority: docs/design/polyforge-vnext/01-workflow-skills.md.
package workflow

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Minimal frontmatter read surface (the F4 lane: candidate-chain
// consumption): the fields that drive model selection — workflow-level
// defaults.model_candidates, the per-step model_candidates override and
// the step kind. Nothing is validated here; the driver's LoadWorkflow is
// the one structural check of a bundle.

// KindVocabulary is the closed step-kind set: the only legal frontmatter
// `kind` values (the driver refuses any other at load) and the only legal
// per-kind keys of the workspace yaml's model lists.
var KindVocabulary = []string{"qa", "spec", "author", "code", "gate", "review"}

// ValidKind reports whether k is in KindVocabulary.
func ValidKind(k string) bool {
	for _, v := range KindVocabulary {
		if k == v {
			return true
		}
	}
	return false
}

// Defaults carries the workflow-level fallback model candidates.
type Defaults struct {
	ModelCandidates []string `yaml:"model_candidates"`
}

// FrontmatterStep is one frontmatter step entry, reduced to the fields
// the read surface needs (the id, the step's V8 KIND, and the per-step
// candidate override). Kind is the polyforge#79 read-surface addition:
// the workspace yaml's per-project kind lists are kind-KEYED, so the compose needs
// the same closed vocabulary the bundle declares.
type FrontmatterStep struct {
	ID              string   `yaml:"id"`
	Kind            string   `yaml:"kind"`
	ModelCandidates []string `yaml:"model_candidates"`
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
