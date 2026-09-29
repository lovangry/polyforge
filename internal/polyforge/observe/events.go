package observe

// events.go: the daemon events file + per-WI serving ring.
//
// Every observation the monitor makes is appended as ONE JSONL line to
// <DataDir>/events.log using common/protocol.EventEntry (the wire-stable
// shape the CLI and the aihub read face share). The same entries are kept
// in an in-memory ring per work item (bounded, live view only) which is
// what GET /jobs/{id}/observe serves: the events layer of the JobView
// projection. The execution-state authority is NEVER here — it stays the
// capsule's runtime-state/execution.json (INV-1); events are news, not
// state.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Event kinds emitted by the observe lane. A closed set: execution
// progress (job lifecycle), infrastructure news (the infra_incident
// family, payload kind ∈ {provider_error, retry, hang_suspected,
// hang_cleared}) and the push bookkeeping.
const (
	EvJobStarted       = "job_started"
	EvAgentRecovered   = "wi_agent_rederived"
	EvNeedsRecovery    = "needs_recovery"
	EvInfraIncident    = "infra_incident"
	EvNeedsInput       = "needs_input"
	EvAnswered         = "answered"
	EvPublished        = "published"
	EvPushFailed       = "push_failed"
	EvPushBackpressure = "push_backpressure"
	EvWrapBlocked      = "wrap_blocked"
	EvJobStopped       = "job_stopped"
)

// Infra-incident payload kinds (04 §5.1a tenth-event family).
const (
	IncidentProviderError = "provider_error"
	IncidentRetry         = "retry"
	HangSuspected         = "hang_suspected"
	HangCleared           = "hang_cleared"
)

// RingCap bounds the per-WI live view served over observe.
const RingCap = 500

// EventsFile is the JSONL file name inside the daemon data directory.
const EventsFile = "events.log"

// EventLog appends protocol.EventEntry lines to the daemon events file
// and mirrors them into per-WI rings. Safe for concurrent use: monitor
// goroutines, flush sub-goroutines and the HTTP read face all touch it.
type EventLog struct {
	mu    sync.Mutex
	f     *os.File
	path  string
	rings map[string][]protocolEvent
}

// protocolEvent is protocol.EventEntry plus the ring's sequence number.
type protocolEvent struct {
	Seq int64 `json:"seq"`
	protocol.EventEntry
}

// NewEventLog opens (creating if needed) <dataDir>/events.log.
func NewEventLog(dataDir string) (*EventLog, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, EventsFile),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &EventLog{f: f, path: filepath.Join(dataDir, EventsFile),
		rings: map[string][]protocolEvent{}}, nil
}

// Path returns the events file location (diagnostic/tests).
func (l *EventLog) Path() string { return l.path }

// Append writes one event for work item wi. body may be nil. The file
// write is best-effort by design (§五: a needs_input flush degrades to
// "write events file, do not block the agent"): an I/O failure is
// returned to the caller, but the ring entry is recorded either way so
// the live observe face keeps working.
func (l *EventLog) Append(wi, kind string, body any) error {
	entry := protocol.EventEntry{
		At:   time.Now().UTC().Format(time.RFC3339Nano),
		Kind: kind,
		WI:   wi,
	}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		entry.Body = raw
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, werr := l.f.Write(append(line, '\n'))
	ring := append(l.rings[wi], protocolEvent{Seq: int64(len(l.rings[wi])) + 1, EventEntry: entry})
	if len(ring) > RingCap {
		ring = ring[len(ring)-RingCap:]
	}
	l.rings[wi] = ring
	return werr
}

// Since returns the ring entries for wi (at most RingCap, oldest first).
// The live view only: a daemon restart empties it, and the file remains
// the durable record.
func (l *EventLog) Since(wi string) []protocolEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]protocolEvent, len(l.rings[wi]))
	copy(out, l.rings[wi])
	return out
}

// Close closes the events file.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// ReadEventsFile parses the whole events file (test/diagnostic face).
func ReadEventsFile(path string) ([]protocolEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []protocolEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e protocolEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
