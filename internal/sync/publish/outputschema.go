package publish

// outputschema.go: the publish-time output-contract mechanical check
// (03-execution-capsule.md §5.1a, polyforge#27 amendment). One check step
// rides the staging→manifest window of every full-scan publish: AFTER the
// staging pin (05 §6 step 4) and BEFORE the manifest submit / HEAD advance
// (steps 5–6), each step that execution.json reports as done has its
// output validated against the step's PINNED `output` JSON Schema.
//
// SIGNAL, NOT VETO (03 §5.1a point 4): a detected mismatch NEVER blocks
// the publish — HEAD advances regardless, the mismatched bytes stay on
// the Revision chain as auditable history, and the mismatch list rides
// the publish_request response for the WI Agent to consume per
// 01-workflow-skills.md §6.3 (falsified-done: atomic done→in_progress
// rewrite + infra_failure attempt archive + bounded candidate advance).
//
// INV-6 BOUNDARY (03 §5.1a point 1 — authoritative here): the check reads
// ONLY the staged bytes about to be written into the Revision, never the
// live Capsule's runtime-state/. runtime-state/ is the WI Agent's
// single-writer private domain; this structurally cannot open it for
// outputs because
//
//   - step outputs enter exclusively as BYTES handed over by the
//     StagedBlock closure (in production freeze.Stager.Block, keyed by
//     content sha inside the Daemon's staging tree) — the check holds no
//     capsule path for outputs at all, and
//   - the only paths the check opens are pinned-schema files under the
//     Capsule's materialized read-only workflow snapshot (03 §4.5:
//     .pi/agents / .claude/agents — outside the sync surface and outside
//     runtime-state/), and every such path passes guardNoRuntimeState,
//     which refuses any path with a runtime-state component.
//
// The validator library is a draft-07-complete implementation
// (github.com/santhosh-tekuri/jsonschema/v5 — the ONE go.mod dependency
// this feature adds; it has zero transitive dependencies), matching the
// schema-authoring contract internal/workflow/schema already enforces on
// the reference bundles (standard draft-07 keywords only).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/sync/revision"
	wfschema "github.com/silgrid/polyforge/internal/workflow/schema"
)

// ErrINV6 marks a validator input that points into the live Capsule's
// runtime-state/ — the single-writer private domain the publish-time check
// must never open (03 §5.1a point 1). Constructing the check with such a
// path fails closed: the check refuses to run rather than read live bytes.
var ErrINV6 = errors.New("publish: INV-6 violation: the output-schema check may not open the live capsule's runtime-state/")

// SchemaMismatch is one detected output-contract violation for one done
// step. This is the signal shape riding the publish_request response
// (04 §5.1): the WI Agent consumes it per 01-workflow-skills.md §6.3.
// skills/pf-execute.md currently names no shape for this field (its §4/§6
// cover only dispatch-side self-validation), so this minimal
// {step_id, pointer, keyword, detail} list is the definition; the skill
// needs one text line to read `schema_mismatches` from the publish
// response (reported, not edited here — skills/ is out of scope).
type SchemaMismatch struct {
	// StepID is the execution.json step whose done-state output failed.
	StepID string `json:"step_id"`
	// Pointer is the JSON pointer of the failing value inside the step's
	// output ("/" = the output root; pointers are relative to the output,
	// not to execution.json).
	Pointer string `json:"pointer"`
	// Keyword is the draft-07 keyword that failed (required, type,
	// minLength, enum, ...).
	Keyword string `json:"keyword"`
	// Detail is the validator's message for this unit.
	Detail string `json:"detail"`
}

