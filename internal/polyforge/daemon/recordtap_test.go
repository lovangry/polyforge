package daemon

// recordtap_test.go: the RecordTap binding's daemon half — the runtime
// lane's test pins that every raw stdout record reaches cfg.RecordTap; this
// pins that the tap's lines land in the observe adapter's Records() stream
// (the Observe lane's Handle face) without blocking the runtime's stdout
// reader when the buffer is full.

import (
	"testing"
	"time"

	pfruntime "github.com/silgrid/polyforge/internal/polyforge/runtime"
)

func TestTapRecordReachesAdapterRecords(t *testing.T) {
	d := &Daemon{
		adapters: map[string]*agentAdapter{},
	}
	d.cfg.ExitPoll = 10 * time.Millisecond // the adapter's exit poll ticker
	fake := &fakeAgent{}
	a := d.installAdapter(jobInfoFor("wi_tap", fake))
	defer a.retireAdapter()

	line := []byte(`{"type":"agent_start"}`)
	d.tapRecord("wi_tap", line)

	select {
	case got := <-a.Records():
		if string(got) != string(line) {
			t.Fatalf("records must carry the line verbatim, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the tapped record never reached the adapter's Records stream")
	}
}

func TestTapRecordFullBufferDropsNeverBlocks(t *testing.T) {
	d := &Daemon{
		adapters: map[string]*agentAdapter{},
	}
	d.cfg.ExitPoll = 10 * time.Millisecond
	fake := &fakeAgent{}
	a := d.installAdapter(jobInfoFor("wi_tap2", fake))
	defer a.retireAdapter()

	// fill the buffer completely, then keep tapping: each call must return
	// promptly (drop, never block the runtime's stdout reader)
	for i := 0; i < cap(a.records)+64; i++ {
		d.tapRecord("wi_tap2", []byte("x"))
	}
	select {
	case got := <-a.Records():
		if string(got) != "x" {
			t.Fatalf("unexpected record %q", got)
		}
	default:
		t.Fatal("the filled buffer must still yield its records")
	}
}

func TestTapRecordUnknownWorkItemDrops(t *testing.T) {
	d := &Daemon{
		adapters: map[string]*agentAdapter{},
	}
	// no adapter registered: the tap must be a quiet no-op
	d.tapRecord("wi_unknown", []byte("x"))
}

// jobInfoFor builds the runtime.JobInfo installAdapter needs.
func jobInfoFor(wi string, agent *fakeAgent) pfruntime.JobInfo {
	return pfruntime.JobInfo{
		WorkItem:   wi,
		InstanceID: "ra_" + wi, // the derivation fence token
		Agent:      agent,
	}
}
