package cli

// verbs_watch.go: `pf watch-probe` — the watch lane's one-shot data face
// (wi_LMSxPTQTtz5x). The session-side watcher MECHANISM is the skill's
// watch.sh (copied verbatim into <workspace>/.polyforge/watchers/ at arm
// time); this verb is the data it consumes:
//
//   - the default face assembles the watch.Snapshot (execution state via
//     the daemon observe face, harness from the capsule's session.json,
//     transcript via the watch module's shared location resolution, delta
//     state next to the log), runs the delta-gated summarize and renders:
//     --json emits the exact documented field set (spec decision 4); the
//     human face is ONE line carrying the metadata fields inline in a
//     fixed order (the script only prefixes the RFC3339 UTC timestamp).
//   - --print-cadence prints the resolved cadence (flag > env > config,
//     defaults 3s / 1m / off) as one shell-sourceable line — the single
//     source the script evals once at startup, so cli.toml beats
//     PF_WATCH_* even inside the script's loops.
//
// The probe owns the delta state (<workspace>/.polyforge/watchers/<wi>.state);
// no --since-size-style flag exists, by construction.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/watch"
)

// Seams for unit tests: production wiring is the daemon observe face and
// the exec dispatcher; tests swap these to fakes.
var (
	watchStateSource = func(ctx *Context) watch.StateSource {
		return daemonStateSource{d: ctx.daemon()}
	}
	watchNewDispatcher = func() watch.Dispatcher {
		return watch.ExecDispatcher{}
	}
)

// daemonStateSource adapts the daemon observe projection (protocol.
// JobView over the UDS face) onto watch.StateSource — the CLI wires the
// daemon face; the watch module never imports the protocol vocabulary.
type daemonStateSource struct {
	d *DaemonClient
}

func (s daemonStateSource) ObserveExecState(wi string) (watch.ExecState, error) {
	raw, err := s.d.Observe(wi, 0)
	if err != nil {
		return watch.ExecState{}, err
	}
	var jv protocol.JobView
	if err := json.Unmarshal(raw, &jv); err != nil {
		return watch.ExecState{}, fmt.Errorf("decode the observe projection: %w", err)
	}
	es := watch.ExecState{
		Status: jv.Status,
		Step:   jv.CurrentStep,
	}
	if jv.Steps != nil {
		if sv, ok := jv.Steps[jv.CurrentStep]; ok && sv != nil {
			es.StepStatus = sv.Status
		}
	}
	if n := len(jv.Events); n > 0 {
		es.LatestEvent = jv.Events[n-1].Kind
		es.LatestSeq = jv.Events[n-1].Seq
	}
	if jv.Pending != nil {
		es.Pending = &watch.PendingQuestion{
			ID:       jv.Pending.ID,
			Step:     jv.Pending.Step,
			Question: jv.Pending.Question,
			AskedAt:  jv.Pending.AskedAt,
		}
	}
	return es, nil
}

var cmdWatchProbe = &Command{
	Name:    "watch-probe",
	Summary: "One-shot watch snapshot for a work item (the session-side watcher's data face)",
	Usage:   "pf watch-probe <id> [--json] [--poll D] [--log D] [--heartbeat D|off] [--print-cadence]",
	Setup: func(fs *flag.FlagSet) {
		fs.Var(&cadenceFlag{}, "poll", "watch_poll override — the flag layer of flag > env > config")
		fs.Var(&cadenceFlag{}, "log", "watch_log override (duration)")
		fs.Var(&cadenceFlag{allowOff: true}, "heartbeat", "watch_heartbeat override (duration or the literal off)")
		fs.Bool("print-cadence", false, "print the resolved cadence (one shell-sourceable line) and exit")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("watch-probe: expected exactly one work item id")
		}
		wi := args[0]
		cad := ctx.Cfg.ResolveWatchCadence(WatchFlagOverrides{
			Poll:      cadenceFlagPtr(fs, "poll"),
			Log:       cadenceFlagPtr(fs, "log"),
			Heartbeat: cadenceFlagPtr(fs, "heartbeat"),
		})
		if boolFlag(fs, "print-cadence") {
			// The cadence face needs nothing but the config: no daemon,
			// no capsule, no transcript, no model — the script sources it
			// once at startup, before the guard even runs.
			fmt.Fprintln(ctx.Stdout, renderCadence(cad))
			return nil
		}
		workspace, err := resolveWorkspace(ctx)
		if err != nil {
			return err
		}
		capsuleDir := filepath.Join(workspace, "capsules", wi)
		sess, err := readCapsuleSession(capsuleDir)
		if err != nil {
			return err
		}
		harness, err := watch.NormalizeHarness(sess.Harness)
		if err != nil {
			return err
		}
		snap, err := (watch.Probe{
			Deps: watch.Deps{
				WI:           wi,
				Workspace:    workspace,
				CapsuleDir:   capsuleDir,
				SessionsRoot: ctx.Cfg.HarnessSessionsRoot,
				Source:       watchStateSource(ctx),
			},
			Models:     watch.ModelConfig(ctx.Cfg.WatchSummarize),
			Harness:    harness,
			Dispatcher: watchNewDispatcher(),
		}).Run()
		if err != nil {
			return err
		}
		if ctx.JSON {
			b, err := json.Marshal(snap)
			if err != nil {
				return err
			}
			ctx.Out(b)
			return nil
		}
		fmt.Fprintln(ctx.Stdout, renderSnapshotLine(snap))
		return nil
	},
}

