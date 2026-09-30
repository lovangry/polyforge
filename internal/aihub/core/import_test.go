package core

// import_test.go: the migration record-import path — the closed status
// set, id/closed_at preservation, seq collision refusal and the
// idempotent skip.

import (
	"encoding/json"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

func newImportEnv(t *testing.T) (*WIManager, *AdminManager, string, string) {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	admin := NewAdminManager(db)
	u, err := admin.CreateUser("importer", "importer@example.com", "admin", "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateProject(CreateProjectRequest{Name: "imp"}); err != nil {
		t.Fatal(err)
	}
	return NewWIManager(db), admin, u.ID, "imp"
}

func TestImportTerminalRecord(t *testing.T) {
	m, _, reporter, project := newImportEnv(t)
	closed := "2026-09-28T10:38:41.336342Z"
	res, err := m.Import(ImportRequest{
		Project: project, ID: "wi_old00000001", Seq: strPtrI(7),
		Goal: "an old wrapped record", Status: api.StatusWrapped,
		Priority: "urgent", Labels: []string{"pilot"}, Reporter: reporter,
		CreatedAt: "2026-09-28T10:23:35.451172Z", UpdatedAt: closed, ClosedAt: &closed,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Outcome != ImportDone {
		t.Fatalf("outcome %q", res.Outcome)
	}
	wi := res.WorkItem
	if wi.ID != "wi_old00000001" || wi.Seq != 7 || wi.Slug != "imp#7" {
		t.Errorf("identity drifted: %s seq=%d", wi.ID, wi.Seq)
	}
	if wi.Status != api.StatusWrapped || wi.ClosedAt == nil || *wi.ClosedAt != closed {
		t.Errorf("terminal record drifted: %+v", wi)
	}
	if wi.Priority != "urgent" || len(wi.Labels) != 1 || wi.Labels[0] != "pilot" {
		t.Errorf("fields lost: %+v", wi)
	}
}

func TestImportIdempotentSkipAndDivergence(t *testing.T) {
	m, _, reporter, project := newImportEnv(t)
	req := ImportRequest{
		Project: project, ID: "wi_old00000001", Goal: "g", Status: api.StatusFailed,
		Reporter: reporter, CreatedAt: "2026-09-28T08:00:00Z", UpdatedAt: "2026-09-28T08:00:00Z",
		ClosedAt: strPtr("2026-09-28T08:00:00Z"),
	}
	if _, err := m.Import(req); err != nil {
		t.Fatalf("first: %v", err)
	}
	res, err := m.Import(req)
	if !IsImportAlreadyExists(err) || res.Outcome != ImportSkipped {
		t.Fatalf("re-import: res=%+v err=%v (want the skip signal)", res, err)
	}
	// A colliding id is one thing; a colliding SEQ on a fresh id is a
	// hard refusal, never a silent bump.
	req2 := req
	req2.ID = "wi_other000001"
	req2.Seq = strPtrI(1)
	if _, err := m.Create(CreateRequest{Project: project, Goal: "fresh", Reporter: reporter}); err != nil {
		t.Fatal(err)
	}
	_, err = m.Import(req2)
	if !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "IMPORT_SEQ_TAKEN" {
		t.Errorf("seq collision: %v", err)
	}
	req2.Seq = nil
	if _, err := m.Import(req2); err != nil {
		t.Fatalf("auto-seq import: %v", err)
	}
}

func TestImportRejectsLiveStateAndBadFields(t *testing.T) {
	m, _, reporter, project := newImportEnv(t)
	base := func(status string) ImportRequest {
		return ImportRequest{
			Project: project, ID: "wi_x0000000001", Goal: "g", Status: status,
			Reporter: reporter, CreatedAt: "2026-09-28T08:00:00Z",
			UpdatedAt: "2026-09-28T08:00:00Z", ClosedAt: strPtr("2026-09-28T08:00:00Z"),
		}
	}
	// live state is never importable
	for _, status := range []string{"queued", "running", "paused", ""} {
		if _, err := m.Import(base(status)); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMPORT_BAD_STATUS" {
			t.Errorf("status %q accepted: %v", status, err)
		}
	}
	// terminal without closed_at
	r := base(api.StatusWrapped)
	r.ClosedAt = nil
	if _, err := m.Import(r); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMPORT_CLOSED_AT_REQUIRED" {
		t.Errorf("wrapped without closed_at: %v", err)
	}
	// blocked never carries closed_at (silently dropped for consistency
	// with the state machine's own transition)
	r = base(api.StatusBlocked)
	res, err := m.Import(r)
	if err != nil || res.WorkItem.ClosedAt != nil {
		t.Errorf("blocked import: %v %+v", err, res.WorkItem)
	}
	// execution-state attrs are rejected exactly like the public create
	r = base(api.StatusWrapped)
	r.Attrs = map[string]any{"current_step": "x"}
	if _, err := m.Import(r); !IsCode(err, api.ErrCoreBadInput) {
		t.Errorf("execution-state attrs accepted: %v", err)
	}
	// bad timestamps
	r = base(api.StatusWrapped)
	r.CreatedAt = "yesterday"
	if _, err := m.Import(r); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMPORT_BAD_TIMESTAMP" {
		t.Errorf("bad timestamp accepted: %v", err)
	}
	// bad id domain
	r = base(api.StatusWrapped)
	r.ID = "../evil"
	if _, err := m.Import(r); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMPORT_BAD_ID" {
		t.Errorf("bad id accepted: %v", err)
	}
	// unknown reporter (the tool must resolve old ids first)
	r = base(api.StatusWrapped)
	r.Reporter = "usr_nobody0000"
	if _, err := m.Import(r); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMPORT_BAD_REPORTER" {
		t.Errorf("unknown reporter accepted: %v", err)
	}
	// unknown target project
	r = base(api.StatusWrapped)
	r.Project = "nope"
	if _, err := m.Import(r); !IsCode(err, api.ErrCoreNotFound) {
		t.Errorf("unknown project accepted: %v", err)
	}
}

