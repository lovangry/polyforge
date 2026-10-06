package observe

// stream.go: classification of the agent stdout record stream — the
// observe lane's half of the stream-heartbeat criterion (the per-WI
// monitor consumes the records Runtime's stdout reader feeds it and
// derives the observations the hang/idle watchdogs need):
//
//  1. lastProgressAt — when the stream last proved AGENT PROGRESS;
//  2. lastNoiseAt    — when it last carried a NON-progress record
//     (extension UI chatter and every unclassified line);
//  3. turnDepth      — whether a turn is open right now;
//  4. incidents      — provider errors riding the ordinary records,
//     surfaced immediately instead of being counted as bulk.
//
// PROGRESS vs NOISE (the F15 ruling): a record proves agent progress
// only when it belongs to the agent's own turn machinery — the message
// family (message_start / message_update / message_end), the
// toolResult family, the lifecycle records (agent_start / agent_end /
// agent_settled, turn_start / turn_end) and the `response` echo. Every
// other record is NOISE: the extension_ui_request stream a background-
// task extension emits continuously (setStatus/setWidget UI updates),
// plus any unclassified or unparseable line. The split exists because
// of the pilot's double-blind wedge: a turn stuck on an in-flight
// model call while the extension UI kept streaming records made the
// old "any record refreshes the heartbeat" rule keep the stream
// forever fresh — the hang criterion (silence WITH a turn open) never
// armed, and the idle criterion (no record, no turn) never armed
// either because the turn was open. UI chatter is not life; only
// progress is.
//
// PINNED REAL RECORD SHAPES (frozen v0 reference capture; the v0 test double emits the same twins): one prompt turn emits, in order
//
//	{"id":"pf-1","type":"response","command":"prompt","success":true}
//	{"type":"agent_start"}
//	{"type":"turn_start"}
//	{"type":"message_start","message":{"role":"system",...}}
//	{"type":"message_end","message":{"role":"system",...}}
//	{"type":"message_start","message":{"role":"user",...}}
//	{"type":"message_end","message":{"role":"user",...}}
//	{"type":"message_start","message":{"role":"assistant","stopReason":"pending",...}}
//	{"type":"message_update","assistantMessageEvent":{"type":"text_start",...}}
//	{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}
//	{"type":"message_update","assistantMessageEvent":{"type":"text_end","content":"ok"}}
//	{"type":"message_end","message":{"role":"assistant","stopReason":"stop",...}}
//	{"type":"turn_end","message":{...},"toolResults":[]}
//	{"type":"agent_end","messages":[...]}
//	{"type":"agent_settled"}
//
// CRITICAL ACCURACY FINDING (frozen from the v0 capture): the harness
// emits NO `type:"error"` record. A provider failure rides the ORDINARY
// records as
//
//	message.stopReason == "error"  +  message.errorMessage
//
// on the assistant message_start, on message_end AND on turn_end.
// Classifying on an assumed "error" record type would match exactly
// nothing, and the provider-error detection would be silently dead in
// production.

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Record type names that carry LIFECYCLE meaning for the heartbeat.
const (
	recAgentStart   = "agent_start"
	recAgentEnd     = "agent_end"
	recAgentSettled = "agent_settled"
	recTurnStart    = "turn_start"
	recTurnEnd      = "turn_end"
	recResponse     = "response"
	recMessageStart = "message_start"
	recMessageEnd   = "message_end"
)

// recMessageUpdate is the streaming delta record of the message family;
// recToolResult is the tool-result family the harness carries alongside
// turn_end's inline toolResults (classified as progress so a tool-heavy
// turn keeps proving life between message bursts).
const (
	recMessageUpdate = "message_update"
	recToolResult    = "toolResult"
)

// recExtUI is the extension-UI chatter record the pilot probe measured
// flowing continuously out of a background-task extension (253 records
// in one stuck window, the large majority of them this type). It is the
// canonical NOISE shape: it proves the extension event loop is alive,
// never that the agent's turn is moving.
const recExtUI = "extension_ui_request"

