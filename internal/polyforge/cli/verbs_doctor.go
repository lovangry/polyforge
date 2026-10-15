package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/harness"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
	"github.com/silgrid/polyforge/internal/polyforge/mapping"
	"github.com/silgrid/polyforge/internal/polyforge/workspace"
)

// verbs_doctor.go: `pf doctor` — the daemon's single health face
// (polyforge#113). One command reports every health dimension the
// daemon depends on:
//
//   - config: an unloadable (old-shape/malformed) file fails here with
//     migration guidance, and every config-dependent check is reported
//     skip — never a faked green;
//   - the workspace mapping family: mapping absent (guidance to add the
//     [workspace] shape), workspace dirs (each mapped path must pre-exist
//     as a directory), aihub projects (every mapped project known to
//     aihub, named with its workspace on a miss; the inverse-ambiguity
//     and duplicate refusals surface through the config check, which
//     names both workspaces);
//   - workspace yaml: every mapped workspace's .polyforge.yaml loads
//     through the daemon's own loader, its per-step model keys checked
//     against the step ids of the workflows the daemon runs (the
//     workflows dir GET /status reports) — the same refusal a claim
//     would meet (DAEMON-414), surfaced before any claim;
//   - the step harnesses (polyforge#159): the workspace yaml check also
//     runs the model/harness compatibility check (the claim's own
//     DAEMON-414 refusal); when any step resolves to the claude harness,
//     "harness claude" judges the daemon's view of the Claude Code CLI
//     (GET /status?harnesses=1: binary found → else fail; version in the
//     verified set, credential variables set, ANTHROPIC_BASE_URL unset →
//     else warn) and lists every claude step with its resolution sources.
//     Credential values are never shown — set/unset only;
//   - the connectivity family: server reach (any HTTP answer proves it),
//     daemon socket (GET /status over the UDS), auth (a signed aihub
//     read getting past the key boundary — the same read face the
//     projects check uses, no admin-only surface);
//   - the harness progress-display family (doctor_display.go): Claude
//     Code's statusLine and pi's pf-chat extension — pass/warn/skip
//     only, never fail, so they never move the exit code. `--harness`
//     runs this family alone (tools/install.sh prints it).
//
// Exit codes: 0 every check passes, 1 any check fails, 2 usage errors
// (the repo convention — unknown flags parse-fail before any check).
// --json emits {"ok": <bool>, "checks": [{"name", "status":
// "pass"|"fail"|"skip"|"warn", "detail"}]}.
//
// The verb loads the config ITSELF (Command.NoConfigLoad): an unloadable
// config is the subject of the report, not an early exit.

// doctorCheck is one check row of the report.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | fail | skip | warn
	Detail string `json:"detail"`
}

// doctorReport is the --json document shape.
type doctorReport struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

// The check names (stable faces; the failure DETAILS carry the pinned
// specifics — the missing path, the project and its workspace, the
// retired key).
const (
	docChConfig   = "config"
	docChMapping  = "workspace mapping"
	docChDirs     = "workspace dirs"
	docChYaml     = "workspace yaml"
	docChProjects = "aihub projects"
	docChServer   = "server reach"
	docChSocket   = "daemon socket"
	docChAuth     = "auth"
	docChClaude   = "harness claude"
)

var cmdDoctor = &Command{
	Name:         "doctor",
	Group:        GroupDiag,
	Summary:      "Check this daemon's health: config, workspace mapping, connectivity, progress display",
	Usage:        "pf doctor [--harness] [--json]",
	NoConfigLoad: true,
	Setup: func(fs *flag.FlagSet) {
		fs.Bool("harness", false, "run only the harness progress-display checks")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("doctor: unexpected arguments %v", args)
		}
		var rep doctorReport
		if boolFlag(fs, "harness") {
			rep = doctorReport{OK: true, Checks: harnessDisplayChecks()}
		} else {
			rep = runDoctorChecks(ctx)
		}
		if ctx.JSON {
			b, err := json.Marshal(rep)
			if err != nil {
				return err
			}
			ctx.Out(b)
		} else {
			for _, c := range rep.Checks {
				fmt.Fprintf(ctx.Stdout, "%-6s %s  %s\n", c.Status, c.Name, c.Detail)
			}
			fmt.Fprintf(ctx.Stdout, "\nok: %t\n", rep.OK)
		}
		// Exit-code semantics are the report's, not a wire error's: 0
		// all-pass, 1 any-fail (renderError prints the one-line verdict;
		// the report above is the payload).
		if !rep.OK {
			return fmt.Errorf("doctor: %d check(s) failed — see the report above", countFails(rep))
		}
		return nil
	},
}

