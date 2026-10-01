package daemon

// wraponce_test.go: the flaw fix's pin — the wrap DUAL PATH pushes ONCE.
// The real machine: flush(wrap) sealed and pushed successfully
// (published, final wrapped) at 04:34:54; one second later the runtime's
// stopOp — the dual close's other half — ran its own final push against
// the already-spent attempt credential and recorded push_failed
// (GIT-500). The fix: an OBCompleted carrying the terminal vocabulary
// (wrapped|failed) tells stopOp the flush path already sealed, and its
// half of the close skips the push entirely (read-only seal + terminal
// close only).
//
// The test spends the push credential exactly the way the real machine
// did: the flush path's PushFunc removes the bare remote the moment its
// own push lands, so any SECOND push by the other half fails loudly and
// degrades EndGeneration to blocked — which is what the assertions pin
// against.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/gitops"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
)

func TestWrapDualPathPushesOnce(t *testing.T) {
	var pushes int32
	td := newTestDaemon(t, func(c *Config) {
		c.PushFunc = func(ctx context.Context, o gitops.Opts, msg string) (string, bool, gitops.PushTimings, error) {
			sha, pushed, pt, err := gitops.CommitPush(o, msg)
			if err == nil && pushed && atomic.AddInt32(&pushes, 1) == 1 {
				// The flush path's seal push just landed. From this moment
				// the attempt credential is spent (the real machine: the
				// sealed state settles and the credential is revoked before
				// the other half runs) — remove the remote so any redundant
				// push by stopOp fails observably.
				_ = os.RemoveAll(filepath.Join(c.GitRemoteBase, "wi_once.git"))
			}
			return sha, pushed, pt, err
		}
	})
	td.submitClaim(t, "wi_once")
	td.waitMonitored(t, "wi_once")

	// The agent's own flush(wrap) — the dual path's first half.
	raw, err := td.client.Observe("wi_once", 0)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	var jv protocol.JobView
	if err := json.Unmarshal(raw, &jv); err != nil || jv.AttemptID == "" {
		t.Fatalf("view undecodable: %v (%s)", err, raw)
	}
	fraw, err := td.client.Flush("wi_once", protocol.FlushRequest{
		WorkItem: "wi_once", AttemptID: jv.AttemptID, SpawnN: jv.SpawnN,
		Reason: protocol.FlushWrap, FinalStatus: protocol.FinishWrapped,
	})
	if err != nil {
		t.Fatalf("flush(wrap): %v", err)
	}
	var ack protocol.FlushAck
	if err := json.Unmarshal(fraw, &ack); err != nil || !ack.OK || !ack.Pushed {
		t.Fatalf("flush(wrap) ack: %v (%s)", err, fraw)
	}

	// The dual path's other half (OBCompleted → stopOp) settles the close.
	waitGone(t, td, "wi_once")

	if n := atomic.LoadInt32(&pushes); n != 1 {
		t.Fatalf("seal pushes = %d, want exactly 1 (the flush path's own)", n)
	}
	// The redundant push would have failed against the spent credential and
	// degraded the terminal verdict to blocked; the wrapped verdict is the
	// proof it never ran.
	if st := td.hub.lastEnd().Status; st != protocol.FinishWrapped {
		t.Fatalf("end-generation status = %q, want wrapped (a second push failed and degraded the verdict)", st)
	}
	if td.hub.endCount() != 1 {
		t.Fatalf("end-generation calls = %d, want 1", td.hub.endCount())
	}
	// And no push_failed diagnostic was tapped for the clean dual close.
	events, rerr := observe.ReadEventsFile(filepath.Join(td.cfg.DataDir, observe.EventsFile))
	if rerr != nil {
		t.Fatalf("read events: %v", rerr)
	}
	for _, e := range events {
		if e.WI == "wi_once" && e.Kind == "push_failed" {
			t.Fatalf("push_failed event from the dual close: %+v", e)
		}
	}
}