// isProgressType answers whether one record type proves agent progress
// (the F15 closed whitelist; everything else — including the empty type
// of an unparseable line and the recExtUI chatter — is noise).
func isProgressType(typ string) bool {
	switch typ {
	case recResponse, recAgentStart, recAgentEnd, recAgentSettled,
		recTurnStart, recTurnEnd, recMessageStart, recMessageEnd,
		recMessageUpdate, recToolResult:
		return true
	}
	return false
}

// maxStreamIncidents bounds the queued incidents: a provider failing
// every retry could queue one entry per record. The queue keeps the
// OLDEST entries and counts the drops.
const maxStreamIncidents = 64

// StreamHeartbeat is one point-in-time reading of the supervised agent's
// record stream.
type StreamHeartbeat struct {
	// ToolDispatchAt is when the stream last saw a message_end with the
	// turn still open — the signature of a tool call dispatched and
	// executing. Zero means no tool is known to be executing. Tool
	// execution (large edits, long builds) legitimately exceeds the
	// HangWindow; the hang lane must treat it as live work, not silence.
	ToolDispatchAt time.Time
	// Observable reports whether any record — progress OR noise — has
	// been seen at all (the stream itself is being received).
	Observable bool
	// LastRecordAt is when the reader last classified ANY record,
	// progress or noise (diagnostics; never a detection anchor).
	LastRecordAt time.Time
	// LastProgressAt is when the reader last classified a record that
	// represents AGENT progress (the F15 whitelist above). This is the
	// ONLY anchor the hang and idle watchdogs judge: UI noise never
	// refreshes it. The `response` echo counts as progress — the F15
	// ruling — because a response echo can only follow a command the
	// harness accepted, and the watchdogs' probe decision rides the
	// probe RESULT (ack vs timeout), never the window age: a daemon-side
	// echo refreshing the window can no longer mask a wedged agent the
	// way the old exclusion feared, and a probe ack's own assistant
	// message burst refreshes progress anyway.
	LastProgressAt time.Time
	// LastNoiseAt is when the reader last classified a NON-progress
	// record: extension UI chatter or any unclassified line. Never a
	// detection anchor — carried for the owner face and diagnostics.
	LastNoiseAt time.Time
	// InFlight reports whether a TURN is open: the stream carried
	// turn_start (or agent_start) and no matching turn_end / agent_end /
	// agent_settled yet. Silence with NO turn in flight is an idle agent
	// (waiting for work or for a human answer) — never a hang, which is
	// the needs_input exemption falling out of the criterion instead of
	// being special-cased.
	InFlight bool
	// Records is the total progress-record count (diagnostics).
	Records int64
	// Noise is the total non-progress record count (diagnostics).
	Noise int64
}

// Silent reports whether the stream has been PROGRESS-silent for at
// least window WHILE A TURN IS IN FLIGHT — the ruled hang criterion.
// Progress-silent means lastProgressAt (UI noise never counts as life);
// anchor is the probe-cleared window restart (zero = none). An
// unobservable or idle stream is never silent in this sense.
func (h StreamHeartbeat) Silent(now time.Time, window time.Duration, anchor time.Time) bool {
	if !h.Observable || !h.InFlight || h.LastProgressAt.IsZero() {
		return false
	}
	eff := h.LastProgressAt
	if anchor.After(eff) {
		eff = anchor
	}
	return now.Sub(eff) >= window
}

// StreamIncident is one classified infra event from the record stream.
type StreamIncident struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"` // provider_error | retry
	Detail string    `json:"detail"`
	// Fatal reports whether the turn ENDED on this error (turn_end with
	// stopReason=error). A non-fatal incident is a mid-turn report the
	// harness may still recover from.
	Fatal bool `json:"fatal"`
}

