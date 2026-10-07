package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// ---------------------------------------------------------------------------
// submit — interactive submit of one WI (claim, or takeover).
// ---------------------------------------------------------------------------

var cmdSubmit = &Command{
	Name:    "submit",
	Summary: "Submit a WI for execution on this machine (claim, or takeover)",
	Usage:   "pf submit <id> [--takeover] [--priority low|normal|high|urgent]",
	Setup: func(fs *flag.FlagSet) {
		fs.Bool("takeover", false, "admin force-takeover instead of a plain claim")
		fs.String("priority", "normal", "queue priority (low|normal|high|urgent)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("submit: expected exactly one work item id")
		}
		if err := submitPrecheck(ctx, args[0]); err != nil {
			return err
		}
		var (
			raw json.RawMessage
			err error
		)
		d := ctx.daemon()
		if boolFlag(fs, "takeover") {
			raw, err = d.Takeover(args[0], stringFlag(fs, "priority"))
		} else {
			raw, err = d.Claim(args[0], stringFlag(fs, "priority"))
		}
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var sr SubmitResponse
			if err := json.Unmarshal(raw, &sr); err == nil && sr.Accepted {
				fmt.Fprintf(ctx.Stdout, "submitted %s (accepted by the local daemon)\n", sr.WorkItemID)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		})
		return nil
	},
}

// submitPrecheck asks aihub — with the CALLER's own api key, not the
// daemon's machine key — for the work item before the local daemon is
// involved: a typed CORE-404 (unknown or hidden id) and an explicit
// terminal status refuse early; every other read outcome defers to the
// daemon's claim, which stays the authority.
func submitPrecheck(ctx *Context, wiID string) error {
	wi, err := ctx.aihub().GetWorkItem(wiID)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.ErrCoreNotFound {
			return err
		}
		return nil
	}
	switch wi.Status {
	case api.StatusWrapped, api.StatusFailed, api.StatusCanceled:
		return fmt.Errorf("submit: %s is terminal (%s); no claim, resume or takeover can start", wiID, wi.Status)
	}
	return nil
}

// ---------------------------------------------------------------------------
// observe — the daemon observe projection (protocol.JobView): current
// step, step table, budget, pending question, recent events.
// ---------------------------------------------------------------------------

var cmdObserve = &Command{
	Name:    "observe",
	Summary: "Observe WI progress (daemon projection: steps, budget, question, events)",
	Usage:   "pf observe <id> [--after N] [--read-through=false]",
	Setup: func(fs *flag.FlagSet) {
		fs.Int64("after", 0, "last received event cursor (first call: 0)")
		fs.Bool("read-through", true, "on the daemon's not-monitored answer, fall back to the aihub progress read-through (layer=read-through)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("observe: expected exactly one work item id")
		}
		raw, err := ctx.daemon().Observe(args[0], int64Flag(fs, "after"))
		if err != nil {
			// The daemon's not-monitored answer (DAEMON-404 on the observe
			// face) is the read-through trigger: fall back to the aihub
			// progress endpoint (02 §5.2) unless the caller opted out with
			// --read-through=false (the old fail-fast behavior).
			if boolFlag(fs, "read-through") && daemonNotMonitored(err) {
				return observeReadThrough(ctx, args[0], err)
			}
			return err
		}
		ctx.finish(raw, func() {
			var jv protocol.JobView
			if err := json.Unmarshal(raw, &jv); err != nil {
				printJSON(ctx.Stdout, raw)
			} else {
				// v1.5: render the session fingerprint (the PUBLIC holder
				// handle) instead of the deleted lineage's attempt/
				// generation fields. Best-effort aihub WI read: the daemon
				// layer's projection carries no session fields (protocol.
				// JobView is frozen), so the additive WI read supplies them;
				// on failure the session line is skipped, never fabricated.
				renderObserve(ctx.Stdout, &jv, wiSessionView(ctx, args[0]))
			}
		})
		return nil
	},
}

