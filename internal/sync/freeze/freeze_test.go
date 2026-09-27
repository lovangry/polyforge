package freeze

// Tests for the freeze window (05 §2.7): shortcut vs confirmation path,
// timeout semantics (forced vs milestone), and staging independence
// (SYNC-INV-7 / B09: no hardlink, in-place writes and truncates of the live
// file after freeze release must not touch pinned bytes).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type fakeQuiescer struct {
	delay time.Duration
	err   error
}

func (f *fakeQuiescer) RequestQuiesce(ctx context.Context) error {
	select {
	case <-time.After(f.delay):
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releasableQuiescer is a quiescer WITH the freeze.Releaser face: it
// records both ends of the window (05 §2.7 acquisition + release).
type releasableQuiescer struct {
	fakeQuiescer
	releases int
}

func (r *releasableQuiescer) ReleaseQuiesce(ctx context.Context) error {
	r.releases++
	return nil
}

// TestReleaseNotifiesTheAgent: the release step (05 §2.7) must notify a
// quiescer that carries the Releaser face.
func TestReleaseNotifiesTheAgent(t *testing.T) {
	q := &releasableQuiescer{}
	if err := Release(context.Background(), Conditions{Quiescer: q}); err != nil {
		t.Fatal(err)
	}
	if q.releases != 1 {
		t.Fatalf("Release must fire the release notification exactly once, got %d", q.releases)
	}
}

// TestReleaseNoOpWhenNobodyToNotify: the process-dead shortcut (nil
// quiescer) and a quiescer without the release face have nobody to notify —
// Release is a no-op, never a panic.
func TestReleaseNoOpWhenNobodyToNotify(t *testing.T) {
	if err := Release(context.Background(), Conditions{}); err != nil {
		t.Fatal(err)
	}
	if err := Release(context.Background(), Conditions{Quiescer: &fakeQuiescer{}}); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireShortcutWhenProcessGroupDead(t *testing.T) {
	m := NewManager()
	// ① all writers dead ② no replacement wrote yet → granted without any
	// confirmation round trip.
	err := m.Acquire(context.Background(), "wi_x", Conditions{
		ProcessGroupDead:   func() bool { return true },
		ReplacementWriting: func() bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcquireShortcutRefusedWhenReplacementWriting(t *testing.T) {
	m := NewManager()
	q := &fakeQuiescer{}
	// A live replacement already wrote: the dead-old-writers judgment no
	// longer holds; the normal confirmation path is taken.
	err := m.Acquire(context.Background(), "wi_x", Conditions{
		Quiescer:           q,
		ProcessGroupDead:   func() bool { return true },
		ReplacementWriting: func() bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcquireShortcutRefusedWhenChildAlive(t *testing.T) {
	m := NewManager()
	// Top-level agent "dead" predicate absent → no shortcut claim possible:
	// providing only a quiescer means the normal path runs.
	err := m.Acquire(context.Background(), "wi_x", Conditions{
		Quiescer: &fakeQuiescer{},
		Timeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcquireForcedTimeoutEscalates(t *testing.T) {
	m := NewManager()
	err := m.Acquire(context.Background(), "wi_x", Conditions{
		Quiescer: &fakeQuiescer{delay: 500 * time.Millisecond},
		Timeout:  20 * time.Millisecond,
	})
	if !errors.Is(err, ErrQuiesceTimeout) {
		t.Fatalf("forced trigger timeout must escalate, got %v", err)
	}
}

func TestAcquireMilestoneTimeoutAbandons(t *testing.T) {
	m := NewManager()
	err := m.Acquire(context.Background(), "wi_x", Conditions{
		Quiescer:  &fakeQuiescer{delay: 500 * time.Millisecond},
		Timeout:   20 * time.Millisecond,
		Milestone: true,
	})
	if !errors.Is(err, ErrMilestoneAbandoned) {
		t.Fatalf("milestone timeout must abandon, got %v", err)
	}
}

func TestLockWISerializes(t *testing.T) {
	m := NewManager()
	release := m.LockWI("wi_x")
	got := make(chan struct{})
	go func() {
		r := m.LockWI("wi_x")
		r()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second LockWI must block while the first holder keeps the mutex")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("LockWI must unblock after release")
	}
	// Different WIs do not block on each other (per-WI granularity).
	r2 := m.LockWI("wi_y")
	r2()
}

func TestStagingIndependentCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.txt")
	if err := os.WriteFile(src, []byte("0123456789ABCDEF"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, _ := sha256File(src)
	s := NewStager(filepath.Join(dir, "daemon"))
	if n, err := s.Stage("wi_x", 3, []StageEntry{{AbsPath: src, Sha256: sum}}); err != nil || n != 1 {
		t.Fatal(err)
	}
	staged := filepath.Join(s.SeqDir("wi_x", 3), sum)
	si, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	li, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	// SYNC-INV-7: staged block and live file must NOT share an inode.
	sst := si.Sys().(*syscall.Stat_t)
	lst := li.Sys().(*syscall.Stat_t)
	if sst.Ino == lst.Ino {
		t.Fatal("staging must never hardlink the live file")
	}
	// Post-release destruction of the live file — in-place mid-write AND
	// truncate (both B09 attack forms) — leaves the pinned bytes intact.
	f, err := os.OpenFile(src, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("XXXX"), 4)
	f.Close()
	got, err := s.Block("wi_x", 3, sum)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0123456789ABCDEF" {
		t.Fatalf("in-place write leaked into staging: %q", got)
	}
	if err := os.Truncate(src, 3); err != nil {
		t.Fatal(err)
	}
	got, err = s.Block("wi_x", 3, sum)
	if err != nil || string(got) != "0123456789ABCDEF" {
		t.Fatalf("truncate leaked into staging: %q %v", got, err)
	}
}

func TestStagingDetectsMidWindowMutation(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.txt")
	os.WriteFile(src, []byte("aaaa"), 0o644)
	s := NewStager(filepath.Join(dir, "daemon"))
	// Stale sha: the copy digests to something else → freeze violation.
	if _, err := s.Stage("wi_x", 0, []StageEntry{{AbsPath: src, Sha256: "deadbeef"}}); err == nil {
		t.Fatal("staged bytes diverging from the scan digest must abort")
	}
}

func TestStagingIdempotentSameContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.txt")
	os.WriteFile(src, []byte("aaaa"), 0o644)
	sum, _ := sha256File(src)
	s := NewStager(filepath.Join(dir, "daemon"))
	if _, err := s.Stage("wi_x", 0, []StageEntry{{AbsPath: src, Sha256: sum}}); err != nil {
		t.Fatal(err)
	}
	// Staging the same content twice within a flow is a no-op.
	if _, err := s.Stage("wi_x", 0, []StageEntry{{AbsPath: src, Sha256: sum}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup("wi_x", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.SeqDir("wi_x", 0)); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the staging dir")
	}
}
