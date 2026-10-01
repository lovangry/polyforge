package observe

// http.go: the observe lane's UDS routes (04 §5.1; the agent UDS
// flush entry per ruling C2, plus the F6-B memory channel on the same
// face). The handler owns ONLY these routes — mux
// assembly into the daemon's socket server happens in phase 3.
//
//	GET  /jobs/{id}/observe — the JobView projection (two layers:
//	                            execution.json direct read + events ring)
//	POST /jobs/{id}/answer   — the interactive answer relay
//	POST /jobs/{id}/flush    — the agent's flush(reason) entry
//	POST /jobs/{id}/recall   — F6-B: narrow memory search proxy
//	POST /jobs/{id}/remember — F6-B: narrow memory write-back proxy
//
// Every handler routes the request to the per-WI monitor over a buffered
// channel and waits bounded (CallTimeout); the monitor's select loop is
// the only writer of monitor state, so no handler ever races it.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Handler returns the lane's route table as a mountable http.Handler.
func (o *Observe) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jobs/{wi}/observe", o.serveObserve)
	mux.HandleFunc("POST /jobs/{wi}/answer", o.serveAnswer)
	mux.HandleFunc("POST /jobs/{wi}/flush", o.serveFlush)
	// F6-B: the narrow memory channel, same face and same attempt-id
	// fence as flush (memory.go). Stateless proxies — no monitor state.
	mux.HandleFunc("POST /jobs/{wi}/recall", o.serveRecall)
	mux.HandleFunc("POST /jobs/{wi}/remember", o.serveRemember)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code string, detail string) {
	ae := api.Error{Code: code, Detail: detail}
	writeJSON(w, ae.HTTPStatus(), ae)
}

func (o *Observe) serveObserve(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("wi")
	m := o.lookup(wi)
	if m == nil {
		writeErr(w, api.ErrDaemonNotFound, "work item is not monitored on this daemon")
		return
	}
	view := m.JobView()
	// The ?after cursor (#21): serve only events with seq > after (the
	// CLI's --after flag). seq counts total appends, so the cursor stays
	// meaningful across ring turnover; an after past every retained seq
	// answers an empty events layer.
	if s := r.URL.Query().Get("after"); s != "" {
		after, err := strconv.ParseInt(s, 10, 64)
		if err != nil || after < 0 {
			writeErr(w, api.ErrDaemonBadInput, "after must be a non-negative integer cursor")
			return
		}
		if after > 0 {
			ring := o.log.SinceAfter(wi, after)
			// Daemon-scope entries (wi="") merge into the cursor answer
			// too, same as the full view — a cursor poll must not silently
			// drop the drain degraded signal. The cursor applies to each
			// counter independently (per-WI and daemon-scope seqs are
			// separate counters).
			ring = append(ring, o.log.SinceAfter("", after)...)
			sort.SliceStable(ring, func(i, j int) bool { return ring[i].At < ring[j].At })
			view.Events = make([]protocol.EventEntry, 0, len(ring))
			for _, e := range ring {
				view.Events = append(view.Events, e)
			}
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// serveAnswer relays a human answer. Caller identity rides X-PF-User
// (04 §5.1: the local UDS face has no authenticated user layer; a
// missing header degrades to trusting the relay's self-reported
// answered_by).
func (o *Observe) serveAnswer(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("wi")
	m := o.lookup(wi)
	if m == nil {
		writeErr(w, api.ErrDaemonNotFound, "work item is not monitored on this daemon")
		return
	}
	var relay AnswerRelay
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&relay); err != nil {
		writeErr(w, api.ErrDaemonBadInput, "answer relay body: "+err.Error())
		return
	}
	caller := r.Header.Get("X-PF-User")
	if caller == "" {
		caller = relay.AnsweredBy // degraded local trust (documented shape)
	}
	call := AnswerCall{Relay: relay, Caller: caller, Resp: make(chan error, 1)}
	select {
	case m.answerCh <- call:
	case <-time.After(o.cfg.CallTimeout):
		writeErr(w, api.ErrDaemonInternal, "monitor did not accept the answer in time")
		return
	}
	var err error
	select {
	case err = <-call.Resp:
	case <-time.After(o.cfg.CallTimeout + o.cfg.AnswerAckWindow):
		writeErr(w, api.ErrDaemonInternal, "answer delivery did not settle in time")
		return
	}
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{
			"outcome": "consumed", "answer_id": relay.AnswerID,
		})
	case errors.Is(err, ErrAnswerAckPending):
		// Delivered, consumption pending: NOT an error-grade outcome —
		// the CLI tells the human so instead of printing a failure.
		writeJSON(w, http.StatusOK, map[string]any{
			"outcome": "ack_pending", "answer_id": relay.AnswerID,
		})
	case errors.Is(err, ErrAnswerConsumed):
		writeErr(w, api.ErrDaemonConflict, err.Error())
	case errors.Is(err, ErrAnswerIdentity):
		writeErr(w, api.ErrDaemonForbidden, err.Error())
	case errors.Is(err, ErrAnswerInFlight):
		writeErr(w, api.ErrDaemonConflict, err.Error())
	default:
		// Delivery failure (agent dead pre-ack, control path lost). The
		// answer_id remains REDELIVERABLE: report the failure but say so.
		writeErr(w, api.ErrDaemonInternal, err.Error()+" (answer_id remains re-deliverable)")
	}
}

