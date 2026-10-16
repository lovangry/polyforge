package workspace

// memorypolicy.go: the `memory_policy:` section of `.polyforge.yaml` —
// what a step agent judges worth one project memory (`record`) and what
// it never writes as one (`skip`). The daemon renders the effective
// policy of the claimed project into every step prompt; the agent judges
// against it at the end of the step.
//
//	memory_policy:
//	  default:                  # optional: every project of this workspace
//	    record: [...]           # optional list of non-empty strings
//	    skip: [...]             # optional list of non-empty strings
//	  <project-name>:           # optional: one project
//	    record: [...]
//	    skip: [...]
//
// Merge: the base is the `default` entry, list by list; a list `default`
// does not declare is the built-in default's (DefaultMemoryPolicy). A
// project entry's lists are APPENDED to the base (duplicates dropped), so
// a project adds to the workspace default and never silently loses it.
// A declared empty list is present: `default: { skip: [] }` governs with
// no skip entries. A missing section is the built-in default for every
// project — never a refusal; a declared section parses strictly like the
// rest of the file (an invalid one refuses the claim, DAEMON-414).

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// MemoryPolicyKey is the section's top-level key.
const MemoryPolicyKey = "memory_policy"

// PolicyEntry is one declared memory_policy entry. A nil list is not
// declared (it falls through); a non-nil empty list is declared empty.
type PolicyEntry struct {
	Record []string
	Skip   []string
}

// MemoryPolicy is the effective policy of one project.
type MemoryPolicy struct {
	Record []string `json:"record"`
	Skip   []string `json:"skip"`
}

// DefaultMemoryPolicySection is the built-in default as the yaml text
// `pf init` appends to a workspace file that has no memory_policy
// section. It is also the built-in policy's single source: the daemon
// parses it for a workspace that declares none.
const DefaultMemoryPolicySection = `# memory_policy: what a step agent writes as a project memory at the end
# of a step (at most one per step, ` + "`pf remember`" + `). record = worth
# remembering, skip = never a memory. A project entry's lists are appended
# to default's:   <project>: { record: [...], skip: [...] }
memory_policy:
  default:
    record:
      - 踩坑和根因（现象→原因→解法）
      - 代码/git 里看不出来的环境与外部系统事实
      - 所有者的裁决和约定
    skip:
      - 过程流水、step 输出、完整 spec/plan（已在票和 capsule 里）
      - 代码/git 能直接看出来的东西
      - 临时状态
      - 密钥和凭据的值
`

var builtinPolicy = func() MemoryPolicy {
	cfg, reason := parse([]byte(DefaultMemoryPolicySection), nil)
	if reason != "" || cfg == nil {
		panic("workspace: the built-in memory_policy section does not parse: " + reason)
	}
	d := cfg.MemoryPolicy[DefaultKey]
	return MemoryPolicy{Record: d.Record, Skip: d.Skip}
}()

// DefaultMemoryPolicy answers the built-in policy (a fresh copy).
func DefaultMemoryPolicy() MemoryPolicy {
	return MemoryPolicy{
		Record: clone(builtinPolicy.Record),
		Skip:   clone(builtinPolicy.Skip),
	}
}

// MemoryPolicyFor answers the effective policy of project: the base
// (the `default` entry, each undeclared list the built-in one) with the
// project entry's lists appended. A nil config or an absent section is
// the built-in default.
func (c *Config) MemoryPolicyFor(project string) MemoryPolicy {
	p := DefaultMemoryPolicy()
	if c == nil || c.MemoryPolicy == nil {
		return p
	}
	if d, ok := c.MemoryPolicy[DefaultKey]; ok {
		if d.Record != nil {
			p.Record = clone(d.Record)
		}
		if d.Skip != nil {
			p.Skip = clone(d.Skip)
		}
	}
	if project != DefaultKey {
		if e, ok := c.MemoryPolicy[project]; ok {
			p.Record = appendNew(p.Record, e.Record)
			p.Skip = appendNew(p.Skip, e.Skip)
		}
	}
	return p
}

// clone copies a list, keeping a non-nil empty list non-nil.
func clone(l []string) []string {
	return append(make([]string, 0, len(l)), l...)
}

func appendNew(base, more []string) []string {
	for _, s := range more {
		if !contains(base, s) {
			base = append(base, s)
		}
	}
	return base
}

// parseMemoryPolicy reads the memory_policy section: DefaultKey or a
// mapped project → { record, skip }.
func parseMemoryPolicy(n *yaml.Node, projects []string) (map[string]PolicyEntry, string) {
	where := MemoryPolicyKey
	if isNull(n) || (n.Kind == yaml.MappingNode && len(n.Content) == 0) {
		return nil, where + ": section is empty — declare `default` or a project entry, or remove it"
	}
	if n.Kind != yaml.MappingNode {
		return nil, where + ": must be a mapping of `default` / project → { record: [...], skip: [...] }"
	}
	entries, reason := mapping(n, where)
	if reason != "" {
		return nil, reason
	}
	out := make(map[string]PolicyEntry, len(entries))
	var unmapped []string
	for _, kv := range entries {
		if kv.key != DefaultKey && projects != nil && !contains(projects, kv.key) {
			unmapped = append(unmapped, kv.key)
		}
		e, reason := policyEntry(kv.val, where+"."+kv.key)
		if reason != "" {
			return nil, reason
		}
		out[kv.key] = e
	}
	if reason := unmappedReason(unmapped, projects); reason != "" {
		return nil, reason
	}
	return out, ""
}