// streamState is the per-handle record-stream bookkeeping. Its own mutex
// because the monitor goroutine updates it on every record while flush
// and view sub-goroutines only read snapshots.
type streamState struct {
	mu sync.Mutex
	// lastProgressAt is the heartbeat anchor: the newest record that
	// proved AGENT progress (the F15 whitelist; UI noise excluded).
	lastProgressAt time.Time
	// lastNoiseAt is the newest NON-progress record (extension UI
	// chatter / unclassified lines) — never a detection input.
	lastNoiseAt time.Time
	// turnDepth > 0 means a turn is open. A counter rather than a bool
	// because agent_start and turn_start both open; it saturates at 0 so
	// a stray close can never wedge the flag negative.
	turnDepth int
	// toolDispatchAt is when the stream last saw message_end with the
	// turn still open — the signature of a tool call dispatched and
	// executing. Cleared when a toolResult arrives (the tool returned)
	// or when the turn ends. The hang lane reads it to exempt live tool
	// execution from the silence window (#83: large file edits routinely
	// exceed HangWindow while the agent is actively working).
	toolDispatchAt time.Time
	records        int64 // progress-record count
	noise          int64 // non-progress record count
	incidents      []StreamIncident
	dropped        int64
	// lastIncidentKey suppresses the triple-report of ONE provider
	// failure: the stream carries the same errorMessage on
	// message_start, message_end and turn_end of the failed turn. The
	// key clears when a new turn opens, so a REPEATED failure on a
	// later turn is reported again.
	lastIncidentKey string
	// cleanTurns counts turn_end records that did NOT end on an error.
	cleanTurns int64
	// chain is the CONSECUTIVE-incident counter that feeds the storm
	// guard: every classified provider/retry incident increments it AT
	// NOTE TIME (stream order — the temporal truth), and a clean
	// turn_end resets it to zero. The monitor additionally resets it on
	// a step change and on recovery. Counting at note time is what makes
	// "2 consecutive incidents" mean consecutive IN THE STREAM, not
	// consecutive at drain time: a clean turn between two failures
	// breaks the chain even when both incidents drain in the same tick.
	chain int
	// lastIncident remembers the newest incident detail so the recovery
	// message can name it even when the drain already emptied the queue.
	lastIncident string
}

func newStreamState() *streamState { return &streamState{} }

// note classifies ONE raw record blob into progress or noise (the F15
// split). A non-JSON or otherwise unclassified line is NOISE: the
// pilot proved that "any output is life" is exactly the assumption a
// chattering extension UI weaponizes against the watchdogs.
func (s *streamState) note(raw []byte) {
	var rec struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	typ := ""
	if json.Unmarshal(raw, &rec) == nil {
		typ = rec.Type
	}
	now := time.Now()
	s.mu.Lock()
	if !isProgressType(typ) {
		// Noise: extension UI chatter, unclassified record types,
		// unparseable lines. It refreshes only the noise clock — the
		// watchdogs never read it.
		s.lastNoiseAt = now
		s.noise++
		s.mu.Unlock()
		return
	}
	s.lastProgressAt = now
	s.records++
	switch typ {
	case recAgentStart, recTurnStart:
		s.turnDepth++
		s.lastIncidentKey = ""
	case recTurnEnd, recAgentEnd, recAgentSettled:
		if s.turnDepth > 0 {
			s.turnDepth--
		}
		s.toolDispatchAt = time.Time{} // turn closed: no tool in flight
	case recMessageEnd:
		if s.turnDepth > 0 {
			// message ended with the turn still open: a tool call was
			// dispatched and is executing (#83). Track it so the hang lane
			// can exempt live tool execution from the silence window.
			s.toolDispatchAt = now
		}
	case recToolResult:
		s.toolDispatchAt = time.Time{} // tool returned: dispatch cleared
	}
	s.mu.Unlock()

	fatalTurn := s.classifyIncident(typ, rec.Message)
	s.mu.Lock()
	if typ == recTurnEnd && !fatalTurn {
		s.cleanTurns++
		s.chain = 0 // a successful turn ends any incident chain
	}
	s.mu.Unlock()
}

// errorCarrier is the probe shape for the fields a real failure uses.
type errorCarrier struct {
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
}