// OutputCheck performs one publish's output-contract check.
type OutputCheck struct {
	// AgentsDir is the Capsule's materialized read-only workflow snapshot
	// agents directory (03 §4.5: .pi/agents or .claude/agents) holding the
	// pinned agents/<step-id>.md files. Guarded at construction: a path
	// with a runtime-state component is refused (ErrINV6).
	AgentsDir string
	// StagedBlock returns the staged bytes for one content sha. In
	// production this is freeze.Stager.Block(wi, seq, sha) — the ONLY byte
	// source for outputs; the check never opens a live capsule path.
	StagedBlock func(sha string) ([]byte, error)

	// compiled caches each pinned step's compiled schema within this
	// publish, keyed by bundle-digest + step id: every step is compiled
	// ONCE per publish (03 §5.1a point 2/3).
	compiled map[string]*jsonschema.Schema
	// compileErr remembers per-key compile failures so one broken schema
	// does not retry (and cannot veto anything — it is skipped).
	compileErr map[string]error
}

// NewOutputCheck constructs the check for one publish flow. agentsDir must
// be the materialized snapshot's agents directory; a path touching the
// live capsule's runtime-state/ is refused with ErrINV6 (fail closed).
func NewOutputCheck(agentsDir string, stagedBlock func(sha string) ([]byte, error)) (*OutputCheck, error) {
	if err := guardNoRuntimeState(agentsDir); err != nil {
		return nil, err
	}
	if stagedBlock == nil {
		return nil, errors.New("publish: output check requires a staged-block source")
	}
	return &OutputCheck{
		AgentsDir:   agentsDir,
		StagedBlock: stagedBlock,
		compiled:    map[string]*jsonschema.Schema{},
		compileErr:  map[string]error{},
	}, nil
}

// guardNoRuntimeState enforces the INV-6 boundary on every filesystem path
// the check may open: any path with a runtime-state path component is
// refused. Applied to the agents dir at construction and to every resolved
// schema file path, it makes the check structurally incapable of opening
// the live Capsule's runtime-state/ even if handed such a path.
func guardNoRuntimeState(path string) error {
	slash := filepath.ToSlash(path)
	for _, part := range strings.Split(slash, "/") {
		if part == "runtime-state" {
			return fmt.Errorf("%w: %s", ErrINV6, path)
		}
	}
	return nil
}

