package daemon

// bridge_attach_test.go — the RT→observe bridge forwards a recovery of a
// work item whose RTStarted attachment is still queued for the observe
// lane (no monitor yet): only a recovery of a work item nothing attached
// is dropped.

import (
	"context"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/supervise"
)

func TestBridgeKeepsRecoveryBehindQueuedStart(t *testing.T) {
	td := newTestDaemon(t, nil)
	const wi = "wi_bridge_q"
	td.submitClaim(t, wi)
	// a second bridge over the same runtime whose observe lane does not
	// drain: the RTStarted attachment stays queued, no monitor exists
	attach := supervise.NewQueue[observe.Attach]()
	obs, err := observe.New(observe.Config{RT: attach, OB: supervise.NewQueue[protocol.OBMsg](), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &Daemon{cfg: td.cfg, rt: td.rt, obs: obs, rtMsgs: supervise.NewQueue[protocol.RTMsg](), attachCh: attach, runCtx: ctx, adapters: map[string]*agentAdapter{}}
	go b.bridge()
	b.rtMsgs.Push(protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: "wi_bridge_none", SpawnN: 2})
	b.rtMsgs.Push(protocol.RTMsg{Kind: protocol.RTStarted, WorkItem: wi, SpawnN: 1})
	b.rtMsgs.Push(protocol.RTMsg{Kind: protocol.RTRecovered, WorkItem: wi, SpawnN: 2})
	var got []protocol.RTMsg
	deadline := time.After(5 * time.Second)
	for len(got) < 2 {
		if at, ok := attach.Pop(); ok {
			got = append(got, at.Msg)
			continue
		}
		select {
		case <-attach.Notify():
		case <-deadline:
			t.Fatalf("bridge forwarded %+v, want the RTStarted and the RTRecovered of %s", got, wi)
		}
	}
	if got[0].Kind != protocol.RTStarted || got[1].Kind != protocol.RTRecovered || got[1].WorkItem != wi {
		t.Fatalf("bridge forwarded %+v, want RTStarted then RTRecovered of %s (the unattached recovery dropped)", got, wi)
	}
}
