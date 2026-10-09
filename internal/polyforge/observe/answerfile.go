package observe

// answerfile.go: the ON-DISK shape of the answer ledger — the delivery
// status cache that let an unconsumed owner answer survive a daemon
// restart (spec A1/A2/A3/A4).
//
// polyforge#106 trim: the legacy WI-Agent answer relay is deleted (the
// driver consumes answers, and the capsule is the authority), so the
// cache no longer carries an awaiting half and no longer admits an
// in-flight or retryable state — every record is history from a previous
// daemon process. The file machinery itself stays exactly as pinned:
// atomic writes at mode 0600, a versioned document, load-time quarantine
// of unreadable/unparsable/unknown-version files (one event, never
// fatal), and the A4 age-out for settled entries.
//
// AUTHORITY SPLIT (unchanged): the capsule's runtime-state/execution.json
// stays the persistent authority for the open pending question and the
// archived answers; this file only remembers what a previous daemon
// process recorded. Losing it costs nothing the capsule does not
// already hold, so no load failure may ever block daemon startup.

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
// carrying any other value is quarantined, never migrated: the version
// bump accompanies the polyforge#106 trim (the awaiting half and the
// two-phase states are gone), and an older document is history, not a
// format to resurrect.
const answerFileVersion = 2

// answerFileMode: answer metadata only now (no answer_text has been
// stored since the relay trim), but the file stays owner-data adjacent —
// keep the restrictive mode (spec A2).
const answerFileMode os.FileMode = 0o600

// appliedMaxAge is the load-time age-out bound for settled `applied`
// entries (spec A4).
const appliedMaxAge = 14 * 24 * time.Hour

// On-disk state names. The enum admits only the settled state — the
// in-flight and retryable encodings died with the relay.
const stateNameFinal = "final"

// On-disk error identity: a closed code set rather than a message, so a
// decoded record compares equal to the class it was recorded with.
// `ack_pending` and the other pre-trim codes still DECODE (an older
// file's history stays readable) but nothing re-delivers anything.
const (
	errCodeNone       = ""
	errCodeConsumed   = "consumed"
	errCodeAckPending = "ack_pending"
	errCodeIdentity   = "identity"
	errCodeOther      = "other"
)

// answerFileEntry is one persisted `applied` record.
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

// answerFileDoc is the whole version-2 document: the settled records
// the previous daemon processes wrote (the awaiting half and the
// in-flight projection are retired with the relay).
type answerFileDoc struct {
	Version int `json:"version"`
	// Applied holds the settled record per answer_id.
	Applied map[string]answerFileEntry `json:"applied"`
}

func newAnswerFileDoc() answerFileDoc {
	return answerFileDoc{
		Version: answerFileVersion,
		Applied: map[string]answerFileEntry{},
	}
}

// normalize fills the nil map a hand-written or minimal document may
// decode into, so every caller can write to it unconditionally.
func (d *answerFileDoc) normalize() {
	if d.Applied == nil {
		d.Applied = map[string]answerFileEntry{}
	}
}

// ---------------------------------------------------------------------------
// state / error codecs

// encodeAnswerState projects the in-memory record onto the on-disk
// enum: every record is settled history now.
func encodeAnswerState() string {
	return stateNameFinal
}

// decodeAnswerState maps the on-disk enum back. Anything that is not
// the literal "final" still loads as a settled record (pre-trim files
// carry retryable/in-flight-encoded entries; they are history).
func decodeAnswerState(name string) bool {
	return true
}

// encodeAnswerErr records the error IDENTITY as a closed code set.
func encodeAnswerErr(err error) (code, message string) {
	switch {
	case err == nil:
		return errCodeNone, ""
	case errors.Is(err, ErrAnswerConsumed):
		return errCodeConsumed, ""
	case errors.Is(err, ErrAnswerIdentity):
		return errCodeIdentity, ""
	default:
		return errCodeOther, err.Error()
	}
}

// decodeAnswerErr rebuilds the recorded error class. The pre-trim
// ack_pending code decodes to a plain delivered-but-unconsumed note —
// nothing acts on it, but the history stays honest.
func decodeAnswerErr(code, message string) error {
	switch code {
	case errCodeNone:
		return nil
	case errCodeConsumed:
		return ErrAnswerConsumed
	case errCodeAckPending:
		return errors.New("observe: recorded pre-trim as delivered, consumption unconfirmed (relay since deleted)")
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

// pruneAnswerDoc applies the A4 retention rule at load: a settled entry
// older than appliedMaxAge is dropped. Entries without a parsable
// settle time are kept (an unknown age is not an old age).
func pruneAnswerDoc(doc *answerFileDoc, now time.Time) bool {
	cutoff := now.Add(-appliedMaxAge)
	changed := false
	for id, e := range doc.Applied {
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
