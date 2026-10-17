package runtime

// stepwip_test: the task-repo WIP push of the shutdown (SaveTaskWIP).

import (
	"context"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/gitops"
)

// A hung WIP push holds SaveTaskWIP no longer than its context, and the
// push itself is cut at the same deadline.
func TestSaveTaskWIPReturnsAtItsDeadline(t *testing.T) {
	g := newWIPRig(t, "wi_wip_bound")
	t.Cleanup(func() { terminateJobAgent(g.r, g.wi) })
	entered := make(chan gitops.Opts, 1)
	release := make(chan struct{})
	inner := pushTaskWIPFn
	pushTaskWIPFn = func(o gitops.Opts, keep, chainOK bool) {
		entered <- o
		<-release
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	want, _ := ctx.Deadline()
	t0 := time.Now()
	g.r.SaveTaskWIP(ctx, g.wi)
	took := time.Since(t0)
	o := <-entered
	close(release)
	pushTaskWIPFn = inner
	if took > 5*time.Second {
		t.Fatalf("SaveTaskWIP took %s on a hung push, want its 300ms context", took)
	}
	if !o.Deadline.Equal(want) {
		t.Fatalf("push deadline = %v, want the context's %v", o.Deadline, want)
	}
}
