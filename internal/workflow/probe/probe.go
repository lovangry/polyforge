// Package probe implements the install-face candidate liveness probe
// (07-deployment-pilot.md §6.1 step 6, polyforge#27 amendment): for the
// bundles present in the daemon's workflows checkout, parse every
// declared model_candidates entry (defaults plus step-level overrides,
// the union per §6.1 step 6) and issue ONE micro request per UNIQUE
// candidate through the harness adapter's model call path (a run probes
// one adapter, so the (adapter, candidate) key reduces to the candidate
// string; bundles declaring the same candidate share its outcome). The probe is an
// INSTALL-FACE criterion, not a runtime dependency: a candidate that is
// unreachable during the Pilot is still handled at run time by the
// ordinary infrastructure-failure in-place resume semantics
// (01-workflow-skills.md §6.3) — the probe neither backstops nor changes
// that behavior.
package probe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/workflow/schema"
)

// MicroPrompt is the probe request body: the smallest thing a candidate
// model can answer. Any successful model call (process exit 0) counts as
// reachable; the reply content is not interpreted.
const MicroPrompt = "Reply with the single word: ok"

// Prober issues one micro request through a harness adapter's model call
// path. Production uses PiProber/ClaudeProber; tests inject fakes.
type Prober interface {
	// Probe returns nil when the candidate answered (reachable), or the
	// error explaining why it did not (unreachable + error).
	Probe(ctx context.Context, candidate string) error
}

// PiProber probes through the pi adapter's model call path:
// `pi --provider <p> --model <m> --no-session -p <micro prompt>`.
// Candidates are provider-qualified ("provider/model", 00 §3.6a): pi
// resolves the PROVIDER from --provider and the local model id from
// --model, so a qualified candidate must be SPLIT — passing the whole
// string to --model makes pi fall back to the default provider for
// unknown model ids (measured on the first real install: 422
// unsupported-conversion from the wrong provider's proxy). A bare
// model id (no "/") is passed to --model alone, provider left to
// pi's own default. The argv family carries NO --no-tools: the real
// WI-Agent derivation runs WITH tools, and on cursor-format
// provider backends the no-tools flag flips the request shape to a
// plain chat-completion form those backends reject (422, measured
// on the first real install) — the probe must ride the SAME argv
// family as the real spawn or it tests a path nothing will ever
// take. The micro prompt answers "OK" without invoking any tool, so
// enabling tools costs only the request's tool-schema bytes.
type PiProber struct {
	// Bin overrides the pi executable path (tests substitute a fake).
	Bin string
}

func (p *PiProber) bin() string {
	if p.Bin == "" {
		return "pi"
	}
	return p.Bin
}

func (p *PiProber) Probe(ctx context.Context, candidate string) error {
	args := []string{}
	what := "pi --candidate " + candidate
	if provider, model, ok := strings.Cut(candidate, "/"); ok {
		args = append(args, "--provider", provider, "--model", model)
		what += " --provider " + provider + " --model " + model
	} else {
		args = append(args, "--model", candidate)
		what += " --model " + candidate
	}
	args = append(args, "--no-session", "-p", MicroPrompt)
	cmd := exec.CommandContext(ctx, p.bin(), args...)
	cmd.Env = os.Environ()
	return runCollectingOutput(cmd, what)
}

// ClaudeProber probes through the claude adapter's model call path:
// `claude -p --model <model> <micro prompt>`. The claude CLI's --model
// takes the local model id only — provider routing is claude's own
// environment, not a CLI flag — so a provider-qualified candidate is
// stripped to its model part; a bare id passes through unchanged.
type ClaudeProber struct {
	Bin string
}

func (c *ClaudeProber) bin() string {
	if c.Bin == "" {
		return "claude"
	}
	return c.Bin
}

