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

// ---------------------------------------------------------------------------
// resume — resume a paused WI (always a fresh claim, generation+1).
// ---------------------------------------------------------------------------

var cmdResume = &Command{
	Name:    "resume",
	Summary: "Resume a paused WI on this machine (fresh generation)",
	Usage:   "pf resume <id> [--priority low|normal|high|urgent]",
	Setup: func(fs *flag.FlagSet) {
		fs.String("priority", "normal", "queue priority (low|normal|high|urgent)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("resume: expected exactly one work item id")
		}
		if err := submitPrecheck(ctx, args[0]); err != nil {
			return err
		}
		raw, err := ctx.daemon().Resume(args[0], stringFlag(fs, "priority"))
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var sr SubmitResponse
			if err := json.Unmarshal(raw, &sr); err == nil && sr.Accepted {
				fmt.Fprintf(ctx.Stdout, "resumed %s (fresh claim accepted by the local daemon)\n", sr.WorkItemID)
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
	Usage:   "pf observe <id> [--after N]",
	Setup: func(fs *flag.FlagSet) {
		fs.Int64("after", 0, "last received event cursor (first call: 0)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("observe: expected exactly one work item id")
		}
		raw, err := ctx.daemon().Observe(args[0], int64Flag(fs, "after"))
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var jv protocol.JobView
			if err := json.Unmarshal(raw, &jv); err == nil {
				renderObserve(ctx.Stdout, &jv)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		})
		return nil
	},
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
			// Interactive default: the unique pending question.
			raw, err := d.Observe(args[0], 0)
			if err != nil {
				return fmt.Errorf("answer: --question is required (observe failed: %w)", err)
			}
			var jv protocol.JobView
			if err := json.Unmarshal(raw, &jv); err != nil {
				return fmt.Errorf("answer: --question is required (observe undecodable)")
			}
			if jv.Pending == nil {
				return fmt.Errorf("answer: %s has no pending question (see `pf observe %s`)", args[0], args[0])
			}
			question = jv.Pending.ID
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
				renderWITable(ctx.Stdout, rec.Items)
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
// pause — pause a running WI via the daemon.
// ---------------------------------------------------------------------------

var cmdPause = &Command{
	Name:    "pause",
	Summary: "Pause a running WI at the next safe boundary",
	Usage:   "pf pause <id>",
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("pause: expected exactly one work item id")
		}
		raw, err := ctx.daemon().Pause(args[0])
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			fmt.Fprintf(ctx.Stdout, "pause requested for %s\n", args[0])
		})
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
		fs.Bool("fail", false, "close the generation with reason=fail instead of wrap")
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
// daemon — process placeholder: the daemon assembly lands in phase 3
// (Runtime + Observe composition).
// ---------------------------------------------------------------------------

// daemonWindowFlagsHelp is the #12 observe supervision-window flag
// block, appended to the `pf daemon` help surface (usage + the
// placeholder text). Go duration strings; omitted or 0 keeps observe's
// own default.
const daemonWindowFlagsHelp = `observe supervision windows (Go duration strings; omitted or 0 keeps the default):
  --hang-window D        stream silence while a turn is in flight before the liveness probe arms (10m)
  --idle-window D        no progress record and no turn in flight before the idle watchdog probes (10m)
  --probe-timeout D      one liveness probe's ack bound (2m)
  --answer-ack-window D  the agent's pf/answer consumption-ack bound (2m)
The full ` + "`pf daemon --help`" + ` flag surface is documented in the README.`

var cmdDaemon = &Command{
	Name:    "daemon",
	Summary: "Run the local pf daemon (placeholder: assembled in phase 3)",
	Usage:   "pf daemon [flags]\n\n" + daemonWindowFlagsHelp,
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		fmt.Fprintln(ctx.Stdout, "pf daemon: not assembled in this phase.")
		fmt.Fprintln(ctx.Stdout, "The daemon hosts Runtime (Start/Stop/Recover/Restart) and Observe")
		fmt.Fprintln(ctx.Stdout, "(per-WI monitor goroutines), composed in phase 3 from the")
		fmt.Fprintln(ctx.Stdout, "internal/polyforge runtime + observe modules over the UDS surface")
		fmt.Fprintln(ctx.Stdout, "this CLI already speaks (internal/common/protocol; routes in")
		fmt.Fprintln(ctx.Stdout, "internal/polyforge/cli/daemon.go). Until then `pf ops status` reports")
		fmt.Fprintln(ctx.Stdout, "the same: no daemon is listening on the configured socket.")
		// #12: the observe supervision windows are settable per daemon
		// process (the real entry is internal/polyforge/daemon.Main).
		fmt.Fprintln(ctx.Stdout, "")
		fmt.Fprintln(ctx.Stdout, daemonWindowFlagsHelp)
		return nil
	},
}
