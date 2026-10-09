package daemon

// routes.go: the daemon's UDS HTTP face. The observe lane's three routes
// (observe/answer/flush) are mounted VERBATIM from observe.Handler();
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
	NeedsInput bool   `json:"needs_input,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
	// LastProgressAgoSec is the F15c owner face: whole seconds since the
	// job's stream last proved AGENT progress (nil = no progress record
	// yet — the CLI renders "-"). UI-noise records never refresh it, so
	// a wedged-but-chattering job reads as stale, not fresh.
	LastProgressAgoSec *int64 `json:"last_progress_ago_sec,omitempty"`
}

type queueStats struct {
	Pending       int `json:"pending"`
	Running       int `json:"running"`
	MaxConcurrent int `json:"max_concurrent"`
}

type statusResponse struct {
	MachineID string `json:"machine_id"`
	Mode      string `json:"mode"`
	// Workspaces is the mapped workspace set (polyforge#113): every
	// [workspace] path the daemon routes capsules into, in mapping
	// declaration order. A multi-workspace daemon must never present one
	// workspace as if it were the only one.
	Workspaces   []string    `json:"workspaces"`
	ShuttingDown bool        `json:"shutting_down"`
	Jobs         []statusJob `json:"jobs"`
	Queue        queueStats  `json:"queue"`
}

type modeResponse struct {
	Mode              string `json:"mode"`
	MaxConcurrentJobs int    `json:"max_concurrent_jobs,omitempty"`
}

type recommendationResponse struct {
	Items []api.WorkItem `json:"items"`
	Total int            `json:"total"`
}

type closeResponse struct {
	WorkItemID string `json:"work_item_id"`
	Outcome    string `json:"outcome"` // sealed | already_terminal
	Status     string `json:"status,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type batchResponse struct {
	Submitted   int      `json:"submitted"`
	WorkItemIDs []string `json:"work_item_ids"`
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
	case errors.Is(err, errModeConflict):
		writeErr(w, api.ErrDaemonConflict, err.Error())
	case errors.Is(err, errBadMode):
		writeErr(w, api.ErrDaemonBadInput, err.Error())
	default:
		writeErrOf(w, err)
	}
}

