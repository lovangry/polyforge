package runtime

// claim_hub_offlock_test.go — the hub calls of a claim (work-item probe,
// journal verify, SessionClaim) and of a live resume (verify, attach) run
// outside opMu (B-8 finish): one hung hub call for work item A no longer
// freezes the doors of every other work item, and the work item it hangs
// for stays single-claimed and refuses competing doors meanwhile.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

// promptly is the "the other work item's door did not wait on the hung
// call" bound: the hung call never returns until the test releases it.
const promptly = 10 * time.Second

func isTransient(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Code == api.ErrDaemonHubUnavailable
}

func isConflictErr(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Code == api.ErrDaemonConflict
}

// nextRT answers the next RT message (an error after a bound, never a
// t.Fatal: the helpers below also run on within's goroutine).
func nextRT(q *supervise.Queue[protocol.RTMsg]) (protocol.RTMsg, error) {
	deadline := time.After(5 * time.Second)
	for {
		if m, ok := q.Pop(); ok {
			return m, nil
		}
		select {
		case <-q.Notify():
		case <-deadline:
			return protocol.RTMsg{}, errors.New("timed out waiting for an RT message")
		}
	}
}

// startLive starts wi and consumes its RTStarted.
func startLive(r *Runtime, toOB *supervise.Queue[protocol.RTMsg], wi string) error {
	if err := r.Start(context.Background(), wi); err != nil {
		return fmt.Errorf("Start(%s): %w", wi, err)
	}
	m, err := nextRT(toOB)
	if err != nil {
		return err
	}
	if m.Kind != protocol.RTStarted || m.WorkItem != wi {
		return fmt.Errorf("want RTStarted(%s), got %+v", wi, m)
	}
	return nil
}

