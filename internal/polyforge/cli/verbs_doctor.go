package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// verbs_doctor.go: `pf doctor` — the machine's single health face
// (polyforge#113). One command reports every health dimension the
// machine depends on:
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
//   - the connectivity family: server reach (any HTTP answer proves it),
//     daemon socket (GET /status over the UDS), auth (a signed aihub
//     read getting past the key boundary — the same read face the
//     projects check uses, no admin-only surface).
//
// Exit codes: 0 every check passes, 1 any check fails, 2 usage errors
// (the repo convention — unknown flags parse-fail before any check).
// --json emits {"ok": <bool>, "checks": [{"name", "status":
// "pass"|"fail"|"skip", "detail"}]}.
//
// The verb loads the config ITSELF (Command.NoConfigLoad): an unloadable
// config is the subject of the report, not an early exit.

// doctorCheck is one check row of the report.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | fail | skip
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
	docChConfig     = "config"
	docChMapping    = "workspace mapping"
	docChDirs       = "workspace dirs"
	docChProjects   = "aihub projects"
	docChServer     = "server reach"
	docChSocket     = "daemon socket"
	docChAuth       = "auth"
	docChWatchModel = "watch.summarize_model"
)

var cmdDoctor = &Command{
	Name:         "doctor",
	Summary:      "Check this machine's health: config, workspace mapping, connectivity",
	Usage:        "pf doctor [--json]",
	NoConfigLoad: true,
	Setup:        func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("doctor: unexpected arguments %v", args)
		}
		rep := runDoctorChecks(ctx)
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
		for _, name := range []string{docChMapping, docChDirs, docChProjects, docChServer, docChSocket, docChAuth, docChWatchModel} {
			add(name, "skip", "config-dependent check not run: the config could not be loaded")
		}
		return doctorReport{OK: false, Checks: checks}
	}
	where := ctx.ConfigPath
	if where == "" {
		where = DefaultConfigPath()
	}
	// The watch summarize-model check LEADS the report (still computed
	// from the loaded config): its name is the operator's first look at
	// the watch lane's health, and leading it keeps the check
	// unambiguous against other checks' details, which embed config and
	// socket paths (wi_LMSxPTQTtz5x).
	var watchModelStatus, watchModelDetail string
	{
		var reachable []string
		for _, h := range []string{"pi", "claude"} {
			if _, lerr := exec.LookPath(h); lerr == nil {
				reachable = append(reachable, h)
			}
		}
		switch {
		case len(reachable) == 0:
			watchModelStatus, watchModelDetail = "skip", "not run: no harness binary (pi, claude) is reachable on this machine"
		default:
			var missing []string
			for _, h := range reachable {
				if strings.TrimSpace(cfg.WatchSummarize[h]) == "" {
					missing = append(missing, h)
				}
			}
			if len(missing) > 0 {
				watchModelStatus = "fail"
				watchModelDetail = fmt.Sprintf("watch.summarize_model entry missing or empty for the reachable harness(es) %s — add to %s:\n"+
					"  [watch.summarize_model.<harness>]\n"+
					"  model = \"<provider>/<model>\"\n"+
					"then run pf doctor again", strings.Join(missing, ", "), where)
			} else {
				watchModelStatus = "pass"
				watchModelDetail = fmt.Sprintf("watch.summarize_model covered for every reachable harness (%s)", strings.Join(reachable, ", "))
			}
		}
	}
	add(docChWatchModel, watchModelStatus, watchModelDetail)
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
	sockOK, sockDetail := probeDaemonSocket(cfg.DaemonSocket)
	add(docChSocket, passOrFail(sockOK), sockDetail)

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

// probeDaemonSocket dials the daemon's UDS face and asks GET /status.
func probeDaemonSocket(sock string) (bool, string) {
	c := NewDaemonClient(sock)
	c.HTTP.Timeout = 5 * time.Second
	raw, err := c.Status()
	if err != nil {
		return false, fmt.Sprintf("daemon socket %s does not answer GET /status: %v — is the daemon running?", sock, err)
	}
	var st struct {
		MachineID string `json:"machine_id"`
	}
	_ = json.Unmarshal(raw, &st)
	return true, fmt.Sprintf("daemon socket %s answers (machine %s)", sock, st.MachineID)
}
