package observe

// events.go: the daemon events file + per-WI serving ring.
//
// Every observation the supervisor makes AND every driver event is
// appended as ONE JSONL line to <DataDir>/events.log using
// common/protocol.EventEntry (the wire-stable shape the CLI and the
// aihub read face share). Each line carries its per-WI id (seq),
// MONOTONIC across daemon restarts: on open the counters are rebuilt from
// the durable files (plus a small snapshot written at rotation, so a WI
// whose newest lines rotated away keeps counting forward). The file
// rotates by size (events.log -> events.log.1 -> ...). The same entries
// are kept in an in-memory ring per work item, pre-filled from the file
// at open; the observe face serves the ring and falls back to the
// durable files for a cursor older than the ring retains (history
// survives restarts). The execution-state authority is NEVER here — it
// stays the capsule's runtime-state/execution.json (INV-1); events are
// news, not state, and an append failure is a logged degradation, never
// a correctness dependency.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/silgrid/polyforge/internal/common/atomicfile"

	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Event kinds emitted by the supervisor. Driver events (step_started,
// step_done, needs_input, answered, interrupted, rewind, terminal,
// step_failed, blocked, ...) land in the same log through the daemon
// binding. A closed set per emitter: job lifecycle, supervisor verdicts
// and the push bookkeeping.
const (
	EvJobStarted       = "job_started"
	EvAgentRecovered   = "wi_agent_rederived"
	EvNeedsRecovery    = "needs_recovery"
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
	// EvSupervisorVerdict records one supervisor verdict (step_dead with
	// its cause and evidence, or driver_stalled) at the moment it is sent
	// to the driver.
	EvSupervisorVerdict = "supervisor_verdict"
	EvJobStopped        = "job_stopped"
	// EvQueueFetchDenied names the signal it actually carries: a
	// project's ready-queue fetch was AUTHORIZATION-DENIED (typed aihub
	// error with HTTP 401/403/404 — e.g. the machine key revoked
	// mid-run), raised by the auto-pull wave (daemon/daemon.go
	// fetchAutoWave).
	// Daemon-scope: appended with wi="" (the fetch owns no job) and
	// kept in the durable events log only. Edge-triggered once per
	// healthy→denied episode; the episode clears on the next successful
	// fetch. Denial MUST surface as this event, never as silent idling; transport-class
	// losses (network/5xx) stay silent idle, unchanged.
	EvQueueFetchDenied = "queue_fetch_denied"
	// EvRetiredEnvIgnored (machine-level, no work item; decision
	// 2026-10-16 G2): a retired POLYFORGE_* variable is set in the
	// daemon's environment with a value that differs from what the daemon
	// resolved — it is no longer read, so harness state would otherwise
	// move without a word.
	EvRetiredEnvIgnored = "retired_env_ignored"
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
	// EvSubmitRejected (v2 §5): an accepted submit's runtime Start failed at
	// dispatch (live or replayed after a restart); body carries the error,
	// intent and the entry's submitted_at/origin. The submit was answered
	// `accepted` earlier, so this durable event is how watchers learn it
	// did not start.
	EvSubmitRejected = "submit_rejected"
	// EvSubmitDropped (v2 §5): a persisted submit was NOT replayed after a
	// restart (resume intent, or aihub no longer shows the WI
	// queued, or the claiming journal already owns it); body names why.
	EvSubmitDropped = "submit_dropped"
	// EvDaemonRestarted (v2 §6): written into every WI the daemon
	// recovered or parked after restart reconciliation; body carries the
	// previous shutdown cause when known (#110) else "unknown".
	EvDaemonRestarted = "daemon_restarted"
)

// RingCap bounds the per-WI live view served over observe.
const RingCap = 500

// EventsFile is the JSONL file name inside the daemon data directory.
const EventsFile = "events.log"

// eventsSeqFile is the counter snapshot written at rotation.
const eventsSeqFile = "events.seq"

// Rotation defaults: the file rotates when it reaches
// DefaultEventsMaxBytes and DefaultEventsKeep rotated generations stay.
const (
	DefaultEventsMaxBytes = 16 << 20
	DefaultEventsKeep     = 3
	// HistoryCap bounds one durable-history answer.
	HistoryCap = 5000
)

// EventLogOpts tunes rotation (zero = the defaults).
type EventLogOpts struct {
	MaxBytes int64
	Keep     int
}

// EventLog appends protocol.EventEntry lines to the daemon events file
// and mirrors them into per-WI rings. Safe for concurrent use: monitor
// goroutines, push sub-goroutines, the driver and the HTTP read face all
// touch it.
type EventLog struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	dir      string
	size     int64
	maxBytes int64
	keep     int
	// rings holds per-WI protocol.EventEntry values. Seq is the per-WI
	// MONOTONIC append ordinal — the total appended count at write time,
	// persisted on every line and rebuilt from the files at open, so the
	// numbering survives truncation of the ring AND daemon restarts (a
	// client's ?after cursor keeps working throughout).
	rings  map[string][]protocol.EventEntry
	totals map[string]int64 // per-WI append count: the monotonic seq source
}

