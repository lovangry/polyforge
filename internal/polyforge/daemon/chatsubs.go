package daemon

// chatsubs.go: the daemon-held half of `pf watch` (v2-converge §3/§4).
//
//   - <data-dir>/chat-subs.json: per chat id, the subscribed WIs with their
//     event cursor and the state-derived decision keys already delivered.
//     Written atomically. A chat id is a notification label, not a
//     credential.
//   - routes under /chats/{id}/..., the per-WI GET /jobs/{wi}/watch snapshot
//     (execution.json authority + events after a cursor + job bookkeeping).
//
// Notification routing: nothing here pushes. A submit subscribes its chat;
// every watcher cycle subscribes its chat to every held WI the chat
// submitted or its owner owns (subscribe-owned). Decisions stay un-acked
// (cursor not advanced) until a watcher of a subscribed chat prints them.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/silgrid/polyforge/internal/common/atomicfile"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
	"github.com/silgrid/polyforge/internal/polyforge/watcher"
)

const chatSubsFile = "chat-subs.json"

var chatIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type subsDoc struct {
	Version int                 `json:"version"`
	Chats   map[string]*chatDoc `json:"chats"`
}

type chatDoc struct {
	Subs map[string]*protocol.ChatSub `json:"subs"`
	// Muted holds the WIs the chat explicitly unsubscribed from: the
	// owner routing (SubscribeNew) never re-adds them; an explicit
	// subscribe clears the mark.
	Muted map[string]bool `json:"muted,omitempty"`
}

// subStore persists subscriptions and cursors.
type subStore struct {
	mu   sync.Mutex
	path string
	doc  subsDoc
}

// newSubStore loads (or starts) the store at path.
func newSubStore(path string) (*subStore, error) {
	s := &subStore{path: path, doc: subsDoc{Version: 1, Chats: map[string]*chatDoc{}}}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.doc); err != nil {
			return nil, fmt.Errorf("%s unparsable: %w", chatSubsFile, err)
		}
		if s.doc.Chats == nil {
			s.doc.Chats = map[string]*chatDoc{}
		}
	case !os.IsNotExist(err):
		return nil, err
	}
	return s, nil
}

func (s *subStore) persistLocked() error {
	blob, err := json.MarshalIndent(&s.doc, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, blob, 0o600)
}

func (s *subStore) chatLocked(id string, create bool) *chatDoc {
	c := s.doc.Chats[id]
	if c == nil && create {
		c = &chatDoc{Subs: map[string]*protocol.ChatSub{}}
		s.doc.Chats[id] = c
	}
	if c != nil && c.Subs == nil {
		c.Subs = map[string]*protocol.ChatSub{}
	}
	return c
}

// Subscribe adds (or re-arms) a subscription; cursor is the event seq the
// watcher should start after for a NEW subscription (history is not
// replayed), slug the WI's display name ("" = unknown). Re-subscribing a
// finished WI re-arms it. Reports whether anything changed.
func (s *subStore) Subscribe(chat, wi, slug string, cursor int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chatLocked(chat, true)
	unmuted := c.Muted[wi]
	delete(c.Muted, wi)
	if sub, ok := c.Subs[wi]; ok {
		fill := sub.Slug == "" && slug != ""
		if fill {
			sub.Slug = slug // a subscription recorded before its slug was known
		}
		if !sub.Done {
			if fill || unmuted {
				return false, s.persistLocked()
			}
			return false, nil
		}
		sub.Done, sub.Notified = false, nil
		return true, s.persistLocked()
	}
	c.Subs[wi] = &protocol.ChatSub{WI: wi, Slug: slug, Cursor: cursor}
	return true, s.persistLocked()
}

// SubscribeNew adds a subscription only when the chat has none for wi
// and has not muted it (a finished one is left finished). Reports
// whether it added one.
func (s *subStore) SubscribeNew(chat, wi, slug string, cursor int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chatLocked(chat, true)
	if c.Subs[wi] != nil || c.Muted[wi] {
		return false, nil
	}
	c.Subs[wi] = &protocol.ChatSub{WI: wi, Slug: slug, Cursor: cursor}
	return true, s.persistLocked()
}

// Settled reports whether SubscribeNew would leave wi alone for the
// chat: it holds a subscription (live or finished) or muted it.
func (s *subStore) Settled(chat, wi string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chatLocked(chat, false)
	return c != nil && (c.Subs[wi] != nil || c.Muted[wi])
}

