package daemon

// sessioncollect.go: the claude adapter's terminal-state session
// collection (03 §4.4 claude amendment — the authoritative layout — and
// 04 §6.2 A step 2, which runs it BEFORE the final flush's frozen scan).
//
// The cross-harness seal contract (00 §3.6a) is that the SEALED capsule
// contains the session record. Runtime placement differs per adapter:
//
//   - pi supports a custom session path, so its harness writes the
//     transcript directly into the capsule-local
//     harness/<pi>/session-data/ — nothing to collect, ZERO change on
//     the pi close path (pinned by TestPiCloseNoCollection);
//   - claude supports NO custom session path: the conversation lives at
//     the harness's DEFAULT session location (this machine, OUTSIDE the
//     capsule) for the whole run — every mid-run session-data read routes
//     there, never to the capsule. (The daemon itself has NO mid-run
//     session-data read path — observe reads runtime-state/execution.json,
//     the proxy reads Core — so this routing statement is about the one
//     read that does exist: this seal-time collection.) At end-generation
//     the daemon copies the conversation JSONL READ-ONLY into the
//     capsule's harness/<claude>/session-data/, and the shared archive
//     step (capsule.ArchiveTranscript) then mirrors it to
//     logs/session/<generation>.jsonl for BOTH adapters.
//
// Locating the source (05 §5.2 / adapter-verification notes,
// supervisor.go BuildCmd): the conversation UUID is the deterministic
// uuid5 derived from the harness session id — mainagent.ClaudeSessionUUID,
// the ONE mapping, never a second one — and the real CLI keeps its
// sessions under CLAUDE_CONFIG_DIR (default ~/.claude), keyed by cwd +
// id. The EXACT subdirectory layout under that root is CLI-version
// specific and was NOT live-measured, so this code pins NO layout as
// gospel: it resolves the store ROOT (configurable, see
// claudeSessionStoreRoot) and searches it for the file named
// <conversation-uuid>.jsonl. A CLI storage-layout change (or a test
// fake) therefore needs at most a root flag change; any subdirectory
// naming is already tolerated.
//
// Failure contract (04 §5.2 DAEMON-513): this generation DID derive a
// harness session (the local harness/<claude>/session.json exists) but
// the default session location has no JSONL for the conversation → the
// typed SESSION_JSONL_MISSING error fails THIS close: no archive is
// produced, nothing is skipped silently, the generation is NOT ended
// server-side and the rest of the seal sequence stays intact (a retry
// after restoring the source completes normally). A generation that
// never derived a harness session (no session.json) or produced an empty
// transcript produces no collection, no archive path and NO error — the
// same empty branch that keeps the empty-chain flush evidence sound.

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/silgrid/polyforge/internal/capsule"
)

