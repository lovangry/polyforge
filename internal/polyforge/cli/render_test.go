package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// Golden tests: exact-output assertions for the human table renderers.

func golden(t *testing.T, name string, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestGoldenWITable(t *testing.T) {
	bundle := "bundle-x"
	items := []api.WorkItem{
		{Slug: "pf-1", Status: "running", Priority: "high", BundleName: &bundle, Goal: "fix the thing"},
		{Slug: "pf-2", Status: "queued", Priority: "normal", Goal: "a goal that is definitely longer than sixty characters in total"},
	}
	var b bytes.Buffer
	renderWITable(&b, items)
	golden(t, "WITable", b.String(),
		`SLUG  STATUS   PRIORITY  BUNDLE    GOAL
----  -------  --------  --------  --------------------------------------------------------------
pf-1  running  high      bundle-x  fix the thing
pf-2  queued   normal    -         a goal that is definitely longer than sixty characters in t…
`)
}

func TestGoldenReadyWITable(t *testing.T) {
	bundle := "bundle-x"
	items := []api.WorkItem{
		{Slug: "pf-1", Status: "queued", Priority: "normal", BundleName: &bundle, Goal: "fix the thing"},
	}
	unclassified := []api.WorkItem{
		{Slug: "pf-2", Status: "queued", Priority: "normal", Goal: "another goal"},
	}
	var b bytes.Buffer
	renderReadyWITable(&b, items, unclassified)
	golden(t, "ReadyWITable", b.String(),
		`SLUG  STATUS  PRIORITY  BUNDLE    GOAL           SEGMENT
----  ------  --------  --------  -------------  ------------
pf-1  queued  normal    bundle-x  fix the thing  items
pf-2  queued  normal    -         another goal   unclassified
`)

	b.Reset()
	renderReadyWITable(&b, nil, nil)
	golden(t, "ReadyWITableEmpty", b.String(),
		`SLUG  STATUS  PRIORITY  BUNDLE  GOAL  SEGMENT
----  ------  --------  ------  ----  -------
`)
}

func TestGoldenObserve(t *testing.T) {
	jv := protocol.JobView{
		WorkItem:    "wi_abc",
		Project:     "polyforge",
		Status:      protocol.JobNeedsInput,
		Generation:  2,
		AttemptID:   "ra_9",
		BundleName:  "bundle-x",
		CurrentStep: "implement",
		Steps: map[string]*protocol.StepView{
			"spec":      {Status: "completed", Attempts: 1, Model: "m-a", StartedAt: "t0"},
			"implement": {Status: "in_progress", Attempts: 2, Model: "m-b", StartedAt: "t1"},
		},
		Budget:  json.RawMessage(`{"requests":{"used":3,"ceiling":50}}`),
		Pending: &protocol.PendingView{ID: "q_1", Step: "implement", Question: "Ship now?", AskedAt: "t2"},
		Events: []protocol.EventEntry{
			{At: "t3", Kind: "step_started", Body: json.RawMessage(`{"step":"implement"}`), Seq: 7},
			{At: "t4", Kind: "needs_input", Seq: 8},
		},
	}
	var b bytes.Buffer
	renderObserve(&b, &jv)
	golden(t, "Observe", b.String(),
		`wi_abc (polyforge) — needs_input
attempt: ra_9  generation: 2  bundle: bundle-x
current step: implement

steps:
STEP       STATUS       ATTEMPTS  MODEL  STARTED
---------  -----------  --------  -----  -------
implement  in_progress  2         m-b    t1
spec       completed    1         m-a    t0

budget:
{
  "requests": {
    "used": 3,
    "ceiling": 50
  }
}

pending question:
  q_1 [implement] asked at t2
  Ship now?

events:
AT  KIND          BODY
--  ------------  --------------------
t3  step_started  {"step":"implement"}
t4  needs_input
next cursor: pf observe wi_abc --after 8
`)
}

func TestGoldenObserveEventLatency(t *testing.T) {
	jv := protocol.JobView{
		WorkItem: "wi_lat",
		Project:  "polyforge",
		Status:   protocol.JobRunning,
		Events: []protocol.EventEntry{
			// Both keys: full latency segment.
			{At: "t1", Kind: "commit_push", Body: json.RawMessage(`{"commit_ms":1234,"push_ms":567,"repo":"polyforge"}`), Seq: 1},
			// Only commit_ms: push segment stays blank.
			{At: "t2", Kind: "commit_push", Body: json.RawMessage(`{"commit_ms":42}`), Seq: 2},
			// Only push_ms: commit segment stays blank.
			{At: "t3", Kind: "commit_push", Body: json.RawMessage(`{"push_ms":7}`), Seq: 3},
			// Neither key: renders exactly as before (truncated raw JSON).
			{At: "t4", Kind: "step_started", Body: json.RawMessage(`{"step":"implement"}`), Seq: 4},
			// Invalid JSON: falls back to raw, no error.
			{At: "t5", Kind: "noise", Body: json.RawMessage(`{not json`), Seq: 5},
			// Null body: blank cell, exactly as before.
			{At: "t6", Kind: "needs_input", Body: json.RawMessage(`null`), Seq: 6},
		},
	}
	var b bytes.Buffer
	renderObserve(&b, &jv)
	golden(t, "ObserveEventLatency", b.String(),
		`wi_lat (polyforge) — running
attempt:   generation: 0

events:
AT  KIND          BODY
--  ------------  ---------------------------
t1  commit_push   commit 1234ms · push 567ms
t2  commit_push   commit 42ms
t3  commit_push   push 7ms
t4  step_started  {"step":"implement"}
t5  noise         {not json
t6  needs_input
next cursor: pf observe wi_lat --after 6
`)
}

func TestGoldenObserveHeaderWall(t *testing.T) {
	base := func() protocol.JobView {
		return protocol.JobView{WorkItem: "wi_w", Project: "polyforge", Status: protocol.JobRunning}
	}

	// Budget carrying wall_ms: header gains the cumulative wall figure.
	jv := base()
	jv.Budget = json.RawMessage(`{"usd":1.5,"wall_ms":11520000,"tokens":9000}`)
	var b bytes.Buffer
	renderObserve(&b, &jv)
	golden(t, "ObserveHeaderWall", b.String(),
		`wi_w (polyforge) — running · wall 3.2h
attempt:   generation: 0

budget:
{
  "usd": 1.5,
  "wall_ms": 11520000,
  "tokens": 9000
}
`)

	// Sub-minute wall renders in seconds.
	jv = base()
	jv.Budget = json.RawMessage(`{"wall_ms":12345}`)
	b.Reset()
	renderObserve(&b, &jv)
	golden(t, "ObserveHeaderWallSeconds", b.String(),
		`wi_w (polyforge) — running · wall 12.3s
attempt:   generation: 0

budget:
{
  "wall_ms": 12345
}
`)

	// Budget without wall_ms: header exactly as today (no wall segment).
	jv = base()
	jv.Budget = json.RawMessage(`{"requests":{"used":3,"ceiling":50}}`)
	b.Reset()
	renderObserve(&b, &jv)
	golden(t, "ObserveHeaderNoWall", b.String(),
		`wi_w (polyforge) — running
attempt:   generation: 0

budget:
{
  "requests": {
    "used": 3,
    "ceiling": 50
  }
}
`)

	// Null / empty / zero wall_ms budgets: no wall segment, no "0ms" artifact.
	for name, budget := range map[string]json.RawMessage{
		"null": json.RawMessage(`null`),
		"empty": json.RawMessage(``),
		"zero": json.RawMessage(`{"wall_ms":0}`),
	} {
		jv = base()
		jv.Budget = budget
		b.Reset()
		renderObserve(&b, &jv)
		want := "wi_w (polyforge) — running\nattempt:   generation: 0\n"
		if name == "zero" {
			want += "\nbudget:\n{\n  \"wall_ms\": 0\n}\n"
		}
		golden(t, "ObserveHeader"+name, b.String(), want)
	}
}

func TestGoldenStatus(t *testing.T) {
	s := StatusResponse{
		MachineID: "m1",
		Mode:      protocol.ModeDrain,
		Jobs: []StatusJob{
			{WorkItemID: "wi_abc", Status: "running", AttemptID: "ra_1", AgentPID: 42,
				LastProgressAgoSec: int64Ptr(754)},
			// D6: a job presented as needs_input shows its pending
			// question's id in the QUESTION column.
			{WorkItemID: "wi_ask", Status: "needs_input", AttemptID: "ra_2", AgentPID: 43,
				NeedsInput: true, QuestionID: "q_zz9"},
		},
		Queue: QueueStats{Pending: 1, Running: 2, MaxConcurrent: 2},
	}
	var b bytes.Buffer
	renderStatus(&b, &s)
	golden(t, "Status", b.String(),
		`machine: m1  mode: drain  queue: 1 pending / 2 running / max 2

WORK ITEM  STATUS       ATTEMPT  AGENT PID  QUESTION  LAST-PROGRESS
---------  -----------  -------  ---------  --------  -------------
wi_abc     running      ra_1     42         -         12:34
wi_ask     needs_input  ra_2     43         q_zz9     -
`)
}

func TestGoldenShip(t *testing.T) {
	s := ShipResponse{
		WorkItemID: "wi_abc",
		Branch:     "polyforge/wi_abc",
		Repos: []ShipRepoOutcome{
			{Name: "polyforge", Head: "deadbee", Branch: "polyforge/wi_abc", Pushed: true, PRURL: "https://x/pr/1"},
			{Name: "tsshd", Head: "cafe123", Branch: "polyforge/wi_abc", Reused: true, CompareURL: "https://x/compare"},
		},
	}
	var b bytes.Buffer
	renderShip(&b, &s)
	golden(t, "Ship", b.String(),
		`shipped wi_abc → branch polyforge/wi_abc
  polyforge:         deadbee  pushed
    pr:      https://x/pr/1
  tsshd:             cafe123  reused (remote branch already at the sealed commit)
    compare: https://x/compare
`)
}

func TestGoldenWIDetail(t *testing.T) {
	milestone := "m1"
	labels := []string{"infra"}
	wi := api.WorkItem{
		ID: "wi_abc", Slug: "pf-42", ProjectName: "polyforge",
		Goal: "do the thing", Status: "queued", Priority: "high",
		Milestone: &milestone, Labels: labels, CreatedAt: "t0",
	}
	var b bytes.Buffer
	renderWIDetail(&b, &wi)
	golden(t, "WIDetail", b.String(),
		`pf-42 (wi_abc)
  status:   queued (priority high)
  project:  polyforge
  goal:     do the thing
  milestone: m1
  labels:   infra
  bundle:   -
  created:  t0
`)
}

func TestGoldenMemories(t *testing.T) {
	items := []api.Memory{
		{ID: "mem_1", Type: "experience.debug", Content: "first line\nsecond line", UpdatedAt: "t1", Tags: []string{"go"}},
	}
	var b bytes.Buffer
	renderMemories(&b, items)
	golden(t, "Memories", b.String(),
		`ID     TYPE              CONTENT     UPDATED  TAGS
-----  ----------------  ----------  -------  ----
mem_1  experience.debug  first line  t1       go
`)
}

// int64Ptr is the LAST-PROGRESS golden helper (a *int64 field).
func int64Ptr(v int64) *int64 { return &v }
