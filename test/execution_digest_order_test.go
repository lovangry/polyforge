// carries: test/v1_harness_test.go, test/execution_digest_order_test.go
package test

// execution_digest_order_test.go: an e2e failure's assertion line is a
// signature — the pre-existing classifier (workflows/_shared/scripts/
// pfstep/preexisting.py) compares the bar's line with the isolated
// re-run's and the base runs' byte for byte before it calls a red
// pre-existing, and the digest those lines carry (execDigest, in
// test/v1_harness_test.go) printed its steps in Go's map iteration
// order: one blocked execution read three times printed its six steps
// in three different orders, and every run's line already differed
// from another's. This pins the digest to sorted step ids — a failure
// that repeats must repeat its words.
//
// Run (targeted): GOWORK=off go test ./test/ -run '^TestExecDigestPrintsStepsInSortedOrder$' -count=1

import "testing"

func TestExecDigestPrintsStepsInSortedOrder(t *testing.T) {
	x := &executionView{WorkflowStatus: "blocked", Version: 25, Steps: map[string]*execStepView{
		"sync":     {Status: "in_progress"},
		"review":   {Status: "done"},
		"fix":      {Status: "done"},
		"merge":    {Status: "in_progress"},
		"publish":  {Status: "pending"},
		"evidence": {Status: "pending"},
	}}
	want := "status=blocked version=25 evidence=pending fix=done merge=in_progress publish=pending review=done sync=in_progress"
	for i := 0; i < 200; i++ {
		if got := execDigest(x); got != want {
			t.Fatalf("execDigest (run %d) = %q, want the sorted step order %q", i, got, want)
		}
	}
}
