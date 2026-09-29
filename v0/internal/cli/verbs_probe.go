package cli

// verbs_probe.go: `pf probe-candidates` — the install-face candidate
// liveness probe (07-deployment-pilot.md §6.1 step 6, polyforge#27):
// for the bundles in the daemon's workflows checkout, issue ONE micro
// request per declared model_candidates entry through the harness
// adapter's model call path and print a reachability table. Exit 0 only
// when every candidate answered. The probe is an install-face criterion,
// not a runtime dependency: it neither backstops nor changes the
// infrastructure-failure in-place resume semantics of a candidate that
// goes unreachable during the Pilot (01-workflow-skills.md §6.3).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/silgrid/polyforge/internal/workflow/probe"
)

var cmdProbeCandidates = &Command{
	Name:    "probe-candidates",
	Summary: "Install-time candidate liveness probe: one micro request per declared model candidate",
	Usage: "pf probe-candidates [--workflows-dir DIR] [--bundle NAME] [--adapter pi|claude]\n" +
		"                    [--pi-bin BIN] [--claude-bin BIN] [--timeout 60s]\n" +
		"  Probes the union of defaults.model_candidates and every step-level override\n" +
		"  of the bundle(s) in the daemon's workflows checkout; prints candidate ->\n" +
		"  reachable/unreachable (+ error); exits 0 only if all reachable.",
	Setup: func(fs *flag.FlagSet) {
		fs.String("workflows-dir", "", "daemon checkout workflows/ dir (defaults $POLYFORGE_HOME/workflows, then ~/.polyforge/workflows)")
		fs.String("bundle", "", "probe only this bundle (default: every bundle in the workflows dir)")
		fs.String("adapter", "pi", "harness adapter whose model call path to probe: pi | claude")
		fs.String("pi-bin", "pi", "pi executable (tests/fakes override)")
		fs.String("claude-bin", "claude", "claude executable (tests/fakes override)")
		fs.Duration("timeout", probe.DefaultTimeout, "per-candidate micro request timeout")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("probe-candidates: expected no positional arguments")
		}
		workflowsDir := stringFlag(fs, "workflows-dir")
		if workflowsDir == "" {
			home := os.Getenv("POLYFORGE_HOME")
			if home == "" {
				home = os.Getenv("HOME") + "/.polyforge"
			}
			workflowsDir = filepath.Join(home, "workflows")
		}
		adapter := stringFlag(fs, "adapter")
		var prober probe.Prober
		switch adapter {
		case "pi":
			prober = &probe.PiProber{Bin: stringFlag(fs, "pi-bin")}
		case "claude":
			prober = &probe.ClaudeProber{Bin: stringFlag(fs, "claude-bin")}
		default:
			return fmt.Errorf("probe-candidates: unknown adapter %q (want pi|claude)", adapter)
		}
		timeout := durationFlag(fs, "timeout")
		runCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		reports, err := probe.ProbeWorkflowsDir(runCtx, prober, workflowsDir, stringFlag(fs, "bundle"))
		if err != nil {
			return err
		}
		if ctx.JSON {
			blob, merr := json.MarshalIndent(reports, "", "  ")
			if merr != nil {
				return merr
			}
			fmt.Fprintln(ctx.Stdout, string(blob))
		} else {
			fmt.Fprint(ctx.Stdout, probe.FormatTable(reports))
		}
		if !probe.AllReachable(reports) {
			// Install verdict (§6.1 step 6): a candidate that never
			// answered fails the install check — exit 1.
			return fmt.Errorf("probe-candidates: unreachable candidates present; installation is not complete")
		}
		return nil
	},
}

func durationFlag(fs *flag.FlagSet, name string) time.Duration {
	v := fs.Lookup(name)
	if v == nil {
		return 0
	}
	d, _ := time.ParseDuration(v.Value.String())
	return d
}
