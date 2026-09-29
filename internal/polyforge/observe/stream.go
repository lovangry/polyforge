package observe

// stream.go: classification of the agent stdout record stream — the
// observe lane's half of the stream-heartbeat criterion (the per-WI
// monitor consumes the records Runtime's stdout reader feeds it and
// derives the three observations the hang watchdog needs):
//
//  1. lastRecordAt — when the stream last proved AGENT PROGRESS;
//  2. turnDepth    — whether a turn is open right now;
//  3. incidents    — provider errors riding the ordinary records,
//     surfaced immediately instead of being counted as bulk.
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

// maxStreamIncidents bounds the queued incidents: a provider failing
// every retry could queue one entry per record. The queue keeps the
// OLDEST entries and counts the drops.
const maxStreamIncidents = 64

// StreamHeartbeat is one point-in-time reading of the supervised agent's
// record stream.
type StreamHeartbeat struct {
	// Observable reports whether any record has been seen at all.
	Observable bool
	// LastRecordAt is when the reader last classified a record that
	// represents AGENT progress. The `response` echo of a command the
	// DAEMON itself just sent is excluded on purpose: counting it would
	// let the daemon's own probe traffic reset the hang window it is
	// trying to measure.
	LastRecordAt time.Time
	// InFlight reports whether a TURN is open: the stream carried
	// turn_start (or agent_start) and no matching turn_end / agent_end /
	// agent_settled yet. Silence with NO turn in flight is an idle agent
	// (waiting for work or for a human answer) — never a hang, which is
	// the needs_input exemption falling out of the criterion instead of
	// being special-cased.
	InFlight bool
	// Records is the total progress-record count (diagnostics).
	Records int64
}

// Silent reports whether the stream has been silent for at least window
// WHILE A TURN IS IN FLIGHT — the ruled hang criterion. anchor is the
// probe-cleared window restart (zero = none). An unobservable or idle
// stream is never silent in this sense.
func (h StreamHeartbeat) Silent(now time.Time, window time.Duration, anchor time.Time) bool {
	if !h.Observable || !h.InFlight || h.LastRecordAt.IsZero() {
		return false
	}
	eff := h.LastRecordAt
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
	// lastRecordAt is the heartbeat (response echoes excluded).
	lastRecordAt time.Time
	// turnDepth > 0 means a turn is open. A counter rather than a bool
	// because agent_start and turn_start both open; it saturates at 0 so
	// a stray close can never wedge the flag negative.
	turnDepth int
	records   int64
	incidents []StreamIncident
	dropped   int64
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

// note classifies ONE raw record blob. A non-JSON line still counts as
// progress: it is output the harness produced.
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
	if typ == recResponse {
		// The daemon's own command echo: the RPC reader is alive, which
		// proves nothing about the agent turn.
		s.mu.Unlock()
		return
	}
	s.lastRecordAt = now
	s.records++
	switch typ {
	case recAgentStart, recTurnStart:
		s.turnDepth++
		s.lastIncidentKey = ""
	case recTurnEnd, recAgentEnd, recAgentSettled:
		if s.turnDepth > 0 {
			s.turnDepth--
		}
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
		Observable:   s.records > 0 || !s.lastRecordAt.IsZero(),
		LastRecordAt: s.lastRecordAt,
		InFlight:     s.turnDepth > 0,
		Records:      s.records,
	}
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