// pauseRetained pauses a live wi into a retained paused record whose
// clean_pause the operator already cleared (the resume door's input).
func pauseRetained(r *Runtime, hub *fakeHub, toOB *supervise.Queue[protocol.RTMsg], wi string) error {
	if err := r.Stop(context.Background(), wi, protocol.FinishPaused); err != nil {
		return fmt.Errorf("pause %s: %w", wi, err)
	}
	m, err := nextRT(toOB)
	if err != nil {
		return err
	}
	if m.Kind != protocol.RTFinished || m.Reason != protocol.FinishPaused {
		return fmt.Errorf("want RTFinished(paused), got %+v", m)
	}
	hub.mu.Lock()
	hub.cleanPause[wi] = false
	hub.mu.Unlock()
	return nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func attachCount(h *fakeHub, wi string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, a := range h.attaches {
		if a.WorkItemID == wi {
			n++
		}
	}
	return n
}

func claimsOf(h *fakeHub, wi string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.claims {
		if h.canonicalID(c.WorkItemID) == wi {
			n++
		}
	}
	return n
}

// A hung probe, journal verify or SessionClaim of work item A leaves B
// fully operable — started, paused, resumed and wrapped while A hangs —
// and A itself stays single-claimed: a second Start of it (under either
// spelling) answers the typed transient, a Stop of it DAEMON-409; once the
// hub answers, A starts with exactly the one claim.
func TestOffLockHungClaimHubCallLeavesOthersResponsive(t *testing.T) {
	for _, kind := range []string{"probe", "verify", "claim"} {
		t.Run(kind, func(t *testing.T) {
			setGitIdentity(t)
			hub := newFakeHub()
			r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
			t.Cleanup(r.Close)
			const a, b = "wi_off_a", "wi_off_b"
			hub.canonical = map[string]string{"proj#9": a}
			ctx := context.Background()
			wantClaims := 1
			if kind == "verify" {
				// a journal entry whose claim answer was lost: the next
				// start verifies it first (claimIntent)
				hub.claimFaults = []claimFault{{err: errors.New("connection reset (scripted)"), afterCommit: true}}
				if err := r.Start(ctx, a); !isTransient(err) {
					t.Fatalf("lost-answer Start = %v, want the typed transient", err)
				}
				wantClaims = 2 // the lost one + its replay
			}
			g := hub.hangCall(t, kind, a)
			aErr := make(chan error, 1)
			go func() { aErr <- r.Start(ctx, a) }()
			g.waitEntered(t, kind+" of "+a)

			within(t, promptly, "Start of another work item", func() {
				if err := startLive(r, toOB, b); err != nil {
					t.Error(err)
				}
			})
			defer terminateJobAgent(r, b)
			within(t, promptly, "pause of another work item", func() {
				if err := pauseRetained(r, hub, toOB, b); err != nil {
					t.Error(err)
				}
			})
			within(t, promptly, "resume of another work item", func() {
				if out, err := r.ResumePaused(ctx, b); err != nil || out != ResumeAttached {
					t.Errorf("ResumePaused(%s) = %q, %v", b, out, err)
				}
			})
			within(t, promptly, "wrap of another work item", func() {
				if err := r.Stop(ctx, b, protocol.FinishWrapped); err != nil {
					t.Errorf("wrap %s: %v", b, err)
				}
			})

			// the hung work item refuses the competing doors meanwhile
			within(t, promptly, "a second Start of the hung work item", func() {
				if err := r.Start(ctx, a); !isTransient(err) {
					t.Errorf("second Start(%s) = %v, want the typed transient", a, err)
				}
			})
			if kind != "probe" {
				// the slug spelling probes (its probe is not gated) and
				// meets the canonical latch
				within(t, promptly, "a Start of the hung work item's slug", func() {
					if err := r.Start(ctx, "proj#9"); !isTransient(err) {
						t.Errorf("Start(proj#9) = %v, want the typed transient", err)
					}
				})
			}
			within(t, promptly, "a Stop of the hung work item", func() {
				if err := r.Stop(ctx, a, protocol.FinishWrapped); !isConflictErr(err) {
					t.Errorf("Stop(%s) during its claim's hub call = %v, want DAEMON-409", a, err)
				}
			})

			g.open()
			if err := recvErr(t, aErr, "the hung Start"); err != nil {
				t.Fatalf("Start(%s) after the hub answered: %v", a, err)
			}
			defer terminateJobAgent(r, a)
			if n := claimsOf(hub, a); n != wantClaims {
				t.Fatalf("%s claimed %d times, want %d", a, n, wantClaims)
			}
			if ji, ok := r.Info(a); !ok || ji.State != StateRunning {
				t.Fatalf("%s after the hub answered: %+v (ok=%v), want RUNNING", a, ji, ok)
			}
			if journaled(r, a) != nil {
				t.Fatal("the settled start left its claiming-journal entry")
			}
		})
	}
}

// A live resume whose session verify hangs latches only its own work
// item: another work item closes promptly, while a Stop, a second resume
// or a Start of the resuming one answer DAEMON-409 / DAEMON-503. Once the
// hub answers, the resume attaches exactly once.
func TestOffLockResumeHungVerifyWhileAnotherCloses(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const p, q = "wi_res_p", "wi_res_q"
	ctx := context.Background()
	must(t, startLive(r, toOB, p))
	must(t, pauseRetained(r, hub, toOB, p))
	must(t, startLive(r, toOB, q))
	defer terminateJobAgent(r, q)

	g := hub.hangCall(t, "verify", p)
	res := make(chan error, 1)
	go func() {
		out, err := r.ResumePaused(ctx, p)
		if err == nil && out != ResumeAttached {
			err = errors.New("resume outcome " + out)
		}
		res <- err
	}()
	g.waitEntered(t, "the resume's verify")

	within(t, promptly, "wrap of another work item", func() {
		if err := r.Stop(ctx, q, protocol.FinishWrapped); err != nil {
			t.Errorf("wrap %s: %v", q, err)
		}
	})
	within(t, promptly, "doors of the resuming work item", func() {
		if err := r.Stop(ctx, p, protocol.FinishWrapped); !isConflictErr(err) {
			t.Errorf("Stop during the resume's verify = %v, want DAEMON-409", err)
		}
		if _, err := r.ResumePaused(ctx, p); !isConflictErr(err) {
			t.Errorf("second resume during the verify = %v, want DAEMON-409", err)
		}
		if err := r.Start(ctx, p); !isTransient(err) {
			t.Errorf("Start during the resume's verify = %v, want the typed transient", err)
		}
	})
	g.open()
	if err := recvErr(t, res, "the hung resume"); err != nil {
		t.Fatalf("resume after the hub answered: %v", err)
	}
	defer terminateJobAgent(r, p)
	if n := attachCount(hub, p); n != 1 {
		t.Fatalf("%s attached %d times, want 1", p, n)
	}
	if ji, _ := r.Info(p); ji.State != StateRunning {
		t.Fatalf("%s after the resume: %s, want RUNNING", p, ji.State)
	}
}

// A Stop during a resume's hung attach answers DAEMON-409 (the attach
// decides the job), a second resume too, and the attach goes out once.
func TestOffLockStopDuringHungAttach(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const p = "wi_att_p"
	ctx := context.Background()
	must(t, startLive(r, toOB, p))
	must(t, pauseRetained(r, hub, toOB, p))

	g := hub.hangCall(t, "attach", p)
	res := make(chan error, 1)
	go func() { _, err := r.ResumePaused(ctx, p); res <- err }()
	g.waitEntered(t, "the resume's attach")
	within(t, promptly, "Stop during the attach", func() {
		if err := r.Stop(ctx, p, protocol.FinishWrapped); !isConflictErr(err) {
			t.Errorf("Stop during the attach = %v, want DAEMON-409", err)
		}
		if _, err := r.ResumePaused(ctx, p); !isConflictErr(err) {
			t.Errorf("second resume during the attach = %v, want DAEMON-409", err)
		}
	})
	g.open()
	if err := recvErr(t, res, "the hung resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer terminateJobAgent(r, p)
	if n := attachCount(hub, p); n != 1 {
		t.Fatalf("attached %d times, want 1 (no double attach)", n)
	}
	// the Stop the attach refused works once it settled
	if err := r.Stop(ctx, p, protocol.FinishWrapped); err != nil {
		t.Fatalf("Stop after the resume settled: %v", err)
	}
}

// A shutdown during each hung claim hub call is not held by it
// (AbortClaims waits only on CLAIMING claims) and the start defers once
// the hub answers: nothing is claimed after a probe, and the journal entry
// a verify or a claim was working on is kept for the next start.
func TestOffLockShutdownDuringHungClaimHubCall(t *testing.T) {
	for _, kind := range []string{"probe", "verify", "claim"} {
		t.Run(kind, func(t *testing.T) {
			setGitIdentity(t)
			hub := newFakeHub()
			r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
			t.Cleanup(r.Close)
			const a = "wi_off_sd"
			ctx := context.Background()
			if kind == "verify" {
				hub.claimFaults = []claimFault{{err: errors.New("connection reset (scripted)"), afterCommit: true}}
				if err := r.Start(ctx, a); !isTransient(err) {
					t.Fatalf("lost-answer Start = %v", err)
				}
			}
			g := hub.hangCall(t, kind, a)
			aErr := make(chan error, 1)
			go func() { aErr <- r.Start(ctx, a) }()
			g.waitEntered(t, kind+" of "+a)

			actx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			within(t, promptly, "AbortClaims during the hung "+kind, func() { r.AbortClaims(actx) })
			if actx.Err() != nil {
				t.Fatal("AbortClaims waited out its whole bound on a pre-claim hub call")
			}
			g.open()
			if err := recvErr(t, aErr, "the hung Start"); !IsClaimDeferred(err) {
				t.Fatalf("Start after the shutdown latch = %v, want the deferral", err)
			}
			if r.jobOf(a) != nil {
				t.Fatal("a deferred start left a job behind")
			}
			rec := journaled(r, a)
			switch kind {
			case "probe":
				if rec != nil || hub.claimCount() != 0 {
					t.Fatalf("a probe-time deferral journaled/claimed: rec=%+v claims=%d", rec, hub.claimCount())
				}
			default:
				if rec == nil {
					t.Fatal("the deferral dropped the claiming-journal entry (the next start must replay it)")
				}
			}
			if n := hub.endCount(); n != 0 {
				t.Fatalf("a deferred start ended %d session(s) — a shutdown must never turn it blocked", n)
			}
		})
	}
}

// A shutdown latched during a resume's hung verify attaches nothing once
// the hub answers: the record stays paused for the next start.
func TestOffLockShutdownDuringHungResumeVerify(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	t.Cleanup(r.Close)
	const p = "wi_res_sd"
	ctx := context.Background()
	must(t, startLive(r, toOB, p))
	must(t, pauseRetained(r, hub, toOB, p))
	g := hub.hangCall(t, "verify", p)
	res := make(chan error, 1)
	go func() { _, err := r.ResumePaused(ctx, p); res <- err }()
	g.waitEntered(t, "the resume's verify")
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	within(t, promptly, "AbortClaims during the resume's verify", func() { r.AbortClaims(actx) })
	g.open()
	err := recvErr(t, res, "the hung resume")
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrDaemonGone {
		t.Fatalf("resume after the shutdown latch = %v, want DAEMON-410", err)
	}
	if n := attachCount(hub, p); n != 0 {
		t.Fatalf("attached %d times behind the shutdown", n)
	}
	if ji, _ := r.Info(p); ji.State != StatePaused {
		t.Fatalf("record after the refused resume: %s, want PAUSED", ji.State)
	}
}
