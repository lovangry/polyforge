package daemon

// routes.go: the daemon's UDS HTTP face. Every route translates the CLI
// contract (internal/polyforge/cli/verbs_daemon.go — the pinned route
// family) into Runtime/observe calls. Every error
// answer is the TOP-LEVEL api.Error envelope {code, detail} (the same
// typed shape the aihub face uses; DAEMON-* codes per reference.md §5).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
	"github.com/silgrid/polyforge/internal/polyforge/supervisor"
	"github.com/silgrid/polyforge/internal/polyforge/workspace"
)

// The wire answer shapes — byte-identical to the CLI's decode structs
// (cli.SubmitResponse etc.); the daemon keeps its own definitions and the
// tests pin the two together field by field.

type submitResponse struct {
	Accepted   bool   `json:"accepted"`
	WorkItemID string `json:"work_item_id"`
}

type statusJob struct {
	WorkItemID string `json:"work_item_id"`
	// Slug is the "<project>#<seq>" display name ("" on a pre-slug record).
	Slug   string `json:"slug,omitempty"`
	Status string `json:"status"`
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
	NeedsInput bool   `json:"needs_input,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
	// StepAgeSec / LastWorkAgoSec are the owner liveness face (the ops
	// status columns): whole seconds since the current dispatch began and
	// since its harness last did REAL work (nil = no dispatch in flight,
	// or no work-class signal yet — the CLI renders "-"). UI-noise
	// records never refresh the work clock, so a wedged-but-chattering
	// job reads as stale, not fresh.
	StepAgeSec     *int64 `json:"step_age_sec,omitempty"`
	LastWorkAgoSec *int64 `json:"last_work_ago_sec,omitempty"`
	// Candidate is the in-flight dispatch's model candidate,
	// `<harness>:<model>[:<effort>]` (polyforge#176); absent between
	// dispatches or on the harness's default model.
	Candidate string `json:"candidate,omitempty"`
	// Origin is how the job was submitted; may be absent.
	Origin *runtime.Origin `json:"origin,omitempty"`
	// StopReason is the recorded verdict of a close in flight or
	// settled (wrapped/failed/blocked/paused) — empty while the job
	// runs. The attention face (D9) derives the terminal decision
	// classes from it.
	StopReason string `json:"stop_reason,omitempty"`
	// AwaitingHub: the restart-time session verify could not reach aihub
	// and is being retried — a RECOVERING job waiting for the hub, not an
	// ordinary recovery.
	AwaitingHub bool `json:"awaiting_hub,omitempty"`
}

type queueStats struct {
	Pending       int `json:"pending"`
	Running       int `json:"running"`
	MaxConcurrent int `json:"max_concurrent"`
	// MaxSource: "ops" (an explicit pf ops max) or "config".
	MaxSource string `json:"max_source"`
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
	Workspaces []string `json:"workspaces"`
	// WorkflowsDir is the workflows dir bundles run from (absolute; pf
	// doctor checks the workspace yaml's step-id keys against it).
	WorkflowsDir string `json:"workflows_dir,omitempty"`
	// Harnesses is the daemon's view of the step harness binaries
	// (polyforge#159): found, version, credential variables set/unset —
	// never a value. Only on request (GET /status?harnesses=1: it may run
	// `claude --version`); pf doctor judges the claude harness by it.
	Harnesses    map[string]harnessStatus `json:"harnesses,omitempty"`
	ShuttingDown bool                     `json:"shutting_down"`
	// EnvWarnings: retired POLYFORGE_* variables the daemon found set to
	// a differing value at boot (no longer read; pf doctor relays them).
	EnvWarnings []string `json:"env_warnings,omitempty"`
	// EnvNames are the NAMES (never values) of the daemon's own
	// environment, sorted: pf doctor flags a daemon carrying ANTHROPIC_*
	// or other credential-looking names outside the ensure allow-list
	// (supervisor/env.go).
	EnvNames []string `json:"env_names,omitempty"`
	// PiModelsEnvRefs are the variable NAMES the step agent dir's
	// models.json interpolates (`$NAME`); pf doctor warns on those no
	// ensure-started daemon carries.
	PiModelsEnvRefs []string    `json:"pi_models_env_refs,omitempty"`
	Jobs            []statusJob `json:"jobs"`
	Queue           queueStats  `json:"queue"`
}

type opsConfigResponse struct {
	Auto bool `json:"auto"`
	Max  int  `json:"max"`
	// MaxSource is where Max comes from: "ops" (explicit pf ops max) or
	// "config" (cli.toml [daemon] / flag / default).
	MaxSource string `json:"max_source"`
}

type closeResponse struct {
	WorkItemID string `json:"work_item_id"`
	Slug       string `json:"slug,omitempty"` // display name, from the job record
	// Outcome: sealed | already_terminal | already_paused.
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
	// Deferred: the hub was unreachable (DAEMON-503) when the claim ran;
	// the claim is queued in the persisted submit queue and retried with
	// backoff, exactly like a submit.
	Deferred bool `json:"deferred,omitempty"`
}

// resumePausedResponse is the POST /jobs/{id}/resume-paused answer
// (wi_seWTgHeKtaa8): the operator's `pf resume` on a work item this
// daemon holds PAUSED — the live-holder continuation. Attached: the
// paused session was re-attached (SessionAttach) and the work item
// continues IN PLACE (same semantics as attaching after a restart,
// the pending-question re-pose included). AlreadyLive: the work item
// is already running here (an earlier resume or a live dispatch won).
type resumePausedResponse struct {
	Resumed    bool   `json:"resumed"`
	WorkItemID string `json:"work_item_id"`
	// Outcome: attached | already_live | closed.
	Outcome string `json:"outcome,omitempty"`
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

func writeAdmissionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errShuttingDown):
		writeErr(w, api.ErrDaemonGone, err.Error())
	default:
		writeErrOf(w, err)
	}
}

// handler assembles the full route table behind the step-origin guard.
func (d *Daemon) handler() http.Handler {
	return d.refuseStepOrigin(d.routes())
}

// routes is the bare route table.
func (d *Daemon) routes() *http.ServeMux {
	mux := http.NewServeMux()
	// v2 (polyforge#106): answer routes through the driver bridge when
	// the WI's job carries a bridge Handle; otherwise it refuses. A step
	// process never gets here (refuseStepOrigin, polyforge#122).
	mux.HandleFunc("POST /jobs/{wi}/answer", func(w http.ResponseWriter, r *http.Request) {
		wi := r.PathValue("wi")
		if h := d.rt.DriverAnswerHandle(wi); h != nil {
			// driver path: decode the same wire shape, deliver to bridge
			var payload struct {
				QuestionID string `json:"question_id"`
				AnswerText string `json:"answer_text"`
				AnsweredBy string `json:"answered_by"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				writeErr(w, api.ErrDaemonBadInput, "decode: "+err.Error())
				return
			}
			if payload.AnsweredBy == "" {
				payload.AnsweredBy = r.Header.Get("X-PF-User")
			}
			// No question named: the daemon resolves the pending one from
			// the capsule's execution.json — accepted ONLY when exactly one
			// question is pending (audit B10). Zero or several is a typed
			// refusal pointing at --question: a guess across several open
			// questions would deliver the answer to an arbitrary one (the
			// steps map's iteration order) and is never honest.
			if payload.QuestionID == "" {
				ids := d.pendingQuestionIDs(wi)
				switch len(ids) {
				case 0:
					writeErr(w, api.ErrDaemonBadInput, "no pending question found for this work item")
					return
				case 1:
					payload.QuestionID = ids[0]
				default:
					writeErr(w, api.ErrDaemonBadInput,
						fmt.Sprintf("this work item has %d pending questions (%s) — answer a specific one with --question",
							len(ids), strings.Join(ids, ", ")))
					return
				}
			}
			if err := h.Answer(payload.QuestionID, payload.AnswerText, payload.AnsweredBy); err != nil {
				writeErr(w, api.ErrDaemonConflict, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "question_id": payload.QuestionID})
			return
		}
		if _, live := d.rt.Info(d.canonWI(wi)); !live {
			writeErr(w, api.ErrDaemonNotFound, "work item is not running on this daemon")
			return
		}
		writeErr(w, api.ErrDaemonConflict,
			"no live bridge handle for work item "+wi+": the workflow is between derivations — retry after the daemon re-derives it")
	})
	mux.HandleFunc("POST /jobs/{wi}/claim", d.submitRoute())
	// polyforge#116: the §6.4 owner move on a workflow parked blocked on an
	// honest incomplete result — the state the bounded close leaves has no
	// live driver, no bridge handle and no job record; the owner's moves
	// are their own route, not a runtime call.
	mux.HandleFunc("POST /jobs/{wi}/resume-blocked", d.resumeBlockedRoute)
	// wi_seWTgHeKtaa8 (v2-converge §7): the live-holder half of the
	// operator's `pf resume` on a PAUSED work item — the CLI has already
	// cleared clean_pause at the hub; this route makes that observable
	// by the ONLINE holder (the startup reconciliation is the only other
	// consumer of that hub state, and it runs once per daemon start).
	mux.HandleFunc("POST /jobs/{wi}/resume-paused", d.resumePausedRoute)
	mux.HandleFunc("POST /jobs/{wi}/pause", d.pauseRoute)
	mux.HandleFunc("POST /jobs/{wi}/wrap", d.wrapRoute)
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
	// the step agent's memory read face (stepmem.go): reads only, scoped
	// to the calling step's own work item and project
	mux.HandleFunc("GET /step/context", d.stepContextRoute)
	mux.HandleFunc("GET /step/memories/search", d.stepSearchRoute)
	mux.HandleFunc("GET /step/memories/{id}", d.stepMemoryRoute)
	mux.HandleFunc("POST /chats/{id}/attention/ack", d.attentionAckRoute)
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
// submit: claim.