// classifyIncident inspects ONE record for the provider-failure shapes
// and queues an incident when it finds one. Returns whether this record
// was a turn that ended on a fatal provider error.
//
// message_start is read too (dedup keeps the report single); turn_end
// marks the queued entry fatal.
func (s *streamState) classifyIncident(typ string, message json.RawMessage) bool {
	switch typ {
	case recMessageStart, recMessageEnd, recTurnEnd:
		if len(message) == 0 {
			return false
		}
		var ec errorCarrier
		if json.Unmarshal(message, &ec) != nil || ec.StopReason != "error" {
			return false
		}
		detail := ec.ErrorMessage
		if detail == "" {
			detail = "harness reported stopReason=error with no errorMessage"
		}
		fatal := typ == recTurnEnd
		s.noteIncident(StreamIncident{
			At: time.Now(), Kind: incidentKindOf(detail),
			Detail: detail, Fatal: fatal,
		})
		return fatal
	}
	return false
}

// incidentKindOf separates a RETRY report (the harness says it is trying
// again — the run continues) from a terminal provider error. Substring
// matching is deliberate: the harnesses carry free text here, so the
// kind is a HINT for the operator and the monitor never branches on it.
func incidentKindOf(detail string) string {
	l := strings.ToLower(detail)
	if strings.Contains(l, "retry") || strings.Contains(l, "rate limit") {
		return IncidentRetry
	}
	return IncidentProviderError
}

// noteIncident queues one classified infra incident (deduplicated within
// the turn that produced it).
func (s *streamState) noteIncident(in StreamIncident) {
	key := in.Kind + "\x00" + in.Detail
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastIncidentKey == key && !in.Fatal {
		return
	}
	if s.lastIncidentKey == key && in.Fatal {
		// The turn-ending report of an already-reported failure:
		// upgrade the queued entry to fatal rather than duplicating it.
		for i := range s.incidents {
			if s.incidents[i].Kind == in.Kind && s.incidents[i].Detail == in.Detail {
				s.incidents[i].Fatal = true
				return
			}
		}
	}
	s.lastIncidentKey = key
	s.lastIncident = in.Detail
	s.chain++
	if len(s.incidents) >= maxStreamIncidents {
		s.dropped++
		return
	}
	s.incidents = append(s.incidents, in)
}

// drain removes and returns the queued incidents, folding the drop count
// into a trailing summary entry.
func (s *streamState) drain() []StreamIncident {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.incidents) == 0 && s.dropped == 0 {
		return nil
	}
	out := s.incidents
	s.incidents = nil
	if s.dropped > 0 {
		out = append(out, StreamIncident{
			At: time.Now(), Kind: IncidentProviderError,
			Detail: strconv.FormatInt(s.dropped, 10) + " further stream incidents dropped (queue bound " + strconv.Itoa(maxStreamIncidents) + ")",
		})
		s.dropped = 0
	}
	return out
}

// heartbeat reads the current heartbeat snapshot.
func (s *streamState) heartbeat() StreamHeartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StreamHeartbeat{
		Observable:     s.records > 0 || s.noise > 0,
		LastRecordAt:   latestOf(s.lastProgressAt, s.lastNoiseAt),
		LastProgressAt: s.lastProgressAt,
		LastNoiseAt:    s.lastNoiseAt,
		InFlight:       s.turnDepth > 0,
		ToolDispatchAt: s.toolDispatchAt,
		Records:        s.records,
		Noise:          s.noise,
	}
}

// latestOf answers the newer of two timestamps (zero when both are).
func latestOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// cleanTurnCount reads the completed-clean-turn counter.
func (s *streamState) cleanTurnCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanTurns
}

// chainValue reads the consecutive-incident counter (storm guard input).
func (s *streamState) chainValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chain
}

// resetChain clears the consecutive-incident counter (step change,
// recovery adoption, or consumption by the storm guard).
func (s *streamState) resetChain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chain = 0
}

// latestIncidentDetail names the newest classified incident.
func (s *streamState) latestIncidentDetail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastIncident
}
