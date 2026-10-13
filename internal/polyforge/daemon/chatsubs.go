package daemon

// chatsubs.go: the daemon-held half of `pf watch` (v2-converge §3/§4).
//
//   - <data-dir>/chat-subs.json: per chat id, the subscribed WIs with their
//     event cursor and the state-derived decision keys already delivered,
//     plus which attention notices the chat was already shown. Written
//     atomically; lives in the daemon data dir, never in an agent-writable
//     place. A chat id is a notification label, not a credential.
//   - watcher liveness: in-memory heartbeats (a watcher posts one per poll);
//     a chat is "live" while its last heartbeat is recent. A daemon restart
//     forgets them and the next poll restores them.
//   - routes under /chats/{id}/..., the per-WI GET /jobs/{wi}/watch snapshot
//     (execution.json authority + events after a cursor + job bookkeeping).
//
// Notification routing: nothing here pushes. Decisions for a WI nobody
// watches stay un-acked (cursor not advanced) and surface through the
// attention route the next time any watcher for the origin chat, or for the
// WI's owner, starts.

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
	"time"

	"github.com/silgrid/polyforge/internal/common/atomicfile"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

const (
	chatSubsFile   = "chat-subs.json"
	chatStatusDir  = "chat-status"
	liveWatcherTTL = 20 * time.Second
)

var chatIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type subsDoc struct {
	Version int                 `json:"version"`
	Chats   map[string]*chatDoc `json:"chats"`
}

type chatDoc struct {
	Subs map[string]*protocol.ChatSub `json:"subs"`
	// AttentionShown lists "<wi>|<reason>" notices this chat was shown.
	AttentionShown []string `json:"attention_shown,omitempty"`
}

// subStore persists subscriptions and cursors.
type subStore struct {
	mu   sync.Mutex
	path string
	doc  subsDoc
	live map[string]time.Time // chat id -> last watcher heartbeat
	now  func() time.Time
}

