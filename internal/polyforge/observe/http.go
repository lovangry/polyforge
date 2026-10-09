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
	// F6-B: the narrow memory channel, same face and same spawn-n
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

// serveAnswer is the answer route's post-trim shape (polyforge#106):
// the WI-Agent answer relay is deleted — the driver consumes answers —
// so this face only answers honestly. An unmonitored work item gets the
// ordinary not-monitored 404 (the fail-fast posture the idle-daemon
// shape has always answered); a monitored one names the missing live
// bridge handle: the workflow is between derivations and no relay path
// exists any more.
func (o *Observe) serveAnswer(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("wi")
	if o.lookup(wi) == nil {
		writeErr(w, api.ErrDaemonNotFound, "work item is not monitored on this daemon")
		return
	}
	writeErr(w, api.ErrDaemonConflict,
		"no live bridge handle for work item "+wi+": the answer relay lane is deleted (polyforge#106) and the workflow is between derivations — retry after the daemon re-derives it")
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
