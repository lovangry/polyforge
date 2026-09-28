package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// daemon returns the daemon-socket client for the resolved config.
func (ctx *Context) daemon() *DaemonClient {
	return NewDaemonClient(ctx.Cfg.DaemonSocket)
}

// ---------------------------------------------------------------------------
// submit — interactive submit of one WI (claim/resume/takeover; 00 §5.1
// routes submit to the daemon face, 04 §5.1 claim/takeover rows).
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
		takeover := boolFlag(fs, "takeover")
		priority := stringFlag(fs, "priority")
		d := ctx.daemon()
		var (
			raw []byte
			err error
		)
		if takeover {
			raw, err = d.Takeover(args[0], priority)
		} else {
			raw, err = d.Claim(args[0], priority)
		}
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			var sr SubmitResponse
			if err := decodeJSON(raw, &sr); err == nil && sr.Accepted {
				fmt.Fprintf(ctx.Stdout, "submitted %s (accepted by the local daemon)\n", sr.WorkItemID)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		}
		return nil
	},
}

// ---------------------------------------------------------------------------
// resume — resume a paused WI (always a fresh claim, generation+1;
// 04 §5.1 resume row).
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
		ctx.emit(raw)
		if !ctx.JSON {
			var sr SubmitResponse
			if err := decodeJSON(raw, &sr); err == nil && sr.Accepted {
				fmt.Fprintf(ctx.Stdout, "resumed %s (fresh claim accepted by the local daemon)\n", sr.WorkItemID)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		}
		return nil
	},
}

// ---------------------------------------------------------------------------
// observe — two-layer observation (04 §5.1a): the daemon event stream when
// this machine has the Capsule; the aihub read-through (row 6) otherwise.
// ---------------------------------------------------------------------------

var cmdObserve = &Command{
	Name:    "observe",
	Summary: "Observe WI progress (daemon event stream, or AIHub read-through)",
	Usage:   "pf observe <id> [--after N] [--read-through]",
	Setup: func(fs *flag.FlagSet) {
		fs.Int64("after", 0, "last received event cursor (first call: 0)")
		fs.Bool("read-through", false, "skip the local daemon layer, render the published HEAD directly")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("observe: expected exactly one work item id")
		}
		after := int64Flag(fs, "after")
		readThrough := boolFlag(fs, "read-through")

		if !readThrough {
			d := ctx.daemon()
			raw, err := d.Observe(args[0], after)
			switch {
			case err == nil:
				ctx.emit(raw)
				if !ctx.JSON {
					var resp ObserveResponse
					if err := decodeJSON(raw, &resp); err == nil {
						renderObserve(ctx.Stdout, &resp)
					} else {
						printJSON(ctx.Stdout, raw)
					}
				}
				return nil
			case daemonSaysNotLocal(err):
				// DAEMON-404: neither a local Capsule nor any published
				// progress on the daemon side — fall through to the
				// read-through layer (04 §5.1a).
			case isDialError(err):
				// Daemon not reachable at all (not running here): treat as
				// "not local", with a note — the local layer being absent
				// must not hide the WI from observation.
				fmt.Fprintln(ctx.Stderr, "pf: daemon not reachable locally; using AIHub read-through")
			default:
				return err
			}
		}
		return observeReadThrough(ctx, args[0])
	},
}

// observeReadThrough renders the latest published HEAD via the aihub
// row-6 progress face.
func observeReadThrough(ctx *Context, wiID string) error {
	raw, err := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey).GetProgress(wiID)
	if err != nil {
		if isClaimedEmptyChainProgress(err) {
			// The claimed-but-no-published-HEAD CORE-504 is the minutes-long
			// first-publish window of every real WI, not an illegal state
			// (polyforge#36): the read-only face renders the empty-chain
			// view instead of hard-erroring. Every OTHER CORE-504 variant
			// (broken published chain) still falls through to the error
			// below — read-only faces degrade, they do not erase
			// diagnostics.
			p := progressView{
				WorkItemID:    wiID,
				FreshnessNote: noteFirstPublishPending,
				claimedNoHead: true,
			}
			if synthesized, merr := json.Marshal(p); merr == nil {
				// --json mode: emit the same shape the Core answers for the
				// never-claimed case (row-6 200 with null fields), so the
				// machine consumer sees a payload, not an error envelope.
				ctx.emit(synthesized)
			}
			if !ctx.JSON {
				renderProgress(ctx.Stdout, &p)
			}
			return nil
		}
		return err
	}
	ctx.emit(raw)
	if !ctx.JSON {
		var p progressView
		if err := decodeJSON(raw, &p); err == nil {
			renderProgress(ctx.Stdout, &p)
		}
	}
	return nil
}

