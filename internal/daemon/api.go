package daemon

// api.go: the Daemon operations face (04 §5.1): a Unix-domain-socket HTTP
// server, local-only by construction (no TCP listener anywhere). The
// proxy_call route additionally verifies the peer via SO_PEERCRED against
// the jobs[] main_agent_pid (04 §5.3): only a WI Agent this daemon derived
// may use the authenticated local proxy, and only for the whitelist.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/sync/aihub"
)

// DAEMON error codes (04 §5.2).
const (
	ErrCodeNotFound     = "DAEMON-404"
	ErrCodeForbidden    = "DAEMON-403"
	ErrCodeConflict     = "DAEMON-409"
	ErrCodeShuttingDown = "DAEMON-410"
	ErrCodeSpawnFail    = "DAEMON-503"
	ErrCodePreflight    = "DAEMON-507"
	ErrCodeFlushFail    = "DAEMON-511"
)

type apiError struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func writeAPIErr(w http.ResponseWriter, code string, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatusFor(code))
	json.NewEncoder(w).Encode(map[string]any{"error": apiError{Code: code, Detail: detail}})
}

func httpStatusFor(code string) int {
	switch code {
	case "DAEMON-400":
		return http.StatusBadRequest
	case ErrCodeNotFound:
		return http.StatusNotFound
	case ErrCodeForbidden:
		return http.StatusForbidden
	case ErrCodeConflict:
		return http.StatusConflict
	case ErrCodeShuttingDown:
		return http.StatusGone
	case ErrCodeSpawnFail:
		return http.StatusServiceUnavailable
	case ErrCodePreflight, ErrCodeFlushFail:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

func writeJSONOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// APIServer is the local operations face.
type APIServer struct {
	d    *Daemon
	path string
	srv  *http.Server
	ln   net.Listener
}

// NewAPIServer binds the socket. A live socket held by another daemon fails
// the connect probe and the bind — the single-instance lock (04 §7.2): we
// only ever remove a socket that no one is serving.
func NewAPIServer(d *Daemon, socketPath string) (*APIServer, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, err
	}
	if conn, err := net.Dial("unix", socketPath); err == nil {
		conn.Close()
		return nil, fmt.Errorf("daemon: socket %s is served by another daemon (single-instance lock)", socketPath)
	}
	os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: cannot bind %s: %w", socketPath, err)
	}
	s := &APIServer{d: d, path: socketPath, ln: &credListener{ln}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs/{id}/claim", s.hSubmit("claim"))
	mux.HandleFunc("POST /jobs/{id}/resume", s.hSubmit("resume"))
	mux.HandleFunc("POST /jobs/{id}/takeover", s.hSubmit("takeover"))
	mux.HandleFunc("GET /jobs/{id}/observe", s.hObserve)
	mux.HandleFunc("POST /jobs/{id}/answer", s.hAnswer)
	mux.HandleFunc("POST /jobs/{id}/publish_request", s.hPublishRequest)
	mux.HandleFunc("POST /jobs/{id}/pause", s.hPause)
	mux.HandleFunc("POST /jobs/{id}/wrap", s.hWrap)
	mux.HandleFunc("GET /status", s.hStatus)
	mux.HandleFunc("POST /mode", s.hMode)
	mux.HandleFunc("POST /batch_submit", s.hBatchSubmit)
	mux.HandleFunc("POST /shutdown", s.hShutdown)
	mux.HandleFunc("POST /proxy_call", s.hProxyCall)
	s.srv = &http.Server{Handler: mux, ConnContext: s.connContext}
	return s, nil
}

// Serve blocks until Close.
func (s *APIServer) Serve() error { return s.srv.Serve(s.ln) }

// Close shuts the face down and removes the socket. With the accepted
// conns kept in non-blocking (poller-registered) mode by credListener's
// SO_PEERCRED handling (see credListener.Accept), srv.Close unblocks
// every idle keep-alive read through the poller — a live pooled CLI
// `pf observe` session no longer hangs the shutdown (polyforge#11
// FIX-ROUND, inference 4).
func (s *APIServer) Close() error {
	err := s.srv.Close()
	os.Remove(s.path)
	return err
}

// ---------------------------------------------------------------------------
// SO_PEERCRED plumbing (04 §5.3)

type peerCredKey struct{}

type credConn struct {
	net.Conn
	pid int
	uid int
}

type credListener struct{ net.Listener }

func (l *credListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	cc := &credConn{Conn: c, pid: -1, uid: -1}
	if uc, ok := c.(*net.UnixConn); ok {
		// SO_PEERCRED via SyscallConn().Control: no File()/Fd() here. The
		// old uc.File()+f.Fd() dance dup'd the fd and (a dup shares the
		// open file description, and File/Fd deliberately return a
		// BLOCKING descriptor) cleared O_NONBLOCK on the SHARED description
		// — flipping the accepted conn into kernel-blocking mode. Its
		// server-side read then blocked inside the syscall, where no close
		// could reach it: APIServer.Close waited on poll.FD.Close's
		// in-flight-IO lock forever whenever a pooled CLI keep-alive conn
		// was left open (a live `pf observe` session would hang a real
		// daemon shutdown). Control runs against the real fd while no I/O
		// is in flight (we are still at accept time) and changes no modes.
		if rc, rerr := uc.SyscallConn(); rerr == nil {
			_ = rc.Control(func(fd uintptr) {
				if cred, gerr := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); gerr == nil {
					cc.pid = int(cred.Pid)
					cc.uid = int(cred.Uid)
				}
			})
		}
	}
	return cc, nil
}