// countFails counts the failing checks of a report.
func countFails(rep doctorReport) int {
	n := 0
	for _, c := range rep.Checks {
		if c.Status == "fail" {
			n++
		}
	}
	return n
}

// runDoctorChecks executes the check ladder and assembles the report.
// Ordering matters: config first; when it fails, every config-dependent
// check is reported skip (not-run) rather than silently passing — the
// not-faked-green contract. The aihub-flavored checks likewise refuse to
// fake a verdict behind an unreachable server.
func runDoctorChecks(ctx *Context) doctorReport {
	checks := []doctorCheck{}
	add := func(name, status, detail string) {
		checks = append(checks, doctorCheck{Name: name, Status: status, Detail: detail})
	}

	cfg, err := LoadConfig(ctx.ConfigPath)
	if err != nil {
		add(docChConfig, "fail", err.Error()+" — see the install docs for the migration steps")
		for _, name := range []string{docChMapping, docChDirs, docChYaml, docChClaude, docChProjects, docChServer, docChSocket, docChAuth} {
			add(name, "skip", "config-dependent check not run: the config could not be loaded")
		}
		checks = append(checks, harnessDisplayChecks()...)
		return doctorReport{OK: false, Checks: checks}
	}
	where := ctx.ConfigPath
	if where == "" {
		where = DefaultConfigPath()
	}
	add(docChConfig, "pass", "loaded from "+where)

	// Mapping absent: the fresh-template shape (zero [workspace]
	// sections) fails with the shape guidance; otherwise pass.
	if cfg.Mapping.Empty() {
		add(docChMapping, "fail", "no workspace mapping is configured: add at least one [workspace.\"<path>\"] section with a projects = [...] list to the config (`pf doctor` re-checks after editing)")
	} else {
		add(docChMapping, "pass", fmt.Sprintf("%d workspace(s) mapping %d project(s)", len(cfg.Mapping.Workspaces()), len(cfg.Mapping.Projects())))
	}

	// Workspace dirs: every mapped workspace must pre-exist as a
	// directory (the daemon creates nothing but capsules/ inside it).
	if cfg.Mapping.Empty() {
		add(docChDirs, "skip", "not run: the mapping is absent")
	} else {
		var missing []string
		for _, ws := range cfg.Mapping.Workspaces() {
			st, serr := os.Stat(ws)
			if serr != nil || !st.IsDir() {
				missing = append(missing, ws)
			}
		}
		if len(missing) > 0 {
			add(docChDirs, "fail", "mapped workspace directory missing or not a directory: "+strings.Join(missing, ", ")+" — create it (the daemon never does) or fix the [workspace] key")
		} else {
			add(docChDirs, "pass", fmt.Sprintf("all %d mapped workspace(s) exist as directories", len(cfg.Mapping.Workspaces())))
		}
	}

	// Server reach: ANY http answer (even an error status) proves the
	// server is reachable; only a transport failure is a fail.
	reachOK, reachDetail := probeServerReach(cfg.Server)
	add(docChServer, passOrFail(reachOK), reachDetail)

	// Daemon socket: independent of aihub.
	sockOK, sockDetail, status := probeDaemonSocket(cfg.DaemonSocket)
	add(docChSocket, passOrFail(sockOK), sockDetail)

	if cfg.Mapping.Empty() {
		add(docChYaml, "skip", "not run: the mapping is absent")
		add(docChClaude, "skip", "not run: the mapping is absent")
	} else {
		st, detail, claudeSteps := workspaceYamlCheck(cfg.Mapping, status.WorkflowsDir, sockOK)
		add(docChYaml, st, detail)
		// the daemon's harness view (it may run `claude --version`) is
		// asked for only when some step actually runs on claude
		var harnesses map[string]HarnessStatus
		if len(claudeSteps) > 0 && sockOK {
			harnesses = probeDaemonHarnesses(cfg.DaemonSocket)
		}
		st, detail = claudeHarnessCheck(claudeSteps, status.WorkflowsDir, sockOK, harnesses)
		add(docChClaude, st, detail)
	}

	// Auth + project knowledge ride the aihub PROJECT read (the same
	// face the CLI uses; no admin-only surface). Reachability gates both:
	// behind a dead server neither can be proven — skip, never fake.
	if !reachOK {
		add(docChProjects, "skip", "not run: aihub unreachable ("+cfg.Server+")")
		add(docChAuth, "skip", "not run: aihub unreachable ("+cfg.Server+")")
	} else if cfg.Mapping.Empty() {
		add(docChProjects, "skip", "not run: the mapping is absent")
		add(docChAuth, "skip", "not run: no mapped project to read with")
	} else {
		client := aihubclient.New(cfg.Server, cfg.APIKey)
		var projectFails []string
		authDenied := false
		authProven := false
		for _, project := range cfg.Mapping.Projects() {
			ws, _ := cfg.Mapping.WorkspaceFor(project)
			_, err := client.GetProject(project)
			if err == nil {
				authProven = true
				continue
			}
			var ae *api.Error
			if errors.As(err, &ae) {
				switch ae.HTTPStatus() {
				case 401, 403:
					authDenied = true
				case 404:
					projectFails = append(projectFails, fmt.Sprintf("project %q (workspace %s) is unknown to aihub: %s", project, ws, ae.Error()))
					continue
				}
			}
			projectFails = append(projectFails, fmt.Sprintf("project %q (workspace %s): %v", project, ws, err))
		}
		if len(projectFails) > 0 {
			add(docChProjects, "fail", strings.Join(projectFails, "; "))
		} else {
			add(docChProjects, "pass", fmt.Sprintf("all %d mapped project(s) are known to aihub", len(cfg.Mapping.Projects())))
		}
		switch {
		case authDenied:
			add(docChAuth, "fail", fmt.Sprintf("aihub rejected the credential with a 401/403 answer — fix the api key (config api_key / PF_API_KEY)"))
		case authProven:
			add(docChAuth, "pass", "the api key got past aihub's auth boundary (the project read answered)")
		default:
			add(docChAuth, "skip", "not proven: the aihub project read failed for another reason")
		}
	}

	checks = append(checks, harnessDisplayChecks()...)

	ok := true
	for _, c := range checks {
		if c.Status == "fail" {
			ok = false
		}
	}
	return doctorReport{OK: ok, Checks: checks}
}

func passOrFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

// probeServerReach classifies server reachability.
func probeServerReach(server string) (bool, string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimSuffix(server, "/") + "/api/v1/projects")
	if err != nil {
		return false, fmt.Sprintf("aihub server %s unreachable: %v", server, err)
	}
	resp.Body.Close()
	return true, fmt.Sprintf("aihub server %s answered (HTTP %d)", server, resp.StatusCode)
}

// probeDaemonSocket dials the daemon's UDS face and asks GET /status;
// it also answers the parsed status (the workflows dir the daemon
// reports; zero when unreachable).
func probeDaemonSocket(sock string) (bool, string, StatusResponse) {
	c := NewDaemonClient(sock)
	c.HTTP.Timeout = 5 * time.Second
	var st StatusResponse
	raw, err := c.Status()
	if err != nil {
		return false, fmt.Sprintf("daemon socket %s does not answer GET /status: %v — is the daemon running?", sock, err), st
	}
	_ = json.Unmarshal(raw, &st)
	return true, fmt.Sprintf("daemon socket %s answers (daemon %s)", sock, st.MachineID), st
}

// probeDaemonHarnesses asks GET /status?harnesses=1 — the daemon's view
// of the step harness binaries (nil when it does not answer or is too old
// to report it).
func probeDaemonHarnesses(sock string) map[string]HarnessStatus {
	c := NewDaemonClient(sock)
	c.HTTP.Timeout = 15 * time.Second
	raw, err := c.StatusWithHarnesses()
	if err != nil {
		return nil
	}
	var st StatusResponse
	_ = json.Unmarshal(raw, &st)
	return st.Harnesses
}

// workspaceYamlCheck loads every mapped workspace's .polyforge.yaml the
// way a claim does, and runs the claim's model/harness compatibility
// check (polyforge#159) for every mapped project over the daemon's
// installed workflows. Without the daemon's workflows dir neither the
// per-step keys nor the model/harness pairs can be judged: a yaml that
// otherwise loads is a warn, never a pass. It also answers every step
// that resolves to the claude harness (the "harness claude" check's
// subject).
func workspaceYamlCheck(m *mapping.Plan, workflowsDir string, daemonUp bool) (string, string, []workspace.StepResolution) {
	var fails []string
	var claudeSteps []workspace.StepResolution
	bundles, _ := workspace.Bundles(workflowsDir, "")
	bundleNames := make([]string, 0, len(bundles))
	for name := range bundles {
		bundleNames = append(bundleNames, name)
	}
	sort.Strings(bundleNames)
	present := 0
	for _, ws := range m.Workspaces() {
		_, serr := os.Stat(workspace.PathFor(ws))
		if serr == nil {
			present++
		}
		cfg, err := workspace.Load(ws, workflowsDir, m.ProjectsOf(ws))
		if err != nil {
			fails = append(fails, err.Error())
			continue
		}
		if workflowsDir == "" {
			continue
		}
		if err := workspace.CheckHarness(ws, cfg, workflowsDir, m.ProjectsOf(ws), "", nil); err != nil {
			fails = append(fails, err.Error())
		}
		for _, project := range m.ProjectsOf(ws) {
			for _, name := range bundleNames {
				for _, r := range workspace.ResolveSteps(name, bundles[name], cfg, project, nil) {
					if r.Harness == harness.Claude {
						claudeSteps = append(claudeSteps, r)
					}
				}
			}
		}
	}
	switch {
	case len(fails) > 0:
		return "fail", strings.Join(fails, "; ") + " — a claim of these projects is refused (DAEMON-414) until fixed", claudeSteps
	case present == 0 && workflowsDir != "":
		return "pass", "no mapped workspace carries a " + workspace.FileName + "; every bundle step's model/harness pair is runnable", claudeSteps
	case present == 0:
		return "pass", "no mapped workspace carries a " + workspace.FileName, claudeSteps
	case workflowsDir == "" && !daemonUp:
		return "warn", fmt.Sprintf("%d %s file(s) parse, but the per-step keys and model/harness pairs are unchecked: the daemon is unreachable, so its workflows dir is unknown", present, workspace.FileName), claudeSteps
	case workflowsDir == "":
		return "warn", fmt.Sprintf("%d %s file(s) parse, but the per-step keys and model/harness pairs are unchecked: the daemon reported no workflows dir (an older daemon?)", present, workspace.FileName), claudeSteps
	}
	return "pass", fmt.Sprintf("%d %s file(s) load; every per-step key is a step id of %s and every step's harness serves its candidates", present, workspace.FileName, workflowsDir), claudeSteps
}

