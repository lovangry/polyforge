package runtime

// memorypolicy.go: the claimed project's effective memory policy, written
// into the capsule at claim (capsule.MemoryPolicyJSON) beside the recall
// snapshot; the driver renders it into every step prompt's memory
// section. Like the snapshot it is an enrichment, never a gate: the
// workspace yaml was validated before the claim (LoadWorkspace), so a
// re-read that fails here (an edit since) falls back to the built-in
// default policy and the claim goes on.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/common/atomicfile"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/workspace"
)

// memoryPolicyDoc is the memory-policy.json shape.
type memoryPolicyDoc struct {
	Project string `json:"project"`
	workspace.MemoryPolicy
	// Source is "workspace" when the project's workspace yaml declares a
	// memory_policy section, else "built-in".
	Source string `json:"source"`
}

func (r *Runtime) writeMemoryPolicy(j *job) {
	var cfg *workspace.Config
	if ws, err := r.cfg.Mapping.WorkspaceFor(j.project); err == nil {
		c, lerr := workspace.Load(ws, "", r.cfg.Mapping.ProjectsOf(ws))
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s: memory policy falls back to the built-in default: %v\n", j.wi, lerr)
		} else {
			cfg = c
		}
	}
	doc := memoryPolicyDoc{Project: j.project, MemoryPolicy: cfg.MemoryPolicyFor(j.project), Source: "built-in"}
	if cfg != nil && cfg.MemoryPolicy != nil {
		doc.Source = "workspace"
	}
	blob, err := json.MarshalIndent(&doc, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: memory policy marshal failed: %v\n", j.wi, err)
		return
	}
	if err := atomicfile.Write(filepath.Join(j.capsuleDir, filepath.FromSlash(capsule.MemoryPolicyJSON)), blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: memory policy write failed: %v\n", j.wi, err)
	}
}