// Check validates every done step's output found in the STAGED
// runtime-state/execution.json bytes this manifest is about to publish.
// ops are the manifest's ops: the execution.json put (SYNC-INV-3: present
// in every manifest) locates the staged bytes by content sha.
//
// The returned error reports check-infrastructure failures (unparsable
// staged bytes, an unreadable agents dir). Per the signal-not-veto rule
// the CALLER treats it as non-fatal for the publish; the mismatch list is
// returned alongside either way. A step whose pinned schema file is
// missing or does not compile is SKIPPED — no mismatch is claimed without
// an actually validated violation, and nothing here can block the flow.
func (c *OutputCheck) Check(ops []revision.Op) ([]SchemaMismatch, error) {
	var execSha string
	for _, op := range ops {
		if op.Op == revision.OpPut && op.Path == revision.ExecutionJSONPath {
			execSha = op.Sha256
			break
		}
	}
	if execSha == "" {
		// No execution bytes ride this manifest: nothing to validate.
		return nil, nil
	}
	raw, err := c.StagedBlock(execSha)
	if err != nil {
		return nil, fmt.Errorf("publish: staged execution.json unreadable: %w", err)
	}
	var exec capsule.Execution
	if err := json.Unmarshal(raw, &exec); err != nil {
		return nil, fmt.Errorf("publish: staged execution.json unparsable: %w", err)
	}

	ids := make([]string, 0, len(exec.Steps))
	for id := range exec.Steps {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic signal order

	out := []SchemaMismatch{}
	for _, id := range ids {
		st := exec.Steps[id]
		if st == nil || st.Status != capsule.StepDone || st.Output == nil {
			continue
		}
		sch, serr := c.schemaFor(exec.BundleDigest, id)
		if sch == nil {
			_ = serr // missing/uncompilable pinned schema: skip, never veto
			continue
		}
		// Re-marshal + UseNumber so numbers keep their exact precision
		// for the validator (library contract: unmarshal with
		// json.UseNumber).
		blob, merr := json.Marshal(st.Output)
		if merr != nil {
			continue // non-JSON-shaped output cannot be claimed as a schema mismatch unit here
		}
		var inst any
		dec := json.NewDecoder(bytes.NewReader(blob))
		dec.UseNumber()
		if derr := dec.Decode(&inst); derr != nil {
			continue
		}
		if verr := sch.Validate(inst); verr != nil {
			var ve *jsonschema.ValidationError
			if errors.As(verr, &ve) {
				for _, b := range ve.BasicOutput().Errors {
					// The library emits one synthetic wrapper unit (keyword
					// location "#", empty keyword) around the real leaves;
					// the signal carries only the leaf units.
					if b.KeywordLocation == "#" || keywordOf(b.KeywordLocation) == "" {
						continue
					}
					out = append(out, SchemaMismatch{
						StepID:  id,
						Pointer: pointerOf(b.InstanceLocation),
						Keyword: keywordOf(b.KeywordLocation),
						Detail:  b.Error,
					})
				}
			}
		}
	}
	return out, nil
}

// schemaFor resolves, parses and compiles the pinned output schema of one
// step from the materialized snapshot, ONCE per publish (cache keyed by
// bundle digest + step id). A missing or invalid step-skill file yields
// (nil, err) — the caller skips the step.
func (c *OutputCheck) schemaFor(bundleDigest, stepID string) (*jsonschema.Schema, error) {
	key := bundleDigest + "\x00" + stepID
	if sch, ok := c.compiled[key]; ok {
		return sch, nil
	}
	if err, ok := c.compileErr[key]; ok {
		return nil, err
	}
	if filepath.Base(stepID) != stepID || stepID == "" || strings.ContainsAny(stepID, `/\`) {
		err := fmt.Errorf("publish: step id %q is not a plain file name", stepID)
		c.compileErr[key] = err
		return nil, err
	}
	path := filepath.Join(c.AgentsDir, stepID+".md")
	if err := guardNoRuntimeState(path); err != nil {
		c.compileErr[key] = err
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		c.compileErr[key] = err
		return nil, err
	}
	sk, err := wfschema.ParseStepSkill(stepID, string(raw))
	if err != nil {
		c.compileErr[key] = err
		return nil, err
	}
	blob, err := json.Marshal(sk.Output)
	if err != nil {
		c.compileErr[key] = err
		return nil, err
	}
	comp := jsonschema.NewCompiler()
	// The reference bundles' output schemas use standard draft-07 keywords
	// (01 §4.4); pin draft-07 as the fallback for schemas without an
	// explicit $schema so the contract is mechanical and fixed.
	comp.Draft = jsonschema.Draft7
	// The URL only names the resource for AddResource/Compile; it is never
	// fetched — the bytes are supplied below. Built from URL-safe
	// projections of the cache key (the raw key carries a NUL separator).
	url := "https://polyforge.invalid/pinned-output-schemas/" + urlKey(bundleDigest) + "/" + urlKey(stepID) + ".json"
	if err := comp.AddResource(url, bytes.NewReader(blob)); err != nil {
		c.compileErr[key] = err
		return nil, err
	}
	sch, err := comp.Compile(url)
	if err != nil {
		c.compileErr[key] = err
		return nil, err
	}
	c.compiled[key] = sch
	return sch, nil
}

// urlKey projects a cache-key component onto URL-safe runes: hex-encode
// anything that is not [A-Za-z0-9._-] so the synthetic resource URL of a
// pinned schema can never carry a control character or path separator.
func urlKey(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[(r>>4)&0xf])
			b.WriteByte(hexDigits[r&0xf])
		}
	}
	return b.String()
}

// pointerOf normalizes a validator instance location into a JSON-pointer
// summary: the empty location (the output root) becomes "/".
func pointerOf(loc string) string {
	if loc == "" {
		return "/"
	}
	if !strings.HasPrefix(loc, "/") {
		return "/" + loc
	}
	return loc
}

// keywordOf extracts the failing schema keyword from a keyword location
// like "#/required" or "#/properties/summary/type" (the last segment).
func keywordOf(loc string) string {
	if i := strings.LastIndexByte(loc, '/'); i >= 0 {
		return loc[i+1:]
	}
	return loc
}
