package runtime

// claim_lock_test.go — a start claims under its canonical id's lock: two
// starts of one work item under different spellings meet on one lock
// (never two claims), a door of the canonical id refuses while a start of
// a known alias probes, and a claim whose compensation has not landed
// refuses the next start until it does — and ends blocked at a restart.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Starts of one work item by slug and by id, either first, with the first
// hung at its probe, its journal verify or its claim: the work item is
// claimed exactly once and exactly one start succeeds.
func TestSlugAndIDStartsClaimOnce(t *testing.T) {
	const a, slug = "wi_cl_a", "proj#41"
	for _, kind := range []string{"probe", "verify", "claim"} {
		for _, first := range []string{a, slug} {
			second := slug
			if first == slug {
				second = a
			}
			t.Run(kind+"/"+first+"-first", func(t *testing.T) {
				setGitIdentity(t)
				hub := newFakeHub()
				r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
				t.Cleanup(func() { r.Close(); terminateJobAgent(r, a) })
				hub.canonical = map[string]string{slug: a}
				ctx := context.Background()
				wantClaims := 1
				if kind == "verify" {
					hub.claimFaults = []claimFault{{err: errors.New("connection reset (scripted)"), afterCommit: true}}
					if err := r.Start(ctx, a); !isTransient(err) {
						t.Fatalf("lost-answer Start = %v, want the typed transient", err)
					}
					wantClaims = 2
				}
				var g *hubGate
				if kind == "probe" {
					g = hub.hangCallAs(t, "probe", first)
				} else {
					g = hub.hangCall(t, kind, a) // goes out under the canonical id
				}
				firstErr := make(chan error, 1)
				go func() { firstErr <- r.Start(ctx, first) }()
				g.waitEntered(t, kind+" of "+first)

				var secondErr error
				within(t, promptly, "the second start", func() { secondErr = r.Start(ctx, second) })
				if kind != "probe" && !isTransient(secondErr) {
					t.Fatalf("Start(%s) during %s's %s = %v, want the typed transient", second, first, kind, secondErr)
				}
				g.open()
				err1 := recvErr(t, firstErr, "the hung start")
				ok := 0
				for _, err := range []error{err1, secondErr} {
					if err == nil {
						ok++
					}
				}
				if ok != 1 {
					t.Fatalf("starts answered %v and %v, want exactly one success", err1, secondErr)
				}
				if n := claimsOf(hub, a); n != wantClaims {
					t.Fatalf("%s claimed %d times, want %d", a, n, wantClaims)
				}
				if ji, ok := r.Info(a); !ok || ji.State != StateRunning {
					t.Fatalf("%s = %s (ok=%v), want RUNNING", a, ji.State, ok)
				}
				if journaled(r, a) != nil || journaled(r, slug) != nil {
					t.Fatal("a settled start left a claiming-journal entry")
				}
			})
		}
	}
}

// The claim goes out (and is journaled) under the canonical id, whatever
// spelling Start was asked for.
func TestSlugStartClaimsUnderCanonicalID(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	const a, slug = "wi_cl_canon", "proj#42"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a) })
	hub.canonical = map[string]string{slug: a}
	g := hub.hangCall(t, "claim", a)
	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background(), slug) }()
	g.waitEntered(t, "the claim")
	if rec := journaled(r, a); rec == nil || rec.WorkItemID != a || journaled(r, slug) != nil {
		t.Fatalf("journal during the claim = %+v (slug key %+v), want the entry under %s", rec, journaled(r, slug), a)
	}
	g.open()
	if err := recvErr(t, done, "the start"); err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	got := hub.claims[0].WorkItemID
	hub.mu.Unlock()
	if got != a {
		t.Fatalf("SessionClaim went out for %q, want the canonical %q", got, a)
	}
	if id := r.Resolve(slug); id != a {
		t.Fatalf("Resolve(%s) = %q, want %s", slug, id, a)
	}
}

// A probe that answers no id claims nothing.
func TestStartRefusesProbeWithoutID(t *testing.T) {
	hub := &noIDHub{fakeHub: newFakeHub()}
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	if err := r.Start(context.Background(), "wi_cl_noid"); err == nil {
		t.Fatal("a start whose probe answered no id succeeded")
	}
	if n := hub.claimCount(); n != 0 || len(r.loadClaiming()) != 0 {
		t.Fatalf("the refused start claimed %d time(s) / journaled %+v", n, r.loadClaiming())
	}
}

type noIDHub struct{ *fakeHub }

func (h *noIDHub) GetWorkItem(idOrSlug string) (*api.WorkItem, error) {
	w, err := h.fakeHub.GetWorkItem(idOrSlug)
	if w != nil {
		w.ID = ""
	}
	return w, err
}