// serveFlush is the agent UDS face's flush(reason) entry (ruling C2:
// the agent calls the daemon; answers flow the other way over stdio).
func (o *Observe) serveFlush(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("wi")
	m := o.lookup(wi)
	if m == nil {
		writeErr(w, api.ErrDaemonNotFound, "work item is not monitored on this daemon")
		return
	}
	var req protocol.FlushRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeErr(w, api.ErrDaemonBadInput, "flush request body: "+err.Error())
		return
	}
	if req.WorkItem != "" && req.WorkItem != wi {
		writeErr(w, api.ErrDaemonBadInput, "flush work_item does not match the route")
		return
	}
	switch req.Reason {
	case protocol.FlushNeedsInput, protocol.FlushWrap, protocol.FlushShutdown:
	default:
		writeErr(w, api.ErrDaemonBadInput, "unknown flush reason: "+req.Reason)
		return
	}
	// The B2 spawn fence (v1.5 §4.3): the request's SpawnN must name the
	// monitor's CURRENT derivation — a zombie survivor of a superseded
	// derivation (the six-layer cleanup admits residue) must never seal
	// work that belongs to the live one. Mismatch — including an absent
	// counter (0), the fail-closed mirror of the old absent-attempt-id
	// refusal — answers DAEMON-409 STALE_SPAWN and never reaches the
	// monitor. EXCEPT the recovery window: a counter that matches the
	// hub's LATEST ANNOUNCED derivation (registered at RTStarted/
	// RTRecovered emission time, which the monitor's adoption lags)
	// passes — the just-resumed agent's own flush is never stale. The
	// monitor queues such a call until its adoption completes (onFlush),
	// so an older counter can still never reach the flush paths.
	cur := m.currentSpawn()
	known := o.spawnKnown(wi)
	if req.SpawnN == 0 ||
		(req.SpawnN != cur && req.SpawnN != known) ||
		// Adoption-window cross-check (the s10 hole, carried over from
		// the attempt-id fence): a successor announcement supersedes the
		// predecessor IMMEDIATELY — a counter that still equals the
		// monitor's lagging cur is already stale when known names a
		// DIFFERENT (newer) derivation; only a zombie speaks in the
		// predecessor's name.
		(req.SpawnN == cur && known != 0 && req.SpawnN != known) {
		writeErr(w, api.ErrDaemonConflict,
			"STALE_SPAWN: flush carries spawn_n "+strconv.Itoa(req.SpawnN)+
				"; the current derivation is "+strconv.Itoa(cur)+
				" (latest announced: "+strconv.Itoa(known)+")")
		return
	}
	// F9 fail-closed gate: a needs_input flush without a question id can
	// never be answered — the answer relay binds on the id — and an empty
	// id accepted here mints an unanswerable hanging pending (the pilot
	// measured exactly that). The question id is minted once by the
	// agent, persisted in pending_question and re-presented verbatim; an
	// empty one is a contract violation and is refused at the entry with
	// DAEMON-400, never reaching the monitor.
	if req.Reason == protocol.FlushNeedsInput && req.QuestionID == "" {
		writeErr(w, api.ErrDaemonBadInput,
			"flush(needs_input) requires a non-empty question_id (minted once, persisted in pending_question, re-presented verbatim; an empty id can never be answered)")
		return
	}
	call := FlushCall{Req: req, Resp: make(chan protocol.FlushAck, 1)}
	select {
	case m.flushCh <- call:
	case <-time.After(o.cfg.CallTimeout):
		writeErr(w, api.ErrDaemonInternal, "monitor did not accept the flush in time")
		return
	}
	// The flush bound is the protocol's own 60s; the ack (even an
	// OK=false one) is the response, never an HTTP error.
	select {
	case ack := <-call.Resp:
		writeJSON(w, http.StatusOK, ack)
	case <-time.After(o.cfg.CallTimeout):
		writeErr(w, api.ErrDaemonInternal, "flush did not settle in time")
	}
}
