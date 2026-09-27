package capsule

// session.go: harness session directories (03 §4.4). Two data classes with
// different rules:
//
//   - session.json: stable session identity metadata, small, MAY sync (as
//     history only, never an identity-restoration basis).
//   - session-data/: opaque local harness working dir, NEVER synced, never
//     rebuilt from a Revision (a new session always starts empty).
//
// Session identity rules (03 §4.4, the ONLY reuse case): the same machine
// AND the same not-yet-ended generation, on WI-Agent crash re-derivation.
// Every generation boundary (pause→resume, takeover) or machine change →
// brand-new session id.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Adapters (04 §6.1a).
const (
	AdapterPi     = "pi"
	AdapterClaude = "claude"
)

// SessionIdentity is the harness/<adapter>/session.json content.
type SessionIdentity struct {
	SessionID string `json:"session_id"`
	Adapter   string `json:"adapter"`
	CreatedAt string `json:"created_at"`
}

// HarnessDir returns <capsule>/harness/<adapter>.
func (c *Capsule) HarnessDir(adapter string) string {
	return filepath.Join(c.Dir, "harness", adapter)
}

// SessionDataDir returns the harness-local session working directory
// (never synced).
func (c *Capsule) SessionDataDir(adapter string) string {
	return filepath.Join(c.HarnessDir(adapter), "session-data")
}

// EnsureHarnessDir creates the empty harness/<adapter>/ tree (03 §6.1
// step 5: created empty for a new generation, never restored from remote,
// never pre-populated).
func (c *Capsule) EnsureHarnessDir(adapter string) error {
	return os.MkdirAll(c.SessionDataDir(adapter), 0o755)
}

// SessionIDPath returns the session.json location.
func (c *Capsule) SessionIDPath(adapter string) string {
	return filepath.Join(c.HarnessDir(adapter), "session.json")
}

// NewSessionID mints a session id for a fresh generation
// (04 §6.1a: sess_<wi>_<generation>).
func NewSessionID(wiID string, generation int64) string {
	return "sess_" + wiID + "_" + itoa(generation)
}

// WriteSessionIdentity persists session.json once, on first harness start
// of this capsule instance (03 §4.4).
func (c *Capsule) WriteSessionIdentity(adapter, sessionID string) error {
	idn := SessionIdentity{
		SessionID: sessionID,
		Adapter:   adapter,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	blob, _ := json.MarshalIndent(idn, "", "  ")
	return AtomicWrite(c.SessionIDPath(adapter), blob, 0o644)
}

// ReadSessionIdentity reads the LOCAL session.json — the ONLY identity
// source any resume path may read (a synced historical copy is audit
// material only, never an identity basis).
func (c *Capsule) ReadSessionIdentity(adapter string) (*SessionIdentity, error) {
	raw, err := os.ReadFile(c.SessionIDPath(adapter))
	if err != nil {
		return nil, err
	}
	var idn SessionIdentity
	if err := json.Unmarshal(raw, &idn); err != nil {
		return nil, err
	}
	return &idn, nil
}

// ArchiveTranscript copies this generation's non-empty session JSONL
// transcripts from harness/<adapter>/session-data/ into the sync surface at
// logs/session/<generation>.jsonl (03 §4.4 A14 / 05 §6.1 step 1: the archive
// is written BEFORE the final-flush freeze scan, so it rides the last
// Revision of the generation in ONE CAS block instead of being re-uploaded
// at every boundary).
//
// A generation that never produced session content (no harness started, or
// only an empty transcript) produces NO archive path — this is what keeps
// the empty-chain flush-evidence branch (EMPTY_SYNC_BYTES_DIGEST) sound.
//
// Resolution note (design ambiguity, recorded): the doc speaks of "the"
// session JSONL; a harness may keep several transcript files in
// session-data/. We concatenate every non-empty *.jsonl in name order into
// the single fixed archive path — one archive per generation either way.
func (c *Capsule) ArchiveTranscript(adapter string, generation int64) (bool, error) {
	dataDir := c.SessionDataDir(adapter)
	entries, err := filepath.Glob(filepath.Join(dataDir, "*.jsonl"))
	if err != nil {
		return false, err
	}
	sort.Strings(entries)
	var total []byte
	for _, p := range entries {
		b, rerr := os.ReadFile(p)
		if rerr != nil || len(b) == 0 {
			continue
		}
		total = append(total, b...)
		if len(total) > 0 && total[len(total)-1] != '\n' {
			total = append(total, '\n')
		}
	}
	if len(total) == 0 {
		return false, nil // nothing archived: no session content this generation
	}
	dst := filepath.Join(c.Dir, "logs", "session", itoa(generation)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	if err := AtomicWrite(dst, total, 0o444); err != nil {
		return false, err
	}
	return true, nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