// resolveWorkspace answers the mapped workspace the process runs in. The
// watch lane is session-side, so the probe reads the machine layout from
// its own working directory: the mapped workspace that contains the cwd
// (the workspace itself or any directory inside it; the longest match
// wins). The mapping (cli.toml's [workspace] sections) is the only
// placement source (polyforge#113).
func resolveWorkspace(ctx *Context) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("watch-probe: resolve the working directory: %w", err)
	}
	if ctx.Cfg.Mapping == nil || ctx.Cfg.Mapping.Empty() {
		return "", fmt.Errorf("watch-probe: no workspace mapping is configured — add a [workspace.\"<path>\"] section to cli.toml; pf doctor checks the mapping")
	}
	best := ""
	for _, ws := range ctx.Cfg.Mapping.Workspaces() {
		if wd == ws || strings.HasPrefix(wd, ws+string(filepath.Separator)) {
			if len(ws) > len(best) {
				best = ws
			}
		}
	}
	if best == "" {
		return "", fmt.Errorf("watch-probe: the working directory %s is not inside a mapped workspace — run the probe from the workspace whose watchers/ holds this WI (pf doctor shows the mapping)", wd)
	}
	return best, nil
}

// readCapsuleSession reads the capsule's session.json: the recorded
// harness decides which summarize-model key and which dispatch binary
// apply.
func readCapsuleSession(capsuleDir string) (capsule.Session, error) {
	var sess capsule.Session
	path := filepath.Join(capsuleDir, capsule.SessionJSON)
	raw, err := os.ReadFile(path)
	if err != nil {
		return sess, fmt.Errorf("watch-probe: read %s: %w — the probe narrates a derived capsule; is %s a capsule of a mapped workspace?", path, err, capsuleDir)
	}
	if err := json.Unmarshal(raw, &sess); err != nil {
		return sess, fmt.Errorf("watch-probe: decode %s: %w", path, err)
	}
	return sess, nil
}

// renderCadence is the --print-cadence face: ONE shell-sourceable line of
// KEY=VALUE pairs (the script evals it), the direct seconds spellings
// sleep can consume, and the watch module's log-tail constant (spec
// decision 2: the tail constants live in the watch module, not the
// script).
func renderCadence(c WatchCadence) string {
	hb := "off"
	if c.Heartbeat > 0 {
		hb = watch.FormatDuration(c.Heartbeat)
	}
	return fmt.Sprintf("watch_poll=%s watch_log=%s watch_heartbeat=%s watch_poll_seconds=%d watch_log_seconds=%d watch_heartbeat_seconds=%d watch_log_tail_lines=%d",
		watch.FormatDuration(c.Poll), watch.FormatDuration(c.Log), hb,
		int64(c.Poll/time.Second), int64(c.Log/time.Second), int64(c.Heartbeat/time.Second),
		watch.WatchLogTailLines)
}

// renderSnapshotLine is the probe's own human output: the metadata fields
// inline in a fixed order, ONE line — the script prefixes the RFC3339 UTC
// timestamp and appends the line to the watcher log verbatim. The closing
// clause is the delta verdict: the summary line, the raw summarize error,
// or the silent-transcript note with its climbing age.
func renderSnapshotLine(snap *watch.Snapshot) string {
	pending := "-"
	if snap.Pending != nil {
		pending = snap.Pending.ID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "wi=%s status=%s step=%s step_status=%s latest_event=%s pending_question=%s transcript=%s size=%d age=%s",
		snap.WI, snap.Status, snap.Step, snap.StepStatus, snap.LatestEvent, pending,
		snap.TranscriptPath, snap.TranscriptSize, snap.TranscriptAge)
	switch {
	case snap.SummaryError != nil:
		fmt.Fprintf(&b, " summary_error: %s", *snap.SummaryError)
	case snap.Summary != nil:
		fmt.Fprintf(&b, " summary: %s", *snap.Summary)
	default:
		fmt.Fprintf(&b, " no new output, age %s", snap.TranscriptAge)
	}
	return b.String()
}

// cadenceFlag is the --poll/--log/--heartbeat flag layer: a duration, or
// (heartbeat only) the literal off. Absent stays distinguishable from a
// zero value.
type cadenceFlag struct {
	val      time.Duration
	set      bool
	allowOff bool
}

func (f *cadenceFlag) String() string { return f.val.String() }

func (f *cadenceFlag) Set(s string) error {
	if f.allowOff && s == "off" {
		f.val, f.set = 0, true
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration%s", s, offHint(f.allowOff))
	}
	if d < 0 {
		return fmt.Errorf("%q must not be negative", s)
	}
	f.val, f.set = d, true
	return nil
}

// cadenceFlagPtr answers the flag's parsed value, nil when the flag was
// absent (the precedence's flag layer is "set beats everything").
func cadenceFlagPtr(fs *flag.FlagSet, name string) *time.Duration {
	v, ok := fs.Lookup(name).Value.(*cadenceFlag)
	if !ok || !v.set {
		return nil
	}
	d := v.val
	return &d
}