func (d *Daemon) submitRoute() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wi := r.PathValue("wi")
		var body struct {
			Priority string     `json:"priority"`
			Models   []string   `json:"models,omitempty"` // manual candidate list (pf submit --models)
			Origin   *reqOrigin `json:"origin,omitempty"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if body.Models != nil {
			m, merr := workspace.NormalizeModels(body.Models)
			if merr != nil {
				writeErr(w, api.ErrDaemonBadInput, "models: "+merr.Error())
				return
			}
			body.Models = m
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
		// The live check is the runtime's own job set (the queue's slot
		// marks are accounting, reconciled a tick later); a pending
		// duplicate is refused on the queue.
		if ji, live := d.rt.Info(canonical); live {
			// Finding 2 (v1.5 review): a RETAINED paused job is not an active
			// run — a submit over it flows to the hub's own gate. A plain
			// claim answers ALREADY_HELD there while the paused session still
			// holds the work item; once an admin force-ended it (WI queued)
			// the claim replaces the paused record and proceeds.
			if ji.State != runtime.StatePaused {
				writeErr(w, api.ErrDaemonConflict, "work item is already active on this daemon")
				return
			}
		}
		if d.q.IsPending(canonical) {
			writeErr(w, api.ErrDaemonConflict, "work item is already queued on this daemon")
			return
		}
		// Pre-claim placement + workspace-yaml check, synchronous on the
		// submit face: an unmapped project (DAEMON-412) or an invalid
		// workspace yaml (DAEMON-414) reaches the operator's CLI BEFORE
		// the job is enqueued and before any capsule or server-side claim
		// state exists. The runtime's own pre-claim check is the
		// backstop. A failed probe refuses nothing here (the submit is
		// durable-queued; the dispatcher retries the probe at Start).
		if probe, err := d.hub.GetWorkItem(canonical); err == nil && probe != nil && probe.ProjectName != "" {
			if _, lerr := d.rt.LoadWorkspace(probe.ProjectName); lerr != nil {
				writeErrOf(w, lerr)
				return
			}
			// polyforge#175: the work item's installed bundle must load
			// (every candidate `<harness>:<model>`) — refused here so
			// `pf submit` sees it directly
			if probe.BundleName != nil {
				if berr := d.rt.CheckBundle(canonical, *probe.BundleName); berr != nil {
					writeErrOf(w, berr)
					return
				}
			}
		}
		// Accepted = enqueued; the dispatcher spawns within one tick,
		// priority-desc against the machine's concurrency cap.
		// `accepted` is answered only once the entry is on disk (v2 §5).
		cursor := d.lastSeq(canonical)
		if err := d.q.Enqueue(qEntry{WorkItemID: canonical, Priority: body.Priority, Models: body.Models, Origin: body.Origin.origin()}); err != nil {
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
	if err := d.rt.Start(d.runCtx, canonical); err != nil {
		// The CLI already flipped the WI blocked -> queued at the hub; a
		// transient hub loss before the claim must not strand it there:
		// queue the claim like a submit (persisted, retried with backoff).
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.ErrDaemonHubUnavailable {
			if qerr := d.q.Enqueue(qEntry{WorkItemID: canonical, Priority: body.Priority, Origin: body.Origin.origin()}); qerr != nil {
				writeErr(w, api.ErrDaemonInternal, "submit queue could not be persisted: "+qerr.Error())
				return
			}
			fmt.Fprintf(os.Stderr, "daemon: resume %s deferred, queued for the claim: %v\n", canonical, err)
			d.autoSubscribe(body.Origin, canonical, cursor)
			writeJSON(w, http.StatusOK, resumeBlockedResponse{Resumed: true, WorkItemID: canonical, Deferred: true})
			return
		}
		if errors.Is(err, runtime.ErrClaimClosed) {
			// a pf wrap/pause landed while this claim was materializing
			writeErr(w, api.ErrDaemonConflict, err.Error())
			return
		}
		writeErrOf(w, err)
		return
	}
	d.autoSubscribe(body.Origin, canonical, cursor)
	writeJSON(w, http.StatusOK, resumeBlockedResponse{Resumed: true, WorkItemID: canonical})
}

// ---------------------------------------------------------------------------
// resume-paused: the live-holder continuation (wi_seWTgHeKtaa8).

// resumePausedRoute serves POST /jobs/{id}/resume-paused — the
// live-holder half of the operator's `pf resume` on a work item this
// daemon holds PAUSED. The CLI has already PATCHed clean_pause=false at
// the hub (the resume's hub-side effect). The startup reconciliation
// (runtime/restart.go) runs exactly once per daemon process, so without
// this route a LIVE holder would never re-observe the cleared flag and a
// paused work item would sit parked until its holder restarted. This
// route runs the same reconciliation for the ONE retained paused record
// NOW: verify the session still holds the WI (paused, clean_pause
// cleared), re-attach it (SessionAttach) and continue the current step
// IN PLACE through the recovery engine — the exact semantics attaching
// after a restart has, including re-posing a pending unanswered question
// and waiting for the human answer before any dispatch.
//
// The daemon's run context, NOT the HTTP request's (see
// resumeBlockedRoute): the recovery continuation must survive the CLI
// hanging up on this answer.
func (d *Daemon) resumePausedRoute(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("wi")
	var body struct {
		Origin *reqOrigin `json:"origin,omitempty"`
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
	outcome, err := d.rt.ResumePaused(d.runCtx, canonical)
	if err != nil {
		writeErrOf(w, err)
		return
	}
	if outcome == runtime.ResumeAttached {
		// the pause close released the job's observe monitor; the
		// in-place resume rebuilds it (the same re-attachment a restart
		// does for its recovered jobs) so the QUESTION column, the M4
		// revalidation and the watch faces all come back with the
		// continuation.
		if ji, live := d.rt.Info(canonical); live {
			d.reattachJob(ji)
		}
	}
	writeJSON(w, http.StatusOK, resumePausedResponse{
		Resumed: outcome == runtime.ResumeAttached, WorkItemID: canonical, Outcome: outcome,
	})
}

// ---------------------------------------------------------------------------
// pause / wrap: the CLI close paths. Both pass the seal guard; the loser
// of a race between two closes answers DAEMON-409, and a close
// that finds the work item already terminal answers gracefully (the
// arbitration's idempotency contract).

// parkedWIClose handles a close that finds no local job because the
// workflow is parked blocked on an honest incomplete result (polyforge#116):
// the bounded close dropped the job, so a wrap (success) or a `--fail` of
// a parked workflow has nothing to seal — refused with the owner-move
// copy: the blocked exits are continue (`pf resume`) and abandon
// (`pf cancel`). Returns true when it answered; a non-blocked work item
// falls through to the caller's terminalOf/404.
func (d *Daemon) parkedWIClose(w http.ResponseWriter, wi string) bool {
	item, err := d.hub.GetWorkItem(wi)
	if err != nil || item == nil || item.Status != api.StatusBlocked {
		return false
	}
	writeErr(w, api.ErrDaemonConflict,
		"the work item is parked blocked (an incomplete result, or a pause that landed while its claim was still materializing — see its claim_paused_ended_blocked event) — the owner's moves are `pf resume <id>` to continue or `pf cancel <id>` to abandon; there is no completed run to wrap or fail")
	return true
}

// closeJob runs one CLI-side close through Runtime.Stop.
func (d *Daemon) closeJob(w http.ResponseWriter, ctx context.Context, wi, reason string) {
	wi = d.canonWI(wi) // a slug spelling closes the canonical job (F2)
	ji, ok := d.rt.Info(wi)
	if !ok {
		if d.parkedWIClose(w, wi) {
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
	if ji.AwaitingHub {
		writeErr(w, api.ErrDaemonConflict, "work item is waiting for aihub: the restart could not verify its session and keeps retrying — it resumes (or closes) once aihub answers; retry then")
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
			WorkItemID: wi, Slug: ji.Slug, Outcome: "already_paused", Status: string(ji.State),
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
	// The memories the run's steps left in the outbox go up before the
	// seal: a RUNNING job's agent is stopped first (no step writes behind
	// the flush), then the bounded final flush (it closes the outbox),
	// then rt.Stop seals — its own teardown finds the driver already
	// stopped. Every other state goes straight to rt.Stop's own admission.
	if ji.State == runtime.StateRunning && ji.Agent != nil && !ji.Agent.Done() {
		if err := ji.Agent.Terminate(outboxStopWait); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: %s: stopping the agent before the close: %v\n", wi, err)
		}
	}
	d.finalFlush(outboxTarget{wi: ji.WorkItem, project: ji.Project, capsuleDir: ji.CapsuleDir}, outboxFlushWait)
	stop := d.rt.Stop
	if d.stopFn != nil {
		stop = d.stopFn
	}
	if err := stop(ctx, wi, reason); err != nil {
		graceful, ae := d.mapCloseErr(err)
		if graceful {
			writeJSON(w, http.StatusOK, closeResponse{WorkItemID: wi, Slug: ji.Slug, Outcome: "already_terminal"})
			return
		}
		// The close did not happen: withdraw the expected-exit
		// suppression — an agent this close already stopped goes to the
		// normal recovery — and reopen the outbox for the run that
		// continues (a recovery installs a new handle and reopens it too).
		d.obs.CloseRefused(wi)
		d.reopenOutbox(ji.WorkItem)
		writeJSON(w, ae.HTTPStatus(), ae)
		return
	}
	writeJSON(w, http.StatusOK, closeResponse{
		WorkItemID: wi, Slug: ji.Slug, Outcome: "sealed", Reason: reason,
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
		if ji.State != runtime.StatePaused {
			running++
		}
		sj := statusJob{
			WorkItemID: ji.WorkItem,
			Slug:       ji.Slug,
			Status:     string(ji.State),
			// the rendered identity is the PUBLIC session handle plus the
			// derivation counter (B2/B3) — never the secret.
			SpawnN:             ji.SpawnN,
			SessionFingerprint: ji.SessionFingerprint,
			AgentPID:           ji.AgentPID,
			Origin:             ji.Origin,
			StopReason:         ji.StopReason,
			AwaitingHub:        ji.AwaitingHub,
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
			sj.Candidate = act.Candidate
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
	maxN, maxSrc := d.q.MaxState()
	var harnesses map[string]harnessStatus
	if r.URL.Query().Get("harnesses") == "1" {
		harnesses = d.harnessStatuses()
	}
	writeJSON(w, http.StatusOK, statusResponse{
		Harnesses:       harnesses,
		MachineID:       d.cfg.MachineID,
		DisplayName:     d.cfg.DisplayName,
		Auto:            d.modes.Auto(),
		Workspaces:      d.cfg.Mapping.Workspaces(),
		WorkflowsDir:    d.cfg.WorkflowsDir,
		ShuttingDown:    d.modes.ShuttingDown(),
		EnvWarnings:     d.cfg.EnvWarnings,
		EnvNames:        supervisor.EnvNames(os.Environ()),
		PiModelsEnvRefs: d.piModelsEnvRefs(),
		Jobs:            jobs,
		Queue: queueStats{
			Pending:       d.q.LenPending(),
			Running:       running,
			MaxConcurrent: maxN,
			MaxSource:     maxSrc,
		},
	})
}

// opsConfigRoute is POST /ops/config {"auto":bool?,"max":int?,
// "max_reset":bool?}: the one switch and the one number. At least one
// must be present; max and max_reset exclude each other. max is an
// explicit cap (persisted); max_reset returns to the configured cap.
func (d *Daemon) opsConfigRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Auto     *bool `json:"auto"`
		Max      *int  `json:"max"`
		MaxReset bool  `json:"max_reset"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Auto == nil && req.Max == nil && !req.MaxReset {
		writeErr(w, api.ErrDaemonBadInput, "ops config requires auto, max and/or max_reset")
		return
	}
	if req.Max != nil && req.MaxReset {
		writeErr(w, api.ErrDaemonBadInput, "ops config: max and max_reset exclude each other")
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
		if !*req.Auto {
			// v2-converge §5 (audit B3b): auto off = only explicit pf
			// submit — the not-yet-started origin=auto entries leave the
			// queue (and the persisted file a restart replays); explicit
			// (cli/chat) entries ride untouched.
			d.autoOffPurge()
		}
	}
	if req.Max != nil {
		if err := d.q.SetExplicitMax(*req.Max); err != nil {
			writeErr(w, api.ErrDaemonBadInput, err.Error())
			return
		}
	}
	if req.MaxReset {
		d.q.ResetMax(d.cfg.MaxConcurrent)
	}
	d.persistModeQuietly()
	maxN, maxSrc := d.q.MaxState()
	writeJSON(w, http.StatusOK, opsConfigResponse{Auto: d.modes.Auto(), Max: maxN, MaxSource: maxSrc})
}

