package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
	"github.com/silgrid/polyforge/internal/polyforge/watcher"
)

// clientAPI adapts the real DaemonClient to watcher.API for one chat.
type clientAPI struct {
	d    *cli.DaemonClient
	chat string
}

func (c clientAPI) Subscriptions() (protocol.ChatSubscriptions, error) {
	return c.d.ChatSubscriptions(c.chat)
}
func (c clientAPI) Snapshot(wi string, after int64) (*protocol.WatchSnapshot, error) {
	return c.d.WatchSnapshot(wi, after)
}
func (c clientAPI) Ack(a protocol.ChatAck) error { return c.d.ChatAck(c.chat, a) }
func (c clientAPI) Heartbeat(pid int) error      { return c.d.ChatHeartbeat(c.chat, pid) }
func (c clientAPI) Attention(owner string) ([]protocol.ChatAttentionItem, error) {
	return c.d.ChatAttention(c.chat, owner)
}

// The real watcher against the real daemon: a chat submits, the step asks a
// question, the watcher prints ONE fixed-template line (no question text),
// acks it, and does not repeat it.
//
// polyforge#144: every human face names the WI by its slug — the wake-up
// line, the progress file, the snapshot and the ops status entry.
func TestWatcherAgainstRealDaemonNeedsInputOnce(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.hub.slugs = map[string]string{"wi_e2e": "proj#7"}
	td.client.Origin = &cli.SubmitOrigin{ChatID: "chat_e2e", Owner: "alice"}
	td.submitClaim(t, "wi_e2e")
	td.waitMonitored(t, "wi_e2e")
	const secret = "drop the production database now"
	td.seedPendingQuestion(t, "wi_e2e", "q-7", "plan", secret)
	waitNeedsInput(t, td, "wi_e2e")

	api := clientAPI{d: td.client, chat: "chat_e2e"}
	run := func() string {
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := watcher.Run(ctx, api, watcher.Options{WaitDecision: true, Out: &out, Poll: 20 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	out := run()
	if !strings.Contains(out, "[polyforge] proj#7 needs_input q-7 — a step is waiting for a human answer; this is a human decision, the agent must not answer it") {
		t.Fatalf("out = %q", out)
	}
	if strings.Contains(out, secret) || strings.Count(out, "\n") != 1 {
		t.Fatalf("exactly one fixed line, no free text: %q", out)
	}
	// acked in the daemon: a fresh watcher (restart) does not repeat it
	subs, _ := td.client.ChatSubscriptions("chat_e2e")
	if len(subs.Subscriptions) != 1 || len(subs.Subscriptions[0].Notified) != 1 {
		t.Fatalf("subscriptions after ack = %+v", subs)
	}
	var out2 bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = watcher.Run(ctx, api, watcher.Options{Out: &out2, Poll: 20 * time.Millisecond})
	if out2.Len() != 0 {
		t.Fatalf("the delivered decision must not repeat: %q", out2.String())
	}
	// the progress file (daemon-provided path) is written by the resident watcher
	sink := &watcher.StatusSink{Primary: subs.StatusPath, Fallback: t.TempDir() + "/fb"}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	_ = watcher.Run(ctx2, api, watcher.Options{Status: sink, Poll: 20 * time.Millisecond})
	raw, err := os.ReadFile(subs.StatusPath)
	if err != nil || !strings.Contains(string(raw), "proj#7 needs input step=plan") {
		t.Fatalf("progress file = %q err=%v", raw, err)
	}
	if snap, err := td.client.WatchSnapshot("wi_e2e", 0); err != nil || snap.Slug != "proj#7" || snap.WorkItem != "wi_e2e" {
		t.Fatalf("snapshot must carry the slug beside the id: %+v %v", snap, err)
	}
	if subs.Subscriptions[0].Slug != "proj#7" {
		t.Fatalf("the auto-subscription must record the slug: %+v", subs.Subscriptions[0])
	}
	var st cli.StatusResponse
	if raw, err := td.client.Status(); err != nil || json.Unmarshal(raw, &st) != nil || len(st.Jobs) != 1 || st.Jobs[0].Slug != "proj#7" {
		t.Fatalf("ops status must carry the slug: %+v %v", st, err)
	}
}

// A manual close (wrap --fail here) leaves execution.json at `running`; the
// subscriber must still get the terminal decision and Done from the hub
// status the daemon consults once the job is no longer local.
func TestWatcherGetsDecisionAfterManualClose(t *testing.T) {
	for _, tc := range []struct {
		name, reason, class string
	}{
		{"wrap", "wrap", "completed"},
		{"fail", "fail", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			td := newTestDaemon(t, nil)
			td.client.Origin = &cli.SubmitOrigin{ChatID: "chat_close", Owner: "alice"}
			wi := "wi_close_" + tc.name
			td.submitClaim(t, wi)
			td.waitMonitored(t, wi)
			if _, err := td.client.Wrap(wi, tc.reason); err != nil {
				t.Fatalf("wrap: %v", err)
			}
			api := clientAPI{d: td.client, chat: "chat_close"}
			var out bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := watcher.Run(ctx, api, watcher.Options{WaitDecision: true, Out: &out, Poll: 20 * time.Millisecond}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), wi+" "+tc.class) {
				t.Fatalf("out = %q, want a %s decision", out.String(), tc.class)
			}
			subs, _ := td.client.ChatSubscriptions("chat_close")
			if len(subs.Subscriptions) != 1 || !subs.Subscriptions[0].Done {
				t.Fatalf("subscription must be Done after the terminal decision: %+v", subs)
			}
		})
	}
}
