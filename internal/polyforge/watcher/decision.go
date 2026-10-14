// Package watcher is the Go implementation of `pf watch` (v2-converge §3):
// the resident per-chat watcher, the one-shot WI snapshot and the fixed
// decision template. It never calls a model and never forwards text written
// by a step agent into a wake-up line.
package watcher

import (
	"fmt"
	"sort"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Rule classifies one decision-worthy kind. Class is the CLOSED, printed
// name of the decision; Phrase is a fixed sentence. Neither ever carries
// text from an event body or from execution.json.
type Rule struct {
	Class  string
	Phrase string
	// StateBacked rules are decided from the authoritative state
	// (execution.json / job bookkeeping) on every poll, never from the
	// event that announces them: the event is only a hint, ignoring it
	// cannot lose the decision (state persists until acted on) and it
	// cannot report a stale one (a question answered since).
	StateBacked bool
}

// THE DECISION TABLE. Event kinds are open strings the daemon and driver
// may add at any time; a kind absent here is simply not decision-worthy.
// To make a new kind wake the human, add it to eventRules (and, if it is a
// new workflow_status, to statusRules). Nothing else needs to change.
var eventRules = map[string]Rule{
	// a step waits for a human answer / gate
	"needs_input": {Class: "needs_input", Phrase: "a step is waiting for a human answer", StateBacked: true},
	// workflow terminal (the outcome comes from workflow_status, below)
	"terminal": {Class: "terminal", Phrase: "the workflow reached an end state", StateBacked: true},
	// a step was judged dead / the model candidates ran out
	"step_dead":            {Class: "step_dead", Phrase: "a step was judged dead and the daemon handled it"},
	"candidates_exhausted": {Class: "candidates_exhausted", Phrase: "all model candidates are exhausted"},
	// daemon-side submit and restart signals
	"submit_rejected":  {Class: "submit_rejected", Phrase: "a submit was accepted but did not start"},
	"submit_dropped":   {Class: "submit_dropped", Phrase: "a queued submit was dropped"},
	"daemon_restarted": {Class: "daemon_restarted", Phrase: "the daemon restarted"},
}

// statusRules maps a status to its terminal decision: the driver's
// execution.json workflow_status (completed, failed_gate,
// failed_rewind_exhausted, blocked) and the aihub status the daemon
// reports once the job is no longer local (wrapped, failed, canceled,
// paused, blocked — the driver never writes those four words to
// execution.json). Final statuses end the subscription; blocked and
// paused do not (the human can continue the work item).
var statusRules = map[string]struct {
	Rule
	Final bool
}{
	// execution.json workflow_status
	"completed":               {Rule{Class: "completed", Phrase: "the workflow completed"}, true},
	"failed_gate":             {Rule{Class: "failed", Phrase: "the workflow failed"}, true},
	"failed_rewind_exhausted": {Rule{Class: "failed", Phrase: "the workflow failed"}, true},
	"blocked":                 {Rule{Class: "blocked", Phrase: "the workflow is blocked and needs a human"}, false},
	// aihub status after a manual close (execution.json is not rewritten
	// by it): wrapped / failed / canceled are final, paused can be resumed.
	"wrapped":  {Rule{Class: "completed", Phrase: "the workflow completed"}, true},
	"failed":   {Rule{Class: "failed", Phrase: "the workflow failed"}, true},
	"canceled": {Rule{Class: "canceled", Phrase: "the work item was canceled"}, true},
	"paused":   {Rule{Class: "paused", Phrase: "the work item was paused"}, false},
}

// Decision is one delivered human decision.
type Decision struct {
	WI string
	// Slug is the WI's display name from the snapshot (the daemon's job
	// record or the aihub record, never agent text); the line shows it,
	// falling back to WI when empty.
	Slug   string
	Class  string
	Phrase string
	// QuestionID is the pending question id of a needs_input decision
	// (D11): the wake line carries it — charset-filtered, the same
	// filter as WI ids — so the owner can answer without free text
	// ever leaving the question. Empty for every other class.
	QuestionID string
	// Key identifies a state-derived decision for dedupe ("" for event
	// decisions, which the cursor dedupes).
	Key string
}

// suffix is the fixed tail of every wake-up line (#122: the agent must not
// answer for the human).
const suffix = "this is a human decision, the agent must not answer it"

// Line renders the decision from the FIXED template. The WI is named by
// its slug ("<project>#<seq>", the canonical id when no slug is known),
// reduced to a safe charset; a needs_input decision also carries the
// pending question id (D11), through the same filter; nothing else varies.
func (d Decision) Line() string {
	qid := ""
	if d.QuestionID != "" {
		qid = " " + safeID(d.QuestionID)
	}
	return fmt.Sprintf("[polyforge] %s %s%s — %s; %s", safeID(api.DisplayName(d.Slug, d.WI)), d.Class, qid, d.Phrase, suffix)
}

// AttentionLine is the fixed-template notice for WIs that need a human but
// had no live watcher; wis are their display names (slug, else id).
func AttentionLine(wis []string) string {
	ids := make([]string, 0, len(wis))
	for _, w := range wis {
		ids = append(ids, safeID(w))
	}
	sort.Strings(ids)
	shown := ids
	more := ""
	if len(shown) > 5 {
		shown, more = ids[:5], fmt.Sprintf(" (+%d more)", len(ids)-5)
	}
	noun := "WIs need"
	if len(ids) == 1 {
		noun = "WI needs"
	}
	return fmt.Sprintf("[polyforge] %d %s you: %s%s — pending human decisions; %s", len(ids), noun, strings.Join(shown, ", "), more, suffix)
}

// safeID keeps only [A-Za-z0-9._:#-] so an id or slug can never smuggle
// text.
func safeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == ':', r == '#':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 80 {
			break
		}
	}
	return b.String()
}

