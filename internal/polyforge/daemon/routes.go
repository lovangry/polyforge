package daemon

// routes.go: the daemon's UDS HTTP face. The observe lane's routes
// (observe/answer) are mounted from observe.Handler();
// everything else translates the CLI contract (internal/polyforge/cli/
// daemon.go — the pinned route family) into Runtime calls. Every error
// answer is the TOP-LEVEL api.Error envelope {code, detail} (the same
// typed shape the aihub face uses; DAEMON-* codes per 04 §5.2).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// The wire answer shapes — byte-identical to the CLI's decode structs
// (cli.SubmitResponse etc.); the daemon keeps its own definitions and the
// tests pin the two together field by field.

type submitResponse struct {
	Accepted   bool   `json:"accepted"`
	WorkItemID string `json:"work_item_id"`
}

// adoptResponse is the POST /jobs/{id}/adopt-session answer: custody
// taken, with the PUBLIC fingerprint of the adopted session (never the
// secret — it arrived IN the request and is never echoed).
type adoptResponse struct {
	Adopted            bool   `json:"adopted"`
	WorkItemID         string `json:"work_item_id"`
	SessionFingerprint string `json:"session_fingerprint"`
}

type statusJob struct {
	WorkItemID string `json:"work_item_id"`
	Status     string `json:"status"`
	// SpawnN is the v1.5 B2 derivation counter; SessionFingerprint is
	// the PUBLIC session handle (B3) — together they replace the deleted
	// attempt-id lineage handle.
	SpawnN             int    `json:"spawn_n,omitempty"`
	SessionFingerprint string `json:"session_fingerprint,omitempty"`
	AgentPID           int    `json:"agent_pid,omitempty"`
	// NeedsInput/QuestionID carry the D6 owner face: the monitor's
	// pending-question projection merged over the runtime state machine
	// (a job the runtime calls running but the monitor holds a pending
	// question for is presented as needs_input — an agent blocked on a
	// human must never read as merely busy).
	NeedsInput bool `json:"needs_input,omitempty"`
	// RepairReason is set with Status "repair_pending": capsule_missing |
	// project_unmapped (v2 §6).
	RepairReason string `json:"repair_reason,omitempty"`
	QuestionID   string `json:"question_id,omitempty"`
	// StepAgeSec / LastWorkAgoSec are the owner liveness face (the ops
	// status columns): whole seconds since the current dispatch began and
	// since its harness last did REAL work (nil = no dispatch in flight,
	// or no work-class signal yet — the CLI renders "-"). UI-noise
	// records never refresh the work clock, so a wedged-but-chattering
	// job reads as stale, not fresh.
	StepAgeSec     *int64 `json:"step_age_sec,omitempty"`
	LastWorkAgoSec *int64 `json:"last_work_ago_sec,omitempty"`
	// Origin is how the job was submitted; absent for legacy records.
	Origin *runtime.Origin `json:"origin,omitempty"`
}

type queueStats struct {
	Pending       int `json:"pending"`
	Running       int `json:"running"`
	MaxConcurrent int `json:"max_concurrent"`
}

type statusResponse struct {
	MachineID string `json:"machine_id"`
	// DisplayName is the display-only daemon name (default: hostname).
	DisplayName string `json:"display_name,omitempty"`
	// Auto is the auto-pull switch (v2-converge §5); the max-jobs number
	// rides queue.max_concurrent.
	Auto bool `json:"auto"`
	// Workspaces is the mapped workspace set (polyforge#113): every
	// [workspace] path the daemon routes capsules into, in mapping
	// declaration order. A multi-workspace daemon must never present one
	// workspace as if it were the only one.
	Workspaces   []string    `json:"workspaces"`
	ShuttingDown bool        `json:"shutting_down"`
	Jobs         []statusJob `json:"jobs"`
	Queue        queueStats  `json:"queue"`
}

type opsConfigResponse struct {
	Auto bool `json:"auto"`
	Max  int  `json:"max"`
}

