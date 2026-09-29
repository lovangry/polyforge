package mainagent

// streamheartbeat.go: the supervisor's half of the polyforge#48
// stream-heartbeat criterion. The stdout reader already classifies every
// protocol record (supervisor.go classifyStdoutLine) because it is the
// ack carrier; this file adds the three observations the daemon's hang
// watchdog needs from that same pass, at the cost of two atomic stores
// per record:
//
//  1. lastRecordAt — when the stream last proved AGENT PROGRESS;
//  2. inFlight     — whether a turn is open right now;
//  3. incidents    — provider errors, surfaced immediately instead of
//     being counted as bulk.
//
// ---------------------------------------------------------------------
// PINNED REAL RECORD SHAPES (pi 0.87.1, `pi --mode rpc`, captured on the
// target box 2026-09-29 and re-verified from a second independent run;
// fixtures in testdata/, regression-pinned by TestPinnedPiRecordShapes).
//
// The classification below is written against THESE names, not against
// plausible ones. One prompt turn emits, in order:
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
// CRITICAL ACCURACY FINDING (the reason step 1 of the ticket demanded a
// real capture before any classifier was written): pi emits NO
// `type:"error"` record and NO pi-side `result` record. A provider
// failure rides the ORDINARY records as
//
//	message.stopReason == "error"  +  message.errorMessage
//
// on the assistant message_start, on message_end AND on turn_end — the
// 401 capture is byte-identical to the success capture except for those
// two fields and an empty content array. Classifying on an assumed
// "error" record type would have matched exactly nothing, and the infra
// signal would have been silently dead in production.
//
// The claude adapter's measured stream (04 §6.1a, polyforge#33) keeps
// its own shapes: `assistant` records and per-turn `result` records with
// is_error / a non-success subtype. Both are mapped here too.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/daemon"
)

// Record type names of the pi RPC event stream that carry LIFECYCLE
// meaning for the heartbeat. Pinned against the real capture above; the
// fixture regression fails if a harness upgrade renames one.
const (
	recAgentStart   = "agent_start"
	recAgentEnd     = "agent_end"
	recAgentSettled = "agent_settled"
	recTurnStart    = "turn_start"
	recTurnEnd      = "turn_end"
	recResponse     = "response"
)

// maxStreamIncidents bounds the per-instance incident queue. A provider
// that fails every retry could otherwise queue one entry per record; the
// queue keeps the OLDEST entries (the first failure explains the outage;
// the thousandth repeats it) and counts the drops.
const maxStreamIncidents = 64

// streamState is the per-Proc record-stream bookkeeping. Its own mutex,
// never Proc.mu: the stdout reader updates it on every record while
// writeStdin may hold Proc.mu across a blocking stdin write (the same
// deadlock reasoning that gave `pending` its own pendMu).
type streamState struct {
	mu sync.Mutex
	// lastRecordAt is the heartbeat: the last record proving AGENT
	// progress. `response` echoes are excluded on purpose — see
	// daemon.StreamHeartbeat.LastRecordAt.
	lastRecordAt time.Time
	// lastAnyRecordAt includes response echoes (auxiliary only).
	lastAnyRecordAt time.Time
	// turnDepth > 0 means a turn is open. A counter rather than a bool
	// because agent_start and turn_start both open (a run wraps its
	// turns) and the stream may interleave them; it saturates at 0 so a
	// stray close can never drive it negative and wedge the flag.
	turnDepth int
	records   int64
	incidents []daemon.StreamIncident
	dropped   int64
	// lastIncidentKey suppresses the triple-report of ONE provider
	// failure: the measured stream carries the same errorMessage on
	// message_start, message_end and turn_end of the failed turn. The
	// key is cleared when a new turn opens, so a REPEATED failure on a
	// later turn is reported again.
	lastIncidentKey string
}

// noteRecord is called once per classified stdout record. typ is the
// record's `type` field ("" for a non-record raw line, which still
// counts as progress: the claude legacy stream and unknown noise are
// output the harness produced).
func (s *streamState) noteRecord(typ string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAnyRecordAt = now
	if typ == recResponse {
		// The daemon's own command echo: the harness RPC reader is
		// alive, which proves nothing about the agent turn. Counting it
		// would let the confirmation probe reset the very window it is
		// measuring.
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
}

// noteIncident queues one classified infra incident (deduplicated within
// the turn that produced it).
func (s *streamState) noteIncident(in daemon.StreamIncident) {
	key := in.Kind + "\x00" + in.Detail
	s.mu.Lock()
	if s.lastIncidentKey == key && !in.Fatal {
		s.mu.Unlock()
		return
	}
	if s.lastIncidentKey == key && in.Fatal {
		// The turn-ending report of an already-reported failure: upgrade
		// the queued entry to fatal rather than queueing a duplicate.
		for i := range s.incidents {
			if s.incidents[i].Kind == in.Kind && s.incidents[i].Detail == in.Detail {
				s.incidents[i].Fatal = true
				s.mu.Unlock()
				return
			}
		}
	}
	s.lastIncidentKey = key
	if len(s.incidents) >= maxStreamIncidents {
		s.dropped++
		s.mu.Unlock()
		return
	}
	s.incidents = append(s.incidents, in)
	s.mu.Unlock()
}

// heartbeat reads the current stream heartbeat.
func (s *streamState) heartbeat() daemon.StreamHeartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return daemon.StreamHeartbeat{
		Observable:      true,
		LastRecordAt:    s.lastRecordAt,
		LastAnyRecordAt: s.lastAnyRecordAt,
		InFlight:        s.turnDepth > 0,
		Records:         s.records,
	}
}

