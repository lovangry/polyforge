package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// API is the daemon face one chat's watcher needs.
type API interface {
	Subscriptions() (protocol.ChatSubscriptions, error)
	Snapshot(wi string, after int64) (*protocol.WatchSnapshot, error)
	Ack(a protocol.ChatAck) error
	Heartbeat(pid int) error
	// Attention lists WIs that need a human and have no live watcher, for
	// this chat or owner, marking them shown.
	Attention(owner string) ([]protocol.ChatAttentionItem, error)
}

// Options configure Run.
type Options struct {
	Owner string
	// WaitDecision: exit 0 after printing the first batch of decisions.
	WaitDecision bool
	Poll         time.Duration
	MaxBackoff   time.Duration
	// Out receives decision lines ONLY; Err receives operational notices.
	Out, Err io.Writer
	Status   *StatusSink
	// OnStatusPath is told which path the progress file landed on (it can
	// change from the fallback to the daemon-provided path).
	OnStatusPath func(path string)
	// ParentAlive, when set, is checked every poll; false ends the run.
	ParentAlive func() bool
	Now         func() time.Time
	// Sleep waits d (or until ctx ends); tests inject a fake clock.
	Sleep func(ctx context.Context, d time.Duration)
}

// PausedLine is the status/notice line while the daemon cannot be reached.
const PausedLine = "daemon unreachable; progress paused"

