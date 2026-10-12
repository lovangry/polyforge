package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// ---------------------------------------------------------------------------
// submit — interactive submit of one WI (claim, or takeover).
// ---------------------------------------------------------------------------

var cmdSubmit = &Command{
	Name:    "submit",
	Group:   GroupExec,
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
	Group:   GroupExec,
	Summary: "DEPRECATED, use `pf watch <wi> --once`: observe WI progress (daemon projection)",
	Usage:   "pf observe <id> [--after N] [--read-through=false]   (deprecated alias of the watch lane: use `pf watch <id> --once`)",
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
	Group:   GroupExec,
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
			// Interactive default: the unique pending question. Two cases
			// (owner ruling on 9f4d3a2's over-broad fallback):
			//   (1) observe SUCCEEDS with no pending question → refuse:
			//       there is genuinely nothing to answer, and POSTing an
			//       empty question_id silently delivers nowhere.
			//   (2) observe FAILS (driver WIs get DAEMON-404) → fall
			//       through with an empty question: the daemon answer
			//       route's pendingQuestionID fallback resolves it.
			raw, err := d.Observe(args[0], 0)
			if err == nil {
				var jv protocol.JobView
				if uerr := json.Unmarshal(raw, &jv); uerr == nil {
					if jv.Pending == nil {
						return fmt.Errorf("answer: %s has no pending question (see `pf observe %s`)", args[0], args[0])
					}
					question = jv.Pending.ID
				}
				// Undecodable body: Pending == nil cannot be established,
				// so treat it like an observe failure — fall through.
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
// pause — operator pause of a running WI: the daemon closes the job
// locally AND the hub record is marked clean_pause=true so a daemon
// restart never auto-resumes an operator-paused work item (M3).
// ---------------------------------------------------------------------------

var cmdPause = &Command{
	Name:    "pause",
	Group:   GroupExec,
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
// resume — one verb, two doors (polyforge#116). The hub status picks the
// door:
//
//   - a workflow parked BLOCKED on an honest incomplete result (§6.5
//     step 5: workflow_status=blocked, position frozen, the owner holds
//     the next move) gets the §6.4 owner move: the CLI lands the hub's
//     resume-blocked door with the human's own credential (authorization
//     is reporter / maintainer / admin), then the daemon's route claims
//     it and re-establishes execution over the parked capsule — the frozen current step
//     re-dispatches in place as a fresh derivation. It must never answer
//     the pause copy again (nothing here was ever paused).
//   - every other status keeps the operator-pause door exactly as it was:
//     a clean_pause=false PATCH on the hub, the WI left to its HOLDER
//     daemon (the session's machine attaches on its next start via
//     session-attach; a work item whose holder is gone moves with `pf
//     takeover`). This deliberately replaces v1's “fresh local claim via
//     the daemon” behavior — the v1.5 pause keeps the session on the hub,
//     and only the holder may resume it. Since the v1.5 review Finding 2
//     the pause also RETAINS the holder daemon's local record, so the
//     attach promised below really can fire. If the holder daemon is LIVE
//     when the resume lands there is deliberately NO auto-pickup this
//     phase: the retained paused record sits until that daemon's next
//     start (whose restart reconciliation attaches); to continue
//     immediately use `pf takeover`.
// ---------------------------------------------------------------------------

var cmdResume = &Command{
	Name:    "resume",
	Group:   GroupExec,
	Summary: "Continue a paused WI (clean_pause=false) or a blocked one (reporter, maintainer or admin); abandon with pf cancel",
	Usage:   "pf resume <id>",
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("resume: expected exactly one work item id")
		}
		if err := submitPrecheck(ctx, args[0]); err != nil {
			return err
		}
		wi, err := ctx.aihub().GetWorkItem(args[0])
		if err != nil {
			return err
		}
		// Dispatch order, decided by WI state (v2 merge of D1 + D2):
		//  1. hub status blocked  -> the human-key resume-blocked door, then
		//     the daemon's claim/start (below).
		//  2. not blocked, and THIS daemon holds the WI parked repair_pending
		//     (capsule missing / project unmapped at restart; the session is
		//     kept) -> the daemon re-checks and resumes it in place
		//     (RepairResume); a paused WI has its clean_pause
		//     cleared first so the in-place attach is not held back.
		//  3. otherwise (paused) -> clear clean_pause on the hub; no daemon.
		// end_pending (D2) is a distinct local state with no door of its own:
		// the daemon retries its SessionEnd itself.
		if wi.Status != api.StatusBlocked && localRepairPending(ctx, wi.ID) {
			if wi.Status == api.StatusPaused {
				if err := patchCleanPause(ctx, args[0], false); err != nil {
					return err
				}
			}
			raw, err := ctx.daemon().Resume(args[0], "")
			if err != nil {
				return err
			}
			ctx.finish(raw, func() {
				fmt.Fprintf(ctx.Stdout, "resumed %s: the repair_pending work item was re-checked and continues in place with its kept session\n", args[0])
			})
			return nil
		}
		if wi.Status != api.StatusBlocked {
			if err := patchCleanPause(ctx, args[0], false); err != nil {
				return err
			}
			fmt.Fprintf(ctx.Stdout, "resumed %s: clean_pause cleared; the paused work item stays with its holder daemon\n", args[0])
			fmt.Fprintln(ctx.Stdout, "(its machine re-attaches on the next daemon start; a work item whose holder is gone moves with `pf takeover <id> --reason ...`)")
			fmt.Fprintln(ctx.Stdout, "(a live holder daemon does NOT pick this up until its next start; to continue immediately use `pf takeover`)")
			return nil
		}
		// The authorization-bearing step runs with the HUMAN's own api key
		// (data plane, like `pf cancel`): the hub's resume-blocked door
		// checks reporter / project maintainer / admin against the caller,
		// and the daemon's machine key is not that caller. Only then does
		// the local daemon claim and start the WI.
		if _, err := ctx.aihub().ResumeBlockedWorkItem(args[0]); err != nil {
			return err
		}
		raw, err := ctx.daemon().ResumeBlocked(args[0])
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			fmt.Fprintf(ctx.Stdout, "resumed %s: the workflow was parked blocked (incomplete result) — the §6.4 owner move re-established execution over the parked capsule\n", args[0])
			fmt.Fprintln(ctx.Stdout, "(the frozen current step re-dispatches in place as a fresh derivation; watch it with `pf observe <id>`)")
		})
		return nil
	},
}

