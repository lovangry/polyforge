package core

// queue_test.go — the w140 unit tests for the ready queue's additive
// 待选 bundle segment (plan step queue_pending_bundle_segment): exactly
// the childless, bundle-less, queued, unblocked, non-waiting tickets;
// epics in no segment; the old segments and entry fields unchanged.

import (
	"encoding/json"
	"testing"
)

// queueIDs is the id list of one wire-level queue segment.
func queueIDs(t *testing.T, q *ReadyQueue, key string) []string {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal queue: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	rawSeg, ok := doc[key]
	if !ok {
		t.Fatalf("queue has no segment %q (wire keys: %v)", key, doc)
	}
	var entries []map[string]any
	if err := json.Unmarshal(rawSeg, &entries); err != nil {
		t.Fatalf("segment %q is not an array of objects: %v", key, err)
	}
	var ids []string
	for _, e := range entries {
		id, _ := e["id"].(string)
		ids = append(ids, id)
		// additivity: every entry (old and new segments) keeps the old fields.
		for _, k := range []string{"id", "slug", "status", "goal", "priority", "bundle_name"} {
			if _, ok := e[k]; !ok {
				t.Fatalf("segment %q entry lost the old field %q (additive-only): %v", key, k, e)
			}
		}
	}
	return ids
}

func hasQueuedID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func TestReadyQueuePendingBundleSegment(t *testing.T) {
	env := newEnv(t)
	mk := func(goal string, bundle, parent *string) string {
		wi, err := env.WIs.Create(CreateRequest{
			Project: "alpha", Goal: goal, Reporter: env.UserID,
			BundleName: bundle, ParentID: parent,
		})
		if err != nil {
			t.Fatalf("create %s: %v", goal, err)
		}
		return wi.ID
	}

	x := mk("executable bound", strPtrTest("q-x"), nil) // bound → items
	tt := mk("pending candidate", nil, nil)             // T: the 待选 candidate
	w := mk("waiting excluded", nil, nil)               // waiting_on → excluded
	bq := mk("blocker", strPtrTest("q-bq"), nil)        // bound blocker → items
	d := mk("dependency excluded", nil, nil)            // blocked → excluded
	e := mk("epic parent", nil, nil)                    // epic → no segment
	ec := mk("epic bound child", strPtrTest("q-ec"), strPtrTest(e))
	v := mk("later unbound", strPtrTest("q-v"), nil) // bound now, unbound below

	if _, err := env.WIs.Patch(w, map[string]json.RawMessage{"waiting_on": json.RawMessage(`"owner"`)}); err != nil {
		t.Fatalf("patch waiting_on: %v", err)
	}
	if err := env.WIs.AddDependency(bq, d, "blocks"); err != nil {
		t.Fatalf("add dependency: %v", err)
	}

	q, err := env.WIs.ReadyQueue("alpha")
	if err != nil {
		t.Fatalf("ready queue: %v", err)
	}

	items := queueIDs(t, q, "items")
	pending := queueIDs(t, q, "pending_bundles")
	// additivity: the old segments keep their keys and the wire top level
	// only gains the new segment.
	raw, _ := json.Marshal(q)
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	for _, k := range []string{"project", "items", "running", "paused"} {
		if _, ok := wire[k]; !ok {
			t.Fatalf("queue lost the old top-level key %q", k)
		}
	}
	if len(wire) != 5 {
		t.Fatalf("queue top level = %d keys, want exactly the old four + pending_bundles", len(wire))
	}

	for _, id := range []string{x, bq, ec, v} {
		if !hasQueuedID(items, id) {
			t.Fatalf("executable segment lost %s (old membership must survive)", id)
		}
	}
	for _, id := range []string{tt, w, d, e} {
		if hasQueuedID(items, id) {
			t.Fatalf("executable segment gained %s (must stay excluded)", id)
		}
	}

	if !hasQueuedID(pending, tt) {
		t.Fatalf("pending-bundle segment does not contain the qualifying ticket %s: %v", tt, pending)
	}
	for _, id := range []string{w, d, e, x, bq, ec, v} {
		if hasQueuedID(pending, id) {
			t.Fatalf("pending-bundle segment contains %s, which must be excluded", id)
		}
	}

	// the epic appears in NO segment at all.
	for _, seg := range []string{"items", "running", "paused", "pending_bundles"} {
		if hasQueuedID(queueIDs(t, q, seg), e) {
			t.Fatalf("epic %s appears in segment %q, want no segment", e, seg)
		}
	}

	// unbind tail: X→NULL returns the ticket to the pending segment.
	if _, err := env.WIs.Patch(v, map[string]json.RawMessage{"bundle_name": json.RawMessage("null")}); err != nil {
		t.Fatalf("unbind %s in R2 window: %v", v, err)
	}
	q2, err := env.WIs.ReadyQueue("alpha")
	if err != nil {
		t.Fatalf("ready queue: %v", err)
	}
	if hasQueuedID(queueIDs(t, q2, "items"), v) {
		t.Fatalf("unbound ticket %s still in the executable segment", v)
	}
	if !hasQueuedID(queueIDs(t, q2, "pending_bundles"), v) {
		t.Fatalf("unbound ticket %s did not return to the pending-bundle segment", v)
	}
}