// claudeHarnessCheck judges the Claude Code CLI the daemon would run for
// the steps resolving to the claude harness (polyforge#159). It never
// shows a credential value, never logs in and never calls a model: the
// daemon reports the binary, `claude --version` and set/unset only.
func claudeHarnessCheck(steps []workspace.StepResolution, workflowsDir string, daemonUp bool, hs map[string]HarnessStatus) (string, string) {
	if len(steps) == 0 {
		if workflowsDir == "" {
			return "pass", "no claude CLI needed as far as can be judged: the daemon's workflows are unknown, so only the workspace yaml was read"
		}
		return "pass", "no claude CLI needed: no step selects the claude harness (every step runs on pi)"
	}
	var rows []string
	for i, r := range steps {
		if i == 12 {
			rows = append(rows, fmt.Sprintf("… %d more", len(steps)-i))
			break
		}
		models := "the CLI's default model"
		if len(r.Served) > 0 {
			models = "[" + strings.Join(r.Served, ", ") + "] (" + r.ModelsFrom + ")"
		}
		rows = append(rows, fmt.Sprintf("%s/%s/%s: claude (%s), %s", r.Project, r.Bundle, r.Step, r.HarnessFrom, models))
	}
	table := fmt.Sprintf("%d step(s) run on claude: %s", len(steps), strings.Join(rows, "; "))
	if !daemonUp {
		return "skip", table + " — the CLI is unchecked: the daemon is unreachable"
	}
	h, ok := hs[harness.Claude]
	if !ok {
		return "warn", table + " — the CLI is unchecked: the daemon reported no harness status (an older daemon?)"
	}
	if !h.Found {
		return "fail", fmt.Sprintf("%s — the claude binary %q does not resolve on the daemon's PATH: install Claude Code (logged in by a human) or set claude_bin / --claude-bin", table, h.Bin)
	}
	status := "pass"
	var notes []string
	warn := func(n string) {
		status = "warn"
		notes = append(notes, n)
	}
	switch {
	case h.VersionError != "":
		warn("version unknown (" + h.VersionError + ")")
	case !h.Verified:
		warn(fmt.Sprintf("version %s is not in the verified set (the adapter's argv/stream contract was verified on another version)", h.Version))
	}
	tokenSet := h.AuthTokenEnv == "set"
	switch {
	case !tokenSet && h.APIKeyEnv != "set":
		warn("no credential variable is visible to the daemon (neither ANTHROPIC_AUTH_TOKEN — env — nor ANTHROPIC_API_KEY); the credential authority layer is the claude-config settings.json env block (polyforge#171), which the daemon itself reports set/unset in the doctor tokens below")
	case tokenSet && h.APIKeyEnv == "set":
		warn("both ANTHROPIC_AUTH_TOKEN and ANTHROPIC_API_KEY are set — which one Claude Code uses is unverified")
	}
	if h.BaseURLEnv == "set" {
		warn("ANTHROPIC_BASE_URL is set in the daemon's environment: claude steps inherit it (they are meant to use the official API)")
	}
	detail := fmt.Sprintf("%s — %s %s at %s; auth token env %s, api key env %s; background tasks %s",
		table, h.Bin, h.Version, h.Path, h.AuthTokenEnv, h.APIKeyEnv, h.BackgroundTasks)
	if len(notes) > 0 {
		detail += " — " + strings.Join(notes, "; ")
	}
	return status, detail
}