func strPtr(s string) *string { return &s }
func strPtrI(i int64) *int64  { return &i }

// The overwrite path: overwrite:true rewrites a same-id record in place
// (new version wins), answers outcome=overwritten and appends one audit
// event per row with an old/new summary; without the flag the skip is
// unchanged.
func TestImportOverwrite(t *testing.T) {
	m, _, reporter, project := newImportEnv(t)
	base := ImportRequest{
		Project: project, ID: "wi_old00000001", Seq: strPtrI(7),
		Goal: "the old goal", Status: api.StatusWrapped, Priority: api.PriorityNormal,
		Labels: []string{"old"}, Reporter: reporter,
		CreatedAt: "2026-09-28T08:00:00Z", UpdatedAt: "2026-09-28T10:00:00Z",
		ClosedAt: strPtr("2026-09-28T10:00:00Z"),
	}
	if _, err := m.Import(base); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// Same-id without overwrite: the skip is unchanged.
	res, err := m.Import(base)
	if !IsImportAlreadyExists(err) || res.Outcome != ImportSkipped {
		t.Fatalf("no-overwrite re-import: res=%+v err=%v (want the skip signal)", res, err)
	}
	// Overwrite: all record fields rewritten in place.
	chg := base
	chg.Goal = "the CHANGED goal"
	chg.Status = api.StatusFailed
	chg.Priority = api.PriorityUrgent
	chg.Labels = []string{"new"}
	chg.UpdatedAt = "2026-09-28T12:00:00Z"
	chg.ClosedAt = strPtr("2026-09-28T12:00:00Z")
	chg.Overwrite = true
	res, err = m.Import(chg)
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if res.Outcome != ImportOverwritten {
		t.Fatalf("outcome %q, want overwritten", res.Outcome)
	}
	wi := res.WorkItem
	if wi.Goal != "the CHANGED goal" || wi.Status != api.StatusFailed ||
		wi.Priority != api.PriorityUrgent || len(wi.Labels) != 1 || wi.Labels[0] != "new" {
		t.Errorf("row not rewritten per the new record: %+v", wi)
	}
	if wi.Seq != 7 || wi.ClosedAt == nil || *wi.ClosedAt != "2026-09-28T12:00:00Z" {
		t.Errorf("identity drifted on overwrite: seq=%d closed_at=%v", wi.Seq, wi.ClosedAt)
	}
	// One audit event per overwritten row, carrying the old/new summary.
	events, err := m.Events(base.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (imported + overwritten)", len(events))
	}
	ev := events[1]
	if ev.Type != "work_item_import_overwritten" {
		t.Fatalf("second event type %q", ev.Type)
	}
	var payload struct {
		Old struct {
			Status string `json:"status"`
			Goal   string `json:"goal"`
		} `json:"old"`
		New struct {
			Status string `json:"status"`
			Goal   string `json:"goal"`
		} `json:"new"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Old.Goal != "the old goal" || payload.Old.Status != api.StatusWrapped {
		t.Errorf("old summary wrong: %+v", payload.Old)
	}
	if payload.New.Goal != "the CHANGED goal" || payload.New.Status != api.StatusFailed {
		t.Errorf("new summary wrong: %+v", payload.New)
	}
	// Seq handling: absent keeps the existing seq; a collision with a
	// DIFFERENT row is refused; a fresh value moves the row.
	chg.Seq = nil
	if _, err := m.Import(chg); err != nil {
		t.Fatalf("seq-less overwrite: %v", err)
	}
	if wi, err := m.Get(base.ID, GetOptions{}); err != nil || wi.Seq != 7 {
		t.Errorf("seq-less overwrite changed seq: %d (%v)", wi.Seq, err)
	}
	if _, err := m.Create(CreateRequest{Project: project, Goal: "fresh", Reporter: reporter}); err != nil {
		t.Fatal(err)
	}
	coll := chg
	coll.Seq = strPtrI(8) // the fresh ticket took seq 8
	if _, err := m.Import(coll); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "IMPORT_SEQ_TAKEN" {
		t.Errorf("overwrite seq collision: %v", err)
	}
	move := chg
	move.Seq = strPtrI(21)
	if _, err := m.Import(move); err != nil {
		t.Fatalf("overwrite seq move: %v", err)
	}
	if wi, err := m.Get(base.ID, GetOptions{}); err != nil || wi.Seq != 21 {
		t.Errorf("seq move lost: %d (%v)", wi.Seq, err)
	}
	// The overwrite answer also rides the admin HTTP face with 200.
}