// handler assembles the full route table.
func (d *Daemon) handler() http.Handler {
	mux := http.NewServeMux()
	obs := d.obs.Handler()
	// The observe lane's own three routes, mounted verbatim (04 §5.1a /
	// ruling C2): the flush route passes through the wrap arbitration
	// gate first (the dual-path close must be one winner, one 409).
	mux.Handle("GET /jobs/{wi}/observe", obs)
	// v2 (polyforge#106): answer routes through the driver bridge when
	// the WI's job carries a bridge Handle; otherwise the observe
	// lane's agent-facing answer path applies.
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
	mux.Handle("POST /jobs/{wi}/flush", d.flushGate(obs))
	// F6-B: the narrow memory channel — mounted verbatim from the
	// observe lane, whose serveFlush fence semantics the two routes
	// share (the flush route alone passes through the wrap arbitration
	// gate first; recall/remember are stateless proxies and carry no
	// close semantics at all).
	mux.Handle("POST /jobs/{wi}/recall", obs)
	mux.Handle("POST /jobs/{wi}/remember", obs)
	mux.HandleFunc("POST /jobs/{wi}/claim", d.submitRoute("claim"))
	mux.HandleFunc("POST /jobs/{wi}/resume", d.submitRoute("resume"))
	mux.HandleFunc("POST /jobs/{wi}/takeover", d.submitRoute("takeover"))
	mux.HandleFunc("POST /jobs/{wi}/adopt-session", d.adoptSessionRoute)
	mux.HandleFunc("POST /jobs/{wi}/pause", d.pauseRoute)
	mux.HandleFunc("POST /jobs/{wi}/wrap", d.wrapRoute)
	mux.HandleFunc("POST /jobs/{wi}/ship", d.shipRoute)
	mux.HandleFunc("GET /status", d.statusRoute)
	mux.HandleFunc("POST /mode", d.modeRoute)
	mux.HandleFunc("POST /batch_submit", d.batchRoute)
	mux.HandleFunc("POST /shutdown", d.shutdownRoute)
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
			Priority string `json:"priority"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if err := d.modes.AdmitSingle(); err != nil {
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
		d.q.Enqueue(qEntry{WorkItemID: canonical, Priority: body.Priority, Intent: intent})
		writeJSON(w, http.StatusOK, submitResponse{Accepted: true, WorkItemID: canonical})
	}
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
// of a race with the agent's flush(wrap) answers DAEMON-409, and a close
// that finds the work item already terminal answers gracefully (the
// arbitration's idempotency contract).

// closeJob runs one CLI-side close through Runtime.Stop.
func (d *Daemon) closeJob(w http.ResponseWriter, ctx context.Context, wi, reason string) {
	wi = d.canonWI(wi) // a slug spelling closes the canonical job (F2)
	ji, ok := d.rt.Info(wi)
	if !ok {
		if st, terr := d.terminalOf(wi); terr == nil {
			writeJSON(w, http.StatusOK, closeResponse{
				WorkItemID: wi, Outcome: "already_terminal", Status: st,
			})
			return
		}
		writeErr(w, api.ErrDaemonNotFound, "no local job for "+wi+" on this daemon")
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
// flush gate: the wrap arbitration on the agent's own flush path. Only
// reason=wrap claims the seal guard (needs_input and shutdown are the
// agent's ordinary lifeline, never a close). The recovery window — new
// instance alive, recovered() not yet back to RUNNING — is answered
// with the typed retryable RECOVERING transient, never a "close in
// progress" refusal: no close is in progress there.
func (d *Daemon) flushGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wi := d.canonWI(r.PathValue("wi")) // F2: gate on the canonical job
		raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeErr(w, api.ErrDaemonBadInput, "body read: "+err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var req protocol.FlushRequest
		if json.Unmarshal(raw, &req) == nil && req.Reason == protocol.FlushWrap {
			if ji, ok := d.rt.Info(wi); ok {
				if ji.State == runtime.StateSealing {
					writeErr(w, api.ErrDaemonConflict, "work item is sealing; a close is already in progress")
					return
				}
				if ji.State == runtime.StateRecovering {
					if d.sealHeld(wi) {
						// close holds the guard with its Stop queued behind
						// the recovery: genuinely "close in progress".
						writeErr(w, api.ErrDaemonConflict, "a close is already in progress for this work item")
						return
					}
					// no close anywhere: the recovery window answers the
					// typed retryable RECOVERING transient.
					writeErr(w, api.ErrDaemonConflict,
						"work item is recovering; instance just spawned; retry shortly")
					return
				}
			}
			if !d.claimSeal(wi) {
				writeErr(w, api.ErrDaemonConflict, "a close is already in progress for this work item")
				return
			}
			defer d.releaseSeal(wi)
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// ops face: status / mode / batch_submit / shutdown.

func (d *Daemon) statusRoute(w http.ResponseWriter, r *http.Request) {
	// D6: the per-job needs_input projection, derived from the observe
	// lane's monitors (keyed by the canonical id — the same key
	// rt.List() reports). F15c rides the same loop: the per-job progress
	// staleness for the LAST-PROGRESS column.
	pending := d.obs.PendingInputs()
	progress := d.obs.ProgressAges()
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
			Status:     string(ji.State),
			// the rendered identity is the PUBLIC session handle plus the
			// derivation counter (B2/B3) — never the secret.
			SpawnN:             ji.SpawnN,
			SessionFingerprint: ji.SessionFingerprint,
			AgentPID:           ji.AgentPID,
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
		if pa, ok := progress[ji.WorkItem]; ok && !pa.LastProgressAt.IsZero() {
			ago := int64(time.Since(pa.LastProgressAt) / time.Second)
			sj.LastProgressAgoSec = &ago
		}
		jobs = append(jobs, sj)
	}
	writeJSON(w, http.StatusOK, statusResponse{
		MachineID:    d.cfg.MachineID,
		Mode:         d.modes.Current(),
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

func (d *Daemon) modeRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action            string `json:"action"`
		Mode              string `json:"mode"`
		MaxConcurrentJobs *int   `json:"max_concurrent_jobs"`
		// Project is the #113 optional ready-queue scope: absent, the
		// recommendation merges ALL mapped projects; present, it covers
		// exactly that project (validated by the CLI against the local
		// mapping before the call).
		Project string `json:"project,omitempty"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	switch req.Action {
	case "set":
		if req.Mode == "" && req.MaxConcurrentJobs == nil {
			writeErr(w, api.ErrDaemonBadInput, "mode set requires mode and/or max_concurrent_jobs")
			return
		}
		if req.Mode != "" {
			switch req.Mode {
			case protocol.ModeInteractive, protocol.ModeBatch, protocol.ModeDrain:
			default:
				writeErr(w, api.ErrDaemonBadInput, "mode must be interactive|batch|drain")
				return
			}
			if err := d.modes.Set(req.Mode); err != nil {
				writeAdmissionErr(w, err)
				return
			}
		}
		if req.MaxConcurrentJobs != nil {
			if err := d.q.SetMaxConcurrent(*req.MaxConcurrentJobs); err != nil {
				writeErr(w, api.ErrDaemonBadInput, err.Error())
				return
			}
		}
		d.persistModeQuietly()
		writeJSON(w, http.StatusOK, modeResponse{Mode: d.modes.Current(), MaxConcurrentJobs: d.q.MaxConcurrent()})
	case "drain":
		if err := d.modes.Set(protocol.ModeDrain); err != nil {
			writeAdmissionErr(w, err)
			return
		}
		d.persistModeQuietly()
		writeJSON(w, http.StatusOK, modeResponse{Mode: d.modes.Current()})
	case "abort-drain":
		if err := d.modes.AbortDrain(); err != nil {
			writeAdmissionErr(w, err)
			return
		}
		d.persistModeQuietly()
		writeJSON(w, http.StatusOK, modeResponse{Mode: d.modes.Current()})
	case "get_recommendation":
		// Batch's read-only wave projection: the aihub ready queue,
		// forwarded as-is. NO job is created (04 §6.1b). polyforge#113:
		// without a scope this merges ALL mapped projects (mapping
		// declaration order); with one, exactly that project — validated
		// against THIS daemon's mapping (the placement authority): an
		// unmapped scope is a typed refusal naming the project, before
		// any wire call.
		if req.Project != "" {
			if _, merr := d.cfg.Mapping.WorkspaceFor(req.Project); merr != nil {
				writeErr(w, api.ErrDaemonUnmappedProject, fmt.Sprintf("project %q is not in the [workspace] mapping of this machine — the recommendation cannot be scoped to it; add the project to a [workspace.\"<path>\"] section in the config (`pf doctor` checks the mapping)", req.Project))
				return
			}
		}
		var items []api.WorkItem
		for _, project := range d.queueProjects(req.Project) {
			pis, err := d.hub.Queue(project)
			if err != nil {
				writeErrOf(w, err)
				return
			}
			items = append(items, pis...)
		}
		writeJSON(w, http.StatusOK, recommendationResponse{Items: items, Total: len(items)})
	default:
		writeErr(w, api.ErrDaemonBadInput, "unknown mode action: "+req.Action)
	}
}