// NewEventLog opens (creating if needed) <dataDir>/events.log.
func NewEventLog(dataDir string) (*EventLog, error) {
	return NewEventLogOpts(dataDir, EventLogOpts{})
}

// NewEventLogOpts is NewEventLog with explicit rotation tuning.
func NewEventLogOpts(dataDir string, o EventLogOpts) (*EventLog, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultEventsMaxBytes
	}
	if o.Keep <= 0 {
		o.Keep = DefaultEventsKeep
	}
	l := &EventLog{
		path:     filepath.Join(dataDir, EventsFile),
		dir:      dataDir,
		maxBytes: o.MaxBytes,
		keep:     o.Keep,
		rings:    map[string][]protocol.EventEntry{},
		totals:   map[string]int64{},
	}
	l.loadSnapshot()
	// rebuild counters + rings from the durable files (oldest first)
	for _, e := range l.scanFiles() {
		if e.Seq <= 0 {
			continue // not a valid persisted event (ids are always persisted)
		}
		if e.Seq > l.totals[e.WI] {
			l.totals[e.WI] = e.Seq
		}
		ring := append(l.rings[e.WI], e)
		if len(ring) > RingCap {
			ring = ring[len(ring)-RingCap:]
		}
		l.rings[e.WI] = ring
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
	l.f = f
	return l, nil
}

// Path returns the events file location (diagnostic/tests).
func (l *EventLog) Path() string { return l.path }

func (l *EventLog) rotatedPath(i int) string { return fmt.Sprintf("%s.%d", l.path, i) }

// loadSnapshot reads the rotation-time counter snapshot.
func (l *EventLog) loadSnapshot() {
	raw, err := os.ReadFile(filepath.Join(l.dir, eventsSeqFile))
	if err != nil {
		return
	}
	var m map[string]int64
	if json.Unmarshal(raw, &m) == nil {
		for wi, n := range m {
			if n > l.totals[wi] {
				l.totals[wi] = n
			}
		}
	}
}

// writeSnapshotLocked persists the counters (tmp + rename).
func (l *EventLog) writeSnapshotLocked() {
	raw, err := json.Marshal(l.totals)
	if err != nil {
		return
	}
	_ = atomicfile.Write(filepath.Join(l.dir, eventsSeqFile), raw, 0o644)
}

// scanFiles parses every durable generation, oldest first.
func (l *EventLog) scanFiles() []protocol.EventEntry {
	var out []protocol.EventEntry
	paths := make([]string, 0, l.keep+1)
	for i := l.keep; i >= 1; i-- {
		paths = append(paths, l.rotatedPath(i))
	}
	paths = append(paths, l.path)
	for _, p := range paths {
		es, _ := ReadEventsFile(p)
		out = append(out, es...)
	}
	return out
}

