package daemon

import (
	"context"
	"testing"
	"time"
)

// N2 (polyforge#167): each job's shutdown WIP snapshot gets its own share
// of the budget left, capped, so one hung hub cannot starve the rest.
func TestWIPSliceSplitsTheBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if s := wipSlice(ctx, 3); s > 20*time.Second || s < 19*time.Second {
		t.Fatalf("slice of 60s over 3 jobs = %s, want ~20s", s)
	}
	if s := wipSlice(ctx, 1); s != wipSliceMax && s > wipSliceMax {
		t.Fatalf("slice %s exceeds the cap %s", s, wipSliceMax)
	}
	if s := wipSlice(context.Background(), 2); s != wipSliceMax {
		t.Fatalf("no deadline: slice %s, want the cap %s", s, wipSliceMax)
	}
}
