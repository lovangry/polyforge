package observe

// answerfile.go: the ON-DISK shape of the answer ledger — the delivery
// status cache that lets an unconsumed owner answer survive a daemon
// restart (spec A1/A2/A3/A4).
//
// AUTHORITY SPLIT (unchanged): the capsule's runtime-state/execution.json
// stays the persistent authority for the open pending question; this file
// only remembers WHAT THE DAEMON DID with the answers it accepted —
// which relays were delivered but never consumed (the F9 re-delivery
// input) and which answer_ids already settled (the idempotency replay
// input). Losing it costs at worst one owner re-submission, so no load
// failure may ever block daemon startup.
//
// SETTLED STATES ONLY. answerInflight is a memory-only reservation and
// has NO on-disk encoding: the serializer projects an in-flight entry to
// `retryable` + its `awaiting` relay, which is exactly the state a crash
// mid-delivery should reload as (the answer is not lost, and the next
// attachment re-delivers it). No load path can therefore produce an
// entry that makes a later submission return ErrAnswerInFlight.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// answerFileName is the single daemon-wide ledger file, kept directly
// under observe Config.DataDir next to events.log (spec A1).
const answerFileName = "answers.json"

// answerFileVersion is the integer document version (spec A2). A file
// carrying any other value is quarantined, never migrated: no prior
// on-disk format exists, and the field is here so a future one can.
const answerFileVersion = 1

// answerFileMode: answer_text is owner-authored content (spec A2).
const answerFileMode os.FileMode = 0o600

// appliedMaxAge is the load-time age-out bound for settled `applied`
// entries that are no longer awaiting (spec A4).
const appliedMaxAge = 14 * 24 * time.Hour

// On-disk state names. The enum deliberately admits only the SETTLED
// states — there is no encoding for answerInflight.
const (
	stateNameFinal     = "final"
	stateNameRetryable = "retryable"
)

// On-disk error identity: a closed code set rather than a message, so a
// replayed outcome compares Is-equal to the sentinel it was settled
// with. `other` carries its message because only `final` entries replay
// their outcome and a non-sentinel delivery error never settles final.
const (
	errCodeNone       = ""
	errCodeConsumed   = "consumed"
	errCodeAckPending = "ack_pending"
	errCodeIdentity   = "identity"
	errCodeOther      = "other"
)