// localRepairPending reports whether the local daemon holds wiID parked as
// repair_pending. Any daemon failure (not running, wrong socket) is "no":
// the plain paused path never needs the daemon.
func localRepairPending(ctx *Context, wiID string) bool {
	raw, err := ctx.daemon().Status()
	if err != nil {
		return false
	}
	var st StatusResponse
	if json.Unmarshal(raw, &st) != nil {
		return false
	}
	for _, j := range st.Jobs {
		if j.WorkItemID == wiID && j.Status == "repair_pending" {
			return true
		}
	}
	return false
}

// abandonRepairPending is the give-up door of a WI the LOCAL daemon holds
// parked repair_pending (capsule missing / project unmapped at restart): the
// WI is still `running` at aihub under the daemon's kept session, so the
// hub's cancel refuses it and only the daemon holds the secret to end that
// session. Authorization follows the continue/abandon doors: the HUMAN's own
// key is checked at aihub first (reporter / maintainer / admin, a read-only
// probe), and only then is the daemon asked — its machine key is not that
// caller. verdict is "canceled" (pf cancel) or "failed" (pf wrap --fail).
// handled=false with a nil error means the WI is not parked here and the
// caller proceeds with its normal path.
func abandonRepairPending(ctx *Context, id, verdict string) (handled bool, err error) {
	wi, gerr := ctx.aihub().GetWorkItem(id)
	if gerr != nil || wi.Status != api.StatusRunning || !localRepairPending(ctx, wi.ID) {
		return false, nil
	}
	if err := ctx.aihub().CloseAuth(id); err != nil {
		return true, err
	}
	raw, err := ctx.daemon().AbandonRepair(id, verdict)
	if err != nil {
		return true, err
	}
	ctx.finish(raw, func() {
		fmt.Fprintf(ctx.Stdout, "%s %s: the repair_pending work item was abandoned — the daemon ended its session and dropped the parked record\n", verdict, id)
	})
	return true, nil
}