// Result is the outcome of evaluating one subscription against a snapshot.
type Result struct {
	Decisions []Decision
	// Cursor is the event cursor to ack.
	Cursor int64
	// Notified is the full set of state keys to ack (still-true ones only,
	// so a recurrence — blocked again, a new question — fires again).
	Notified []string
	// Done: the WI reached a final state; stop polling it.
	Done bool
}

// Evaluate reconciles the AUTHORITATIVE state first and only then replays
// the events after the cursor: a WI that finished while the watcher was
// down is reported from execution.json whatever the event ring still holds.
func Evaluate(sub protocol.ChatSub, snap *protocol.WatchSnapshot) Result {
	res := Result{Cursor: sub.Cursor}
	have := map[string]bool{}
	for _, k := range sub.Notified {
		have[k] = true
	}
	cur := map[string]bool{}
	fire := func(key string, r Rule) {
		cur[key] = true
		if !have[key] {
			res.Decisions = append(res.Decisions, Decision{WI: snap.WorkItem, Slug: snap.Slug, Class: r.Class, Phrase: r.Phrase, Key: key})
		}
	}

	// 1. state: execution.json authority, then the job bookkeeping.
	pendingID := ""
	if ex := snap.Execution; ex != nil {
		if q := ex.PendingQuestion; q != nil && q.ID != "" {
			pendingID = q.ID
		}
		if sr, ok := statusRules[ex.WorkflowStatus]; ok {
			fire(sr.Class, sr.Rule)
			res.Done = sr.Final
		}
	}
	// A manual close leaves execution.json at `running`: when the daemon
	// holds no live job, the hub status (wrapped / failed / canceled /
	// paused / blocked) decides, and overrides a stale execution.json.
	if sr, ok := statusRules[snap.HubStatus]; ok && snap.HubStatus != "" {
		fire(sr.Class, sr.Rule)
		res.Done = sr.Final
	}
	if j := snap.Job; j != nil {
		if pendingID == "" && j.QuestionID != "" && (j.Status == protocol.JobNeedsInput) {
			pendingID = j.QuestionID
		}
	}
	if pendingID != "" {
		r := eventRules["needs_input"]
		key := "needs_input:" + pendingID
		cur[key] = true
		if !have[key] {
			res.Decisions = append(res.Decisions, Decision{
				WI: snap.WorkItem, Slug: snap.Slug, Class: r.Class, Phrase: r.Phrase,
				QuestionID: pendingID, Key: key,
			})
		}
	}

	// 2. events after the cursor, in seq order; state-backed kinds are
	// skipped (decided above), unknown kinds are not decisions.
	for _, e := range snap.Events {
		if e.Seq > res.Cursor {
			res.Cursor = e.Seq
		}
		if e.Seq <= sub.Cursor {
			continue
		}
		r, ok := eventRules[e.Kind]
		if !ok || r.StateBacked {
			continue
		}
		res.Decisions = append(res.Decisions, Decision{WI: snap.WorkItem, Slug: snap.Slug, Class: r.Class, Phrase: r.Phrase})
	}

	for k := range cur {
		res.Notified = append(res.Notified, k)
	}
	sort.Strings(res.Notified)
	return res
}