// ClaudeSessionUUID derives the claude conversation UUID from the
// polyforge harness session id (uuid5, fixed namespace). The real CLI
// demands a UUID for --session-id/--resume while §3.6a mints
// "sess_<wi>_<generation>"; the map is DETERMINISTIC (RFC 4122
// name-based UUIDv5 under a fixed namespace), so the same harness
// session id always maps to the same claude conversation id and the
// crash-continuation reuse (03 §4.4: same id, same machine, same
// generation) re-derives exactly the conversation to resume — no extra
// bookkeeping, no stored harness id. (Measured live by the adapter
// verification: the CLI accepts and persists a name-derived uuid5 as
// the conversation id, and --resume continues it after a real turn.)
//
// It lives HERE, in the daemon package, because import direction forces
// the shared home (mainagent imports daemon, never the reverse) and
// because there is exactly ONE harness-session-id → conversation-UUID
// mapping in the codebase, never a second one: the mainagent
// ClaudeAdapter pins it in the spawn argv, and the daemon's seal-time
// terminal-state collection (below) resolves the SAME conversation id
// to locate the JSONL at the claude harness's default session location.
func ClaudeSessionUUID(sessionID string) string {
	h := sha1.New()
	h.Write(claudeUUIDNS[:])
	h.Write([]byte("polyforge/mainagent/claude-session/" + sessionID))
	var b [16]byte
	copy(b[:], h.Sum(nil)[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5 (name-based, SHA-1)
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// claudeUUIDNS is the RFC 4122 URL namespace, the fixed UUIDv5 namespace
// ClaudeSessionUUID hashes under.
var claudeUUIDNS = [16]byte{
	0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// ErrSessionJSONLMissing is the DAEMON-513 SESSION_JSONL_MISSING shape
// (04 §5.2): the claude adapter's terminal-state collection found the
// generation's session identity (session.json) but no conversation JSONL
// at the claude harness's default session location. The close that hits
// it fails typed; it never silently passes.
var ErrSessionJSONLMissing = errors.New("daemon: session JSONL missing (DAEMON-513 SESSION_JSONL_MISSING)")

// claudeSessionStoreRoot resolves the claude harness's default session
// store root, in precedence order:
//
//  1. Config.ClaudeSessionStoreRoot (the --claude-session-root flag /
//     POLYFORGE_CLAUDE_SESSION_ROOT env, wired in cmd/pf/daemon.go) —
//     a CLI storage-layout change needs at most this flag change;
//  2. CLAUDE_CONFIG_DIR, the real CLI's own relocation variable
//     (the adapter-verification measurement: conversations live under
//     CLAUDE_CONFIG_DIR);
//  3. $HOME/.claude, the CLI's measured default.
func (d *Daemon) claudeSessionStoreRoot() string {
	if d.cfg.ClaudeSessionStoreRoot != "" {
		return d.cfg.ClaudeSessionStoreRoot
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".claude")
	}
	return ".claude"
}

// collectTerminalSession performs the adapter-routed seal-time step of
// 04 §6.2 A step 2, BEFORE the final flush's frozen scan (the caller is
// the FinalFlush Archive hook, which runs pre-scan): for a claude job it
// copies this generation's conversation JSONL READ-ONLY from the claude
// harness's default session location into the capsule-local
// harness/<claude>/session-data/, where the shared archive step then
// finds it. Every other adapter is a strict no-op — the pi path is
// ZERO-change by contract (its transcript is capsule-local already).
//
// The copy never writes at, renames or truncates the source: the source
// location is only ever opened for reading (pinned by
// TestClaudeSealCollectsSessionJSONL's byte/mode/mtime identity check).
func (d *Daemon) collectTerminalSession(jr *jobRuntime) error {
	if jr.adapter != capsule.AdapterClaude {
		// pi (and any future capsule-local adapter): the transcript is
		// already in the capsule; there is no external location to
		// collect from and nothing here may touch the capsule.
		return nil
	}
	// The DAEMON-513 gate (03 §4.4): the typed failure applies only when
	// THIS generation actually derived a harness session. session.json is
	// written once at spawn time (04 §6.1 step 7) and never removed, so
	// its absence means "no session this generation" — no collection, no
	// archive, no error.
	if _, err := os.Stat(jr.cap.SessionIDPath(jr.adapter)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	uuid := ClaudeSessionUUID(jr.sessionID)
	root := d.claudeSessionStoreRoot()
	src, err := findSessionJSONL(root, uuid)
	if err != nil {
		return fmt.Errorf("%w: session %s: looked for %s.jsonl under %s", ErrSessionJSONLMissing, jr.sessionID, uuid, root)
	}
	// READ-ONLY collection: open the source for reading only, never for
	// writing — the source location is the CLI's own store.
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("daemon: reading claude session JSONL %s: %w", src, err)
	}
	if len(data) == 0 {
		// Empty transcript (03 §4.4): no archive path, no error — the
		// archive step's empty branch stays sound.
		return nil
	}
	dst := filepath.Join(jr.cap.SessionDataDir(jr.adapter), uuid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// 0o444: the collection copy is read-only material inside the capsule;
	// it is the daemon's ONLY write into harness/<adapter>/session-data/
	// (03 §4.4, the recorded exception to "only the harness reads/writes
	// it"), performed after the generation's harness process has ended.
	return os.WriteFile(dst, data, 0o444)
}

// findSessionJSONL locates the conversation JSONL under the store root.
// The real CLI's subdirectory naming (cwd-keyed project dirs) is
// version-specific and unmeasured, so the search is layout-agnostic: any
// regular file NAMED <uuid>.jsonl at any depth under root matches. No
// match — including a missing root — is the DAEMON-513 shape.
func findSessionJSONL(root, uuid string) (string, error) {
	want := uuid + ".jsonl"
	found := ""
	werr := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err // unreadable root/subtree: report, never guess
		}
		if de.Type().IsRegular() && de.Name() == want {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if werr != nil && !errors.Is(werr, fs.ErrNotExist) && !errors.Is(werr, fs.SkipAll) {
		return "", werr
	}
	if found == "" {
		return "", ErrSessionJSONLMissing
	}
	return found, nil
}