func (c *ClaudeProber) Probe(ctx context.Context, candidate string) error {
	model := candidate
	if _, rest, ok := strings.Cut(candidate, "/"); ok {
		model = rest
	}
	cmd := exec.CommandContext(ctx, c.bin(),
		"-p", "--model", model, MicroPrompt)
	cmd.Env = os.Environ()
	return runCollectingOutput(cmd, "claude --candidate "+candidate+" --model "+model)
}

// runCollectingOutput runs the micro request with stdout/stderr attached
// to a real file (a direct fd pass-down, never an os.Pipe): a harness
// process killed on the per-candidate timeout may leave orphaned children,
// and a pipe-based capture would block Wait on their inherited write end
// long past the deadline. The file keeps the error detail in the report.
func runCollectingOutput(cmd *exec.Cmd, what string) error {
	tmp, err := os.CreateTemp("", "pf-probe-")
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer os.Remove(tmp.Name())
	cmd.Stdout, cmd.Stderr = tmp, tmp
	rerr := cmd.Run()
	if rerr == nil {
		tmp.Close()
		return nil
	}
	_, _ = tmp.Seek(0, 0)
	raw := make([]byte, 4096)
	n, _ := tmp.Read(raw)
	tmp.Close()
	detail := strings.TrimSpace(string(raw[:n]))
	if detail == "" {
		return fmt.Errorf("%s: %w", what, rerr)
	}
	return fmt.Errorf("%s: %w: %s", what, rerr, detail)
}