// Unsubscribe drops the chat's subscription to wi and mutes wi, so the
// owner routing does not re-add it. Reports whether a subscription was
// dropped.
func (s *subStore) Unsubscribe(chat, wi string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chatLocked(chat, true)
	had := c.Subs[wi] != nil
	if !had && c.Muted[wi] {
		return false, nil
	}
	delete(c.Subs, wi)
	if c.Muted == nil {
		c.Muted = map[string]bool{}
	}
	c.Muted[wi] = true
	return had, s.persistLocked()
}

// List answers the chat's subscriptions, sorted by WI.
func (s *subStore) List(chat string) []protocol.ChatSub {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []protocol.ChatSub{}
	if c := s.chatLocked(chat, false); c != nil {
		for _, sub := range c.Subs {
			out = append(out, *sub)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WI < out[j].WI })
	return out
}

// Ack records a watcher's progress; unknown WIs are ignored (the
// subscription may have been removed meanwhile).
func (s *subStore) Ack(chat string, a protocol.ChatAck) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chatLocked(chat, false)
	if c == nil || c.Subs[a.WI] == nil {
		return nil
	}
	sub := c.Subs[a.WI]
	sub.Cursor, sub.Notified, sub.Done = a.Cursor, a.Notified, a.Done
	return s.persistLocked()
}

// ---------------------------------------------------------------------------
// Routes.
// ---------------------------------------------------------------------------

// chatID validates the path chat id (it names files on the watcher side).
func chatID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !chatIDRe.MatchString(id) {
		writeErr(w, api.ErrDaemonBadInput, "chat id must be 1-64 characters of [A-Za-z0-9_-]")
		return "", false
	}
	return id, true
}

func (d *Daemon) subscriptionsRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, protocol.ChatSubscriptions{ChatID: id, Subscriptions: d.subs.List(id)})
}

func (d *Daemon) subscribeRoute(subscribe bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := chatID(w, r)
		if !ok {
			return
		}
		var body struct {
			WI string `json:"wi"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.WI) == "" {
			writeErr(w, api.ErrDaemonBadInput, "wi is required")
			return
		}
		wi := d.canonWI(strings.TrimSpace(body.WI))
		var (
			changed bool
			err     error
		)
		slug := d.slugOf(wi)
		if subscribe {
			changed, err = d.subs.Subscribe(id, wi, slug, d.lastSeq(wi))
		} else {
			changed, err = d.subs.Unsubscribe(id, wi)
		}
		if err != nil {
			writeErr(w, api.ErrDaemonInternal, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, protocol.ChatSubscribeAnswer{ChatID: id, WI: wi, Slug: slug, Changed: changed})
	}
}

func (d *Daemon) ackRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	var a protocol.ChatAck
	if !decodeBody(w, r, &a) {
		return
	}
	if a.WI == "" || a.Cursor < 0 {
		writeErr(w, api.ErrDaemonBadInput, "ack needs wi and a non-negative cursor")
		return
	}
	if err := d.subs.Ack(id, a); err != nil {
		writeErr(w, api.ErrDaemonInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// subscribeOwnedRoute is POST /chats/{id}/subscribe-owned {"owner"}:
// every watcher cycle subscribes its chat to every held work item the
// chat submitted or the owner owns (origin owner, else the WI reporter)
// that the chat has no subscription for and has not unsubscribed from,
// and gets the added ones back. A
// new subscription's cursor sits just before the WI's newest decision
// event, so a decision nobody has been told about yet is replayed; the
// state-backed decisions come from the snapshot anyway. A WI the chat is
// already subscribed to (or muted) costs one map lookup, and a reporter is fetched
// from the hub once per WI, so the per-cycle call stays cheap.
func (d *Daemon) subscribeOwnedRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner string `json:"owner"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	owner := strings.TrimSpace(body.Owner)
	added := []protocol.ChatSub{}
	jobs, _ := d.statusJobs()
	for _, j := range jobs {
		if d.subs.Settled(id, j.WorkItemID) {
			continue
		}
		mine := j.Origin != nil && j.Origin.ChatID == id
		if !mine && !(owner != "" && d.ownerOf(j) == owner) {
			continue
		}
		cursor := d.lastSeq(j.WorkItemID)
		if e, ok := d.obs.Events().Newest(j.WorkItemID, watcher.IsEventDecision); ok {
			cursor = e.Seq - 1
		}
		fresh, err := d.subs.SubscribeNew(id, j.WorkItemID, j.Slug, cursor)
		if err != nil {
			writeErr(w, api.ErrDaemonInternal, err.Error())
			return
		}
		if fresh {
			added = append(added, protocol.ChatSub{WI: j.WorkItemID, Slug: j.Slug, Cursor: cursor})
		}
	}
	writeJSON(w, http.StatusOK, protocol.ChatSubscriptions{ChatID: id, Subscriptions: added})
}

// ownerOf answers a WI's owner: the origin owner, else the WI reporter
// (a WI's reporter never changes: the first successful hub read is kept).
func (d *Daemon) ownerOf(j statusJob) string {
	if j.Origin != nil && j.Origin.Owner != "" {
		return j.Origin.Owner
	}
	if r, ok := d.reporters.Load(j.WorkItemID); ok {
		return r.(string)
	}
	if d.hub != nil {
		if item, err := d.hub.GetWorkItem(j.WorkItemID); err == nil && item != nil {
			d.reporters.Store(j.WorkItemID, item.Reporter)
			return item.Reporter
		}
	}
	return ""
}

// autoSubscribe subscribes the submitting chat (when the request carries
// one) to the WI, starting after cursor.
func (d *Daemon) autoSubscribe(o *reqOrigin, wi string, cursor int64) {
	if d.subs == nil || o == nil || !chatIDRe.MatchString(o.ChatID) {
		return
	}
	if _, err := d.subs.Subscribe(o.ChatID, wi, d.slugOf(wi), cursor); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: auto-subscribe %s to %s: %v\n", o.ChatID, wi, err)
	}
}