// newSubStore loads (or starts) the store at path.
func newSubStore(path string) (*subStore, error) {
	s := &subStore{path: path, live: map[string]time.Time{}, now: time.Now,
		doc: subsDoc{Version: 1, Chats: map[string]*chatDoc{}}}
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
// replayed). Re-subscribing a finished WI re-arms it. Reports whether
// anything changed.
func (s *subStore) Subscribe(chat, wi string, cursor int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chatLocked(chat, true)
	if sub, ok := c.Subs[wi]; ok {
		if !sub.Done {
			return false, nil
		}
		sub.Done, sub.Notified = false, nil
		return true, s.persistLocked()
	}
	c.Subs[wi] = &protocol.ChatSub{WI: wi, Cursor: cursor, SubscribedAt: s.now().UTC().Format(time.RFC3339)}
	return true, s.persistLocked()
}

func (s *subStore) Unsubscribe(chat, wi string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chatLocked(chat, false)
	if c == nil || c.Subs[wi] == nil {
		return false, nil
	}
	delete(c.Subs, wi)
	return true, s.persistLocked()
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

// Heartbeat marks the chat's watcher live.
func (s *subStore) Heartbeat(chat string) {
	s.mu.Lock()
	s.live[chat] = s.now()
	s.mu.Unlock()
}

func (s *subStore) liveLocked(chat string) bool {
	t, ok := s.live[chat]
	return ok && s.now().Sub(t) <= liveWatcherTTL
}

// CoveredBy reports whether any chat subscribed to wi has a live watcher.
func (s *subStore) Covered(wi string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.doc.Chats {
		if sub := c.Subs[wi]; sub != nil && !sub.Done && s.liveLocked(id) {
			return true
		}
	}
	return false
}

// MarkShown records an attention notice as shown to chat; it answers true
// when the notice was new for that chat.
func (s *subStore) MarkShown(chat, key string, record bool) (fresh bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chatLocked(chat, record)
	if c != nil {
		for _, k := range c.AttentionShown {
			if k == key {
				return false, nil
			}
		}
	}
	if !record {
		return true, nil
	}
	c.AttentionShown = append(c.AttentionShown, key)
	if len(c.AttentionShown) > 200 {
		c.AttentionShown = c.AttentionShown[len(c.AttentionShown)-200:]
	}
	return true, s.persistLocked()
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

func (d *Daemon) statusPathFor(chat string) string {
	return filepath.Join(d.cfg.DataDir, chatStatusDir, chat+".status")
}

func (d *Daemon) subscriptionsRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, protocol.ChatSubscriptions{
		ChatID: id, StatusPath: d.statusPathFor(id), Subscriptions: d.subs.List(id),
	})
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
		if subscribe {
			changed, err = d.subs.Subscribe(id, wi, d.lastSeq(wi))
		} else {
			changed, err = d.subs.Unsubscribe(id, wi)
		}
		if err != nil {
			writeErr(w, api.ErrDaemonInternal, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, protocol.ChatSubscribeAnswer{ChatID: id, WI: wi, Changed: changed})
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

func (d *Daemon) heartbeatRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	d.subs.Heartbeat(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (d *Daemon) attentionRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	owner := strings.TrimSpace(r.URL.Query().Get("owner"))
	items, err := d.attention(id, owner, r.URL.Query().Get("mark") == "1")
	if err != nil {
		writeErr(w, api.ErrDaemonInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.ChatAttention{ChatID: id, Items: items})
}

// attention lists the WIs that need a human, have no live subscribed
// watcher, and belong to this chat (origin) or this owner (origin owner,
// falling back to the WI reporter), skipping what the chat was already
// shown. mark records the listed notices as shown.
func (d *Daemon) attention(chat, owner string, mark bool) ([]protocol.ChatAttentionItem, error) {
	items := []protocol.ChatAttentionItem{}
	jobs, _ := d.statusJobs()
	for _, j := range jobs {
		reason := ""
		switch {
		case j.NeedsInput:
			reason = "needs_input"
		default:
			continue
		}
		if d.subs.Covered(j.WorkItemID) {
			continue
		}
		mine := j.Origin != nil && j.Origin.ChatID == chat
		if !mine && !(owner != "" && d.ownerOf(j) == owner) {
			continue
		}
		key := j.WorkItemID + "|" + reason + "|" + j.QuestionID
		fresh, err := d.subs.MarkShown(chat, key, mark)
		if err != nil {
			return nil, err
		}
		if fresh {
			items = append(items, protocol.ChatAttentionItem{WI: j.WorkItemID, Reason: reason})
		}
	}
	sort.Slice(items, func(i, k int) bool { return items[i].WI < items[k].WI })
	return items, nil
}

// ownerOf answers a WI's owner: the origin owner, else the WI reporter.
func (d *Daemon) ownerOf(j statusJob) string {
	if j.Origin != nil && j.Origin.Owner != "" {
		return j.Origin.Owner
	}
	if d.hub != nil {
		if item, err := d.hub.GetWorkItem(j.WorkItemID); err == nil && item != nil {
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
	if _, err := d.subs.Subscribe(o.ChatID, wi, cursor); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: auto-subscribe %s to %s: %v\n", o.ChatID, wi, err)
	}
}

// lastSeq is the highest retained event seq of a WI (0 = none).
func (d *Daemon) lastSeq(wi string) int64 {
	if d.obs == nil {
		return 0
	}
	ring := d.obs.Events().SinceAfter(wi, 0)
	if len(ring) == 0 {
		return 0
	}
	return ring[len(ring)-1].Seq
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
	writeJSON(w, http.StatusOK, d.watchSnapshot(d.canonWI(r.PathValue("wi")), after))
}

func (d *Daemon) watchSnapshot(wi string, after int64) protocol.WatchSnapshot {
	snap := protocol.WatchSnapshot{WorkItem: wi, Events: []protocol.EventEntry{}}
	snap.Queued = d.q != nil && d.q.IsPending(wi)
	jobs, _ := d.statusJobs()
	localLive := false
	for _, j := range jobs {
		if j.WorkItemID != wi {
			continue
		}
		snap.Known = true
		localLive = j.Status != string(runtime.StatePaused)
		wj := &protocol.WatchJob{Status: j.Status,
			QuestionID: j.QuestionID, LastProgressAgoSec: j.LastWorkAgoSec}
		if j.Origin != nil {
			wj.Origin = &protocol.WatchOrigin{Kind: j.Origin.Kind, ChatID: j.Origin.ChatID, Owner: j.Origin.Owner}
		}
		snap.Job = wj
	}
	if exec := d.readExecution(wi); exec != nil {
		snap.Known = true
		snap.Execution = exec
	}
	if !localLive && !snap.Queued && d.hub != nil {
		// No live local job: a manual close (wrap / wrap --fail / cancel /
		// pause) leaves execution.json at `running`, so the hub's status
		// is the authority for how the WI ended.
		if item, err := d.hub.GetWorkItem(wi); err == nil && item != nil {
			snap.HubStatus = item.Status
		}
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

// readExecution reads the WI's execution.json (the authority), from the
// live job's capsule or, for a WI the daemon no longer holds, from the
// mapped workspaces' capsules/<wi>.
func (d *Daemon) readExecution(wi string) *protocol.WatchExecution {
	var candidates []string
	if d.rt != nil {
		if ji, ok := d.rt.Info(wi); ok && ji.CapsuleDir != "" {
			candidates = append(candidates, filepath.Join(ji.CapsuleDir, capsule.ExecutionJSON))
		}
	}
	if d.cfg.Mapping != nil {
		for _, ws := range d.cfg.Mapping.Workspaces() {
			candidates = append(candidates, filepath.Join(ws, "capsules", wi, capsule.ExecutionJSON))
		}
	}
	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var ex capsule.Execution
		if json.Unmarshal(raw, &ex) != nil {
			continue
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
	return nil
}