func (d *Daemon) shutdownRoute(w http.ResponseWriter, r *http.Request) {
	// Attribute the shutdown to the dialing process (polyforge#110):
	// the UDS peer credential — the kernel's SO_PEERCRED record of who
	// is on the other end, never anything from the request body
	// (caller-supplied; the CLI sends an empty object).
	cred, ok := requestPeerCred(r.Context())
	cause := RouteShutdownCause(cred, ok)
	// Answer immediately; the bounded sequence runs to completion in the
	// background and Run returns when it finishes (reference.md §6.2).
	writeJSON(w, http.StatusOK, map[string]any{"shutting_down": true})
	go func() {
		if err := d.Shutdown(context.Background(), cause); err != nil {
			// already surfaced on stderr by the sequence; nothing else to do
			_ = err
		}
	}()
}

// refuseStepOrigin is the step-origin guard on the whole UDS face
// (v2-converge.md Appendix C(b)): the agent executing a work item
// must never drive the daemon. Every request that is not a read (GET/HEAD —
// status, watch snapshot, subscriptions, attention stay open; a GET
// attention with mark=1 or ack=1 records delivery state, so a step could
// hide a human's notices — it is guarded like a mutation) is refused
// DAEMON-403 when its UDS peer — the kernel's SO_PEERCRED record, never
// anything from the request body — belongs to a step process tree of this
// daemon (runtime.StepProcessOrigin: the PF_WI_ID/PF_DAEMON_ID markers on
// the peer, an ancestor or its process-group leader). A request whose peer
// credential is unavailable is refused too: the guard fails closed. An
// answer keeps its own #122 wording (answerFromStepProcess).
func (d *Daemon) refuseStepOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || attentionRecords(r) {
			if reason := d.stepOriginRefusal(r); reason != "" {
				writeErr(w, api.ErrDaemonForbidden, reason)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// attentionRecords reports a GET /chats/{id}/attention that changes state:
// mark=1 (shown at fetch) or ack=1 (pending until acked). A plain list is a
// read.
func attentionRecords(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/chats/") || !strings.HasSuffix(r.URL.Path, "/attention") {
		return false
	}
	q := r.URL.Query()
	return q.Get("mark") == "1" || q.Get("ack") == "1"
}

// stepOriginRefusal answers the refusal reason for a mutating request
// from a step process (or from an unidentifiable peer), "" when the peer
// may proceed.
func (d *Daemon) stepOriginRefusal(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/jobs/") && strings.HasSuffix(r.URL.Path, "/answer") {
		return d.answerFromStepProcess(r)
	}
	cred, ok := requestPeerCred(r.Context())
	if !ok {
		return fmt.Sprintf("%s %s refused: the caller's identity (UDS peer credential) is unavailable — only a human session may drive the daemon", r.Method, r.URL.Path)
	}
	if stepWI, isStep := runtime.StepProcessOrigin(cred.pid, d.cfg.MachineID); isStep {
		return fmt.Sprintf("%s %s refused: the caller (pid %d) belongs to the step process tree of work item %s — a step agent must never drive the daemon; only a human session may", r.Method, r.URL.Path, cred.pid, stepWI)
	}
	return ""
}

