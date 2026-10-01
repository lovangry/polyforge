package observe

// transcript_test.go: the F3 commit-boundary transcript-sync regressions
// (real files on disk): the live harness transcript is copied onto the
// capsule's declared session path BEFORE the commit/push at every
// execution.json step boundary and on flush(wrap), a source that cannot
// be located is the once-per-attempt typed advisory (transcript_missing)
// and never blocks the push, and a resumed derivation that already
// writes into the capsule is not re-copied. The per-derivation transcript
// facts ride RTRecovered attachments — the production adoption path — so
// these tests exercise the monitor exactly as the daemon drives it.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// liveFixture builds one real live transcript (a harness-store-shaped
// .jsonl) plus the declared capsule session path.
type liveFixture struct {
	capsuleDir string
	dst        string
	src        string
	body       []byte
}

func newLiveFixture(t *testing.T, capsuleDir, liveDir string) liveFixture {
	t.Helper()
	f := liveFixture{capsuleDir: capsuleDir, dst: filepath.Join(capsuleDir, "session", "session-ra_wi_test.jsonl")}
	f.src = filepath.Join(liveDir, "2026-09-30T01-02-03-000Z_sess.jsonl")
	f.body = []byte("{\"type\":\"session\",\"id\":\"s1\"}\n{\"type\":\"message_end\"}\n")
	if err := os.MkdirAll(filepath.Dir(f.src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.src, f.body, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// adopt delivers the per-derivation transcript facts through the
// production path: an RTRecovered attachment on rtCh.
func adopt(m *Monitor, h Handle, wi WIInfo) {
	m.rtCh <- Attach{
		Msg:    protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: wi.WorkItem, SpawnN: 1},
		Handle: h,
		WI:     wi,
	}
}

// pushOnceRecorded is a PushFunc that records whether the capsule's
// declared session path existed (with the live bytes) AT PUSH TIME — the
// ordering pin: the copy must precede the commit. The flag is atomic: the
// push runs in the monitor's sub-goroutine, the poller in the test.
func pushOnceRecorded(f liveFixture, sawCopy *atomic.Bool) PushFunc {
	return func(ctx context.Context, o gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
		if raw, err := os.ReadFile(f.dst); err == nil && string(raw) == string(f.body) {
			sawCopy.Store(true)
		}
		return "sha_sync", true, gitops.PushTimings{}, nil
	}
}

func TestStepBoundarySyncsTranscriptBeforePush(t *testing.T) {
	dir := t.TempDir()
	capsuleDir := filepath.Join(dir, "capsule")
	if err := os.MkdirAll(filepath.Join(capsuleDir, "runtime-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newLiveFixture(t, capsuleDir, filepath.Join(dir, "pi-sessions"))

	var sawCopy atomic.Bool
	h := newFakeHandle("ra_wi_test")
	m, ob := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = pushOnceRecorded(f, &sawCopy)
	})
	adopt(m, h, WIInfo{
		WorkItem:       "wi_test",
		Project:        "proj",
		CapsuleDir:     f.capsuleDir,
		BundleName:     "bundle-a",
		Transcript:     f.dst,
		LiveTranscript: f.src,
		SpawnAt:        time.Now().Add(-time.Minute),
	})

	// one execution.json step-boundary write: the version bump schedules
	// the async commit pipeline (sync → push).
	m.writeExec(t, 1, "code")

	soon(t, 5*time.Second, "the step-boundary push never ran", func() bool {
		return sawCopy.Load()
	})
	// the capsule copy is byte-identical to the live source, and the
	// source was never modified.
	raw, err := os.ReadFile(f.dst)
	if err != nil || string(raw) != string(f.body) {
		t.Fatalf("capsule session copy wrong: %v (%q)", err, raw)
	}
	srcRaw, serr := os.ReadFile(f.src)
	if serr != nil || string(srcRaw) != string(f.body) {
		t.Fatal("the live source must be untouched by the sync")
	}
	if len(eventsOf(m, EvPublished)) == 0 {
		t.Fatal("the push must have published")
	}
	expectNoOB(t, ob, 100*time.Millisecond, "no recovery may fire for a healthy transcript sync")
}

func TestFlushWrapSyncsTranscriptBeforePush(t *testing.T) {
	dir := t.TempDir()
	capsuleDir := filepath.Join(dir, "capsule")
	if err := os.MkdirAll(filepath.Join(capsuleDir, "runtime-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newLiveFixture(t, capsuleDir, filepath.Join(dir, "pi-sessions"))

	var sawCopy atomic.Bool
	h := newFakeHandle("ra_wi_test")
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = pushOnceRecorded(f, &sawCopy)
	})
	adopt(m, h, WIInfo{
		WorkItem:       "wi_test",
		Project:        "proj",
		CapsuleDir:     f.capsuleDir,
		BundleName:     "bundle-a",
		Transcript:     f.dst,
		LiveTranscript: f.src,
		SpawnAt:        time.Now().Add(-time.Minute),
	})
	// the adoption must be processed before the flush rides the channel
	// (otherwise the select order races the transcript facts).
	soon(t, 5*time.Second, "the transcript attachment was never adopted", func() bool {
		return len(eventsOf(m, EvAgentRecovered)) > 0
	})

	call := FlushCall{Req: protocol.FlushRequest{
		WorkItem: "wi_test", SpawnN: 1,
		Reason: protocol.FlushWrap, FinalStatus: protocol.FinishWrapped,
	}, Resp: make(chan protocol.FlushAck, 1)}
	m.flushCh <- call
	ack := <-call.Resp
	if !ack.OK {
		t.Fatalf("wrap flush failed: %+v", ack)
	}
	if !sawCopy.Load() {
		t.Fatal("the wrap seal push must see the transcript copy in the capsule")
	}
	if raw, err := os.ReadFile(f.dst); err != nil || string(raw) != string(f.body) {
		t.Fatalf("capsule session copy missing after wrap: %v", err)
	}
}

func TestTranscriptMissingWarnsOnceAndPushProceeds(t *testing.T) {
	dir := t.TempDir()
	capsuleDir := filepath.Join(dir, "capsule")
	if err := os.MkdirAll(filepath.Join(capsuleDir, "runtime-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	// no live file anywhere and no runtime-recorded path: the locator
	// (wired like the daemon wires it, to a scratch root holding nothing)
	// answers "" — the typed advisory path.
	h := newFakeHandle("ra_wi_test")
	var pushed atomic.Bool
	located := 0
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.locateLive = func(capsuleDir string, spawnAt time.Time) string {
			located++
			return ""
		}
		c.push = func(ctx context.Context, o gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
			pushed.Store(true)
			return "sha_nomiss", true, gitops.PushTimings{}, nil
		}
	})
	adopt(m, h, WIInfo{
		WorkItem:   "wi_test",
		Project:    "proj",
		CapsuleDir: capsuleDir,
		BundleName: "b",
		Transcript: filepath.Join(capsuleDir, "session", "session-ra_wi_test.jsonl"),
		SpawnAt:    time.Now().Add(-time.Minute),
	})

	// two step boundaries: the warning fires ONCE, both pushes proceed.
	m.writeExec(t, 1, "code")
	soon(t, 5*time.Second, "first push never ran", func() bool { return pushed.Load() })
	soon(t, 5*time.Second, "the transcript_missing advisory never fired", func() bool {
		return len(eventsOf(m, EvTranscriptMissing)) == 1
	})
	pushed.Store(false)
	m.writeExec(t, 2, "review")
	soon(t, 5*time.Second, "second push never ran", func() bool { return pushed.Load() })
	if n := len(eventsOf(m, EvTranscriptMissing)); n != 1 {
		t.Fatalf("transcript_missing fired %d times, want exactly once per attempt", n)
	}
	if located == 0 {
		t.Fatal("the on-demand locator must have been consulted")
	}
	if _, err := os.Stat(filepath.Join(capsuleDir, "session", "session-ra_wi_test.jsonl")); !os.IsNotExist(err) {
		t.Fatal("no source means no capsule copy — nothing to fabricate")
	}
}

func TestResumedDerivationWritingInCapsuleIsNotReCopied(t *testing.T) {
	dir := t.TempDir()
	capsuleDir := filepath.Join(dir, "capsule")
	if err := os.MkdirAll(filepath.Join(capsuleDir, "runtime-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newLiveFixture(t, capsuleDir, filepath.Join(dir, "pi-sessions"))
	// a RESUMED derivation continues the capsule copy itself: src == dst,
	// and the copy already exists (it is the very file the resume came
	// from).
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dst, f.body, 0o644); err != nil {
		t.Fatal(err)
	}
	h := newFakeHandle("ra_wi_test")
	var pushed atomic.Bool
	m, _ := newTestMonitor(t, h, func(c *monitorCfg) {
		c.push = func(ctx context.Context, o gitops.Opts, message string) (string, bool, gitops.PushTimings, error) {
			pushed.Store(true)
			return "sha_resume", true, gitops.PushTimings{}, nil
		}
	})
	adopt(m, h, WIInfo{
		WorkItem:       "wi_test",
		Project:        "proj",
		CapsuleDir:     f.capsuleDir,
		BundleName:     "bundle-a",
		Transcript:     f.dst,
		LiveTranscript: f.dst, // the resume source IS the capsule copy
		SpawnAt:        time.Now().Add(-time.Minute),
	})

	// stamp a distinct mtime; a needless self-copy would rewrite it.
	before := time.Now().Add(-time.Hour)
	if err := os.Chtimes(f.dst, before, before); err != nil {
		t.Fatal(err)
	}
	m.writeExec(t, 1, "code")
	soon(t, 5*time.Second, "push never ran", func() bool { return pushed.Load() })
	st, err := os.Stat(f.dst)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(before) {
		t.Fatal("a resumed derivation writing into the capsule must not be re-copied over itself")
	}
}