// daemonNotMonitored reports whether err is the daemon's not-monitored
// answer (DAEMON-404 from the observe face: no local capsule for the WI).
func daemonNotMonitored(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Code == api.ErrDaemonNotFound
}

// observeReadThrough is the second observe layer: the aihub §5.2 progress
// read-through, called with the CALLER's ordinary api key. The rendered
// view is visibly a different layer (labeled layer=read-through, with the
// as-of-last-publish freshness note); daemon-layer-only content (the
// live events ring, --after cursor semantics, pending-question
// interactivity) has no read-through equivalent and is never fabricated.
// daemonErr is the daemon's not-monitored answer, surfaced when the
// read-through itself fails so neither layer's failure is swallowed.
func observeReadThrough(ctx *Context, wiID string, daemonErr error) error {
	raw, err := ctx.aihub().GetProgressRaw(wiID)
	if err != nil {
		// Neither layer's failure is swallowed: the combined error names
		// the daemon's not-monitored answer AND the read-through miss.
		// Typed read-through errors keep their code (the CLI's exit-code
		// and error rendering key on it); renderError prints the detail.
		var ae *api.Error
		if errors.As(err, &ae) {
			return &api.Error{Code: ae.Code, Extra: ae.Extra,
				Detail: fmt.Sprintf("daemon does not monitor %s (%s); read-through failed: %s",
					wiID, daemonErr, ae.Detail)}
		}
		return fmt.Errorf("observe: daemon does not monitor %s (%s); read-through failed: %w",
			wiID, daemonErr, err)
	}
	ctx.finish(raw, func() {
		var p api.ProgressResponse
		if err := json.Unmarshal(raw, &p); err != nil {
			printJSON(ctx.Stdout, raw)
			return
		}
		renderReadThrough(ctx.Stdout, &p)
	})
	return nil
}

// ---------------------------------------------------------------------------
// answer — answer the pending needs_input question: --question names
// it explicitly, or the unique pending question is used (the JobView
// carries at most one).
// ---------------------------------------------------------------------------

var cmdAnswer = &Command{
	Name:    "answer",
	Summary: "Answer the pending needs_input question of a WI",
	Usage:   "pf answer <id> --text \"...\" [--question q_xxx] [--answer-id a_xxxx] [--by user]",
	Setup: func(fs *flag.FlagSet) {
		fs.String("question", "", "question id (default: the unique pending question)")
		fs.String("text", "", "natural-language answer (required)")
		fs.String("answer-id", "", "explicit answer id (default: generated a_ + 6 chars)")
		fs.String("by", "", "answerer user id (default: config user_id / PF_USER_ID)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("answer: expected exactly one work item id")
		}
		text := stringFlag(fs, "text")
		if text == "" {
			return fmt.Errorf("answer: --text is required")
		}
		who := stringFlag(fs, "by")
		if who == "" {
			who = ctx.Cfg.UserID
		}
		if who == "" {
			return fmt.Errorf("answer: answered_by identity required (--by or PF_USER_ID / user_id in config)")
		}
		d := ctx.daemon()
		question := stringFlag(fs, "question")
		if question == "" {
			// Interactive default: the unique pending question. Driver WIs
			// answer through the daemon's execution.json fallback, so an
			// observe failure here is not fatal — pass empty and let the
			// daemon's answer route resolve it.
			raw, err := d.Observe(args[0], 0)
			if err == nil {
				var jv protocol.JobView
				if json.Unmarshal(raw, &jv) == nil && jv.Pending != nil {
					question = jv.Pending.ID
				}
			}
			// question may remain "" (driver WI, observe 404): the daemon
			// answer route's pendingQuestionID fallback handles it.
		}
		answerID := stringFlag(fs, "answer-id")
		if answerID == "" {
			answerID = newAnswerID()
		}
		payload := AnswerPayload{
			QuestionID: question,
			AnswerID:   answerID,
			AnswerText: text,
			AnsweredAt: time.Now().UTC().Format(time.RFC3339),
			AnsweredBy: who,
		}
		raw, err := d.Answer(args[0], payload)
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			fmt.Fprintf(ctx.Stdout, "answer %s delivered for question %s\n", payload.AnswerID, payload.QuestionID)
		})
		return nil
	},
}