// With the slug registered as an alias, a start by slug probes under the
// canonical id's lock: a Stop by the canonical id meanwhile answers
// DAEMON-409 (the start is under way), not "no active job".
func TestStopByIDDuringAliasedSlugProbe(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	const a, slug = "wi_cl_alias", "proj#43"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, a) })
	hub.canonical = map[string]string{slug: a}
	r.NoteAlias(slug, a)
	g := hub.hangCallAs(t, "probe", slug)
	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background(), slug) }()
	g.waitEntered(t, "the slug's probe")
	within(t, promptly, "Stop by the canonical id", func() {
		if err := r.Stop(context.Background(), a, protocol.FinishWrapped); !isConflictErr(err) {
			t.Errorf("Stop(%s) during the slug's probe = %v, want DAEMON-409", a, err)
		}
	})
	if !r.Busy(a) || !r.Busy(slug) {
		t.Fatal("the probing start is not reported busy")
	}
	g.open()
	if err := recvErr(t, done, "the start"); err != nil {
		t.Fatal(err)
	}
}

// A failed start whose compensation SessionEnd does not land: the journal
// entry is marked ending, a Start of the work item answers the typed
// transient (never a replay of the claim being ended) until the retry
// lands the close, and then starts afresh.
func TestClaimEndRetryRefusesStart(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.EndRetryBase = 20 * time.Millisecond
	r.cfg.EndRetryMax = 40 * time.Millisecond
	const bad = "wi_cl_ending"
	t.Cleanup(func() { r.Close(); terminateJobAgent(r, bad) })
	hub.reposFor = map[string][]api.ProjectRepo{bad: {{Name: "app", URL: "/nonexistent/app.git"}}}
	hub.mu.Lock()
	hub.end[bad] = errors.New("hub unreachable (scripted)")
	hub.mu.Unlock()
	if err := r.Start(context.Background(), bad); err == nil {
		t.Fatal("the start whose clone fails succeeded")
	}
	rec := journaled(r, bad)
	if rec == nil || !rec.Ending {
		t.Fatalf("journal after the failed compensation = %+v, want the entry marked ending", rec)
	}
	claims := hub.claimCount()
	for i := 0; i < 3; i++ {
		if err := r.Start(context.Background(), bad); !isTransient(err) {
			t.Fatalf("Start during the compensation retry = %v, want the typed transient", err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if n := hub.claimCount() - claims; n != 0 {
		t.Fatalf("a start during the compensation retry claimed %d time(s)", n)
	}
	hub.mu.Lock()
	delete(hub.end, bad)
	hub.reposFor = nil
	hub.mu.Unlock()
	waitFor(t, "the compensation retry to land", func() bool { return journaled(r, bad) == nil })
	if last := hub.lastEnd(); last.Status != api.StatusBlocked || last.SessionID != rec.SessionID {
		t.Fatalf("the retried close = %+v, want blocked under the claim's session", last)
	}
	hub.forceEnd(bad) // the owner's resume of the blocked work item
	if err := r.Start(context.Background(), bad); err != nil {
		t.Fatalf("Start after the compensation landed: %v", err)
	}
}

// At a restart a journal entry marked ending is ended blocked — its claim
// is never replayed; a close that still fails keeps the entry for the
// background retry.
func TestRestartEndsEndingJournalEntry(t *testing.T) {
	for _, landed := range []bool{true, false} {
		name := "lands"
		if !landed {
			name = "keeps-failing"
		}
		t.Run(name, func(t *testing.T) {
			setGitIdentity(t)
			hub := newFakeHub()
			r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
			r.cfg.EndRetryBase = time.Hour // the background retry stays out of the way
			t.Cleanup(r.Close)
			const x = "wi_cl_restart_ending"
			sess := fakeSession(x)
			if err := r.noteClaiming(x, sess, randomKey(), nil); err != nil {
				t.Fatal(err)
			}
			r.markClaimEnding(x, sess, "clone failed (scripted)")
			hub.hold(x, sess, api.StatusRunning)
			if !landed {
				hub.mu.Lock()
				hub.end[x] = errors.New("hub unreachable (scripted)")
				hub.mu.Unlock()
			}
			if err := r.Restart(context.Background()); err != nil {
				t.Fatalf("Restart: %v", err)
			}
			if n := hub.claimCount(); n != 0 {
				t.Fatalf("the ending entry's claim was replayed %d time(s)", n)
			}
			if r.jobOf(x) != nil {
				t.Fatal("the ending entry left a job")
			}
			rec := journaled(r, x)
			if landed {
				if rec != nil {
					t.Fatalf("the landed close kept the entry %+v", rec)
				}
				if e := hub.lastEnd(); e.Status != api.StatusBlocked || e.SessionID != sess || e.Note != "clone failed (scripted)" {
					t.Fatalf("restart close = %+v, want blocked with the journaled note", e)
				}
				return
			}
			if rec == nil || !rec.Ending {
				t.Fatalf("the failed close dropped the entry: %+v", rec)
			}
		})
	}
}
