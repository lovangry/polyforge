package observe

// memory.go: the F6-B lane — the UDS narrow memory channel, mounted on
// the same face and behind the same fence as flush
// (ruling C2's "you → daemon" transport; the F6 ruling adds the two
// memory verbs to it):
//
//	POST /jobs/{wi}/recall    body {attempt_id|spawn_n, query, top_k}
//	                                → {items:[...]}
//	POST /jobs/{wi}/remember  body {attempt_id|spawn_n, type, content,
//	                                visibility, title?, tags?}
//	                                → {id, status}
//
// The fence accepts EITHER proof of current-derivation identity: the
// v1.5 spawn_n counter (B2, preferred — the same dual-match as the
// flush fence) or the legacy attempt id (deleted in the cleanup
// commit). A request carrying NEITHER is refused.
//
// The agent holds NO aihub credential and no aihub URL — every server
// call is made BY THE DAEMON, whose machine credential never reaches an
// agent env. The routes are stateless proxies: after the fence they call
// the injected MemoryFace (the daemon binds it to the shared aihub
// client), so they touch no monitor state and need no monitor-goroutine
// serialization.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// MemoryFace is the narrow aihub memory proxy seam: everything the two
// UDS routes need and nothing more. The daemon binds it to its own
// aihub client; tests bind fakes. nil disables both routes (the typed
// not-configured refusal).
type MemoryFace interface {
	// Recall runs the semantic memory search for the work item's
	// project and answers the top-K hits (each carrying a bounded
	// content excerpt). wi is the CANONICAL work-item id of the route.
	Recall(wi, query string, topK int) ([]MemoryHit, error)
	// Remember stores one memory in the work item's project, attributed
	// to the work item, and answers the created memory id.
	Remember(wi string, req MemoryRemember) (MemoryStored, error)
}