func (o *Options) defaults() {
	if o.Poll <= 0 {
		o.Poll = 2 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 30 * time.Second
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
}

// ErrParentGone ends a run whose harness process disappeared.
var ErrParentGone = errors.New("parent process gone")

// Run is the resident watcher. Every cycle it heartbeats, lists the chat's
// subscriptions and — per WI — reconciles the authoritative state BEFORE it
// replays events after the daemon-held cursor (so a WI that ended while the
// watcher was down is reported, never missed, and a reconnect after a
// daemon restart needs no special case). Decisions print as fixed-template
// lines on Out; progress goes to the status file. With WaitDecision the
// first non-empty batch is printed, acked and Run returns nil.
func Run(ctx context.Context, api API, opts Options) error {
	opts.defaults()
	var (
		backoff    = time.Second
		paused     bool
		lastNotice time.Time
		attended   bool
		// unacked holds the lines already printed whose ack failed (repo
		// review 2026-10-16 B-23): the daemon still holds the old cursor,
		// so the next cycle re-derives the same decisions — they are not
		// printed again, only their acks retried, until an ack round
		// fully succeeds.
		unacked = map[string]bool{}
	)
	for {
		if opts.ParentAlive != nil && !opts.ParentAlive() {
			return ErrParentGone
		}
		if ctx.Err() != nil {
			return nil
		}
		lines, batch, err := cycle(api, &opts, &attended)
		if err != nil {
			// The attention notice is marked shown by the daemon when it is
			// fetched: a cycle that fails after fetching it still prints it
			// (repo review 2026-10-16 B-25, the WA-4 item).
			for _, l := range batch.out {
				if _, werr := fmt.Fprintln(opts.Out, l); werr != nil {
					return werr
				}
			}
			if !paused || opts.Now().Sub(lastNotice) >= time.Minute {
				fmt.Fprintf(opts.Err, "polyforge watch: %s (%s); retrying in %s\n", pausedText(err), shortErr(err), backoff)
				lastNotice = opts.Now()
			}
			paused = true
			writeStatus(&opts, []string{PausedLine})
			sleepChecked(ctx, &opts, backoff)
			if backoff *= 2; backoff > opts.MaxBackoff {
				backoff = opts.MaxBackoff
			}
			continue
		}
		if paused {
			fmt.Fprintln(opts.Err, "polyforge watch: daemon reachable again; reconciled")
			paused = false
		}
		backoff = time.Second
		writeStatus(&opts, lines)
		printed := 0
		for _, l := range batch.out {
			if unacked[l] {
				continue
			}
			if _, werr := fmt.Fprintln(opts.Out, l); werr != nil {
				return werr // stdout gone: nobody is listening
			}
			printed++
		}
		// Ack only after the lines are out: a crash between print and
		// ack re-delivers (at-least-once), never loses.
		var ackErr error
		for _, a := range batch.acks {
			if aerr := api.Ack(a); aerr != nil && ackErr == nil {
				ackErr = aerr
			}
		}
		if ackErr != nil {
			if len(unacked) == 0 {
				fmt.Fprintf(opts.Err, "polyforge watch: ack failed (%s); retrying without re-printing\n", shortErr(ackErr))
			}
			for _, l := range batch.out {
				unacked[l] = true
			}
		} else {
			unacked = map[string]bool{}
		}
		if printed > 0 && opts.WaitDecision {
			return nil
		}
		opts.Sleep(ctx, opts.Poll)
	}
}

// sleepChecked sleeps d in poll-sized slices so a long backoff still
// notices the parent going away.
func sleepChecked(ctx context.Context, o *Options, d time.Duration) {
	for d > 0 && ctx.Err() == nil {
		step := d
		if step > o.Poll {
			step = o.Poll
		}
		o.Sleep(ctx, step)
		d -= step
		if o.ParentAlive != nil && !o.ParentAlive() {
			return
		}
	}
}

type batch struct {
	out  []string
	acks []protocol.ChatAck
}

func pausedText(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return "daemon error " + ae.Code + "; progress paused"
	}
	return PausedLine
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func writeStatus(o *Options, lines []string) {
	if o.Status == nil {
		return
	}
	p, err := o.Status.Write(lines)
	if err != nil {
		fmt.Fprintf(o.Err, "polyforge watch: cannot write the progress file: %v\n", err)
		return
	}
	if o.OnStatusPath != nil {
		o.OnStatusPath(p)
	}
}

// cycle runs one poll: the whole of it fails (returns err) only when the
// daemon cannot list subscriptions; a single WI's failure shows as its
// progress line and is retried next cycle.
func cycle(d API, o *Options, attended *bool) ([]string, batch, error) {
	var b batch
	subs, err := d.Subscriptions()
	if err != nil {
		return nil, b, err
	}
	_ = d.Heartbeat(os.Getpid())
	if !*attended {
		items, aerr := d.Attention(o.Owner)
		if aerr != nil {
			return nil, b, aerr
		}
		*attended = true
		if len(items) > 0 {
			wis := make([]string, 0, len(items))
			for _, it := range items {
				wis = append(wis, api.DisplayName(it.Slug, it.WI))
			}
			b.out = append(b.out, AttentionLine(wis))
		}
	}
	attention := append([]string(nil), b.out...)
	now := o.Now()
	var lines []string
	for _, sub := range subs.Subscriptions {
		if sub.Done {
			continue
		}
		snap, err := d.Snapshot(sub.WI, sub.Cursor)
		if err == nil && snap.LastSeq < sub.Cursor {
			// Defensive: the daemon's event seq is persisted per line and
			// rebuilt at open (and snapshotted at rotation), so it is
			// monotonic across restarts. It can still fall below a cursor
			// if the daemon's data dir (events.log*) was wiped or replaced
			// while the chat's subscription survived — then replay from
			// the start rather than wait on ids that will never come.
			sub.Cursor = 0
			snap, err = d.Snapshot(sub.WI, 0)
		}
		if err != nil {
			var ae *api.Error
			if !errors.As(err, &ae) {
				// transport: the daemon went away mid-cycle; only the
				// already-fetched (and daemon-marked) attention line survives
				return nil, batch{out: attention}, err
			}
			lines = append(lines, safeID(api.DisplayName(sub.Slug, sub.WI))+" unavailable ("+ae.Code+")")
			continue
		}
		lines = append(lines, ProgressLine(snap, now))
		res := Evaluate(sub, snap)
		for _, dec := range res.Decisions {
			b.out = append(b.out, dec.Line())
		}
		if len(res.Decisions) > 0 || res.Cursor != sub.Cursor || res.Done || !sameKeys(res.Notified, sub.Notified) {
			b.acks = append(b.acks, protocol.ChatAck{WI: sub.WI, Cursor: res.Cursor, Notified: res.Notified, Done: res.Done})
		}
	}
	return lines, b, nil
}

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