// claimedNoHeadDetail anchors the ONE CORE-504 variant that is a legal
// state rather than a broken chain: the Core's typed row-6 answer for
// "claimed but nothing published yet" (internal/core/capsule/capsule.go
// Store.Progress, api/openapi.yaml getProgress). The server contract pins
// the code with an empty reason for every CORE-504, so the deterministic
// detail text is the only wire-side distinguisher; all other variants
// (execution.json missing at HEAD, undecodable block, unreadable or
// unparsable manifest) must keep erroring. Mirrored by the daemon-side
// predicate in internal/daemon/api.go.
const claimedNoHeadDetail = "was claimed but has no published HEAD"

// noteFirstPublishPending is the empty-chain rendering for the
// claimed-but-no-HEAD state (polyforge#36): it extends the Core's own
// never-claimed row-6 note so both observe layers agree on the wording.
const noteFirstPublishPending = "no published revision yet (claimed, first publish pending)"

// isClaimedEmptyChainProgress reports whether err is exactly the
// claimed-empty-chain CORE-504 (the first-publish window), the one
// variant the read-through observe renders instead of surfacing.
func isClaimedEmptyChainProgress(err error) bool {
	var ae *APIError
	if !asAPIError(err, &ae) || ae.Code != "CORE-504" {
		return false
	}
	return strings.Contains(ae.Detail, claimedNoHeadDetail)
}

// submitPrecheck asks the Core — with the CALLER's own api key, not the
// daemon's machine key — for the work item before the local daemon is
// involved (real-usage fix, polyforge#17 dogfood round: a submit of a
// terminal or mistyped id used to answer "accepted", and the doomed
// claim then failed with no operator-visible surface anywhere). Refusals:
// a typed CORE-404 (unknown or hidden id) and an explicit terminal
// status; every other read outcome defers to the daemon's spawn-time
// claim, which stays the authority.
func submitPrecheck(ctx *Context, wiID string) error {
	raw, err := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey).GetWorkItem(wiID, nil)
	if err != nil {
		var ae *APIError
		if asAPIError(err, &ae) && ae.Code == "CORE-404" {
			return err
		}
		return nil
	}
	var d struct {
		Status string `json:"status"`
	}
	if decodeJSON(raw, &d) == nil {
		switch d.Status {
		case "wrapped", "failed", "cancelled":
			return fmt.Errorf("submit: %s is terminal (%s); no claim, resume or takeover can start", wiID, d.Status)
		}
	}
	return nil
}

// daemonSaysNotLocal reports whether the daemon answered the typed
// "object not found" code (DAEMON-404: this machine holds no Capsule and
// Core has no published progress either).
func daemonSaysNotLocal(err error) bool {
	var ae *APIError
	if asAPIError(err, &ae) {
		return ae.Code == "DAEMON-404"
	}
	return false
}

// isDialError reports whether the failure is a socket-level connection
// problem (daemon not running on this machine) rather than a typed answer.
func isDialError(err error) bool {
	var ae *APIError
	if asAPIError(err, &ae) {
		return false
	}
	var uerr *url.Error
	if ok := asType(err, &uerr); ok {
		var nerr *net.OpError
		return asType(uerr.Err, &nerr)
	}
	var nerr *net.OpError
	return asType(err, &nerr)
}

// asType is a tiny errors.As equivalent for the two concrete types used
// above.
func asType[T any](err error, target *T) bool {
	if err == nil {
		return false
	}
	if v, ok := err.(T); ok {
		*target = v
		return true
	}
	if uerr, ok := err.(interface{ Unwrap() error }); ok {
		return asType(uerr.Unwrap(), target)
	}
	return false
}

// ---------------------------------------------------------------------------
// answer — answer a needs_input question (00 §3.4a relay message; the
// daemon forwards the payload verbatim, identity via X-PF-User).
// ---------------------------------------------------------------------------