// answerFromStepProcess is the #122 wording of the guard for the answer
// route: a question is a human's to answer. It answers the refusal
// reason, "" when the peer may answer.
func (d *Daemon) answerFromStepProcess(r *http.Request) string {
	cred, ok := requestPeerCred(r.Context())
	if !ok {
		return "answer refused: the caller's identity (UDS peer credential) is unavailable — only a human session may answer"
	}
	if stepWI, isStep := runtime.StepProcessOrigin(cred.pid, d.cfg.MachineID); isStep {
		return fmt.Sprintf("answer refused: the caller (pid %d) belongs to the step process tree of work item %s — a step agent must never answer a question; only a human session may (polyforge#122)", cred.pid, stepWI)
	}
	return ""
}

// pendingQuestionIDs answers the ids of ALL currently open pending
// questions in the capsule's execution.json (empty when none), sorted.
// The answer route accepts a no-id answer only when exactly one id comes
// back (audit B10).
func (d *Daemon) pendingQuestionIDs(wi string) []string {
	wid := d.rt.CanonicalID(wi)
	if wid == "" {
		wid = wi
	}
	ex := d.loadExecution(wid, nil)
	if ex == nil {
		return nil
	}
	var ids []string
	for _, st := range ex.Steps {
		if st != nil && st.PendingQuestion != nil && st.PendingQuestion.ID != "" {
			ids = append(ids, st.PendingQuestion.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
