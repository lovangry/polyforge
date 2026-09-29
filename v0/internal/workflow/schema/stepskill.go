package schema

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

// StepSkillFrontmatter is the parsed agents/<step-id>.md frontmatter
// (01-workflow-skills.md §4.4). Hard-required: name, description, tools,
// output (a JSON Schema object). Forbidden: model (INV-1g — model selection
// lives only in workflow.md frontmatter's model_candidates).
//
// The AIHub creation transaction does NOT call this validator (02 §4.5); it is
// a library for bundle tests and execution-side warnings (01 §4.4 校验时机).
type StepSkillFrontmatter struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Tools       any            `yaml:"tools"` // scalar "a, b" or list form; 01 §4.4 does not pin the shape
	Output      map[string]any `yaml:"output"`
}

// ToolsList normalizes the tools declaration to a list of tool names.
func (s *StepSkillFrontmatter) ToolsList() []string {
	switch t := s.Tools.(type) {
	case string:
		var out []string
		for _, part := range strings.Split(t, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	case []any:
		var out []string
		for _, e := range t {
			if str, ok := e.(string); ok && strings.TrimSpace(str) != "" {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

var allowedStepSkillKeys = map[string]bool{
	"name": true, "description": true, "tools": true, "output": true,
}

// ParseStepSkill parses an agents/<step-id>.md definition body into its
// frontmatter struct, enforcing the §4.4 hard-required set and the V7
// name-equality rule. It is the shared read path for two consumers:
// ValidateStepSkill (bundle tests / execution-side warnings, 01 §4.4) and
// the publish-time output-schema check (03 §5.1a), which needs the parsed
// `output` JSON Schema object, not just a verdict.
func ParseStepSkill(fileName, content string) (*StepSkillFrontmatter, error) {
	front, _, ok := SplitFrontmatter([]byte(content))
	if !ok {
		return nil, ruleErr("V7", "step skill %s: missing --- delimited frontmatter", fileName)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(front, &node); err != nil {
		return nil, ruleErr("V7", "step skill %s: invalid YAML: %v", fileName, err)
	}
	if len(node.Content) == 0 {
		return nil, ruleErr("V7", "step skill %s: empty frontmatter", fileName)
	}
	if err := walkNode(&node, "skill", allowedStepSkillKeys); err != nil {
		return nil, err
	}
	var sk StepSkillFrontmatter
	if err := node.Decode(&sk); err != nil {
		return nil, ruleErr("V7", "step skill %s: frontmatter does not match the schema: %v", fileName, err)
	}
	if sk.Name == "" {
		return nil, ruleErr("V7", "step skill %s: name is required", fileName)
	}
	if sk.Name != fileName {
		return nil, ruleErr("V7", "step skill %s: name %q must equal the file name", fileName, sk.Name)
	}
	if sk.Description == "" {
		return nil, ruleErr("V7", "step skill %s: description is required", fileName)
	}
	if len(sk.ToolsList()) == 0 {
		return nil, ruleErr("V7", "step skill %s: tools is required", fileName)
	}
	if sk.Output == nil {
		return nil, ruleErr("V7", "step skill %s: output (JSON Schema) is required", fileName)
	}
	// Output must be a JSON Schema object: at minimum a mapping; validate it
	// round-trips through JSON.
	if _, err := json.Marshal(sk.Output); err != nil {
		return nil, ruleErr("V7", "step skill %s: output is not a JSON object: %v", fileName, err)
	}
	// INV-1g: model key is forbidden — walkNode already rejects unknown keys,
	// so reaching here means "model" was absent.
	return &sk, nil
}

// ValidateStepSkill validates an agents/<step-id>.md definition body.
// fileName must be the step id (the file name); name must equal it (V7).
// Errors are reported with rule id V7 per 01 §4.4.
func ValidateStepSkill(fileName, content string) error {
	_, err := ParseStepSkill(fileName, content)
	return err
}