type closeResponse struct {
	WorkItemID string `json:"work_item_id"`
	// Outcome: sealed | already_terminal | already_paused | abandoned (a repair_pending give-up) — plus
	// "terminated" (polyforge#116): a parked workflow's §6.4 terminate
	// verdict, landed at the hub by the terminate-blocked door (no local
	// seal exists — the bounded close already sealed the parked capsule).
	Outcome string `json:"outcome"`
	Status  string `json:"status,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// resumeBlockedResponse is the POST /jobs/{id}/resume-blocked answer
// (polyforge#116): the §6.4 owner move re-established execution — the
// parked workflow is unblocked and the frozen step re-dispatches in place.
type resumeBlockedResponse struct {
	Resumed    bool   `json:"resumed"`
	WorkItemID string `json:"work_item_id"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code, detail string) {
	ae := api.Error{Code: code, Detail: detail}
	writeJSON(w, ae.HTTPStatus(), ae)
}

// writeErrOf surfaces a typed api error verbatim (CORE-* transparency)
// and wraps everything else as DAEMON-500.
func writeErrOf(w http.ResponseWriter, err error) {
	var ae *api.Error
	if errors.As(err, &ae) {
		writeJSON(w, ae.HTTPStatus(), ae)
		return
	}
	writeErr(w, api.ErrDaemonInternal, err.Error())
}

// writeRepairErr answers a RepairResume refusal: the unmapped project keeps
// the DAEMON-412 placement code, a missing capsule the new DAEMON-517;
// both explain exactly what to restore.
func writeRepairErr(w http.ResponseWriter, err error) {
	var unm *runtime.UnmappedMidflightError
	if errors.As(err, &unm) {
		writeErr(w, api.ErrDaemonUnmappedProject, err.Error())
		return
	}
	writeErr(w, api.ErrDaemonRepairPending, err.Error())
}

func writeAdmissionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errShuttingDown):
		writeErr(w, api.ErrDaemonGone, err.Error())
	default:
		writeErrOf(w, err)
	}
}