// slugOf answers a WI's display name: the local job record's slug, else
// the aihub record's; "" when neither knows it (callers show the id).
func (d *Daemon) slugOf(wi string) string {
	if ji, ok := d.rt.Info(wi); ok && ji.Slug != "" {
		return ji.Slug
	}
	if d.hub != nil {
		if item, err := d.hub.GetWorkItem(wi); err == nil && item != nil {
			return item.Slug
		}
	}
	return ""
}

// lastSeq is the highest retained event seq of a WI (0 = none).
func (d *Daemon) lastSeq(wi string) int64 {
	if d.obs == nil {
		return 0
	}
	return d.obs.Events().LastSeq(wi)
}

// ---------------------------------------------------------------------------
// GET /jobs/{wi}/watch?after=<seq>
// ---------------------------------------------------------------------------

func (d *Daemon) watchSnapshotRoute(w http.ResponseWriter, r *http.Request) {
	var after int64
	if s := r.URL.Query().Get("after"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, api.ErrDaemonBadInput, "after must be a non-negative integer cursor")
			return
		}
		after = n
	}
	wi := r.PathValue("wi")
	canonical, item := d.resolveWI(wi)
	// the resolution already read aihub (record or failure): the snapshot
	// does not read it again
	hubRead := item != nil || canonical == ""
	if canonical == "" {
		canonical = wi
	}
	writeJSON(w, http.StatusOK, d.watchSnapshot(canonical, item, hubRead, after))
}

// watchSnapshot answers one work item's snapshot by its canonical id.
// item is its aihub record when the caller already read it; hubRead
// says the caller already asked aihub (so a nil item is a failed read,
// not one still to make).
func (d *Daemon) watchSnapshot(wi string, item *api.WorkItem, hubRead bool, after int64) protocol.WatchSnapshot {
	snap := protocol.WatchSnapshot{WorkItem: wi, Events: []protocol.EventEntry{}}
	snap.Queued = d.q != nil && d.q.IsPending(wi)
	jobs, _ := d.statusJobs()
	localLive, held := false, false
	for _, j := range jobs {
		if j.WorkItemID != wi {
			continue
		}
		snap.Known, held = true, true
		snap.Slug = j.Slug
		localLive = j.Status != string(runtime.StatePaused)
		wj := &protocol.WatchJob{Status: j.Status,
			QuestionID: j.QuestionID, LastProgressAgoSec: j.LastWorkAgoSec}
		if j.Origin != nil {
			wj.Origin = &protocol.WatchOrigin{Kind: j.Origin.Kind, ChatID: j.Origin.ChatID, Owner: j.Origin.Owner}
		}
		snap.Job = wj
	}
	// The hub record (read at most once per snapshot, by the resolution
	// or here) serves only when no live local job answers: it names a
	// closed WI's capsule (project + slug), its display name,
	// and — a manual close (wrap / wrap --fail / cancel / pause) leaves
	// execution.json at `running` — its status, the authority for how
	// the WI ended.
	if localLive || (snap.Queued && held) {
		item = nil
	} else if !hubRead && d.hub != nil {
		if it, err := d.hub.GetWorkItem(wi); err == nil && it != nil {
			item = it
		}
	}
	if item != nil {
		if snap.Slug == "" {
			snap.Slug = item.Slug
		}
		if !snap.Queued {
			snap.HubStatus = item.Status
		}
	}
	if exec := d.readExecution(wi, item); exec != nil {
		snap.Known = true
		snap.Execution = exec
	}
	if d.obs != nil {
		// live progress of the in-flight dispatch (current command, recent
		// files, tool calls); absent between steps / when nothing is monitored
		if lp, ok := d.obs.Progress(wi); ok && lp.InDispatch {
			snap.Progress = watchProgress(lp)
		}
		snap.LastSeq = d.lastSeq(wi)
		snap.Events = append(snap.Events, d.obs.Events().SinceAfter(wi, after)...)
	}
	return snap
}