// policyEntry reads one `{ record: [...], skip: [...] }` entry.
func policyEntry(n *yaml.Node, where string) (PolicyEntry, string) {
	var e PolicyEntry
	if isNull(n) || (n.Kind == yaml.MappingNode && len(n.Content) == 0) {
		return e, where + ": entry is empty — declare `record` and/or `skip`, or remove it"
	}
	if n.Kind != yaml.MappingNode {
		return e, where + ": must be `{ record: [...], skip: [...] }`"
	}
	keys, reason := mapping(n, where)
	if reason != "" {
		return e, reason
	}
	for _, kv := range keys {
		list, reason := policyList(kv.val, where+"."+kv.key)
		switch {
		case kv.key != "record" && kv.key != "skip":
			return e, fmt.Sprintf("%s.%s: not allowed (an entry's keys are `record` and `skip`)", where, kv.key)
		case reason != "":
			return e, reason
		case kv.key == "record":
			e.Record = list
		default:
			e.Skip = list
		}
	}
	return e, ""
}

// policyList reads one list of unique, non-empty plain strings (each
// trimmed); a declared empty list answers an empty, non-nil list.
func policyList(n *yaml.Node, where string) ([]string, string) {
	if n.Kind != yaml.SequenceNode {
		return nil, where + ": must be a list of strings"
	}
	list := make([]string, 0, len(n.Content))
	for i, item := range deref(n).Content {
		item = deref(item)
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
			return nil, fmt.Sprintf("%s[%d]: must be a plain string", where, i)
		}
		s := strings.TrimSpace(item.Value)
		if s == "" {
			return nil, fmt.Sprintf("%s[%d]: is empty", where, i)
		}
		if contains(list, s) {
			return nil, fmt.Sprintf("%s[%d]: repeats %q", where, i, s)
		}
		list = append(list, s)
	}
	return list, ""
}

// MemoryPolicyDeclared reports whether the workspace's `.polyforge.yaml`
// exists and whether its top level declares the memory_policy key (valid
// or not — validity is Load's). A file that is no readable YAML mapping
// answers an error.
func MemoryPolicyDeclared(root string) (exists, declared bool, err error) {
	raw, err := os.ReadFile(PathFor(root))
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	declared, err = declaresPolicy(raw)
	return true, declared, err
}

// declaresPolicy answers whether a yaml document's top-level mapping has
// the memory_policy key. An empty or comments-only document has none.
func declaresPolicy(raw []byte) (bool, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(raw)).Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	if len(doc.Content) == 0 || isNull(deref(doc.Content[0])) {
		return false, nil
	}
	root := deref(doc.Content[0])
	if root.Kind != yaml.MappingNode {
		return false, fmt.Errorf("the file is not a mapping")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == MemoryPolicyKey {
			return true, nil
		}
	}
	return false, nil
}

// ensureMemoryPolicy gives the workspace's `.polyforge.yaml` the default
// memory_policy section when it declares none: an absent file is created
// with it; an existing one gets it APPENDED as text — its bytes before
// the section are kept exactly (hand-written comments included), never
// re-marshalled. A file that already declares the key is untouched, as
// is one that does not load (it is refused, not repaired). It answers
// created | appended | unchanged.
func ensureMemoryPolicy(root string) (string, error) {
	path := PathFor(root)
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("%s is a symlink that does not resolve: %w", path, err)
		}
		path = target
	}
	raw, err := os.ReadFile(path)
	var mode fs.FileMode = 0o644
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "created", writeAtomic(path, []byte(DefaultMemoryPolicySection), mode)
	case err != nil:
		return "", err
	}
	if fi, serr := os.Stat(path); serr == nil {
		mode = fi.Mode().Perm()
	}
	if _, reason := parse(raw, nil); reason != "" {
		return "", fmt.Errorf("%s: %s — fix it first (`pf doctor` names the defect); the memory_policy section was not added", path, reason)
	}
	has, err := declaresPolicy(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if has {
		return "unchanged", nil
	}
	out := append([]byte(nil), raw...)
	if len(out) > 0 {
		if out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, '\n')
	}
	out = append(out, DefaultMemoryPolicySection...)
	if _, reason := parse(out, nil); reason != "" {
		return "", fmt.Errorf("%s: appending the memory_policy section would not load (%s) — add it by hand; the file was not changed", path, reason)
	}
	return "appended", writeAtomic(path, out, mode)
}