// MemoryHit is one recall answer row.
type MemoryHit struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	// Excerpt is a bounded prefix of the memory's content; the full
	// authority document stays behind the aihub read face.
	Excerpt   string   `json:"excerpt,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
}

// MemoryRemember is the remember request body minus the fence fields —
// the fields the agent authors. The route fills WorkItemID itself (the
// fenced route id); the face fills project and ownership from it.
type MemoryRemember struct {
	Type       string   `json:"type"`
	Title      string   `json:"title,omitempty"`
	Content    string   `json:"content"`
	Visibility string   `json:"visibility"`
	Tags       []string `json:"tags,omitempty"`
}

// MemoryStored is the remember answer.
type MemoryStored struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// The UDS memory request bodies (the fence fields are route business,
// the rest is the agent's payload). AttemptID is the legacy fence
// proof; SpawnN is the v1.5 B2 proof — one of the two must name the
// current derivation.
type memoryRecallRequest struct {
	AttemptID string `json:"attempt_id"`
	SpawnN    int    `json:"spawn_n"`
	Query     string `json:"query"`
	TopK      int    `json:"top_k"`
}

type memoryRememberRequest struct {
	AttemptID  string   `json:"attempt_id"`
	SpawnN     int      `json:"spawn_n"`
	Type       string   `json:"type"`
	Title      string   `json:"title,omitempty"`
	Content    string   `json:"content"`
	Visibility string   `json:"visibility"`
	Tags       []string `json:"tags,omitempty"`
}

// recallTopK bounds the recall answer count: default 10, ceiling 50
// (the same bound as the F6-A snapshot).
const (
	memoryRecallDefaultTopK = 10
	memoryRecallMaxTopK     = 50
	// memoryRememberVisibility is the closed visibility set the narrow
	// write channel accepts (the aihub face's own set).
	memoryRememberMaxBody = 2 << 20
)

var memoryVisibilities = map[string]bool{
	"admin": true, "private": true, "project": true, "team": true, "public": true,
}

// serveRecall is the UDS recall route: fence, then one stateless search
// proxy call.
func (o *Observe) serveRecall(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("wi")
	m := o.lookup(wi)
	if m == nil {
		writeErr(w, api.ErrDaemonNotFound, "work item is not monitored on this daemon")
		return
	}
	var req memoryRecallRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, api.ErrDaemonBadInput, "recall request body: "+err.Error())
		return
	}
	if !o.memoryFence(w, m, wi, req.SpawnN, req.AttemptID, "recall") {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, api.ErrDaemonBadInput, "recall requires a non-empty query")
		return
	}
	if o.cfg.MemoryFace == nil {
		writeErr(w, api.ErrDaemonInternal, "memory face is not configured on this daemon")
		return
	}
	topK := req.TopK
	if topK <= 0 {
		topK = memoryRecallDefaultTopK
	}
	if topK > memoryRecallMaxTopK {
		topK = memoryRecallMaxTopK
	}
	// The face receives the CANONICAL id (F2): a slug spelling that
	// reached the monitor through lookup is an alias, never an authority
	// for attribution or server calls.
	hits, err := o.cfg.MemoryFace.Recall(m.canonicalWI(), req.Query, topK)
	if err != nil {
		writeMemoryFaceErr(w, err)
		return
	}
	if hits == nil {
		hits = []MemoryHit{}
	}
	_ = o.log.Append(wi, EvMemoryRecall, map[string]any{
		"top_k": topK, "hits": len(hits),
	})
	writeJSON(w, http.StatusOK, struct {
		Items []MemoryHit `json:"items"`
	}{Items: hits})
}

// serveRemember is the UDS remember route: fence, validate the closed
// field set, then one stateless write-proxy call. The memory is stored
// in the work item's project and attributed to the work item — the
// agent never names a project, and never could name another's.
func (o *Observe) serveRemember(w http.ResponseWriter, r *http.Request) {
	wi := r.PathValue("wi")
	m := o.lookup(wi)
	if m == nil {
		writeErr(w, api.ErrDaemonNotFound, "work item is not monitored on this daemon")
		return
	}
	var req memoryRememberRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, memoryRememberMaxBody)).Decode(&req); err != nil {
		writeErr(w, api.ErrDaemonBadInput, "remember request body: "+err.Error())
		return
	}
	if !o.memoryFence(w, m, wi, req.SpawnN, req.AttemptID, "remember") {
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeErr(w, api.ErrDaemonBadInput, "remember requires content")
		return
	}
	if req.Type == "" {
		writeErr(w, api.ErrDaemonBadInput, "remember requires a memory type")
		return
	}
	if req.Visibility == "" {
		req.Visibility = "project"
	}
	if !memoryVisibilities[req.Visibility] {
		writeErr(w, api.ErrDaemonBadInput, "remember visibility must be one of admin|private|project|team|public")
		return
	}
	if o.cfg.MemoryFace == nil {
		writeErr(w, api.ErrDaemonInternal, "memory face is not configured on this daemon")
		return
	}
	// The face receives the CANONICAL id (F2): a slug spelling that
	// reached the monitor through lookup is an alias, never an authority
	// for attribution or server calls.
	//
	// write_ms (M6's latency sample) is the wall-clock duration of the
	// WHOLE face call as seen from this append site — the Store.Create
	// commit round-trip (row + FTS5 trigram in one Core tx, INV-7a: the
	// write's return means retrievable), idempotent replays (the same
	// key's early-exit answer) timed just as honestly as fresh writes.
	start := time.Now()
	stored, err := o.cfg.MemoryFace.Remember(m.canonicalWI(), MemoryRemember{
		Type:       req.Type,
		Title:      req.Title,
		Content:    req.Content,
		Visibility: req.Visibility,
		Tags:       req.Tags,
	})
	writeMS := time.Since(start).Milliseconds()
	if err != nil {
		writeMemoryFaceErr(w, err)
		return
	}
	_ = o.log.Append(wi, EvMemoryRemember, map[string]any{
		"id": stored.ID, "type": req.Type, "visibility": req.Visibility,
		"write_ms": writeMS,
	})
	writeJSON(w, http.StatusOK, stored)
}

// memoryFence is the memory routes' share of the flush-mode fence (§二):
// the request must prove it speaks for the monitor's CURRENT
// derivation — either with the v1.5 spawn_n counter (B2, the same
// dual-match as serveFlush: current, or the hub's latest ANNOUNCED
// counter during the recovery-adoption window, with the same
// lagging-current cross-check) or with the legacy attempt id (the
// pre-v1.5 proof, deleted in the cleanup commit). A request carrying
// NEITHER proof answers DAEMON-409 and never reaches the face: a zombie
// survivor of a superseded derivation must never read or write the
// live one's memory. Answers false when the caller already wrote the
// refusal.
func (o *Observe) memoryFence(w http.ResponseWriter, m *Monitor, wi string, spawnN int, attemptID, verb string) bool {
	if spawnN > 0 {
		cur := m.currentSpawn()
		known := o.spawnKnown(wi)
		if (spawnN == cur && (known == 0 || spawnN == known)) || spawnN == known {
			return true
		}
		writeErr(w, api.ErrDaemonConflict,
			"STALE_SPAWN: "+verb+" carries spawn_n "+strconv.Itoa(spawnN)+
				"; the current derivation is "+strconv.Itoa(cur)+
				" (latest announced: "+strconv.Itoa(known)+")")
		return false
	}
	cur := m.currentAttempt()
	if attemptID == "" ||
		(attemptID != cur && attemptID != o.instanceKnown(wi)) {
		writeErr(w, api.ErrDaemonConflict,
			"STALE_SPAWN: "+verb+" carries no current-derivation proof "+
				"(spawn_n absent, attempt_id "+attemptID+"); the current instance is "+cur)
		return false
	}
	return true
}

// writeMemoryFaceErr surfaces a typed api error verbatim (MEM-*/CORE-*
// transparency — the aihub face's own refusal reasons are the agent's
// to read) and wraps everything else as DAEMON-500.
func writeMemoryFaceErr(w http.ResponseWriter, err error) {
	var ae *api.Error
	if errors.As(err, &ae) {
		writeJSON(w, ae.HTTPStatus(), ae)
		return
	}
	writeErr(w, api.ErrDaemonInternal, err.Error())
}