// answerFileEntry is one persisted `applied` idempotency outcome.
type answerFileEntry struct {
	State        string `json:"state"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// WorkItem is the owning work item, so a finished work item's
	// entries can be dropped wholesale (spec A4).
	WorkItem string `json:"work_item,omitempty"`
	// SettledAt is the RFC3339Nano UTC moment the entry reached this
	// state — the age-out input.
	SettledAt string `json:"settled_at,omitempty"`
}

// answerFileDoc is the whole version-1 document: BOTH halves of the
// ledger (spec: persisting only one half turns every restart into an
// idempotency hole).
type answerFileDoc struct {
	Version int `json:"version"`
	// Awaiting holds the delivered-but-unconsumed relays per WORK ITEM,
	// verbatim (all five AnswerRelay fields).
	Awaiting map[string]map[string]AnswerRelay `json:"awaiting"`
	// Applied holds the settled outcome per answer_id.
	Applied map[string]answerFileEntry `json:"applied"`
}

func newAnswerFileDoc() answerFileDoc {
	return answerFileDoc{
		Version:  answerFileVersion,
		Awaiting: map[string]map[string]AnswerRelay{},
		Applied:  map[string]answerFileEntry{},
	}
}

// normalize fills the nil maps a hand-written or minimal document may
// decode into, so every caller can write to them unconditionally.
func (d *answerFileDoc) normalize() {
	if d.Awaiting == nil {
		d.Awaiting = map[string]map[string]AnswerRelay{}
	}
	if d.Applied == nil {
		d.Applied = map[string]answerFileEntry{}
	}
	for wi, m := range d.Awaiting {
		if m == nil {
			d.Awaiting[wi] = map[string]AnswerRelay{}
		}
	}
}

// isAwaiting reports whether answerID sits in any work item's awaiting
// half (the "never prune a live awaiting entry" guard).
func (d answerFileDoc) isAwaiting(answerID string) bool {
	for _, m := range d.Awaiting {
		if _, ok := m[answerID]; ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// state / error codecs

// encodeAnswerState projects an in-memory state onto the on-disk enum.
// answerInflight has no encoding of its own: it is projected to
// retryable, which is precisely how a crash mid-delivery must reload.
func encodeAnswerState(s answerState) string {
	if s == answerFinal {
		return stateNameFinal
	}
	return stateNameRetryable
}

// decodeAnswerState maps the on-disk enum back. Anything that is not
// the literal "final" loads as retryable — never as in-flight.
func decodeAnswerState(name string) answerState {
	if name == stateNameFinal {
		return answerFinal
	}
	return answerRetryable
}

// encodeAnswerErr records the error IDENTITY as a closed code set.
func encodeAnswerErr(err error) (code, message string) {
	switch {
	case err == nil:
		return errCodeNone, ""
	case errors.Is(err, ErrAnswerConsumed):
		return errCodeConsumed, ""
	case errors.Is(err, ErrAnswerAckPending):
		return errCodeAckPending, ""
	case errors.Is(err, ErrAnswerIdentity):
		return errCodeIdentity, ""
	default:
		return errCodeOther, err.Error()
	}
}

// decodeAnswerErr rebuilds the sentinel a replay must return verbatim.
func decodeAnswerErr(code, message string) error {
	switch code {
	case errCodeNone:
		return nil
	case errCodeConsumed:
		return ErrAnswerConsumed
	case errCodeAckPending:
		return ErrAnswerAckPending
	case errCodeIdentity:
		return ErrAnswerIdentity
	case errCodeOther:
		if message == "" {
			return errors.New("observe: answer delivery failed (recorded before restart)")
		}
		return errors.New(message)
	default:
		// An unknown code from a future writer: keep the entry usable
		// rather than crash — the message (if any) still names it.
		if message == "" {
			return errors.New("observe: answer settled with an unrecognized recorded outcome: " + code)
		}
		return errors.New(message)
	}
}

// ---------------------------------------------------------------------------
// atomic write

// answerFilePath is <dataDir>/answers.json.
func answerFilePath(dataDir string) string { return filepath.Join(dataDir, answerFileName) }

// atomicWriteFile lands data at path via tmp+fsync+rename — the same
// contract internal/polyforge/runtime/state.go's atomicWrite uses (that
// one is unexported in its own package; this is the observe-side copy,
// deliberately not a cross-package export). A crash at any instant
// leaves either the previous file or the new one, never a torn one.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// writeAnswerFile persists the document atomically at mode 0600.
func writeAnswerFile(dataDir string, doc answerFileDoc) error {
	if dataDir == "" {
		return errors.New("observe: answer ledger has no data directory")
	}
	doc.Version = answerFileVersion
	doc.normalize()
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return atomicWriteFile(answerFilePath(dataDir), raw, answerFileMode)
}

// ---------------------------------------------------------------------------
// load + quarantine + prune

// ledgerRecorder is the one-event callback the loader uses to report a
// quarantine or a failed write. The hub binds it to the events log; a
// nil recorder makes the reporting a no-op (unit tests, memory-only
// ledgers).
type ledgerRecorder func(kind string, body map[string]any)

func (r ledgerRecorder) record(body map[string]any) {
	if r == nil {
		return
	}
	r(EvAnswerLedger, body)
}

// loadAnswerFile reads <dataDir>/answers.json (spec A3).
//
//   - missing file            → an empty document (first boot);
//   - unreadable / unparsable / unknown version → the file is moved
//     aside to answers.json.corrupt-<RFC3339 UTC>, ONE event is
//     recorded, and an EMPTY document is returned.
//
// It NEVER returns a fatal error: the daemon must start. pruned reports
// whether the load-time age-out dropped anything, so the caller can
// rewrite the trimmed file.
func loadAnswerFile(dataDir string, rec ledgerRecorder, now time.Time) (doc answerFileDoc, pruned bool) {
	if dataDir == "" {
		return newAnswerFileDoc(), false
	}
	path := answerFilePath(dataDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			quarantineAnswerFile(path, rec, "unreadable: "+err.Error())
		}
		return newAnswerFileDoc(), false
	}
	var parsed answerFileDoc
	if err := json.Unmarshal(raw, &parsed); err != nil {
		quarantineAnswerFile(path, rec, "unparsable: "+err.Error())
		return newAnswerFileDoc(), false
	}
	if parsed.Version != answerFileVersion {
		quarantineAnswerFile(path, rec,
			"unsupported version: "+strconv.Itoa(parsed.Version))
		return newAnswerFileDoc(), false
	}
	parsed.normalize()
	pruned = pruneAnswerDoc(&parsed, now)
	return parsed, pruned
}

// quarantineAnswerFile moves a bad file aside and records ONE event.
// A rename failure is itself non-fatal (the caller starts empty either
// way); the event then names the rename error too.
func quarantineAnswerFile(path string, rec ledgerRecorder, reason string) {
	dst := path + ".corrupt-" + time.Now().UTC().Format(time.RFC3339)
	body := map[string]any{
		"detail": "quarantine",
		"reason": reason,
		"path":   path,
		"moved":  dst,
	}
	if err := os.Rename(path, dst); err != nil {
		body["error"] = err.Error()
		delete(body, "moved")
	}
	rec.record(body)
}

// pruneAnswerDoc applies the A4 retention rule at load: an `applied`
// entry settled more than appliedMaxAge ago AND not present in the
// awaiting half is dropped. A live awaiting entry is NEVER pruned, at
// any age — those are exactly the unconsumed owner answers this whole
// file exists to protect. Entries without a parsable settle time are
// kept (an unknown age is not an old age).
func pruneAnswerDoc(doc *answerFileDoc, now time.Time) bool {
	cutoff := now.Add(-appliedMaxAge)
	changed := false
	for id, e := range doc.Applied {
		if doc.isAwaiting(id) {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, e.SettledAt)
		if err != nil {
			continue
		}
		if ts.Before(cutoff) {
			delete(doc.Applied, id)
			changed = true
		}
	}
	return changed
}