// rotateLocked moves the full file aside and opens a fresh one. The
// counter snapshot is written FIRST so ids stay monotonic even when the
// generation holding a WI's newest line is later dropped.
func (l *EventLog) rotateLocked() error {
	l.writeSnapshotLocked()
	if err := l.f.Close(); err != nil {
		return err
	}
	_ = os.Remove(l.rotatedPath(l.keep))
	for i := l.keep - 1; i >= 1; i-- {
		_ = os.Rename(l.rotatedPath(i), l.rotatedPath(i+1))
	}
	if err := os.Rename(l.path, l.rotatedPath(1)); err != nil {
		// keep appending to the oversized file rather than lose the log
		f, oerr := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if oerr == nil {
			l.f = f
		}
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.f, l.size = f, 0
	return nil
}

// Append writes one event for work item wi. body may be nil. The file
// write is best-effort by design (§五: an events-file failure never
// blocks the agent, and execution.json stays the authority): an I/O
// failure is returned to the caller as a degradation signal, but the
// ring entry (and the in-memory counter) is recorded either way so the
// live observe face keeps working.
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
	l.mu.Lock()
	defer l.mu.Unlock()
	l.totals[wi]++
	entry.Seq = l.totals[wi]
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	var werr error
	if l.f == nil {
		werr = os.ErrClosed
	} else {
		n, e := l.f.Write(line)
		l.size += int64(n)
		werr = e
		if werr == nil && l.size >= l.maxBytes {
			if rerr := l.rotateLocked(); rerr != nil {
				werr = rerr
			}
		}
	}
	ring := append(l.rings[wi], entry)
	if len(ring) > RingCap {
		ring = ring[len(ring)-RingCap:]
	}
	l.rings[wi] = ring
	return werr
}

// SinceAfter returns the entries for wi whose seq is strictly greater
// than after — the watch snapshot's ?after cursor (#21). The ring
// answers when it still holds the cursor's successor; a cursor older than
// the ring (or from before a restart that outlived the ring) is served
// from the DURABLE files, so history is never limited to the in-memory
// ring (at most HistoryCap entries per answer, oldest first).
func (l *EventLog) SinceAfter(wi string, after int64) []protocol.EventEntry {
	l.mu.Lock()
	ring := l.rings[wi]
	total := l.totals[wi]
	covered := len(ring) > 0 && ring[0].Seq <= after+1
	if covered || total <= after {
		out := make([]protocol.EventEntry, 0, len(ring))
		for _, e := range ring {
			if e.Seq > after {
				out = append(out, e)
			}
		}
		l.mu.Unlock()
		return out
	}
	l.mu.Unlock()
	return l.History(wi, after)
}

// LastSeq is the highest seq ever appended for wi (0 = none) — the
// monotonic counter itself, so it stays exact past HistoryCap and never
// touches the durable files.
func (l *EventLog) LastSeq(wi string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totals[wi]
}

// Recent returns a copy of wi's in-memory ring (the newest <= RingCap
// entries, oldest first; rebuilt from the files at open). It never reads
// the durable files — the face for hot-path "what happened last" polls.
func (l *EventLog) Recent(wi string) []protocol.EventEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]protocol.EventEntry(nil), l.rings[wi]...)
}

// History reads the durable files for wi's entries with seq > after
// (oldest first, at most HistoryCap). It is read-only and runs outside
// the append lock; a concurrent rotation can only make it see a
// generation boundary twice, which the seq filter absorbs.
func (l *EventLog) History(wi string, after int64) []protocol.EventEntry {
	var out []protocol.EventEntry
	seen := map[int64]bool{}
	for _, e := range l.scanFiles() {
		if e.WI != wi || e.Seq <= after || seen[e.Seq] {
			continue
		}
		seen[e.Seq] = true
		out = append(out, e)
		if len(out) >= HistoryCap {
			break
		}
	}
	return out
}

// Close closes the events file.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// ReadEventsFile parses one events file (test/diagnostic face).
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