func (s *APIServer) connContext(ctx context.Context, c net.Conn) context.Context {
	if cc, ok := c.(*credConn); ok {
		return context.WithValue(ctx, peerCredKey{}, cc)
	}
	return ctx
}

func peerPID(ctx context.Context) int {
	if cc, ok := ctx.Value(peerCredKey{}).(*credConn); ok {
		return cc.pid
	}
	return -1
}

// ---------------------------------------------------------------------------
// Handlers

func (s *APIServer) hSubmit(intent string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wi := r.PathValue("id")
		var body struct {
			Priority string `json:"priority"`
		}
		_ = decodeJSON(r, &body)
		if body.Priority == "" {
			body.Priority = "normal"
		}
		if err := s.d.Submit(SubmitIntent{WorkItemID: wi, Intent: intent, Priority: body.Priority}); err != nil {
			writeSubmitErr(w, err)
			return
		}
		// Opportunistically spawn immediately (the dispatcher also drains
		// the queue on its tick).
		_, _ = s.d.SpawnNext(r.Context())
		writeJSONOK(w, map[string]any{"accepted": true, "work_item_id": wi})
	}
}

func writeSubmitErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrShuttingDown):
		writeAPIErr(w, ErrCodeShuttingDown, "graceful shutdown in progress")
	case errors.Is(err, ErrModeConflict):
		writeAPIErr(w, ErrCodeConflict, err.Error())
	case errors.Is(err, ErrPreflight):
		writeAPIErr(w, ErrCodePreflight, err.Error())
	case errors.Is(err, ErrFlushFailed):
		writeAPIErr(w, ErrCodeFlushFail, err.Error())
	case errors.Is(err, ErrNoLocalJob):
		writeAPIErr(w, ErrCodeNotFound, err.Error())
	case errors.Is(err, ErrCloseInProgress):
		writeAPIErr(w, ErrCodeConflict, err.Error())
	case errors.Is(err, ErrSecretRolledBack):
		// 04 §7.3: the scene is quarantined by the classifier; recovery is an
		// admin force-takeover, never a retry of this call.
		writeAPIErr(w, ErrCodeForbidden, ReasonSecretRolledBack+": capsule quarantined; recovery is admin force-takeover")
	default:
		// 04 §5.1 amendment (close routes): a typed Core verdict comes back
		// verbatim (CORE-* passthrough), not flattened into DAEMON-500 —
		// CORE-403 stale_generation on the post-crash pause etc. stay
		// machine-readable for the CLI and the recovery tooling.
		var ae *aihub.APIError
		if errors.As(err, &ae) {
			writeCoreErr(w, ae)
			return
		}
		if strings.Contains(err.Error(), "DAEMON-409") {
			writeAPIErr(w, ErrCodeConflict, err.Error())
			return
		}
		writeAPIErr(w, "DAEMON-500", err.Error())
	}
}