// newAnswerID mints an answer id: "a_" + 6 random hex chars.
func newAnswerID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("a_%06x", time.Now().UnixNano()&0xffffff)
	}
	return "a_" + hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// batch-confirm — Batch wave: read the scheduler recommendation, let the
// human confirm (possibly edited), submit the wave in one call.
// ---------------------------------------------------------------------------

var cmdBatchConfirm = &Command{
	Name:    "batch-confirm",
	Summary: "Batch wave: show the recommendation, confirm and submit it",
	Usage: "pf batch-confirm                # show the recommended wave only (no Job is created)\n" +
		"pf batch-confirm --all           # confirm and submit the whole recommendation\n" +
		"pf batch-confirm <id> [<id>...]  # confirm and submit exactly this set",
	Setup: func(fs *flag.FlagSet) {
		fs.Bool("all", false, "submit the entire recommendation as the confirmed wave")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		all := boolFlag(fs, "all")
		ids := args
		d := ctx.daemon()

		// Always fetch the scheduler recommendation first (the daemon
		// forwards the aihub ready-queue answer read-only).
		raw, err := d.Mode(ModeRequest{Action: "get_recommendation"})
		if err != nil {
			return err
		}
		var rec struct {
			Items []api.WorkItem `json:"items"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("batch-confirm: cannot parse recommendation: %w", err)
		}

		if !all && len(ids) == 0 {
			// Read-only recommendation display; NO Job is created.
			ctx.finish(raw, func() {
				fmt.Fprintf(ctx.Stdout, "%d recommended work item(s)\n", len(rec.Items))
				renderWITable(ctx.Stdout, wiViewsOf(rec.Items))
				fmt.Fprintln(ctx.Stdout, "\nconfirm with: pf batch-confirm --all  or  pf batch-confirm <id> ...")
			})
			return nil
		}
		if all {
			ids = make([]string, 0, len(rec.Items))
			for _, e := range rec.Items {
				ids = append(ids, e.ID)
			}
		}
		if len(ids) == 0 {
			return fmt.Errorf("batch-confirm: the recommendation is empty; nothing to submit")
		}
		sraw, err := d.BatchSubmit(ids)
		if err != nil {
			return err
		}
		ctx.finish(sraw, func() {
			fmt.Fprintf(ctx.Stdout, "wave submitted: %d work item(s)\n", len(ids))
			printJSON(ctx.Stdout, sraw)
		})
		return nil
	},
}

// ---------------------------------------------------------------------------
// pause — operator pause of a running WI: the daemon closes the job
// locally AND the hub record is marked clean_pause=true so a daemon
// restart never auto-resumes an operator-paused work item (M3).
// ---------------------------------------------------------------------------

var cmdPause = &Command{
	Name:    "pause",
	Summary: "Pause a running WI at the next safe boundary (operator pause: clean_pause on the hub)",
	Usage:   "pf pause <id>",
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("pause: expected exactly one work item id")
		}
		// Merge amend: the early hub PATCH + undo dance is GONE — the
		// pause is one atomic fenced transition now. The daemon's pause
		// close carries Status=paused + CleanPause=true on the SAME
		// SessionEnd that flips the status, so there is no crash window
		// in which an operator-paused WI could be auto-resumed, and no
		// undo path that could strand a stale marker on a non-paused WI.
		raw, err := ctx.daemon().Pause(args[0])
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			fmt.Fprintf(ctx.Stdout, "pause requested for %s (clean_pause set on the hub; restart will not auto-resume it)\n", args[0])
		})
		return nil
	},
}

// patchCleanPause PATCHes the work item with the additive clean_pause
// field (the aihub server accepts-and-ignores unknown PATCH keys until
// Lane A implements the pause face).
func patchCleanPause(ctx *Context, wiID string, value bool) error {
	_, err := ctx.aihub().PatchWorkItem(wiID, map[string]any{"clean_pause": value})
	return err
}

// setCleanPause wraps patchCleanPause with the warning-only error
// policy of the pause verb; "" means the flag landed.
func setCleanPause(ctx *Context, wiID string, value bool) string {
	if err := patchCleanPause(ctx, wiID, value); err != nil {
		return fmt.Sprintf("could not set clean_pause=%v on the hub (%v); the daemon pause request still carries the flag", value, err)
	}
	return ""
}

// ---------------------------------------------------------------------------
// resume — clear the operator pause: flips clean_pause back to false on
// the hub and leaves the paused work item to its HOLDER daemon (the
// session's machine attaches on its next start via session-attach;
// a work item whose holder is gone moves with `pf takeover`). This
// deliberately replaces v1's “fresh local claim via the daemon”
// behavior — the v1.5 pause keeps the session on the hub, and only the
// holder may resume it. Since the v1.5 review Finding 2 the pause also
// RETAINS the holder daemon's local record, so the attach promised
// below really can fire. If the holder daemon is LIVE when the resume
// lands there is deliberately NO auto-pickup this phase: the retained
// paused record sits until that daemon's next start (whose restart
// reconciliation attaches); to continue immediately use `pf takeover`.
// ---------------------------------------------------------------------------

var cmdResume = &Command{
	Name:    "resume",
	Summary: "Clear the operator pause of a WI (clean_pause=false; the holder daemon attaches on its next start)",
	Usage:   "pf resume <id>",
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("resume: expected exactly one work item id")
		}
		if err := submitPrecheck(ctx, args[0]); err != nil {
			return err
		}
		if err := patchCleanPause(ctx, args[0], false); err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout, "resumed %s: clean_pause cleared; the paused work item stays with its holder daemon\n", args[0])
		fmt.Fprintln(ctx.Stdout, "(its machine re-attaches on the next daemon start; a work item whose holder is gone moves with `pf takeover <id> --reason ...`)")
		fmt.Fprintln(ctx.Stdout, "(a live holder daemon does NOT pick this up until its next start; to continue immediately use `pf takeover`)")
		return nil
	},
}

// ---------------------------------------------------------------------------
// wrap — wrap (or fail) a running WI via the daemon.
// ---------------------------------------------------------------------------

var cmdWrap = &Command{
	Name:    "wrap",
	Summary: "Wrap a running WI as terminal success (--fail for terminal failure)",
	Usage:   "pf wrap <id> [--fail]",
	Setup: func(fs *flag.FlagSet) {
		fs.Bool("fail", false, "end the session with reason=fail instead of wrap")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("wrap: expected exactly one work item id")
		}
		reason := "wrap"
		if boolFlag(fs, "fail") {
			reason = "fail"
		}
		raw, err := ctx.daemon().Wrap(args[0], reason)
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			fmt.Fprintf(ctx.Stdout, "%s requested for %s\n", reason, args[0])
		})
		return nil
	},
}

// ---------------------------------------------------------------------------
// ship — the wrap→git delivery bridge: push a closed WI's sealed capsule
// commits to each inventory repo's origin as the fixed-name branch
// polyforge/<id>, then open/reuse a PR when a credential exists (a
// missing PR credential is NOT an error; only the push credential is).
// ---------------------------------------------------------------------------

var cmdShip = &Command{
	Name:    "ship",
	Summary: "Push a closed WI's sealed capsule commits to git origin (branch polyforge/<id>)",
	Usage: "pf ship <id>\n" +
		"\n" +
		"Typed errors:\n" +
		"  DAEMON-404  this machine holds no sealed capsule for the WI\n" +
		"  DAEMON-403  reserved branch target (main/master is never pushed)\n" +
		"  DAEMON-409  the remote branch moved past the sealed commit\n" +
		"              (never force-pushed over it)\n" +
		"  DAEMON-512  push preflight failed: no usable local git push identity\n" +
		"              or the origin is unreachable",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("ship: expected exactly one work item id")
		}
		raw, err := ctx.daemon().Ship(args[0])
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var sr ShipResponse
			if err := json.Unmarshal(raw, &sr); err == nil {
				renderShip(ctx.Stdout, &sr)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		})
		return nil
	},
}

// ---------------------------------------------------------------------------
// ops — the daemon ops face: status / mode / shutdown.
// ---------------------------------------------------------------------------

var cmdOps = &Command{
	Name:    "ops",
	Summary: "Daemon ops: local job status, mode switching, graceful shutdown",
	Usage: "pf ops status\n" +
		"pf ops mode                                   # query the current mode (via status)\n" +
		"pf ops mode set --max-concurrent-jobs N [--mode interactive|batch|drain]\n" +
		"pf ops mode drain                             # enter autonomous drain mode\n" +
		"pf ops mode abort-drain                       # leave drain mode\n" +
		"pf ops shutdown                               # graceful shutdown (quiesce)",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("ops: expected status|mode|shutdown")
		}
		d := ctx.daemon()
		switch args[0] {
		case "status":
			raw, err := d.Status()
			if err != nil {
				return err
			}
			ctx.finish(raw, func() {
				var s StatusResponse
				if err := json.Unmarshal(raw, &s); err == nil {
					renderStatus(ctx.Stdout, &s)
				} else {
					printJSON(ctx.Stdout, raw)
				}
			})
			return nil
		case "mode":
			return opsMode(ctx, d, args[1:])
		case "shutdown":
			raw, err := d.Shutdown()
			if err != nil {
				return err
			}
			ctx.finish(raw, func() {
				fmt.Fprintln(ctx.Stdout, "graceful shutdown requested; the daemon quiesces and exits")
			})
			return nil
		default:
			return fmt.Errorf("ops: unknown action %q", args[0])
		}
	},
}

// opsMode handles `pf ops mode [set|drain|abort-drain]`. A bare query has
// no POST /mode "get" action — it reads GET /status.
func opsMode(ctx *Context, d *DaemonClient, args []string) error {
	if len(args) == 0 {
		raw, err := d.Status()
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var s StatusResponse
			if err := json.Unmarshal(raw, &s); err == nil {
				fmt.Fprintf(ctx.Stdout, "mode: %s\n", s.Mode)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		})
		return nil
	}
	switch args[0] {
	case "set":
		f := subFlags(ctx, "ops mode set")
		maxJobs := f.Int("max-concurrent-jobs", 0, "max concurrent jobs on this machine (n >= 1)")
		mode := f.String("mode", "", "also switch the mode: interactive|batch|drain")
		rest, err := parseCommandArgs(f, args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return fmt.Errorf("ops mode set: unexpected arguments %v", rest)
		}
		switch *mode {
		case "", protocol.ModeInteractive, protocol.ModeBatch, protocol.ModeDrain:
		default:
			return fmt.Errorf("ops mode set: --mode must be interactive|batch|drain")
		}
		req := ModeRequest{Action: "set", Mode: *mode}
		if *maxJobs >= 1 {
			req.MaxConcurrentJobs = maxJobs
		} else if *maxJobs < 0 {
			return fmt.Errorf("ops mode set: --max-concurrent-jobs must be >= 1")
		}
		if req.Mode == "" && req.MaxConcurrentJobs == nil {
			return fmt.Errorf("ops mode set: expected --max-concurrent-jobs N and/or --mode M")
		}
		raw, err := d.Mode(req)
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			printJSON(ctx.Stdout, raw)
		})
		return nil
	case "drain", "abort-drain":
		raw, err := d.Mode(ModeRequest{Action: args[0]})
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			printJSON(ctx.Stdout, raw)
		})
		return nil
	default:
		return fmt.Errorf("ops mode: unknown sub-action %q (set|drain|abort-drain)", args[0])
	}
}

// ---------------------------------------------------------------------------
// daemon — help-only CLI registration. The daemon IS assembled: cmd/pf/
// main.go intercepts `pf daemon` argv before CLI dispatch and calls
// internal/polyforge/daemon.Main directly. This command registration
// exists only so `pf help daemon` presents the real launch surface; its
// Run is unreachable in a correct build and merely reprints the help.
// ---------------------------------------------------------------------------

// daemonUsage is the STATIC help text for `pf help daemon`. It documents
// the daemon launch surface (flags, env vars, defaults) as defined in
// internal/polyforge/daemon/main.go. It is NOT generated from the daemon
// flag set — keep it in sync manually.
const daemonUsage = `pf daemon [--flag value] ...

Starts the local polyforge daemon (a UDS server hosting the runtime and
observe projections). The real entry is assembled: cmd/pf/main.go
intercepts ` + "`pf daemon`" + ` argv before CLI dispatch and calls
internal/polyforge/daemon.Main. This CLI registration exists only to
serve ` + "`pf help daemon`" + ` presentation.

Launch flags (with environment variable and default, where one exists):

  --config PATH
      shared config file (default ~/.config/polyforge/cli.toml)
  --socket PATH
      unix socket path to listen on (env PF_DAEMON_SOCKET;
      default /run/polyforge.sock)
  --server URL
      aihub base URL (env PF_SERVER)
  --api-key KEY
      machine aihub api key (env PF_API_KEY)
  --machine-id ID
      machine identity (env POLYFORGE_MACHINE_ID; default m-<hostname>)
  --workspace-root PATH
      capsule workspace root (env POLYFORGE_WORKSPACE_ROOT)
  --data-dir PATH
      daemon data dir (env POLYFORGE_DATA_DIR)
  --project NAME
      ready-queue project scope for batch/drain (env PF_PROJECT)
  --workflows-dir PATH
      workflows/ checkout dir (default ./workflows)
  --executor-prompt PATH
      executor skill file injected into every agent
      (default ./skills/pf-execute/SKILL.md)
  --pi-bin PATH
      harness binary (default pi)
  --harness-sessions-root PATH
      harness session store root for the live-transcript sync
      (env POLYFORGE_HARNESS_SESSIONS_ROOT; default ~/.pi/agent/sessions)
  --cgroup-root PATH
      cgroup v2 root for layer-0 cleanup (env POLYFORGE_CGROUP_ROOT)
  --git-remote-base URL
      aihub bare-repo base URL (default <server>/git)
  --max-concurrent-jobs N
      max concurrent jobs on this machine (default 4)
  --recall-topk N
      claim-time memory snapshot hit count
      (env POLYFORGE_RECALL_TOPK; default 10, max 50)
  --mode interactive|batch|drain
      initial mode (default interactive)

Supervision-window flags (#12; Go duration strings; omitted or 0 keeps
observe's own default):

  --hang-window D
      stream silence while a turn is in flight before the liveness probe
      arms (10m)
  --idle-window D
      no progress record and no turn in flight before the idle watchdog
      probes (10m)
  --probe-timeout D
      one liveness probe's ack bound (2m)
  --answer-ack-window D
      the agent's pf/answer consumption-ack bound (2m)
  --revalidate-every D
      periodic session revalidation cadence for RUNNING jobs
      (5m; negative disables)

The full ` + "`pf daemon --help`" + ` flag surface is documented in the README.`

var cmdDaemon = &Command{
	Name:    "daemon",
	Summary: "Run the local pf daemon (assembled: main.go intercepts `pf daemon` before CLI dispatch; this verb serves `pf help daemon` only)",
	Usage:   daemonUsage,
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		// In a correct build this Run is unreachable: cmd/pf/main.go
		// intercepts `pf daemon` argv and calls daemon.Main. If it is
		// ever reached, do not lie about the daemon — reprint the help.
		fmt.Fprintln(ctx.Stdout, daemonUsage)
		fmt.Fprintln(ctx.Stdout, "\n(this path is help-only: cmd/pf/main.go intercepts `pf daemon` and")
		fmt.Fprintln(ctx.Stdout, "runs internal/polyforge/daemon.Main directly)")
		return nil
	},
}