func (d *Daemon) batchRoute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WIIDs []string `json:"wi_ids"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.WIIDs) == 0 {
		writeErr(w, api.ErrDaemonBadInput, "batch_submit requires a non-empty wi_ids set")
		return
	}
	if err := d.modes.AdmitBatch(); err != nil {
		writeAdmissionErr(w, err)
		return
	}
	// The whole confirmed wave is enqueued (04 §12 ruling 0: never
	// refuse the batch for exceeding the cap — pending entries
	// backfill as slots open); duplicates of locally active/pending
	// ids are skipped, the wave is never widened. Every entry is
	// canonicalized (F2) so the queue and the runtime agree on keys.
	// polyforge#113: an entry whose project is unmapped has no capsule
	// home on this machine — it is skipped loudly (the wave is never
	// widened with unplaceable work).
	submitted := make([]string, 0, len(body.WIIDs))
	for _, wi := range body.WIIDs {
		canonical := wi
		priority := "normal"
		project := ""
		if item, err := d.hub.GetWorkItem(wi); err == nil && item != nil {
			if item.ID != "" {
				canonical = item.ID
			}
			priority = item.Priority
			project = item.ProjectName
		}
		if project != "" {
			if _, merr := d.cfg.Mapping.WorkspaceFor(project); merr != nil {
				fmt.Fprintf(os.Stderr, "daemon: batch_submit skipped %s: project %q is not in the [workspace] mapping (`pf doctor` checks the mapping)\n", canonical, project)
				continue
			}
		}
		d.rt.NoteAlias(wi, canonical)
		if _, live := d.rt.Info(canonical); live || d.q.IsPending(canonical) {
			continue
		}
		d.q.Enqueue(qEntry{WorkItemID: canonical, Priority: priority, Intent: "claim"})
		submitted = append(submitted, canonical)
	}
	writeJSON(w, http.StatusOK, batchResponse{Submitted: len(submitted), WorkItemIDs: submitted})
}

func (d *Daemon) shutdownRoute(w http.ResponseWriter, r *http.Request) {
	// Answer immediately; the bounded sequence runs to completion in the
	// background and Run returns when it finishes (04 §6.5).
	writeJSON(w, http.StatusOK, map[string]any{"shutting_down": true})
	go func() {
		if err := d.Shutdown(context.Background()); err != nil {
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