// writeCoreErr surfaces one typed Core verdict verbatim: the envelope
// carries the Core's own error code (CORE-403 etc.) with the Core's
// reason/detail as the detail string, and the HTTP status the Core's own
// envelope answered with (APIError.Status) — no invented mapping.
func writeCoreErr(w http.ResponseWriter, ae *aihub.APIError) {
	detail := ae.Detail
	if ae.Reason != "" {
		detail = ae.Reason + ": " + ae.Detail
	}
	status := ae.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": apiError{Code: ae.Code, Detail: detail}})
}

func (s *APIServer) hObserve(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("id")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	s.d.mu.Lock()
	jr, ok := s.d.jobs[wi]
	s.d.mu.Unlock()
	if ok {
		// First layer: the live ring (a VIEW; the authority remains
		// runtime-state/execution.json).
		resp := jr.ring.Observe(after, func() map[string]any {
			if e, err := jr.cap.LoadExecution(); err == nil && e != nil {
				snap := map[string]any{
					"current_step": e.CurrentStep, "workflow_status": e.WorkflowStatus,
					"version": e.Version,
				}
				// The open needs_input question is presented in the 00 §3.4a
				// observation shape (same shape both layers; the ring's
				// needs_input/answered EVENTS carry the §3.4a payload too).
				if v := renderPendingQuestion(e); v != nil {
					snap["pending_question"] = v
				}
				return snap
			}
			return map[string]any{}
		})
		writeJSONOK(w, resp)
		return
	}
	// Second layer: no local capsule → Core read-through of the latest
	// published HEAD, with the freshness note (04 §5.1a). The rendered
	// progress is the published execution.json VERBATIM; the open pending
	// question (if the last published revision caught the step entering
	// needs_input) is presented in the same 00 §3.4a observation shape as
	// the first layer, extracted from that published state.
	headSeq, progress, err := s.d.client.Progress(wi)
	if err != nil {
		writeAPIErr(w, ErrCodeNotFound, "no local job and no published progress for "+wi)
		return
	}
	if progress == nil {
		progress = map[string]any{}
	}
	if e := executionFromProgress(progress); e != nil {
		if v := renderPendingQuestion(e); v != nil {
			progress["pending_question"] = v
		}
	}
	writeJSONOK(w, map[string]any{
		"events":         []map[string]any{},
		"state_snapshot": progress,
		"head_seq":       headSeq,
		"read_through":   true,
		"freshness_note": "数据截至最近一次发布",
	})
}

// pendingQuestionView is the 00 §3.4a needs_input observation shape — the
// presentation-layer projection of the persisted pending_question (03
// §4.2). timeout_hint is optional in §3.4a and the persisted schema carries
// none, so it is omitted rather than invented.
type pendingQuestionView struct {
	QuestionID string `json:"question_id"`
	StepID     string `json:"step_id"`
	AskedAt    string `json:"asked_at"`
	Content    string `json:"content"`
	Status     string `json:"status"` // pending | answered (00 §3.4a)
}

// renderPendingQuestion projects one execution state's open question into
// the §3.4a shape, or nil when no question is open. A question only stays
// in pending_question while unanswered (consumption clears it in the same
// atomic write), so the rendered status is always "pending" — an
// "answered" question never appears in an observe stream.
func renderPendingQuestion(e *capsule.Execution) *pendingQuestionView {
	if e == nil {
		return nil
	}
	q, step := e.PendingQuestion()
	if q == nil {
		return nil
	}
	return &pendingQuestionView{
		QuestionID: q.QuestionID, StepID: step, AskedAt: q.AskedAt,
		Content: q.Content, Status: "pending",
	}
}

// executionFromProgress decodes the read-through progress payload (the
// published execution.json as a generic JSON map) back into the typed
// execution state for question rendering. A decode failure yields nil —
// the verbatim map is still rendered, only the question projection is
// skipped.
func executionFromProgress(progress map[string]any) *capsule.Execution {
	blob, err := json.Marshal(progress)
	if err != nil {
		return nil
	}
	var e capsule.Execution
	if json.Unmarshal(blob, &e) != nil {
		return nil
	}
	return &e
}

