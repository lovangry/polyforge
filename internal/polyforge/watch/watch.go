// Package watch is the watch lane's DATA layer (Go, inside the pf
// binary) for wi_LMSxPTQTtz5x: the one-shot snapshot the session-side
// watcher consumes through `pf watch-probe`. The MECHANISM — the poll
// loop, the two channels, log-file management, terminal self-clean,
// session-restart re-arm — lives in the skill's watch.sh, copied verbatim
// into <workspace>/.polyforge/watchers/ at arm time; this package owns
// the snapshot assembly, the delta state the probe persists (the script
// stays stateless), the shared transcript location resolution and the
// delta-gated summarize dispatch.
package watch

import (
	"fmt"
	"os"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/sessionjsonl"
)

// The spec's tail constants (spec decision 2): they live in the watch
// module, never in the script. The script sources WatchLogTailLines once
// at startup through `pf watch-probe --print-cadence`
// (watch_log_tail_lines=<n>).
const (
	// TranscriptTailRecords is the bounded tail of session-jsonl records
	// the snapshot carries.
	TranscriptTailRecords = 200
	// TranscriptTailBytes caps the tail BY BYTES: raw jsonl records carry
	// whole assistant messages, so a records-only bound routinely feeds a
	// multi-hundred-KB prompt and the summarize dispatch dies at its
	// timeout (caught live on polyforge#124/#81, 2026-10-11: a 447KB
	// transcript saturated the full 5-minute dispatch timeout every
	// probe, stalling the LOG channel to one line per ~6 minutes). Keep
	// the NEWEST bytes under the cap; oldest records drop first.
	TranscriptTailBytes = 32 * 1024
	// WatchLogTailLines is the watcher-log tail the wake report carries.
	WatchLogTailLines = 50
)

// PendingQuestion is the parked question projection the snapshot carries
// (null when none). The id is the contract: the wake report must surface
// WHICH question parked the work item.
type PendingQuestion struct {
	ID       string `json:"id"`
	Step     string `json:"step,omitempty"`
	Question string `json:"question,omitempty"`
	AskedAt  string `json:"asked_at,omitempty"`
}

// Snapshot is the probe's --json document (spec decision 4): the EXACT
// documented field set — no more, no less. summary is a string (the
// model's line, labeled non-authoritative); on a summarize failure it is
// null and summary_error carries the raw error text. pending_question is
// null when none.
type Snapshot struct {
	WI             string           `json:"wi"`
	Step           string           `json:"step"`
	StepStatus     string           `json:"step_status"`
	LatestEvent    string           `json:"latest_event"`
	Pending        *PendingQuestion `json:"pending_question"`
	TranscriptPath string           `json:"transcript_path"`
	TranscriptTail []string         `json:"transcript_tail"`
	TranscriptSize int64            `json:"transcript_size"`
	TranscriptAge  string           `json:"transcript_age"`
	Summary        *string          `json:"summary"`
	SummaryError   *string          `json:"summary_error"`
	Timestamp      string           `json:"timestamp"`

	// Status is the observe projection's WI-level status
	// (needs_input/running/done/sealing). Deliberately OUT of the
	// --json document (the documented field set is exact): it serves the
	// human line and the script's terminal detection.
	Status string `json:"-"`
}

// ExecState is the execution-state input the snapshot consumes (step,
// status, latest event, pending question). The CLI wires it to the
// daemon's observe face; tests wire fakes — the watch module itself never
// imports the daemon or the protocol vocabulary.
type ExecState struct {
	Status      string // the observe projection's status (needs_input, running, done, ...)
	Step        string
	StepStatus  string
	LatestEvent string
	LatestSeq   int64
	Pending     *PendingQuestion
}

// StateSource is the execution-state face the snapshot consumes.
type StateSource interface {
	ObserveExecState(wi string) (ExecState, error)
}

// Deps wires one probe invocation: the WI, the machine workspace root
// (whose .polyforge/watchers/ holds the delta state), the WI's capsule
// directory, the sessions-root override from the config ("" = the harness
// default) and the execution-state source.
type Deps struct {
	WI           string
	Workspace    string
	CapsuleDir   string
	SessionsRoot string
	Source       StateSource
}

// Delta is the growth verdict the probe derives from the persisted state
// file: the state path, the previous state it read, the next state it
// will persist, and whether the transcript grew.
type Delta struct {
	StatePath string
	Prev      State
	Next      State
	Grew      bool
}

// Assemble builds the snapshot (summary fields untouched) and the delta
// verdict: it reads the execution state through the source, resolves the
// live transcript through the SHARED locator, fills the transcript
// fields (bounded tail, size, climbing age) and compares against the
// persisted delta state — read before write, so a crash-then-rearm
// re-derives growth from the file alone.
func Assemble(deps Deps) (*Snapshot, Delta, error) {
	if deps.Source == nil {
		return nil, Delta{}, fmt.Errorf("watch: no execution-state source wired")
	}
	es, err := deps.Source.ObserveExecState(deps.WI)
	if err != nil {
		return nil, Delta{}, fmt.Errorf("watch: %s: observe the execution state: %w", deps.WI, err)
	}
	now := time.Now()
	snap := &Snapshot{
		WI:             deps.WI,
		Step:           es.Step,
		StepStatus:     es.StepStatus,
		LatestEvent:    es.LatestEvent,
		Pending:        es.Pending,
		Timestamp:      now.UTC().Format(time.RFC3339),
		TranscriptTail: []string{},
		Status:         es.Status,
	}
	statePath := StatePath(deps.Workspace, deps.WI)
	prev, err := LoadState(statePath)
	if err != nil {
		return nil, Delta{}, err
	}
	tpath := LocateTranscript(deps.SessionsRoot, deps.CapsuleDir)
	fillTranscript(snap, tpath, now)
	next := State{TranscriptPath: tpath, Size: snap.TranscriptSize, UpdatedAt: snap.Timestamp}
	// Growth: the transcript exists, and it is either a different file
	// than the state recorded or bigger than the recorded size. A
	// missing transcript never counts as growth.
	grew := snap.TranscriptSize > 0 &&
		(tpath != prev.TranscriptPath || snap.TranscriptSize > prev.Size)
	return snap, Delta{StatePath: statePath, Prev: prev, Next: next, Grew: grew}, nil
}

// fillTranscript fills the snapshot's transcript fields. An unresolvable
// transcript leaves the path empty, the size 0 and the age "-" — the
// snapshot still renders (the wake channel must never depend on the
// transcript being found).
func fillTranscript(snap *Snapshot, path string, now time.Time) {
	if path == "" {
		snap.TranscriptAge = "-"
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		snap.TranscriptAge = "-"
		return
	}
	snap.TranscriptPath = path
	snap.TranscriptSize = info.Size()
	if recs, err := sessionjsonl.Tail(path, TranscriptTailRecords); err == nil {
		total := 0
		for _, r := range recs {
			snap.TranscriptTail = append(snap.TranscriptTail, r.Raw)
			total += len(r.Raw) + 1
			for total > TranscriptTailBytes && len(snap.TranscriptTail) > 1 {
				total -= len(snap.TranscriptTail[0]) + 1
				snap.TranscriptTail = snap.TranscriptTail[1:]
			}
		}
	}
	age := now.Sub(info.ModTime())
	if age < 0 {
		age = 0
	}
	snap.TranscriptAge = FormatDuration(age)
}

// FormatDuration renders a duration the way the watch lane speaks it:
// whole minutes "<n>m", whole seconds "<n>s", otherwise the
// seconds-truncated Go spelling. Zero renders "0s".
func FormatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	default:
		return d.Truncate(time.Second).String()
	}
}