var cmdAnswer = &Command{
	Name:    "answer",
	Summary: "Answer a pending needs_input question of a WI",
	Usage:   "pf answer <id> --question q_xxx --text \"...\" [--answer-id a_xxxx] [--by user]",
	Setup: func(fs *flag.FlagSet) {
		fs.String("question", "", "question_id shown by observe (required)")
		fs.String("text", "", "natural-language answer (required)")
		fs.String("answer-id", "", "explicit answer id (default: generated a_ + 6 chars)")
		fs.String("by", "", "answerer user id (default: config user_id / PF_USER_ID)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("answer: expected exactly one work item id")
		}
		question := stringFlag(fs, "question")
		text := stringFlag(fs, "text")
		answerID := stringFlag(fs, "answer-id")
		by := stringFlag(fs, "by")
		if question == "" || text == "" {
			return fmt.Errorf("answer: --question and --text are required")
		}
		who := by
		if who == "" {
			who = ctx.Cfg.UserID
		}
		if who == "" {
			return fmt.Errorf("answer: answered_by identity required (--by or PF_USER_ID / user_id in config)")
		}
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
		raw, err := ctx.daemon().Answer(args[0], payload)
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			fmt.Fprintf(ctx.Stdout, "answer %s delivered for question %s\n", payload.AnswerID, payload.QuestionID)
		}
		return nil
	},
}

// newAnswerID mints an answer id: "a_" + 6 random hex chars (the format
// pinned by 00 §3.4a).
func newAnswerID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable; fall back to time-based.
		return fmt.Sprintf("a_%06x", time.Now().UnixNano()&0xffffff)
	}
	return "a_" + hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// batch-confirm — Batch wave: read the scheduler recommendation, let the
// human confirm (possibly edited), submit the wave in one call
// (00 §3.4b; 04 §5.1 mode(get_recommendation) + batch_submit).
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

		// Always fetch the scheduler recommendation first (04 §6.1b step 1;
		// the daemon forwards the aihub ready-queue answer read-only).
		raw, err := d.Mode(ModeRequest{Action: "get_recommendation"})
		if err != nil {
			return err
		}
		var rec readyQueueView
		if err := decodeJSON(raw, &rec); err != nil {
			return fmt.Errorf("batch-confirm: cannot parse recommendation: %w", err)
		}

		if !all && len(ids) == 0 {
			// Read-only recommendation display; NO Job is created (04 §6.1b
			// step 1: the daemon never treats a recommendation as confirmed).
			ctx.emit(raw)
			if !ctx.JSON {
				renderReadyQueue(ctx.Stdout, &rec)
				fmt.Fprintln(ctx.Stdout, "\nconfirm with: pf batch-confirm --all  or  pf batch-confirm <id> ...")
			}
			return nil
		}
		if all {
			ids = make([]string, 0, len(rec.Executable))
			for _, e := range rec.Executable {
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
		ctx.emit(sraw)
		if !ctx.JSON {
			fmt.Fprintf(ctx.Stdout, "wave submitted: %d work item(s)\n", len(ids))
			printJSON(ctx.Stdout, sraw)
		}
		return nil
	},
}

// ---------------------------------------------------------------------------
// pause — pause a running WI via the daemon (generation close needs the
// daemon-generated flush_evidence, 02 §6.4 / 04 §6.2 path A).
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
		ctx.emit(raw)
		if !ctx.JSON {
			fmt.Fprintf(ctx.Stdout, "pause requested for %s\n", args[0])
			printJSON(ctx.Stdout, raw)
		}
		return nil
	},
}

// ---------------------------------------------------------------------------
// wrap — wrap (or fail) a running WI via the daemon; same final-flush
// requirement as pause (02 §6.4).
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
		ctx.emit(raw)
		if !ctx.JSON {
			fmt.Fprintf(ctx.Stdout, "%s requested for %s\n", reason, args[0])
			printJSON(ctx.Stdout, raw)
		}
		return nil
	},
}

// ---------------------------------------------------------------------------
// ship — the wrap→git delivery bridge (00 §5.1 ship verb; 04 §5.1 ship
// row, baseline amendment polyforge#37): push the sealed capsule commits
// of a CLOSED work item to each inventory repo's origin as the fixed-name
// branch polyforge/<id>, then open/reuse a PR when a credential exists.
// ---------------------------------------------------------------------------

