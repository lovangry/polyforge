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
// hang_cleared, idle_suspect, idle_cleared}) and the push bookkeeping.
const (
	EvJobStarted       = "job_started"
	EvAgentRecovered   = "wi_agent_rederived"
	EvNeedsRecovery    = "needs_recovery"
	EvInfraIncident    = "infra_incident"
	EvNeedsInput       = "needs_input"
	EvPublished        = "published"
	EvPushFailed       = "push_failed"
	EvPushBackpressure = "push_backpressure"
	// EvPushPoisonedLocal is the V8-repair classification of the LOCAL
	// gitlink precheck refusal (gitops' typed GIT-412
	// LOCAL_GITLINK_DETECTED — the poison never entered local history).
	// Deliberately a SEPARATE kind from push_failed: the M8 read-through
	// counts push_failed/published/wrap_blocked only, and a local
	// precheck refusal is not "an already-occurred boundary sync request
	// that failed" — it is the daemon declining to CREATE a commit that
	// never legally existed, so it belongs in neither M8's numerator nor
	// its denominator. A server-side pre-receive rejection of an
	// already-committed gitlink (should one ever occur again) stays an
	// ordinary push_failed and is counted, unchanged.
	EvPushPoisonedLocal = "push_poisoned_local"
	// EvTranscriptMissing is the F3 typed warning: a commit-boundary
	// transcript sync could not locate the derivation's live harness
	// transcript (the harness wrote nothing under its session store).
	// Advisory only — the commit/push proceed without the transcript;
	// one event per attempt, never a spam loop.
	EvTranscriptMissing = "transcript_missing"
	// EvVersionStagnant is the C3' belt-and-braces warning: execution.json
	// was rewritten (mtime advanced) N consecutive times WITHOUT a
	// top-level Version bump — the archival contract is violated, the
	// monitor schedules a fallback commit anyway, and the event names the
	// violation so the operator can fix the agent's write path. Version
	// stays the primary archival semantic.
	EvVersionStagnant = "version_stagnant"
	// EvNoProgress is the F15c owner face: the agent's stream has
	// carried NO progress record for longer than the idle window — the
	// same progress-silence signal the hang/idle watchdogs judge, landed
	// in the events log so the owner can see a stalled job even while
	// the watchdogs are still inside their probe/confirmation cycle.
	// Throttled to at most one event per window; cleared by resumed
	// progress, a probe round and recovery adoption. Exempt while a
	// question is pending (needs_input is designed stillness and already
	// owns the QUESTION face) and in every suppressed state.
	EvNoProgress = "no_progress"
	EvJobStopped = "job_stopped"
	// EvDrainDegraded is the drain loop's authorization-class degraded
	// signal (daemon/daemon.go fetchDrainWave): a drain-mode wave fetch of
	// the ready queue was denied for authorization reasons (typed aihub
	// error with HTTP 401/403/404 — e.g. the machine key revoked mid-run).
	// Daemon-scope: appended with wi="" (the fetch owns no job) and
	// merged into every live monitor's JobView events layer, so any
	// observe view can read it. Edge-triggered once per silent→degraded
	// episode; the episode clears on the next successful fetch. Denial
	// MUST surface as this event, never as silent idling; transport-class
	// losses (network/5xx) stay silent idle, unchanged.
	EvDrainDegraded = "drain_degraded"
	// EvSessionSuperseded is the v1.5 M4 verdict record: the periodic
	// session revalidation learned another daemon holds this work item
	// (matches=false). Payload carries the CURRENT holder's public
	// fingerprint — never a secret. The monitor stops supervising and
	// the runtime kills/quarantines/releases; no hub transition happens
	// (superseded is not terminal).
	EvSessionSuperseded = "session_superseded"
	// EvSessionVerifyFailed is the M4 transport-class record: a periodic
	// session-verify could not reach aihub. Log-only by contract — no
	// action is ever taken on a transport error; the retry is bounded.
	EvSessionVerifyFailed = "session_verify_failed"
)

// Infra-incident payload kinds (04 §5.1a tenth-event family).
const (
	IncidentProviderError = "provider_error"
	IncidentRetry         = "retry"
	HangSuspected         = "hang_suspected"
	HangCleared           = "hang_cleared"
	// IdleSuspected/IdleCleared are the F5 idle watchdog's probe
	// bookkeeping: the same confirmation-probe in/out shape as the hang
	// pair, fired when the stream is quiet with NO turn in flight.
	IdleSuspected = "idle_suspect"
	IdleCleared   = "idle_cleared"
)

// RingCap bounds the per-WI live view served over observe.
const RingCap = 500

// EventsFile is the JSONL file name inside the daemon data directory.
const EventsFile = "events.log"

// EventLog appends protocol.EventEntry lines to the daemon events file
// and mirrors them into per-WI rings. Safe for concurrent use: monitor
// goroutines, push sub-goroutines and the HTTP read face all touch it.
type EventLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
	// rings holds per-WI protocol.EventEntry values. Seq is the per-WI
	// MONOTONIC append ordinal — the total appended count at write time,
	// not a ring index: truncation only bounds RETENTION, never the
	// numbering, so a client's ?after cursor keeps working across ring
	// turnover (#21). The ring is the ONLY seq authority: the durable
	// file keeps writing the bare entry without one (a daemon restart
	// legitimately renumbers seq from 1, so written-back seqs would
	// collide with earlier runs' lines).
	rings  map[string][]protocol.EventEntry
	totals map[string]int64 // per-WI append count: the monotonic seq source
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
		rings: map[string][]protocol.EventEntry{}, totals: map[string]int64{}}, nil
}

// Path returns the events file location (diagnostic/tests).
func (l *EventLog) Path() string { return l.path }

// Append writes one event for work item wi. body may be nil. The file
// write is best-effort by design (§五: an events-file failure never
// blocks the agent): an I/O failure is
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
	l.totals[wi]++
	seq := l.totals[wi]
	// The file line above keeps the wire-stable bare shape (no seq);
	// the ring entry is the one that carries it, so the serve face can
	// hand clients their next ?after cursor.
	entry.Seq = seq
	ring := append(l.rings[wi], entry)
	if len(ring) > RingCap {
		ring = ring[len(ring)-RingCap:]
	}
	l.rings[wi] = ring
	return werr
}

// Since returns the ring entries for wi (at most RingCap, oldest first).
// The live view only: a daemon restart empties it, and the file remains
// the durable record.
func (l *EventLog) Since(wi string) []protocol.EventEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]protocol.EventEntry, len(l.rings[wi]))
	copy(out, l.rings[wi])
	return out
}

// SinceAfter returns the retained entries for wi whose seq is strictly
// greater than after — the GET /jobs/{id}/observe ?after cursor (#21).
// Because seq counts TOTAL appends, not ring slots, an after value that
// falls in the truncated span simply yields the whole retained ring.
func (l *EventLog) SinceAfter(wi string, after int64) []protocol.EventEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	ring := l.rings[wi]
	out := make([]protocol.EventEntry, 0, len(ring))
	for _, e := range ring {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out
}

// Close closes the events file.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// ReadEventsFile parses the whole events file (test/diagnostic face).
func ReadEventsFile(path string) ([]protocol.EventEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []protocol.EventEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e protocol.EventEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