// watchProgress projects the observe lane's LiveProgress onto the wire
// block; nil when the dispatch has nothing to show yet.
func watchProgress(lp observe.LiveProgress) *protocol.WatchProgress {
	p := &protocol.WatchProgress{Command: lp.Command, RecentFiles: lp.RecentFiles}
	if lp.Command != "" {
		age := lp.CommandAgeSec
		p.CommandAgeSec = &age
	}
	if lp.ToolCalls > 0 {
		n := int64(lp.ToolCalls)
		p.ToolCalls = &n
	}
	if p.Command == "" && len(p.RecentFiles) == 0 && p.ToolCalls == nil {
		return nil
	}
	return p
}

// loadExecution reads the WI's execution.json (the authority), from the
// job record's capsule or, for a WI the daemon no longer holds, from the
// capsule its hub record names: capsules/<project>-<seq> under the
// project's mapped workspace (item nil = no record to fall back on). It
// is the one execution.json reader of the daemon (the watch projection
// and the no-id answer both use it).
func (d *Daemon) loadExecution(wi string, item *api.WorkItem) *capsule.Execution {
	var candidates []string
	if d.rt != nil {
		if ji, ok := d.rt.Info(wi); ok && ji.CapsuleDir != "" {
			candidates = append(candidates, filepath.Join(ji.CapsuleDir, capsule.ExecutionJSON))
		}
	}
	if len(candidates) == 0 && item != nil && d.cfg.Mapping != nil {
		if ws, err := d.cfg.Mapping.WorkspaceFor(item.ProjectName); err == nil {
			candidates = append(candidates, filepath.Join(ws, "capsules", api.PathName(item.Slug, wi), capsule.ExecutionJSON))
		}
	}
	for _, p := range candidates {
		if ex := readExecutionFile(p); ex != nil {
			return ex
		}
	}
	return nil
}

// readExecutionFile parses one execution.json; nil when absent or torn.
func readExecutionFile(path string) *capsule.Execution {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ex capsule.Execution
	if json.Unmarshal(raw, &ex) != nil {
		return nil
	}
	return &ex
}

// pendingQuestions answers, per running job with a live driver, the id
// of the question its execution.json (the authority) holds at the
// current step — the status face presents such a job as needs_input
// (D6), and an answer to it is accepted. Before the driver is
// (re-)derived nothing could take the answer, so nothing is shown. A
// pending without an id is unanswerable and ignored (the monitor's
// pending_invalid names it).
func (d *Daemon) pendingQuestions() map[string]string {
	out := map[string]string{}
	for _, ji := range d.rt.List() {
		if ji.State != runtime.StateRunning || ji.Agent == nil || ji.CapsuleDir == "" {
			continue
		}
		ex := readExecutionFile(filepath.Join(ji.CapsuleDir, capsule.ExecutionJSON))
		if ex == nil {
			continue
		}
		if st := ex.Steps[ex.CurrentStep]; st != nil && st.PendingQuestion != nil && st.PendingQuestion.ID != "" {
			out[ji.WorkItem] = st.PendingQuestion.ID
		}
	}
	return out
}

// readExecution projects loadExecution onto the watch wire shape.
func (d *Daemon) readExecution(wi string, item *api.WorkItem) *protocol.WatchExecution {
	ex := d.loadExecution(wi, item)
	if ex == nil {
		return nil
	}
	out := &protocol.WatchExecution{
		WorkflowStatus: ex.WorkflowStatus, CurrentStep: ex.CurrentStep, UpdatedAt: ex.UpdatedAt,
		Steps: map[string]protocol.WatchStep{},
	}
	ids := make([]string, 0, len(ex.Steps))
	for id := range ex.Steps {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic pick when several steps hold a question
	for _, id := range ids {
		st := ex.Steps[id]
		if st == nil {
			continue
		}
		out.Steps[id] = protocol.WatchStep{Status: st.Status, StartedAt: st.StepStartedAt}
		if st.PendingQuestion != nil && st.PendingQuestion.ID != "" && out.PendingQuestion == nil {
			out.PendingQuestion = &protocol.WatchPendingQuestion{ID: st.PendingQuestion.ID,
				Step: st.PendingQuestion.Step, Question: st.PendingQuestion.Question, AskedAt: st.PendingQuestion.AskedAt}
		}
	}
	return out
}
