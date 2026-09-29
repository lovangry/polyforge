package daemon

// sessioncollect_test.go: the claude adapter's terminal-state session
// collection (03 §4.4 claude amendment / 04 §6.2 A step 2) and its
// DAEMON-513 failure contract (04 §5.2), pinned from four angles:
//
//   - (a) a claude close collects the conversation JSONL READ-ONLY from
//     the harness default session location into
//     harness/<claude>/session-data/ and the shared archive mirrors it
//     to logs/session/<generation>.jsonl (TestClaudeSealCollects…);
//   - (b) a missing source fails the close typed DAEMON-513 — surfaced
//     in the seal answer, nothing silently archived, the rest of the
//     seal sequence intact and a retry after restoring the source
//     completes (TestClaudeSealMissingJSONL…);
//   - (c) the pi adapter path is ZERO-change: no collection, capsule
//     harness layout unchanged (TestPiCloseNoCollection);
//   - (d) the read-only pin: the source location is byte-identical
//     (bytes + mode + mtime) after the collection (inside (a)).
//
// The fake "harness default session location" is a test temp dir wired
// through Config.ClaudeSessionStoreRoot — the same knob the real
// deployment points at the CLI's session store (cmd/pf --claude-session-root).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
)

// claudeJobEnv boots a daemon environment whose adapter is claude, with
// the harness default session store rooted at storeRoot (the fake of the
// CLI's CLAUDE_CONFIG_DIR tree), and drives one working job in it.
func claudeJobEnv(t *testing.T, storeRoot string) (*denv, *capsule.Capsule, string) {
	t.Helper()
	e := newDaemonEnv(t, func(c *Config) {
		c.Adapter = capsule.AdapterClaude
		c.ClaudeSessionStoreRoot = storeRoot
	})
	capc, _ := spawnWorkingJob(t, e)
	sid := e.spawner.spawns[0].SessionID
	return e, capc, sid
}

// statSnapshot captures the read-only-pin evidence for one file.
type statSnapshot struct {
	path  string
	data  []byte
	mode  os.FileMode
	mtime time.Time
}

func snapshotFile(t *testing.T, path string) statSnapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return statSnapshot{path: path, data: data, mode: fi.Mode(), mtime: fi.ModTime()}
}