func (s *APIServer) hAnswer(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("id")
	var relay AnswerRelay
	if err := decodeJSON(r, &relay); err != nil {
		writeAPIErr(w, "DAEMON-400", err.Error())
		return
	}
	// The submitter's authenticated identity (v1 local face: the caller
	// presents its user id; answered_by must match it — 00 §3.4a).
	//
	// KNOWN v1 DEGRADATION (kept deliberately): this face has no
	// authenticated user layer, so X-PF-User is caller-SELF-DECLARED and an
	// absent header degrades to trusting relay.AnsweredBy itself. The socket
	// ALREADY hands us the peer's kernel identity via SO_PEERCRED (uid in
	// credConn, see peerPID's sibling); a future hardening can map that uid
	// to the local user name (getpwuid-style lookup) and treat a mismatching
	// X-PF-User as a forgery instead of a declaration. Not done in v1:
	// answered_by carries AIHub user ids ("usr_..."), not POSIX uid
	// mappings, so the mapping is a deployment decision, not a trivial
	// change.
	callerID := r.Header.Get("X-PF-User")
	if callerID == "" {
		callerID = relay.AnsweredBy // degraded local trust; recorded as ambiguity
	}
	s.d.mu.Lock()
	jr, ok := s.d.jobs[wi]
	s.d.mu.Unlock()
	if !ok {
		writeAPIErr(w, ErrCodeNotFound, "no local job for "+wi)
		return
	}
	src := &execQuestionSource{load: func() (string, string, bool) {
		e, err := jr.cap.LoadExecution()
		if err != nil || e == nil {
			return "", "", false
		}
		q, step := e.PendingQuestion()
		if q == nil {
			return "", step, false
		}
		return q.QuestionID, step, true
	}}
	deliver := func(rel AnswerRelay) error {
		if jr.agent == nil {
			return errors.New("daemon: agent not attached")
		}
		if err := jr.agent.DeliverAnswer(rel); err != nil {
			return err
		}
		jr.ring.Append(EvAnswered, map[string]any{"question_id": rel.QuestionID, "answer_id": rel.AnswerID})
		return nil
	}
	if err := s.d.ledger.Submit(relay, callerID, src, deliver); err != nil {
		if errors.Is(err, ErrAnswerConsumed) {
			writeAPIErr(w, ErrCodeConflict, err.Error())
			return
		}
		if errors.Is(err, ErrAnswerIdentity) {
			writeAPIErr(w, ErrCodeForbidden, err.Error())
			return
		}
		writeAPIErr(w, "DAEMON-500", err.Error())
		return
	}
	writeJSONOK(w, map[string]any{"applied": true, "answer_id": relay.AnswerID})
}

// execQuestionSource reads the persisted comparison basis (03 §4.2
// pending_question) — the ONLY binding authority for answer relays.
type execQuestionSource struct {
	load func() (questionID, stepID string, ok bool)
}

func (e *execQuestionSource) OpenQuestion() (string, string) {
	q, step, _ := e.load()
	return q, step
}

func (s *APIServer) hPublishRequest(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("id")
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIErr(w, "DAEMON-400", err.Error())
		return
	}
	out, err := s.d.PublishRequest(r.Context(), wi, body.Reason)
	if err != nil {
		writeSubmitErr(w, err)
		return
	}
	writeJSONOK(w, out)
}

// hPause is the 00 §5.1 pause verb on the daemon face: the 04 §6.2 A-path
// close with reason "pause" — boundary notice, final flush, Core
// end-generation carrying the Daemon-generated flush_evidence (02 §6.4),
// then seal. The WI is paused server-side; resume is a fresh claim
// (generation+1) through the ordinary claim route — nothing here re-claims
// or re-queues anything.
func (s *APIServer) hPause(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("id")
	// No fields are defined on the pause body; an empty object is the
	// whole request (the generation token never travels — the daemon
	// holds it and signs the Core call itself).
	var body struct{}
	_ = decodeJSON(r, &body)
	out, err := s.d.PauseJob(r.Context(), wi)
	if err != nil {
		writeSubmitErr(w, err)
		return
	}
	writeJSONOK(w, out)
}