var cmdShip = &Command{
	Name:    "ship",
	Summary: "Push a closed WI's sealed capsule commits to git origin (branch polyforge/<id>)",
	Usage: "pf ship <id>\n" +
		"\n" +
		"Pushes the sealed commit of every inventory repo of the WI's local\n" +
		"sealed capsule to its git origin as the fixed-name branch\n" +
		"polyforge/<id>, then opens or reuses a PR when the gh CLI is present\n" +
		"and authenticated. Without a PR credential the answer carries a\n" +
		"compare URL and STILL SUCCEEDS — a missing PR credential is not an\n" +
		"error (only the push credential is).\n" +
		"\n" +
		"Typed errors:\n" +
		"  DAEMON-404  this machine holds no sealed capsule for the WI\n" +
		"  DAEMON-403  reserved branch target (main/master is never pushed)\n" +
		"  DAEMON-409  the remote branch moved past the sealed commit\n" +
		"              (never force-pushed over it)\n" +
		"  DAEMON-512  push preflight failed: no usable local git push identity\n" +
		"              (SSH/credential helper) or the origin is unreachable",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("ship: expected exactly one work item id")
		}
		raw, err := ctx.daemon().Ship(args[0])
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			var sr ShipResponse
			if err := decodeJSON(raw, &sr); err == nil {
				renderShip(ctx.Stdout, &sr)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		}
		return nil
	},
}

// ---------------------------------------------------------------------------
// ops — the daemon ops face: status / mode / shutdown (04 §5.1).
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
			ctx.emit(raw)
			if !ctx.JSON {
				var s StatusResponse
				if err := decodeJSON(raw, &s); err == nil {
					renderStatus(ctx.Stdout, &s)
				} else {
					printJSON(ctx.Stdout, raw)
				}
			}
			return nil
		case "mode":
			return opsMode(ctx, d, args[1:])
		case "shutdown":
			raw, err := d.Shutdown()
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				fmt.Fprintln(ctx.Stdout, "graceful shutdown requested; the daemon quiesces and exits")
			}
			return nil
		default:
			return fmt.Errorf("ops: unknown action %q", args[0])
		}
	},
}

// opsMode handles `pf ops mode [set|drain|abort-drain]`. A bare query has
// no POST /mode "get" action (hMode rejects it) — it reads GET /status.
func opsMode(ctx *Context, d *DaemonClient, args []string) error {
	if len(args) == 0 {
		raw, err := d.Status()
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			var s StatusResponse
			if err := decodeJSON(raw, &s); err == nil {
				fmt.Fprintf(ctx.Stdout, "mode: %s\n", s.Mode)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		}
		return nil
	}
	switch args[0] {
	case "set":
		f := flag.NewFlagSet("pf ops mode set", flag.ContinueOnError)
		f.SetOutput(ctx.Stderr)
		maxJobs := f.Int("max-concurrent-jobs", 0, "max concurrent jobs on this machine (n >= 1)")
		mode := f.String("mode", "", "also switch the mode: interactive|batch|drain")
		rest, err := parseCommandArgs(f, args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return fmt.Errorf("ops mode set: unexpected arguments %v", rest)
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
		ctx.emit(raw)
		if !ctx.JSON {
			printJSON(ctx.Stdout, raw)
		}
		return nil
	case "drain", "abort-drain":
		raw, err := d.Mode(ModeRequest{Action: args[0]})
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			printJSON(ctx.Stdout, raw)
		}
		return nil
	default:
		return fmt.Errorf("ops mode: unknown sub-action %q (set|drain|abort-drain)", args[0])
	}
}

// ---------------------------------------------------------------------------
// Flag accessors for flags registered in Setup (values are already parsed
// by the dispatcher when Run executes).
// ---------------------------------------------------------------------------

func boolFlag(fs *flag.FlagSet, name string) bool {
	return fs.Lookup(name).Value.(flag.Getter).Get().(bool)
}
func stringFlag(fs *flag.FlagSet, name string) string {
	return fs.Lookup(name).Value.(flag.Getter).Get().(string)
}
func int64Flag(fs *flag.FlagSet, name string) int64 {
	return fs.Lookup(name).Value.(flag.Getter).Get().(int64)
}
func intFlag(fs *flag.FlagSet, name string) int {
	return fs.Lookup(name).Value.(flag.Getter).Get().(int)
}