// ---------------------------------------------------------------------------
// wrap — wrap (or fail) a running WI via the daemon.
// ---------------------------------------------------------------------------

var cmdWrap = &Command{
	Name:    "wrap",
	Group:   GroupExec,
	Summary: "Wrap a running WI as terminal success (--fail = the owner's failed verdict; to abandon a blocked WI use pf cancel)",
	Usage:   "pf wrap <id> [--fail]",
	Setup: func(fs *flag.FlagSet) {
		fs.Bool("fail", false, "end the session with reason=fail instead of wrap (the owner's failed verdict; to abandon a blocked WI use pf cancel)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("wrap: expected exactly one work item id")
		}
		reason := "wrap"
		if boolFlag(fs, "fail") {
			reason = "fail"
		}
		// A WI parked blocked has no local job: `--fail` is the owner's
		// terminate verdict, landed at the hub's terminate-blocked door with
		// the HUMAN's own api key (reporter / maintainer / admin is checked
		// against the person typing, not the daemon's machine key). No
		// daemon step follows: the bounded close already dropped the job.
		if reason == "fail" {
			if abandoned, aerr := abandonRepairPending(ctx, args[0], "failed"); abandoned || aerr != nil {
				return aerr
			}
			if wi, gerr := ctx.aihub().GetWorkItem(args[0]); gerr == nil && wi.Status == api.StatusBlocked {
				raw, err := ctx.aihub().TerminateBlockedWorkItem(args[0])
				if err != nil {
					return err
				}
				ctx.finish(raw, func() {
					fmt.Fprintf(ctx.Stdout, "%s requested for %s (the parked workflow was terminated at the hub)\n", reason, args[0])
				})
				return nil
			}
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
	Group:   GroupDelivery,
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
// ops — the daemon ops face: status / auto / max / shutdown.
// ---------------------------------------------------------------------------

var cmdOps = &Command{
	Name:    "ops",
	Group:   GroupExec,
	Summary: "Daemon ops: local job status, auto-pull switch, max concurrent jobs, graceful shutdown",
	Usage: "pf ops status\n" +
		"pf ops auto [on|off]    # off = only explicit pf submit; on = the daemon also pulls the ready queue itself (bare = show)\n" +
		"pf ops max [N]          # max concurrent jobs on this machine, N >= 1 (bare = show)\n" +
		"pf ops shutdown         # graceful shutdown (quiesce)",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("ops: expected status|auto|max|shutdown")
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
		case "auto":
			return opsAuto(ctx, d, args[1:])
		case "max":
			return opsMax(ctx, d, args[1:])
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

// opsShow answers a bare `pf ops auto|max` from GET /status.
func opsShow(ctx *Context, d *DaemonClient, render func(s *StatusResponse) string) error {
	raw, err := d.Status()
	if err != nil {
		return err
	}
	ctx.finish(raw, func() {
		var s StatusResponse
		if err := json.Unmarshal(raw, &s); err == nil {
			fmt.Fprintln(ctx.Stdout, render(&s))
		} else {
			printJSON(ctx.Stdout, raw)
		}
	})
	return nil
}

func opsApply(ctx *Context, d *DaemonClient, req OpsConfigRequest) error {
	raw, err := d.OpsConfig(req)
	if err != nil {
		return err
	}
	ctx.finish(raw, func() { printJSON(ctx.Stdout, raw) })
	return nil
}

// opsAuto handles `pf ops auto [on|off]`.
func opsAuto(ctx *Context, d *DaemonClient, args []string) error {
	switch len(args) {
	case 0:
		return opsShow(ctx, d, func(s *StatusResponse) string { return "auto: " + onOff(s.Auto) })
	case 1:
		switch args[0] {
		case "on", "off":
			on := args[0] == "on"
			return opsApply(ctx, d, OpsConfigRequest{Auto: &on})
		}
	}
	return fmt.Errorf("ops auto: expected on|off")
}

// opsMax handles `pf ops max [N]`.
func opsMax(ctx *Context, d *DaemonClient, args []string) error {
	switch len(args) {
	case 0:
		return opsShow(ctx, d, func(s *StatusResponse) string { return fmt.Sprintf("max: %d", s.Queue.MaxConcurrent) })
	case 1:
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 {
			return fmt.Errorf("ops max: N must be an integer >= 1")
		}
		return opsApply(ctx, d, OpsConfigRequest{Max: &n})
	}
	return fmt.Errorf("ops max: expected a single N")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
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
// flag set — keep it in sync manually. The retired selection flags and
// env mirrors are NOT listed anywhere on this face (polyforge#113): the
// config's [workspace] mapping is the only workspace source.
const daemonUsage = `pf daemon [--flag value] ...

Starts the local polyforge daemon (a UDS server hosting the runtime and
observe projections). The real entry is assembled: cmd/pf/main.go
intercepts ` + "`pf daemon`" + ` argv before CLI dispatch and calls
internal/polyforge/daemon.Main. This CLI registration exists only to
serve ` + "`pf help daemon`" + ` presentation.

Workspace mapping (polyforge#113): the daemon reads the [workspace]
sections of the shared config — one [workspace."<path>"] section per
workspace, each carrying the projects whose capsules live under it.
Capsules are placed at <workspace>/capsules/<wi_id> per the claimed
work item's project; an unmapped project is refused at claim time; a
config with zero sections refuses startup. There is no launch-time
workspace flag and no default project: the mapping is the only source.
pf doctor checks the mapping and the connectivity faces.

Launch flags (with environment variable and default, where one exists):

  --config PATH
      shared config file: its [workspace] mapping is the capsule
      placement source (default ~/.config/polyforge/cli.toml)
  --socket PATH
      unix socket path to listen on (env PF_DAEMON_SOCKET;
      default /run/polyforge.sock)
  --server URL
      aihub base URL (env PF_SERVER)
  --api-key KEY
      machine aihub api key (env PF_API_KEY)
  --machine-id ID
      daemon id (env POLYFORGE_MACHINE_ID; default m-<hostname>). The id
      is recorded in <data-dir>/daemon-id on first start and that file
      wins afterwards; a differing flag/env value refuses to start.
  --display-name NAME
      daemon display name shown by pf ops status (env
      POLYFORGE_DISPLAY_NAME; default hostname)
  --data-dir PATH
      daemon data dir (env POLYFORGE_DATA_DIR)
  --workflows-dir PATH
      workflows/ checkout dir (default ./workflows)
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
  --auto
      auto-pull: the daemon claims ready-queue work itself (default
      off; unset keeps the persisted switch; --auto=false forces off)
  --mode interactive|drain
      DEPRECATED alias for one release: interactive = --auto=false,
      drain = --auto=true (prints a warning)

Supervision flags (v2-converge 2.4; Go duration strings; omitted or 0 keeps
the default; a step overrides them in workflow.md: budget.wall_minutes = T,
silence_minutes = X, tool_minutes = Y):

  --silence-window D
      X: no real work signal after dispatch, or no model output while
      waiting for the model, before the step is judged dead
      (10m token-level harnesses, 30m message-level)
  --tool-window D
      Y: one tool running longer than this is judged hung (30m)
  --step-timeout D
      T: cumulative step runtime across its dispatches (90m)
  --stall-window D
      N: the driver between steps, not waiting on a human, for longer
      than this is judged stalled (10m; negative disables)
  --model-error-grace D
      grace after a turn ends on a model error before the step is
      judged dead (1m)
  --revalidate-every D
      periodic session revalidation cadence for RUNNING jobs
      (5m; negative disables)

The full ` + "`pf daemon --help`" + ` flag surface is documented in the README.`

var cmdDaemon = &Command{
	Name:    "daemon",
	Group:   GroupProcess,
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