// hWrap is the 00 §5.1 wrap verb: the same 04 §6.2 A-path close with a
// TERMINAL reason (wrap|fail). After a successful wrap the job is sealed
// and gone from jobs[]; a second wrap answers the typed DAEMON-404.
func (s *APIServer) hWrap(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("id")
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIErr(w, "DAEMON-400", err.Error())
		return
	}
	if body.Reason == "" {
		body.Reason = "wrap" // the CLI's non---fail default
	}
	if body.Reason != "wrap" && body.Reason != "fail" {
		writeAPIErr(w, "DAEMON-400", "reason must be wrap or fail")
		return
	}
	out, err := s.d.WrapJob(r.Context(), wi, body.Reason)
	if err != nil {
		writeSubmitErr(w, err)
		return
	}
	writeJSONOK(w, out)
}

func (s *APIServer) hStatus(w http.ResponseWriter, r *http.Request) {
	s.d.mu.Lock()
	jobs := make([]map[string]any, 0, len(s.d.jobs))
	for wi, jr := range s.d.jobs {
		jobs = append(jobs, map[string]any{
			"work_item_id": wi, "harness_adapter": jr.adapter,
			"harness_session_id": jr.sessionID, "ring_latest": jr.ring.Latest(),
		})
	}
	s.d.mu.Unlock()
	// Stale-heartbeat exposure (04 §7.2/§9, monitoring signal ONLY — the
	// daemon never auto-takes-over anything on this list; takeover is a
	// human decision, deliberately not wired here).
	now := time.Now()
	threshold := s.d.staleHeartbeatThreshold()
	stale := s.d.beats.Stale(now, threshold)
	if stale == nil {
		stale = []StaleMachine{}
	}
	writeJSONOK(w, map[string]any{
		"machine_id":                        s.d.cfg.MachineID,
		"mode":                              s.d.modes.Current(),
		"shutting_down":                     s.d.modes.ShuttingDown(),
		"jobs":                              jobs,
		"queue":                             map[string]any{"pending": s.d.jobq.LenPending(), "running": s.d.jobq.Running(), "max_concurrent": s.d.jobq.MaxConcurrent()},
		"stale_machines":                    stale,
		"stale_heartbeat_threshold_seconds": int64(threshold.Seconds()),
	})
}

func (s *APIServer) hMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action            string `json:"action"` // set | drain | abort-drain | get_recommendation
		Mode              string `json:"mode"`
		MaxConcurrentJobs *int   `json:"max_concurrent_jobs"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIErr(w, "DAEMON-400", err.Error())
		return
	}
	switch body.Action {
	case "set":
		if body.MaxConcurrentJobs != nil {
			if err := s.d.jobq.SetMaxConcurrent(*body.MaxConcurrentJobs); err != nil {
				writeAPIErr(w, "DAEMON-400", err.Error())
				return
			}
		}
		if body.Mode != "" {
			if err := s.d.modes.Set(body.Mode); err != nil {
				writeSubmitErr(w, err)
				return
			}
		}
		writeJSONOK(w, map[string]any{"mode": s.d.modes.Current()})
	case "drain":
		if err := s.d.modes.Set(ModeDrain); err != nil {
			writeSubmitErr(w, err)
			return
		}
		writeJSONOK(w, map[string]any{"mode": s.d.modes.Current()})
	case "abort-drain":
		if err := s.d.modes.AbortDrain(); err != nil {
			writeSubmitErr(w, err)
			return
		}
		writeJSONOK(w, map[string]any{"mode": s.d.modes.Current()})
	case "get_recommendation":
		// Read-only: fetch candidates, create NOTHING (04 §6.1b step 1).
		rq, err := s.d.GetRecommendation(r.Context())
		if err != nil {
			writeSubmitErr(w, err)
			return
		}
		writeJSONOK(w, rq)
	default:
		writeAPIErr(w, "DAEMON-400", "unknown mode action")
	}
}

func (s *APIServer) hBatchSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WIIDs      []string          `json:"wi_ids"`
		Priorities map[string]string `json:"priorities"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIErr(w, "DAEMON-400", err.Error())
		return
	}
	if err := s.d.BatchSubmit(body.WIIDs, body.Priorities); err != nil {
		writeSubmitErr(w, err)
		return
	}
	writeJSONOK(w, map[string]any{"accepted": len(body.WIIDs)})
}

func (s *APIServer) hShutdown(w http.ResponseWriter, r *http.Request) {
	// Latch FIRST (04 §6.5 step 1), answer, then run the sequence.
	s.d.modes.SetShuttingDown()
	writeJSONOK(w, map[string]any{"shutting_down": true})
	go func() {
		_ = s.d.Shutdown(context.Background())
	}()
}