// writeFakeSessionJSONL plants the conversation JSONL at a NESTED path
// under the fake default session location. The nesting is deliberate:
// the real CLI's subdirectory naming (cwd-keyed project dirs) is
// version-specific and unmeasured, so the daemon's resolution must
// tolerate any layout under the configured root, and a nested plant is
// what pins that.
func writeFakeSessionJSONL(t *testing.T, storeRoot, uuid, content string) string {
	t.Helper()
	nested := filepath.Join(storeRoot, "projects", "-home-ws", uuid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return nested
}

// TestClaudeSealCollectsSessionJSONL (a + d): a claude job's close first
// collects the conversation JSONL read-only from the harness default
// session location into harness/<claude>/session-data/, then the shared
// archive mirrors it to logs/session/<generation>.jsonl. Both capsule
// copies are in the SEALED capsule, and the source is byte-identical.
func TestClaudeSealCollectsSessionJSONL(t *testing.T) {
	storeRoot := t.TempDir()
	e, capc, sid := claudeJobEnv(t, storeRoot)
	uuid := ClaudeSessionUUID(sid) // the SAME mapping the adapter's argv pins
	srcJSONL := `{"type":"user","message":{"role":"user","content":"kickoff"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
`
	src := writeFakeSessionJSONL(t, storeRoot, uuid, srcJSONL)
	before := snapshotFile(t, src)

	if _, err := e.d.PauseJob(context.Background(), e.wi); err != nil {
		t.Fatalf("claude close with a present session JSONL: %v", err)
	}
	assertSealedCapsule(t, capc.Dir)

	// The sealed capsule contains the session record (00 §3.6a): the
	// collection copy in the harness-local session dir ...
	collected := filepath.Join(capc.SessionDataDir(capsule.AdapterClaude), uuid+".jsonl")
	after := snapshotFile(t, collected)
	if string(after.data) != srcJSONL {
		t.Fatalf("collected copy must be byte-identical to the source, got:\n%s", after.data)
	}
	if after.mode.Perm() != 0o444 {
		t.Fatalf("the collection copy is read-only material, mode: %v", after.mode)
	}
	// ... and the mirror archive logs/session/<generation>.jsonl
	// (the shared A14 step sources it from session-data/).
	mirror := snapshotFile(t, filepath.Join(capc.Dir, "logs", "session", "1.jsonl"))
	if string(mirror.data) != srcJSONL {
		t.Fatalf("logs/session/1.jsonl must mirror the transcript, got:\n%s", mirror.data)
	}

	// (d) read-only pin: the source at the default session location is
	// byte-identical after the collection — same bytes, same mode, same
	// mtime; nothing at the source location was touched.
	afterSrc := snapshotFile(t, src)
	if string(afterSrc.data) != string(before.data) {
		t.Fatal("the source JSONL must be byte-identical after collection")
	}
	if afterSrc.mode != before.mode || !afterSrc.mtime.Equal(before.mtime) {
		t.Fatalf("the source location must be untouched: mode %v→%v mtime %v→%v",
			before.mode, afterSrc.mode, before.mtime, afterSrc.mtime)
	}
	// The server-side close did land: paused, job record gone.
	if s := e.wiStatus(t); s != "paused" {
		t.Fatalf("wi status after the claude close: %s", s)
	}
	if jobs := e.daemonStateJobs(t); len(jobs) != 0 {
		t.Fatalf("jobs[] must be empty after the close: %+v", jobs)
	}
}

// TestClaudeSealMissingJSONLFailsTyped (b): the generation DID derive a
// harness session (session.json on disk) but the default session location
// has no JSONL for the conversation → the close fails with the typed
// DAEMON-513 SESSION_JSONL_MISSING, surfaced in the seal answer; no
// archive is produced, the generation is NOT ended server-side, and the
// rest of the seal sequence stays intact — a retry after restoring the
// source completes normally.
func TestClaudeSealMissingJSONLFailsTyped(t *testing.T) {
	storeRoot := t.TempDir() // exists, but holds no <uuid>.jsonl
	e, capc, sid := claudeJobEnv(t, storeRoot)
	uuid := ClaudeSessionUUID(sid)

	api, err := NewAPIServer(e.d, e.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	c := &udsClient{path: e.socket}

	var errBody map[string]any
	if code, err := c.do("POST", "/jobs/"+e.wi+"/pause", map[string]any{}, &errBody); err != nil || code != 500 {
		t.Fatalf("pause with a missing session JSONL: %d %v", code, err)
	}
	apiErr := errBody["error"].(map[string]any)
	if apiErr["code"] != ErrCodeSessionJSONL {
		t.Fatalf("the seal answer must carry the typed DAEMON-513, got: %v", errBody)
	}
	detail := apiErr["detail"].(string)
	if !strings.Contains(detail, "SESSION_JSONL_MISSING") || !strings.Contains(detail, uuid) {
		t.Fatalf("the DAEMON-513 detail must name the reason and the conversation id: %q", detail)
	}

	// Nothing silently passed: no archive, no collection copy.
	if _, err := os.Stat(filepath.Join(capc.Dir, "logs", "session", "1.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("a failed collection must produce no archive, stat err = %v", err)
	}
	entries, _ := os.ReadDir(capc.SessionDataDir(capsule.AdapterClaude))
	if len(entries) != 0 {
		t.Fatalf("a failed collection must leave session-data/ empty: %+v", entries)
	}
	// The rest of the seal sequence is intact: the WI is still running
	// server-side (the generation was NOT ended), the job record and the
	// capsule stay — the capsule is NOT sealed (still writable).
	if s := e.wiStatus(t); s != "running" {
		t.Fatalf("a DAEMON-513 close must not end the generation, status: %s", s)
	}
	if jobs := e.daemonStateJobs(t); len(jobs) != 1 {
		t.Fatalf("the jobs[] record must survive a DAEMON-513 close: %+v", jobs)
	}
	if fi, err := os.Stat(capc.Dir); err != nil || fi.Mode().Perm()&0o222 == 0 {
		t.Fatalf("the capsule must remain unsealed after the failed close: %v %v", fi, err)
	}
	if _, err := capc.ReadSessionIdentity(capsule.AdapterClaude); err != nil {
		t.Fatalf("session.json must be intact after the failed close: %v", err)
	}

	// The retry after restoring the source completes the SAME close.
	src := writeFakeSessionJSONL(t, storeRoot, uuid, "{\"type\":\"user\",\"turn\":1}\n")
	var out FinishOutcome
	if code, err := c.do("POST", "/jobs/"+e.wi+"/pause", map[string]any{}, &out); err != nil || code != 200 {
		t.Fatalf("pause retry after restoring the source: %d %v", code, err)
	}
	assertSealedCapsule(t, capc.Dir)
	collected := filepath.Join(capc.SessionDataDir(capsule.AdapterClaude), uuid+".jsonl")
	if b, err := os.ReadFile(collected); err != nil || string(b) == "" {
		t.Fatalf("the retried close must collect the transcript: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(capc.Dir, "logs", "session", "1.jsonl")); err != nil {
		t.Fatalf("the retried close must archive the transcript: %v", err)
	} else if string(b) != readAll(t, src) {
		t.Fatalf("the archive must mirror the restored transcript, got: %s", b)
	}
	if s := e.wiStatus(t); s != "paused" {
		t.Fatalf("wi status after the retried close: %s", s)
	}
}

// TestClaudeSealEmptyTranscriptNoArchive: an EMPTY transcript at the
// default session location is the 03 §4.4 empty branch — no archive
// path, no DAEMON-513, the close completes.
func TestClaudeSealEmptyTranscriptNoArchive(t *testing.T) {
	storeRoot := t.TempDir()
	e, capc, sid := claudeJobEnv(t, storeRoot)
	uuid := ClaudeSessionUUID(sid)
	if err := os.WriteFile(filepath.Join(storeRoot, uuid+".jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.PauseJob(context.Background(), e.wi); err != nil {
		t.Fatalf("an empty transcript must not fail the close: %v", err)
	}
	assertSealedCapsule(t, capc.Dir)
	if _, err := os.Stat(filepath.Join(capc.Dir, "logs", "session", "1.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("an empty transcript produces no archive, stat err = %v", err)
	}
}

// TestPiCloseNoCollection (c): the pi adapter path is ZERO-change. The
// pi transcript is capsule-local already, so a pi close performs NO
// collection — even with a claude session store root that does not
// exist (consulting it would fail the close), the capsule's harness
// layout is unchanged, session-data/ is untouched and the shared archive
// mirrors the capsule-local transcript as before.
func TestPiCloseNoCollection(t *testing.T) {
	// A store root that does not exist: the pi path must never look at it.
	absentRoot := filepath.Join(t.TempDir(), "no-such-cc-store")
	e := newDaemonEnv(t, func(c *Config) {
		c.ClaudeSessionStoreRoot = absentRoot
	})
	capc, _ := spawnWorkingJob(t, e)
	// The pi runtime model: the harness writes its transcript directly
	// into the capsule-local session dir.
	piTranscript := "{\"type\":\"prompt_command\",\"turn\":1}\n"
	piJSONL := filepath.Join(capc.SessionDataDir(capsule.AdapterPi), "sess-transcript.jsonl")
	if err := os.WriteFile(piJSONL, []byte(piTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotFile(t, piJSONL)

	// Harness layout before the close — the pi path must leave it alone.
	layoutBefore := harnessLayout(t, capc)

	if _, err := e.d.PauseJob(context.Background(), e.wi); err != nil {
		t.Fatalf("pi close must not consult the claude session store: %v", err)
	}
	assertSealedCapsule(t, capc.Dir)

	// The capsule-local transcript is untouched by the close (byte-identical,
	// still in place — a collection copy is never made for pi). The seal
	// strips write bits tree-wide (SealReadOnly), so the byte identity is
	// the pin, not the mode.
	after := snapshotFile(t, piJSONL)
	if string(after.data) != string(before.data) {
		t.Fatal("the pi session-data transcript must be untouched by the close")
	}
	// The shared archive still mirrors the capsule-local transcript.
	if b, err := os.ReadFile(filepath.Join(capc.Dir, "logs", "session", "1.jsonl")); err != nil {
		t.Fatalf("the pi archive must still ride the close: %v", err)
	} else if string(b) != piTranscript {
		t.Fatalf("the pi archive must mirror the capsule-local transcript, got: %s", b)
	}
	// Zero-change pin: no harness/claude tree exists at all, and the
	// harness layout is exactly what it was before the close.
	if _, err := os.Stat(capc.HarnessDir(capsule.AdapterClaude)); !os.IsNotExist(err) {
		t.Fatalf("a pi close must never create the claude harness tree: %v", err)
	}
	if got := harnessLayout(t, capc); got != layoutBefore {
		t.Fatalf("the pi capsule harness layout must be unchanged by the close:\nbefore:\n%s\nafter:\n%s", layoutBefore, got)
	}
	if s := e.wiStatus(t); s != "paused" {
		t.Fatalf("wi status after the pi close: %s", s)
	}
}

// TestClaudeSessionStoreRootResolution pins the default-location
// resolution precedence: explicit config > CLAUDE_CONFIG_DIR > ~/.claude
// (the adapter-verification measurement: conversations live under
// CLAUDE_CONFIG_DIR, default ~/.claude). A CLI storage-layout or
// relocation change needs at most the config knob.
func TestClaudeSessionStoreRootResolution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	d := &Daemon{cfg: Config{}}
	if got := d.claudeSessionStoreRoot(); got != filepath.Join(home, ".claude") {
		t.Fatalf("default root must be $HOME/.claude, got %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/cc")
	if got := d.claudeSessionStoreRoot(); got != "/custom/cc" {
		t.Fatalf("CLAUDE_CONFIG_DIR must be honored, got %q", got)
	}
	d.cfg.ClaudeSessionStoreRoot = "/flag/root"
	if got := d.claudeSessionStoreRoot(); got != "/flag/root" {
		t.Fatalf("the explicit config knob must win, got %q", got)
	}
}

// TestFindSessionJSONL pins the layout-agnostic source search: nested
// layouts match, a missing file — including a missing ROOT — is the
// DAEMON-513 shape, never a guess.
func TestFindSessionJSONL(t *testing.T) {
	root := t.TempDir()
	if _, err := findSessionJSONL(root, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrSessionJSONLMissing) {
		t.Fatalf("no JSONL under the root must be the typed 513 shape, got %v", err)
	}
	if _, err := findSessionJSONL(filepath.Join(root, "absent"), "u"); !errors.Is(err, ErrSessionJSONLMissing) {
		t.Fatalf("a missing root must be the typed 513 shape, got %v", err)
	}
	u := "11111111-2222-3333-4444-555555555555"
	want := writeFakeSessionJSONL(t, root, u, "{}\n")
	got, err := findSessionJSONL(root, u)
	if err != nil || got != want {
		t.Fatalf("nested layout must resolve, got %q %v (want %q)", got, err, want)
	}
}

// harnessLayout renders the capsule's harness/ tree (relative paths) —
// the zero-change comparison basis for the pi regression.
func harnessLayout(t *testing.T, capc *capsule.Capsule) string {
	t.Helper()
	var b strings.Builder
	base := filepath.Join(capc.Dir, "harness")
	filepath.Walk(base, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(capc.Dir, p)
		b.WriteString(rel)
		if fi.IsDir() {
			b.WriteString("/")
		}
		b.WriteString("\n")
		return nil
	})
	return b.String()
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