// handler assembles the full route table.
func (d *Daemon) handler() http.Handler {
	mux := http.NewServeMux()
	obs := d.obs.Handler()
	// The observe lane's own routes, mounted verbatim (04 §5.1a).
	mux.Handle("GET /jobs/{wi}/observe", obs)
	// v2 (polyforge#106): answer routes through the driver bridge when
	// the WI's job carries a bridge Handle; otherwise the observe
	// lane's honest no-live-handle refusal applies.
	mux.HandleFunc("POST /jobs/{wi}/answer", func(w http.ResponseWriter, r *http.Request) {
		wi := r.PathValue("wi")
		if h := d.rt.DriverAnswerHandle(wi); h != nil {
			// driver path: decode the same wire shape, deliver to bridge
			var payload struct {
				QuestionID string `json:"question_id"`
				Answer     string `json:"answer"`
				AnswerText string `json:"answer_text"` // CLI sends this field name
				AnsweredBy string `json:"answered_by"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, `{"code":"BAD-400","detail":"decode: `+err.Error()+`"}`, 400)
				return
			}
			// Wire compatibility: the CLI sends answer_text, the observe
			// face sends answer — accept both.
			if payload.Answer == "" {
				payload.Answer = payload.AnswerText
			}
			if payload.AnsweredBy == "" {
				payload.AnsweredBy = r.Header.Get("X-PF-User")
			}
			// When the CLI couldn't pre-resolve the question ID (observe
			// face returns 404 for driver WIs), read it from the capsule's
			// execution.json pending_question.
			if payload.QuestionID == "" {
				if qid := d.pendingQuestionID(wi); qid != "" {
					payload.QuestionID = qid
				}
			}
			if payload.QuestionID == "" {
				http.Error(w, `{"code":"ANSWER-400","detail":"no pending question found for this work item"}`, 400)
				return
			}
			if err := h.Answer(payload.QuestionID, payload.Answer, payload.AnsweredBy); err != nil {
				http.Error(w, `{"code":"ANSWER-409","detail":"`+err.Error()+`"}`, 409)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
			return
		}
		obs.ServeHTTP(w, r)
	})
	mux.HandleFunc("POST /jobs/{wi}/claim", d.submitRoute("claim"))
	mux.HandleFunc("POST /jobs/{wi}/resume", d.submitRoute("resume"))
	// polyforge#116: the §6.4 owner move on a workflow parked blocked on an
	// honest incomplete result — the state the bounded close leaves has no
	// live driver, no bridge handle and no job record; the owner's moves
	// are their own route, not a runtime call.
	mux.HandleFunc("POST /jobs/{wi}/resume-blocked", d.resumeBlockedRoute)
	// v2 §6 abandon: the give-up of a repair_pending WI (session ended by
	// the daemon after the CLI's human-key authorization step).
	mux.HandleFunc("POST /jobs/{wi}/abandon-repair", d.abandonRepairRoute)
	mux.HandleFunc("POST /jobs/{wi}/takeover", d.submitRoute("takeover"))
	mux.HandleFunc("POST /jobs/{wi}/adopt-session", d.adoptSessionRoute)
	mux.HandleFunc("POST /jobs/{wi}/pause", d.pauseRoute)
	mux.HandleFunc("POST /jobs/{wi}/wrap", d.wrapRoute)
	mux.HandleFunc("POST /jobs/{wi}/ship", d.shipRoute)
	mux.HandleFunc("GET /status", d.statusRoute)
	mux.HandleFunc("POST /ops/config", d.opsConfigRoute)
	mux.HandleFunc("POST /shutdown", d.shutdownRoute)
	mux.HandleFunc("POST /chat/init", d.chatInitRoute)
	// pf watch: the per-WI snapshot and the daemon-held chat subscriptions.
	mux.HandleFunc("GET /jobs/{wi}/watch", d.watchSnapshotRoute)
	mux.HandleFunc("GET /chats/{id}/subscriptions", d.subscriptionsRoute)
	mux.HandleFunc("POST /chats/{id}/subscribe", d.subscribeRoute(true))
	mux.HandleFunc("POST /chats/{id}/unsubscribe", d.subscribeRoute(false))
	mux.HandleFunc("POST /chats/{id}/ack", d.ackRoute)
	mux.HandleFunc("POST /chats/{id}/heartbeat", d.heartbeatRoute)
	mux.HandleFunc("GET /chats/{id}/attention", d.attentionRoute)
	return mux
}

// decodeBody reads one bounded JSON body; an empty body is not an error
// (callers that tolerate {} keep tolerating it).
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeErr(w, api.ErrDaemonBadInput, "body read: "+err.Error())
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeErr(w, api.ErrDaemonBadInput, "body: "+err.Error())
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// submit family: claim | resume | takeover.

func (d *Daemon) submitRoute(intent string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wi := r.PathValue("wi")
		var body struct {
			Priority string     `json:"priority"`
			Origin   *reqOrigin `json:"origin,omitempty"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if err := d.modes.AdmitSubmit(); err != nil {
			writeAdmissionErr(w, err)
			return
		}
		// Canonical id (F2): the queue, the dispatcher and the runtime
		// bookkeeping all key by the canonical id the claim resolves to;
		// a slug spelling would leave the queue's slot marks unable to
		// reconcile against the runtime's canonical job keys. The
		// operator's spelling is registered as an alias so later route
		// calls by it resolve without another aihub round-trip. An
		// unresolvable spelling passes through unchanged — the claim
		// path resolves (or refuses) server-side, exactly as before.
		canonical := d.canonWI(wi)
		d.rt.NoteAlias(wi, canonical)
		// v2 §6 "continue": a repair_pending WI (capsule missing / project
		// unmapped at restart) is resumed IN PLACE with its kept session —
		// re-check first; still broken answers the typed error.
		if intent == "resume" {
			cursor := d.lastSeq(canonical)
			if held, rerr := d.rt.RepairResume(d.runCtx, canonical); held {
				if rerr != nil {
					writeRepairErr(w, rerr)
					return
				}
				d.autoSubscribe(body.Origin, canonical, cursor)
				writeJSON(w, http.StatusOK, submitResponse{Accepted: true, WorkItemID: canonical})
				return
			}
		}
		// The live check is the runtime's own job set (the queue's slot
		// marks are accounting, reconciled a tick later); a pending
		// duplicate is refused on the queue.
		if ji, live := d.rt.Info(canonical); live {
			// Finding 2 (v1.5 review): a RETAINED paused job is not an active
			// run — a submit over it flows to the hub's own gate. A plain
			// claim answers ALREADY_HELD there (the paused session still
			// holds the work item); a takeover replaces the paused record
			// (the s11 continuation shape) and proceeds.
			if ji.State != runtime.StatePaused {
				writeErr(w, api.ErrDaemonConflict, "work item is already active on this daemon")
				return
			}
		}
		if d.q.IsPending(canonical) {
			writeErr(w, api.ErrDaemonConflict, "work item is already queued on this daemon")
			return
		}
		// polyforge#113 placement pre-check, synchronous on the submit
		// face: the work item's project must be mapped, or the refusal
		// reaches the operator's CLI (typed DAEMON-412) BEFORE the job is
		// enqueued and before any capsule or server-side claim state
		// exists. The dispatcher's runtime-side guard is the backstop.
		if probe, err := d.hub.GetWorkItem(canonical); err == nil && probe != nil && probe.ProjectName != "" {
			if _, merr := d.cfg.Mapping.WorkspaceFor(probe.ProjectName); merr != nil {
				writeErr(w, api.ErrDaemonUnmappedProject, fmt.Sprintf("project %q is not in the [workspace] mapping of this machine — the claim is refused before any capsule is created; add the project to a [workspace.\"<path>\"] section in the config (`pf doctor` checks the mapping)", probe.ProjectName))
				return
			}
		}
		// Accepted = enqueued; the dispatcher spawns within one tick,
		// priority-desc against the machine's concurrency cap.
		// `accepted` is answered only once the entry is on disk (v2 §5).
		cursor := d.lastSeq(canonical)
		if err := d.q.Enqueue(qEntry{WorkItemID: canonical, Priority: body.Priority, Intent: intent, Origin: body.Origin.origin()}); err != nil {
			writeErr(w, api.ErrDaemonInternal, "submit queue could not be persisted: "+err.Error())
			return
		}
		// pf watch: the submitting chat is subscribed (subscription =
		// adoption); events from here on are its to consume.
		d.autoSubscribe(body.Origin, canonical, cursor)
		writeJSON(w, http.StatusOK, submitResponse{Accepted: true, WorkItemID: canonical})
	}
}

// ---------------------------------------------------------------------------
// resume-blocked: the §6.4 owner move on a parked workflow (polyforge#116).

// resumeBlockedRoute serves POST /jobs/{id}/resume-blocked — the owner's
// resume verdict on a workflow parked blocked on an honest incomplete
// result. The bounded close dropped the job and ended the session, so this
// route re-establishes execution AFTER the CLI has landed the hub's
// resume-blocked door with the human's own credential (never the daemon's
// machine key — the door's owner/maintainer/admin check is about the human):
// (blocked → queued, the claim gate stays queued-only), then a plain claim
// that runs synchronously — the claim/start path's owner move
// (bridge.Start → driver.ResumeCurrent) unblocks the parked workflow and
// the loop re-dispatches the frozen in_progress step in place as a fresh
// derivation (the parked journal and prior step output survive). The claim
// is the authority for everything else; a refused door (the WI is not
// blocked, or it is terminal) surfaces verbatim.
func (d *Daemon) resumeBlockedRoute(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("wi")
	var body struct {
		Priority string     `json:"priority"`
		Origin   *reqOrigin `json:"origin,omitempty"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := d.modes.AdmitSubmit(); err != nil {
		writeAdmissionErr(w, err)
		return
	}
	canonical := d.canonWI(wi)
	d.rt.NoteAlias(wi, canonical)
	// The live check mirrors the submit family: a live run owns starts —
	// the owner's verdict is for the parked state only (a retained paused
	// record flows to the hub's own gate, as in the submit family).
	if ji, live := d.rt.Info(canonical); live {
		if ji.State != runtime.StatePaused {
			writeErr(w, api.ErrDaemonConflict, "work item is already active on this daemon")
			return
		}
	}
	if d.q.IsPending(canonical) {
		writeErr(w, api.ErrDaemonConflict, "work item is already queued on this daemon")
		return
	}
	// The hub's resume-blocked door is NOT called here (v2-converge §7):
	// its owner/maintainer/admin check must evaluate the HUMAN who typed
	// `pf resume`, so the CLI lands the door with the user's own api key
	// first; this route is only the claim/start half. A WI still blocked
	// at the hub is refused by the claim gate itself.
	// The daemon's run context, NOT the HTTP request's: the claim's ctx
	// is the driver's parent (bridge.Start derives from it) — a request-
	// scoped ctx would cancel the driver mid-dispatch the moment the CLI
	// hangs up on this answer (the dispatch then dies with "context
	// canceled" and the recovery lane re-derives at the job counter —
	// measured exactly that way on the s28 shape). The dispatcher starts
	// every claim on d.runCtx for the same reason.
	d.rt.NoteOrigin(canonical, body.Origin.origin())
	cursor := d.lastSeq(canonical)
	if err := d.rt.StartIntent(d.runCtx, canonical, "claim"); err != nil {
		writeErrOf(w, err)
		return
	}
	d.autoSubscribe(body.Origin, canonical, cursor)
	writeJSON(w, http.StatusOK, resumeBlockedResponse{Resumed: true, WorkItemID: canonical})
}

// abandonRepairRoute serves POST /jobs/{id}/abandon-repair — the give-up of
// a WI parked repair_pending (v2 §6). The WI is still running at aihub under
// this daemon's kept session, so the hub's cancel refuses it and only the
// daemon holds the secret to end that session (canceled for `pf cancel`,
// failed for `pf wrap --fail`; a refused SessionEnd is retried through the
// end_pending lane). Like the resume-blocked door, the human authorization
// (reporter / maintainer / admin) is the CLI's job and runs with the human's
// own key BEFORE this route; this is the daemon half. A WI that is not
// parked here answers DAEMON-404.
func (d *Daemon) abandonRepairRoute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Verdict string `json:"verdict"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	var status string
	switch body.Verdict {
	case "canceled":
		status = api.StatusCanceled
	case "failed":
		status = api.StatusFailed
	default:
		writeErr(w, api.ErrDaemonBadInput, "verdict must be canceled or failed")
		return
	}
	wi := d.canonWI(r.PathValue("wi"))
	held, err := d.rt.AbandonRepair(wi, status, "abandoned while parked repair_pending by the owner ("+body.Verdict+")")
	if err != nil {
		writeErrOf(w, err)
		return
	}
	if !held {
		writeErr(w, api.ErrDaemonNotFound, wi+" is not parked repair_pending on this daemon")
		return
	}
	writeJSON(w, http.StatusOK, closeResponse{WorkItemID: wi, Outcome: "abandoned", Status: status})
}

// ---------------------------------------------------------------------------
// adopt-session: custody of an externally minted session (#109).

// adoptSessionRoute takes a session secret minted OUTSIDE any runtime
// start — the CLI-side takeover's mint — into this daemon's durable
// custody (#109). Without it the secret would die with the CLI process
// and the hub row would keep naming a session no process can ever use
// (the #97/#109 orphaned holder). The adoption is verify-backed: the
// daemon confirms with the hub that the presented secret really is the
// live holder BEFORE journaling it, so custody is never recorded for a
// swap that did not land.
func (d *Daemon) adoptSessionRoute(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("wi")
	var body struct {
		SessionID string `json:"session_id"`
		Owner     string `json:"owner"`
		Reason    string `json:"reason"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.SessionID) == "" {
		writeErr(w, api.ErrDaemonBadInput, "session_id required (the minted session secret)")
		return
	}
	canonical := d.canonWI(wi)
	// Custody makes no sense for work already live here (a retained
	// PAUSED record coexists: the operator may take over a WI paused on
	// this very daemon — the eventual takeover start replaces the record).
	if ji, live := d.rt.Info(canonical); live && ji.State != runtime.StatePaused {
		writeErr(w, api.ErrDaemonConflict, "work item is already active on this daemon")
		return
	}
	v, err := d.hub.SessionVerify(canonical, body.SessionID)
	if err != nil {
		writeErrOf(w, err)
		return
	}
	if !v.Matches {
		writeErr(w, api.ErrDaemonConflict, "session is not the live holder of "+canonical)
		return
	}
	if err := d.rt.AdoptSession(canonical, body.SessionID); err != nil {
		writeErrOf(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adoptResponse{
		Adopted:            true,
		WorkItemID:         canonical,
		SessionFingerprint: api.SessionFingerprint(body.SessionID),
	})
}

// ---------------------------------------------------------------------------
// pause / wrap: the CLI close paths. Both pass the seal guard; the loser
// of a race between two closes answers DAEMON-409, and a close
// that finds the work item already terminal answers gracefully (the
// arbitration's idempotency contract).

// parkedWIClose handles a close that finds no local job because the
// workflow is parked blocked on an honest incomplete result (polyforge#116):
// the bounded close dropped the job, so the owner's moves on this state are
// the §6.4 face, not a runtime close — `--fail` is the owner's terminate
// verdict, landed by the CLI at the hub's terminate-blocked door with the
// human's key (refused here with a hint); a wrap (success) or a pause of a parked workflow has nothing to
// seal — refused with the owner-move copy. Returns true when it answered;
// a non-blocked work item falls through to the caller's terminalOf/404.
func (d *Daemon) parkedWIClose(w http.ResponseWriter, wi, reason string) bool {
	item, err := d.hub.GetWorkItem(wi)
	if err != nil || item == nil || item.Status != api.StatusBlocked {
		return false
	}
	// The terminate verdict (`--fail`) is NOT landed here: the hub's
	// terminate-blocked door authorizes the HUMAN (reporter / maintainer /
	// admin), so the CLI calls it with the user's own key; the daemon's
	// machine key is not that caller. There is no local state to clean —
	// the bounded close already dropped the job.
	if reason == protocol.FinishFailed {
		writeErr(w, api.ErrDaemonConflict,
			"the workflow is parked blocked — its failed verdict is landed by the CLI with your own credential (`pf wrap --fail <id>` from an up-to-date pf), or abandon it with `pf cancel <id>`")
		return true
	}
	writeErr(w, api.ErrDaemonConflict,
		"the workflow is parked blocked (incomplete result) — the owner's moves are `pf resume <id>` to continue or `pf cancel <id>` to abandon (`pf wrap --fail <id>` records a failed verdict); there is no completed run to wrap or pause")
	return true
}

// closeJob runs one CLI-side close through Runtime.Stop.
func (d *Daemon) closeJob(w http.ResponseWriter, ctx context.Context, wi, reason string) {
	wi = d.canonWI(wi) // a slug spelling closes the canonical job (F2)
	ji, ok := d.rt.Info(wi)
	if !ok {
		if d.parkedWIClose(w, wi, reason) {
			return
		}
		if st, terr := d.terminalOf(wi); terr == nil {
			writeJSON(w, http.StatusOK, closeResponse{
				WorkItemID: wi, Outcome: "already_terminal", Status: st,
			})
			return
		}
		writeErr(w, api.ErrDaemonNotFound, "no local job for "+wi+" on this daemon")
		return
	}
	if ji.RepairPending != "" {
		writeErr(w, api.ErrDaemonConflict, wi+" is parked repair_pending — fix the cause and `pf resume "+wi+"`, or give it up with `pf cancel "+wi+"` / `pf wrap --fail "+wi+"`")
		return
	}
	if ji.State == runtime.StateSealing || ji.State == runtime.StateRecovering {
		writeErr(w, api.ErrDaemonConflict, "work item is "+string(ji.State)+"; a close is already in progress")
		return
	}
	// Finding 2 (v1.5 review): a repeat pause of a RETAINED paused job is
	// the graceful idempotency answer, not a conflict — the close already
	// happened and the record is exactly where the contract wants it.
	if reason == reasonPaused && ji.State == runtime.StatePaused {
		writeJSON(w, http.StatusOK, closeResponse{
			WorkItemID: wi, Outcome: "already_paused", Status: string(ji.State),
		})
		return
	}
	if !d.claimSeal(wi) {
		writeErr(w, api.ErrDaemonConflict, "a close is already in progress for this work item")
		return
	}
	defer d.releaseSeal(wi)
	// The agent's death in this close is expected: arm the monitor's
	// suppression BEFORE the teardown starts, so a fast exit poll can
	// never win the race against the RTFinished teardown and journal a
	// spurious needs_recovery for a healthy run (the runtime's SEALING
	// fence drops the OB message either way — the event log is the
	// observable lie this closes).
	d.obs.MarkClosing(wi)
	if err := d.rt.Stop(ctx, wi, reason); err != nil {
		graceful, ae := d.mapCloseErr(err)
		if graceful {
			writeJSON(w, http.StatusOK, closeResponse{WorkItemID: wi, Outcome: "already_terminal"})
			return
		}
		writeJSON(w, ae.HTTPStatus(), ae)
		return
	}
	writeJSON(w, http.StatusOK, closeResponse{
		WorkItemID: wi, Outcome: "sealed", Reason: reason,
	})
}

// terminalOf looks the work item up at aihub and reports its status when
// terminal ("" + error otherwise).
func (d *Daemon) terminalOf(wi string) (string, error) {
	item, err := d.hub.GetWorkItem(wi)
	if err != nil {
		return "", err
	}
	if terminalStatus(item.Status) {
		return item.Status, nil
	}
	return "", errors.New("work item is " + item.Status + " elsewhere")
}

// reasonPaused is the pause close reason, passed through SessionEnd's
// status field: the hub marks the work item paused (the operator pause —
// clean_pause semantics live hub-side). Since the v1.5 review Finding 2
// the pause close RETAINS the runtime record (StatePaused): the session
// secret survives locally so the holder daemon's restart can attach
// once the operator clears clean_pause (see runtime.pauseOp).
const reasonPaused = protocol.FinishPaused

func (d *Daemon) pauseRoute(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !decodeBody(w, r, &body) {
		return
	}
	d.closeJob(w, r.Context(), r.PathValue("wi"), reasonPaused)
}

func (d *Daemon) wrapRoute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	reason := protocol.FinishWrapped
	switch body.Reason {
	case "", "wrap":
	case "fail":
		reason = protocol.FinishFailed
	default:
		writeErr(w, api.ErrDaemonBadInput, "unknown wrap reason: "+body.Reason)
		return
	}
	d.closeJob(w, r.Context(), r.PathValue("wi"), reason)
}

// ---------------------------------------------------------------------------
// ops face: status / ops config (auto + max) / shutdown.

// statusJobs builds the jobs[] bookkeeping (with the running-slot count);
// the status route and the watch lane share it. The per-job needs_input
// projection (D6) is derived from the observe lane's monitors (keyed by
// the canonical id — the same key rt.List() reports); the same loop
// carries the per-job liveness reading (step age + last real-work-signal
// age).
func (d *Daemon) statusJobs() ([]statusJob, int) {
	pending := d.obs.PendingInputs()
	activity := d.obs.Activities()
	jobs := make([]statusJob, 0, 8)
	// running slots: a retained PAUSED job (Finding 2) holds no agent and
	// no queue slot — the ops face must not count it as running work.
	running := 0
	for _, ji := range d.rt.List() {
		if ji.State != runtime.StatePaused && ji.RepairPending == "" {
			running++
		}
		sj := statusJob{
			WorkItemID: ji.WorkItem,
			Status:     string(ji.State),
			// the rendered identity is the PUBLIC session handle plus the
			// derivation counter (B2/B3) — never the secret.
			SpawnN:             ji.SpawnN,
			SessionFingerprint: ji.SessionFingerprint,
			AgentPID:           ji.AgentPID,
			Origin:             ji.Origin,
		}
		if ji.RepairPending != "" {
			// v2 §6: parked at restart (capsule missing / project
			// unmapped), session kept — shown as repair_pending, the
			// reason kind alongside.
			sj.Status = "repair_pending"
			sj.RepairReason = ji.RepairPending
		}
		if pi, ok := pending[ji.WorkItem]; ok {
			sj.QuestionID = pi.QuestionID
			// the presentation rule: runtime says running but the monitor
			// holds a pending question → needs_input wins. Any other
			// runtime state (sealing, recovering) keeps its own name — a
			// close in progress outranks the question it may carry.
			if ji.State == runtime.StateRunning {
				sj.Status = protocol.JobNeedsInput
				sj.NeedsInput = true
			}
		}
		if act, ok := activity[ji.WorkItem]; ok && act.InDispatch {
			if !act.StepStartedAt.IsZero() {
				age := int64(time.Since(act.StepStartedAt) / time.Second)
				sj.StepAgeSec = &age
			}
			if !act.LastWorkAt.IsZero() {
				ago := int64(time.Since(act.LastWorkAt) / time.Second)
				sj.LastWorkAgoSec = &ago
			}
		}
		jobs = append(jobs, sj)
	}
	return jobs, running
}

func (d *Daemon) statusRoute(w http.ResponseWriter, r *http.Request) {
	// D6: the per-job needs_input projection, derived from the observe
	// lane's monitors (keyed by the canonical id — the same key
	// rt.List() reports). F15c rides the same loop: the per-job progress
	// staleness for the LAST-PROGRESS column.
	jobs, running := d.statusJobs()
	writeJSON(w, http.StatusOK, statusResponse{
		MachineID:    d.cfg.MachineID,
		DisplayName:  d.cfg.DisplayName,
		Auto:         d.modes.Auto(),
		Workspaces:   d.cfg.Mapping.Workspaces(),
		ShuttingDown: d.modes.ShuttingDown(),
		Jobs:         jobs,
		Queue: queueStats{
			Pending:       d.q.LenPending(),
			Running:       running,
			MaxConcurrent: d.q.MaxConcurrent(),
		},
	})
}

// opsConfigRoute is POST /ops/config {"auto":bool?,"max":int?}: the one
// switch and the one number. At least one must be present.
func (d *Daemon) opsConfigRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Auto *bool `json:"auto"`
		Max  *int  `json:"max"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Auto == nil && req.Max == nil {
		writeErr(w, api.ErrDaemonBadInput, "ops config requires auto and/or max")
		return
	}
	if req.Max != nil && *req.Max < 1 {
		writeErr(w, api.ErrDaemonBadInput, "max must be >= 1 (DAEMON-400)")
		return
	}
	if req.Auto != nil {
		if err := d.modes.SetAuto(*req.Auto); err != nil {
			writeAdmissionErr(w, err)
			return
		}
	}
	if req.Max != nil {
		if err := d.q.SetMaxConcurrent(*req.Max); err != nil {
			writeErr(w, api.ErrDaemonBadInput, err.Error())
			return
		}
	}
	d.persistModeQuietly()
	writeJSON(w, http.StatusOK, opsConfigResponse{Auto: d.modes.Auto(), Max: d.q.MaxConcurrent()})
}

func (d *Daemon) shutdownRoute(w http.ResponseWriter, r *http.Request) {
	// Attribute the shutdown to the dialing process (polyforge#110):
	// the UDS peer credential — the kernel's SO_PEERCRED record of who
	// is on the other end, never anything from the request body
	// (caller-supplied; the CLI sends an empty object).
	cred, ok := requestPeerCred(r.Context())
	cause := RouteShutdownCause(cred, ok)
	// Answer immediately; the bounded sequence runs to completion in the
	// background and Run returns when it finishes (04 §6.5).
	writeJSON(w, http.StatusOK, map[string]any{"shutting_down": true})
	go func() {
		if err := d.Shutdown(context.Background(), cause); err != nil {
			// already surfaced on stderr by the sequence; nothing else to do
			_ = err
		}
	}()
}

// pendingQuestionID reads the capsule's execution.json to find the
// current pending question ID (driver WIs: the observe face doesn't
// carry it, but execution.json does).
func (d *Daemon) pendingQuestionID(wi string) string {
	wid := d.rt.CanonicalID(wi)
	if wid == "" {
		wid = wi
	}
	execPath := filepath.Join(d.rt.CapsuleDirFor(wid), "runtime-state", "execution.json")
	data, err := os.ReadFile(execPath)
	if err != nil {
		return ""
	}
	var exec struct {
		Steps map[string]struct {
			PendingQuestion *struct {
				ID string `json:"id"`
			} `json:"pending_question"`
		} `json:"steps"`
	}
	if json.Unmarshal(data, &exec) != nil {
		return ""
	}
	for _, st := range exec.Steps {
		if st.PendingQuestion != nil && st.PendingQuestion.ID != "" {
			return st.PendingQuestion.ID
		}
	}
	return ""
}