// ---------------------------------------------------------------------------
// proxy_call (04 §5.3): whitelist-only, SO_PEERCRED-bound, no passthrough
// default. The proxy signs with the DAEMON's own credentials and the WI's
// generation token; the WI Agent itself holds no AIHub key (D3 / 04 §8).

var proxyWhitelist = map[string]bool{
	"read_workflow_ref":  true,
	"report_step_result": true,
	"memory_read":        true,
	"memory_write":       true,
	"publish_request":    true,
}

func (s *APIServer) hProxyCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkItemID string         `json:"work_item_id"`
		Operation  string         `json:"operation"`
		Payload    map[string]any `json:"payload"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIErr(w, "DAEMON-400", err.Error())
		return
	}
	if !proxyWhitelist[body.Operation] {
		writeAPIErr(w, ErrCodeForbidden, "PROXY_OPERATION_NOT_ALLOWED: "+body.Operation)
		return
	}
	// Peer must be a WI Agent process this daemon derived (SO_PEERCRED).
	pid := peerPID(r.Context())
	s.d.mu.Lock()
	jr, ok := s.d.jobs[body.WorkItemID]
	s.d.mu.Unlock()
	if !ok || pid < 0 {
		writeAPIErr(w, ErrCodeForbidden, "proxy_call peer is not a derived WI Agent of this work item")
		return
	}
	if j := s.d.st.FindJob(body.WorkItemID); j == nil || j.MainAgentPID != pid {
		writeAPIErr(w, ErrCodeForbidden, "proxy_call pid does not match the job's main_agent_pid")
		return
	}
	switch body.Operation {
	case "read_workflow_ref":
		detail, err := s.d.client.GetWI(body.WorkItemID)
		if err != nil {
			writeSubmitErr(w, err)
			return
		}
		budget, _ := s.d.client.GetBudget(body.WorkItemID)
		writeJSONOK(w, map[string]any{"work_item": detail, "budget": budget})
	case "report_step_result":
		// End-generation passthrough: the proxy signs with the daemon-held
		// token; flush evidence must be present but is NOT validated here
		// (the Core's 02 §6.4 chain is the validator).
		reason, _ := body.Payload["reason"].(string)
		if reason == "" {
			writeAPIErr(w, "DAEMON-400", "report_step_result requires reason")
			return
		}
		feRaw, err := json.Marshal(body.Payload["flush_evidence"])
		var fe *aihub.FlushEvidence
		if err == nil && string(feRaw) != "null" {
			fe = &aihub.FlushEvidence{}
			if uerr := json.Unmarshal(feRaw, fe); uerr != nil {
				fe = nil
			}
		}
		waitingOn, _ := body.Payload["waiting_on"].(string)
		if serr := s.d.client.EndGeneration(body.WorkItemID, reason, waitingOn, jr.token, fe); serr != nil {
			// 04 §7.3: a rolled-back credential ends this path in scene
			// isolation — the agent's own close-out is as dead as its
			// token; recovery is an admin force-takeover, never a retry.
			if isSecretRolledBack(serr) {
				_ = s.d.quarantineRolledBack(body.WorkItemID)
				writeAPIErr(w, ErrCodeForbidden, ReasonSecretRolledBack+": capsule quarantined; recovery is admin force-takeover")
				return
			}
			writeSubmitErr(w, serr)
			return
		}
		writeJSONOK(w, map[string]any{"ok": true})
	case "memory_read":
		out, err := s.d.client.MemorySearch(body.Payload)
		if err != nil {
			writeSubmitErr(w, err)
			return
		}
		writeJSONOK(w, out)
	case "memory_write":
		out, err := s.d.client.MemoryCreate(body.Payload)
		if err != nil {
			writeSubmitErr(w, err)
			return
		}
		writeJSONOK(w, out)
	case "publish_request":
		reason, _ := body.Payload["reason"].(string)
		out, err := s.d.PublishRequest(r.Context(), body.WorkItemID, reason)
		if err != nil {
			writeSubmitErr(w, err)
			return
		}
		writeJSONOK(w, out)
	default:
		writeAPIErr(w, ErrCodeForbidden, "PROXY_OPERATION_NOT_ALLOWED")
	}
}