// Candidates returns the union of a workflow's declared model candidates
// (07 §6.1 step 6): defaults.model_candidates plus every step-level
// override, order-preserving (defaults first, then steps in declaration
// order), de-duplicated.
func Candidates(fm *schema.Frontmatter) []string {
	seen := map[string]bool{}
	var out []string
	add := func(list []string) {
		for _, c := range list {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	if fm.Defaults != nil {
		add(fm.Defaults.ModelCandidates)
	}
	for _, st := range fm.Steps {
		add(st.ModelCandidates)
	}
	return out
}

// Row is one candidate's reachability outcome.
type Row struct {
	Candidate string `json:"candidate"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

// Report is the probe outcome for one bundle.
type Report struct {
	Bundle string `json:"bundle"`
	// BundleError is set when the bundle itself could not be parsed
	// (invalid/missing workflow.md): no candidate was probed.
	BundleError string `json:"bundle_error,omitempty"`
	Rows        []Row  `json:"rows"`
	// AllReachable: every probed candidate answered (false for any
	// unreachable candidate or a bundle error).
	AllReachable bool `json:"all_reachable"`
}

// ProbeBundle issues one micro request per declared candidate of one
// bundle, sequentially (install-time tool: no need for concurrency).
func ProbeBundle(ctx context.Context, prober Prober, bundle string, fm *schema.Frontmatter) *Report {
	outcomes := map[string]error{}
	for _, c := range Candidates(fm) {
		outcomes[c] = prober.Probe(ctx, c)
	}
	return bundleReport(bundle, fm, outcomes)
}

// bundleReport builds one bundle's report from probed candidate outcomes:
// every declared candidate gets a row, in the per-bundle candidate order;
// an outcome of nil marks the candidate reachable, an error marks it
// unreachable with that error's text.
func bundleReport(bundle string, fm *schema.Frontmatter, outcomes map[string]error) *Report {
	rep := &Report{Bundle: bundle, Rows: []Row{}}
	for _, c := range Candidates(fm) {
		row := Row{Candidate: c}
		if err := outcomes[c]; err != nil {
			row.Error = err.Error()
		} else {
			row.Reachable = true
		}
		rep.Rows = append(rep.Rows, row)
	}
	rep.AllReachable = true
	for _, r := range rep.Rows {
		if !r.Reachable {
			rep.AllReachable = false
		}
	}
	return rep
}

// ProbeWorkflowsDir probes the declared candidates of every bundle in a
// daemon workflows checkout (workflows/<bundle-name>/, the §6.1 step 4
// set). bundleFilter, when non-empty, restricts the run to that bundle.
// Reports are sorted by bundle name; a bundle without declared
// candidates reports an empty (all-reachable) row set.
//
// ONE micro request per UNIQUE candidate, not per (bundle, candidate)
// pair: a run probes exactly one adapter (--adapter, default pi), so
// the global (adapter, candidate) key reduces to the candidate string,
// and the reference bundles — which declare the same candidates — share
// the outcome. Each bundle's report still carries a row for every
// candidate it declares (its own per-bundle order), all reading the same
// shared probe result.
func ProbeWorkflowsDir(ctx context.Context, prober Prober, workflowsDir, bundleFilter string) ([]*Report, error) {
	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		return nil, fmt.Errorf("probe: workflows dir %s unreadable: %w", workflowsDir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if bundleFilter != "" && e.Name() != bundleFilter {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if bundleFilter != "" && len(names) == 0 {
		return nil, fmt.Errorf("probe: bundle %q not found under %s", bundleFilter, workflowsDir)
	}
	// First pass: validate every bundle's frontmatter (recording bundle
	// errors in their sorted-report slots) and collect the run's unique
	// candidate set, order-preserving by first declaration.
	reports := make([]*Report, len(names))
	type validBundle struct {
		idx int
		fm  *schema.Frontmatter
	}
	var valid []validBundle
	seen := map[string]bool{}
	var unique []string
	for i, name := range names {
		bundleDir := filepath.Join(workflowsDir, name)
		fm, ferr := schema.ValidateWorkflowFrontmatter(bundleDir, name)
		if ferr != nil {
			// A bundle that fails its own frontmatter validation cannot
			// be probed: record the failure as the bundle's report — the
			// install check fails closed without crashing the probe run.
			reports[i] = &Report{
				Bundle: name, BundleError: ferr.Error(), Rows: []Row{}, AllReachable: false,
			}
			continue
		}
		valid = append(valid, validBundle{idx: i, fm: fm})
		for _, c := range Candidates(fm) {
			if !seen[c] {
				seen[c] = true
				unique = append(unique, c)
			}
		}
	}
	// One micro request per unique candidate, sequentially (install-time
	// tool: no concurrency, §6.1 step 6).
	outcomes := make(map[string]error, len(unique))
	for _, c := range unique {
		outcomes[c] = prober.Probe(ctx, c)
	}
	// Second pass: every bundle's report reads the shared outcomes, so a
	// candidate declared by several bundles shows the SAME result in each.
	for _, v := range valid {
		reports[v.idx] = bundleReport(names[v.idx], v.fm, outcomes)
	}
	return reports, nil
}

// AllReachable reports whether every report says all candidates answered.
// The install verdict (§6.1 step 6): exit 0 only when this is true. An
// empty bundle set has nothing unreachable and trivially passes.
func AllReachable(reports []*Report) bool {
	for _, r := range reports {
		if !r.AllReachable {
			return false
		}
	}
	return true
}

// FormatTable renders the reachability table (candidate -> reachable/
// unreachable + error), the human output of `pf probe-candidates`.
func FormatTable(reports []*Report) string {
	var b strings.Builder
	for _, rep := range reports {
		fmt.Fprintf(&b, "bundle %s\n", rep.Bundle)
		if rep.BundleError != "" {
			fmt.Fprintf(&b, "  ERROR: %s\n", rep.BundleError)
			continue
		}
		if len(rep.Rows) == 0 {
			b.WriteString("  (no model_candidates declared)\n")
			continue
		}
		for _, row := range rep.Rows {
			status := "unreachable"
			if row.Reachable {
				status = "reachable"
			}
			if row.Error != "" {
				fmt.Fprintf(&b, "  %-40s %-11s %s\n", row.Candidate, status, row.Error)
			} else {
				fmt.Fprintf(&b, "  %-40s %s\n", row.Candidate, status)
			}
		}
	}
	return b.String()
}

// DefaultTimeout bounds one candidate's micro request.
const DefaultTimeout = 60 * time.Second