// drain removes and returns the queued incidents.
func (s *streamState) drain() []daemon.StreamIncident {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.incidents) == 0 {
		return nil
	}
	out := s.incidents
	s.incidents = nil
	if s.dropped > 0 {
		out = append(out, daemon.StreamIncident{
			At: time.Now(), Kind: daemon.IncidentProviderError,
			Detail: fmt.Sprintf("%d further stream incidents dropped (queue bound %d)", s.dropped, maxStreamIncidents),
		})
		s.dropped = 0
	}
	return out
}

// ---------------------------------------------------------------------------
// Incident classification against the PINNED shapes.

// errorCarrier is the probe shape for the fields a pi failure actually
// uses. Kept separate from stdoutRecord so the ack path is untouched:
// stopReason/errorMessage live INSIDE the message object, which
// stdoutRecord holds as a RawMessage for the assistant-text extraction.
type errorCarrier struct {
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
}

// classifyIncident inspects ONE record for the measured infra shapes and
// queues an incident when it finds one. Called from the stdout reader
// after the record has been classified for acks, so it can never delay
// or drop an ack.
//
// pi: message_end / turn_end whose message carries stopReason=="error".
// message_start is deliberately NOT read — it carries the same
// errorMessage but arrives first and the dedup would then swallow the
// authoritative message_end. Instead every carrier is read and the
// per-turn dedup keeps the report single.
//
// claude: a result record with is_error or a non-success subtype
// (claudeResultFailed, already the supervisor's own test).
func (p *Proc) classifyIncident(rec stdoutRecord) {
	if p.stream == nil {
		return
	}
	switch rec.Type {
	case "message_start", "message_end", recTurnEnd:
		if len(rec.Message) == 0 {
			return
		}
		var ec errorCarrier
		if json.Unmarshal(rec.Message, &ec) != nil {
			return
		}
		if ec.StopReason != "error" {
			return
		}
		detail := ec.ErrorMessage
		if detail == "" {
			detail = "harness reported stopReason=error with no errorMessage"
		}
		in := daemon.StreamIncident{
			At: time.Now(), Kind: incidentKindOf(detail),
			Detail: detail, Fatal: rec.Type == recTurnEnd,
		}
		p.stream.noteIncident(in)
		p.logIncident(in)
	case "result":
		if !claudeResultFailed(rec) {
			return
		}
		detail := rec.ErrText
		if detail == "" {
			detail = "claude turn failed (subtype=" + orDefault(rec.Subtype, "unknown") + ")"
		}
		in := daemon.StreamIncident{
			At: time.Now(), Kind: incidentKindOf(detail), Detail: detail, Fatal: true,
		}
		p.stream.noteIncident(in)
		p.logIncident(in)
	}
}

// incidentKindOf separates a RETRY report (the harness says it is trying
// again — the run continues) from a terminal provider error. Substring
// matching is deliberate and documented as best-effort: the harnesses
// carry free text here, so the kind is a HINT for the operator and the
// watchdog never branches on it. Both kinds surface identically.
func incidentKindOf(detail string) string {
	l := strings.ToLower(detail)
	if strings.Contains(l, "retry") || strings.Contains(l, "retrying") || strings.Contains(l, "rate limit") {
		return daemon.IncidentRetry
	}
	return daemon.IncidentProviderError
}

// logIncident writes the immediate operator-visible line. The job's
// observe ring gets the same incident through DrainStreamIncidents on
// the daemon's next watchdog tick (≤200ms).
func (p *Proc) logIncident(in daemon.StreamIncident) {
	fatal := ""
	if in.Fatal {
		fatal = " (turn ended on it)"
	}
	fmt.Fprintf(os.Stderr, "mainagent: harness stream infra incident [%s]%s: %s\n", in.Kind, fatal, in.Detail)
}

// ---------------------------------------------------------------------------
// The AgentStreamProbe / AgentLivenessProbe faces.

// StreamHeartbeat implements daemon.AgentStreamProbe.
func (p *Proc) StreamHeartbeat() daemon.StreamHeartbeat {
	if p.stream == nil {
		return daemon.StreamHeartbeat{}
	}
	return p.stream.heartbeat()
}

// DrainStreamIncidents implements daemon.AgentStreamProbe.
func (p *Proc) DrainStreamIncidents() []daemon.StreamIncident {
	if p.stream == nil {
		return nil
	}
	return p.stream.drain()
}

// probeAckBound is the turn-scale bound of ONE confirmation probe. The
// probe is only ever sent AFTER the hang window already expired, so this
// is not a latency cost on any healthy path; it is the width of the
// benefit of the doubt a legitimately-slow turn gets before the kill.
const probeAckBound = 2 * time.Minute

// ProbeLiveness implements daemon.AgentLivenessProbe: the ruled
// confirmation step between "the stream went silent past the window" and
// "kill the agent". One pf/probe rides the ordinary control channel
// (under pi: a steer prompt delivered at the agent's next safe
// boundary — which is precisely the boundary a hung agent never
// reaches), and the ack is awaited turn-scale.
//
// A nil return means the agent answered and the kill is CANCELLED: the
// stream window restarts and the agent keeps its process. Any error —
// timeout, dead control path, dead agent — confirms the verdict.
func (p *Proc) ProbeLiveness(ctx context.Context) error {
	if err := p.SendControl(MethodProbe, map[string]any{"reason": "watchdog_hang_confirmation"}); err != nil {
		return err
	}
	return p.awaitAckBound(ctx, MethodProbe, probeAckBound)
}