// TestReadyQueueWaitingOnRetainedValueExclusion (polyforge#149): a
// queued ticket holding EITHER retained waiting_on value ('owner' or
// 'dependency:<slug>') is excluded from the executable `items` segment
// AND the `pending_bundles` segment; clearing waiting_on re-includes it.
func TestReadyQueueWaitingOnRetainedValueExclusion(t *testing.T) {
	env := newEnv(t)
	mk := func(goal string, bundle *string) string {
		wi, err := env.WIs.Create(CreateRequest{
			Project: "alpha", Goal: goal, Reporter: env.UserID,
			BundleName: bundle,
		})
		if err != nil {
			t.Fatalf("create %s: %v", goal, err)
		}
		return wi.ID
	}
	wo := mk("waiting owner ticket", nil)               // waiting_on=owner
	wd := mk("waiting dependency ticket", nil)          // waiting_on=dependency:slug
	bo := mk("bound waiting owner", strPtrTest("q-wo")) // bound + waiting_on=owner

	patch := func(id, v string) {
		t.Helper()
		if _, err := env.WIs.Patch(id, map[string]json.RawMessage{
			"waiting_on": json.RawMessage(`"` + v + `"`),
		}); err != nil {
			t.Fatalf("patch waiting_on=%q on %s: %v", v, id, err)
		}
	}
	patch(wo, "owner")
	patch(wd, "dependency:demo-bundle")
	patch(bo, "owner")

	q, err := env.WIs.ReadyQueue("alpha")
	if err != nil {
		t.Fatalf("ready queue: %v", err)
	}
	for _, id := range []string{wo, wd, bo} {
		if hasQueuedID(queueIDs(t, q, "items"), id) {
			t.Fatalf("waiting ticket %s appears in the executable segment", id)
		}
		if hasQueuedID(queueIDs(t, q, "pending_bundles"), id) {
			t.Fatalf("waiting ticket %s appears in the pending-bundles segment", id)
		}
	}

	// clearing waiting_on re-includes both (the plain segments by kind).
	clear := func(id string) {
		t.Helper()
		if _, err := env.WIs.Patch(id, map[string]json.RawMessage{"waiting_on": json.RawMessage("null")}); err != nil {
			t.Fatalf("clear waiting_on on %s: %v", id, err)
		}
	}
	clear(wo)
	clear(bo)
	q2, err := env.WIs.ReadyQueue("alpha")
	if err != nil {
		t.Fatalf("ready queue 2: %v", err)
	}
	if !hasQueuedID(queueIDs(t, q2, "pending_bundles"), wo) {
		t.Fatalf("cleared ticket %s not re-included in pending_bundles", wo)
	}
	if !hasQueuedID(queueIDs(t, q2, "items"), bo) {
		t.Fatalf("cleared bound ticket %s not re-included in items", bo)
	}
	if hasQueuedID(queueIDs(t, q2, "items"), wd) || hasQueuedID(queueIDs(t, q2, "pending_bundles"), wd) {
		t.Fatalf("still-waiting ticket %s leaked into a segment", wd)
	}
}
