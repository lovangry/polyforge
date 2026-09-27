package workitem_test

// R2C-F10 regressions (02 §2.4/§2.5/§5.1b): waiting_on's empty value is the
// NULL "no waiting" value (§2.4: "waiting_on 非空是「被阻塞」的可见信号" —
// non-empty is the blocked signal; the ready queue filters on it), and
// `blocked` is defined BY its non-empty waiting_on (§2.5: "blocked 是有明确
// 等待对象（waiting_on 非空）"), so clearing waiting_on on a blocked WI is
// refused — unblock (a fresh claim) is the only recovery path.

import (
	"encoding/json"
	"testing"

	"github.com/silgrid/polyforge/internal/core/workitem"
)

// TestPatchWaitingOnEmptyStringNormalizesToNull: PATCH waiting_on:"" must
// store NULL, never a non-NULL empty string — the stored empty string used to make a
// queued EXECUTION ticket read as waiting and land in the ready-queue's
// blocked list even though nothing blocked it.
func TestPatchWaitingOnEmptyStringNormalizesToNull(t *testing.T) {
	h := setup(t)
	d, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "g", Reporter: "usr_worker", BundleName: "demo"})
	if err != nil {
		t.Fatal(err)
	}

	// PATCH "" on a fresh queued ticket: stored NULL, ticket stays
	// executable in the ready queue.
	p, err := h.wi.Patch(d.ID, map[string]json.RawMessage{"waiting_on": json.RawMessage(`""`)})
	if err != nil {
		t.Fatal(err)
	}
	if p.WaitingOn != nil {
		t.Fatalf("PATCH waiting_on:\"\" must read back as NULL, got %q", *p.WaitingOn)
	}
	var raw string
	var isNull bool
	if err := h.db.QueryRow(`SELECT waiting_on IS NULL, IFNULL(waiting_on,'') FROM work_items WHERE id=?1`, d.ID).Scan(&isNull, &raw); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatalf("PATCH waiting_on:\"\" must store NULL, got %q", raw)
	}
	q, err := h.wi.ReadyQueue("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Executable) != 1 || q.Executable[0].ID != d.ID {
		t.Fatalf("queued execution ticket must stay executable: %+v", q.Executable)
	}
	for _, e := range q.Blocked {
		if e.ID == d.ID {
			t.Fatal("nothing blocks the ticket; it must not appear in blocked")
		}
	}

	// Round trip through a real waiting value and back: "owner" makes it
	// blocked-visible, "" clears to NULL and restores executability.
	if _, err := h.wi.Patch(d.ID, map[string]json.RawMessage{"waiting_on": json.RawMessage(`"owner"`)}); err != nil {
		t.Fatal(err)
	}
	q, _ = h.wi.ReadyQueue("p1")
	if len(q.Blocked) != 1 || q.Blocked[0].ID != d.ID || q.Blocked[0].WaitingOn == nil || *q.Blocked[0].WaitingOn != "owner" {
		t.Fatalf("waiting_on=owner must land the ticket in blocked: %+v", q.Blocked)
	}
	if _, err := h.wi.Patch(d.ID, map[string]json.RawMessage{"waiting_on": json.RawMessage(`""`)}); err != nil {
		t.Fatal(err)
	}
	q, _ = h.wi.ReadyQueue("p1")
	if len(q.Executable) != 1 || len(q.Blocked) != 0 {
		t.Fatalf("clearing via \"\" must restore executability: exec=%d blocked=%d", len(q.Executable), len(q.Blocked))
	}

	// Read-path defense for legacy rows: a '' written by the pre-fix code
	// (simulated directly) must read as no-waiting, never as a blocked
	// signal, so the queue cannot mis-sort old data.
	if _, err := h.db.Exec(`UPDATE work_items SET waiting_on='' WHERE id=?1`, d.ID); err != nil {
		t.Fatal(err)
	}
	got, err := h.wi.Get(d.ID, workitem.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.WaitingOn != nil {
		t.Fatalf("legacy '' row must read as NULL (02 §2.4: empty = no waiting), got %q", *got.WaitingOn)
	}
	q, _ = h.wi.ReadyQueue("p1")
	if len(q.Executable) != 1 || len(q.Blocked) != 0 {
		t.Fatalf("legacy '' row must not block: exec=%d blocked=%d", len(q.Executable), len(q.Blocked))
	}
}

// TestPatchWaitingOnClearOnBlockedRefused: §2.5 defines blocked by its
// non-empty waiting_on; clearing it (null or "") on a blocked WI is
// refused — the row keeps its waiting object — while re-pointing the
// waiting object to another domain value stays legal.
func TestPatchWaitingOnClearOnBlockedRefused(t *testing.T) {
	h := setup(t)
	d, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "g", Reporter: "usr_worker", BundleName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	// Enter blocked the documented way: non-empty waiting_on + blocked
	// status (as EndGeneration reason=block would leave it).
	if _, err := h.wi.Patch(d.ID, map[string]json.RawMessage{"waiting_on": json.RawMessage(`"owner"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE work_items SET status='blocked' WHERE id=?1`, d.ID); err != nil {
		t.Fatal(err)
	}

	for _, clear := range []string{`""`, `null`} {
		p, err := h.wi.Patch(d.ID, map[string]json.RawMessage{"waiting_on": json.RawMessage(clear)})
		if err == nil {
			t.Fatalf("clearing waiting_on (%s) on a blocked WI must be refused, got %+v", clear, p.WaitingOn)
		}
		e := ae(t, err)
		if e.Code != "CORE-400" || e.Reason != "BLOCKED_NEEDS_WAITING_ON" {
			t.Fatalf("clear on blocked: got %v", e)
		}
		// Nothing changed: still blocked with its waiting object.
		got, err := h.wi.Get(d.ID, workitem.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "blocked" || got.WaitingOn == nil || *got.WaitingOn != "owner" {
			t.Fatalf("refused clear may change nothing: %+v", got.WorkItem)
		}
	}

	// Re-pointing the waiting object is legal (still non-empty, still in
	// the §2.4 domain); a domain violation on blocked is still CORE-400.
	p, err := h.wi.Patch(d.ID, map[string]json.RawMessage{"waiting_on": json.RawMessage(`"runtime:mac_a"`)})
	if err != nil || p.WaitingOn == nil || *p.WaitingOn != "runtime:mac_a" {
		t.Fatalf("re-pointing waiting_on on a blocked WI must be accepted: %v %+v", err, p.WaitingOn)
	}
	_, err = h.wi.Patch(d.ID, map[string]json.RawMessage{"waiting_on": json.RawMessage(`"nonsense"`)})
	if e := ae(t, err); e.Code != "CORE-400" {
		t.Fatalf("domain violation must stay CORE-400: %v", e)
	}

	// Clearing on non-blocked statuses stays legal: queued (already
	// covered above) and paused (human-suspended, no waiting object by
	// definition, §2.5).
	if _, err := h.db.Exec(`UPDATE work_items SET status='paused', waiting_on=NULL WHERE id=?1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.wi.Patch(d.ID, map[string]json.RawMessage{"waiting_on": json.RawMessage(`""`)}); err != nil {
		t.Fatalf("clearing on a paused WI must be legal: %v", err)
	}
}
