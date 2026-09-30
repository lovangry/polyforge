package observe

// stream_test.go: classification against the pinned real record shapes
// (the frozen v0 capture twins). If a harness upgrade renames a record type,
// these fail first.

import (
	"testing"
	"time"
)

func TestStreamFullTurnLifecycle(t *testing.T) {
	s := newStreamState()

	feedRecord := func(rec string) { s.note([]byte(rec)) }

	feedRecord(fxAgentStart)
	feedRecord(fxTurnStart)
	feedRecord(fxResponse) // the daemon's own command echo: NOT progress
	before := s.heartbeat()
	if !before.InFlight {
		t.Fatal("turn_start must mark a turn in flight")
	}
	feedRecord(fxMsgStart)
	feedRecord(fxTextDelta)
	feedRecord(fxMsgEndStop)
	feedRecord(fxTurnEndStop)
	feedRecord(fxAgentEnd)
	feedRecord(fxSettled)

	hb := s.heartbeat()
	if hb.InFlight {
		t.Fatal("turn_end/agent_end/agent_settled must close the turn")
	}
	if hb.Records != before.Records+6 {
		t.Fatalf("records: got %d, want %d (response echo excluded)", hb.Records, before.Records+6)
	}
	if hb.LastRecordAt.IsZero() {
		t.Fatal("lastRecordAt must be set")
	}
	// A silent idle stream (no turn in flight) is never "silent" in the
	// hang sense.
	if hb.Silent(time.Now(), time.Hour, time.Time{}) {
		t.Fatal("idle stream must not be hang-silent")
	}
}

func TestStreamProviderErrorIncidentDedup(t *testing.T) {
	s := newStreamState()
	const detail = "upstream API error (401): 401 status code (no body)"

	s.note([]byte(fxTurnStart))
	// The same failure rides message_start... (not read for incident
	// content in this lane, but it must not break anything)
	s.note([]byte(fxMsgStart))
	// ... message_end ...
	s.note([]byte(fxMsgEndErr(detail)))
	// ... AND turn_end — one incident, upgraded to fatal, never three.
	s.note([]byte(fxTurnEndErr(detail)))

	inc := s.drain()
	if len(inc) != 1 {
		t.Fatalf("incidents: got %d, want exactly 1 (per-turn dedup)", len(inc))
	}
	if inc[0].Kind != IncidentProviderError {
		t.Fatalf("kind: got %s, want %s", inc[0].Kind, IncidentProviderError)
	}
	if !inc[0].Fatal {
		t.Fatal("the turn_end report must mark the incident fatal")
	}
	if inc[0].Detail != detail {
		t.Fatalf("detail not preserved verbatim: %q", inc[0].Detail)
	}
	// A clean turn ends the incident chain.
	s.note([]byte(fxTurnStart))
	s.note([]byte(fxMsgEndStop))
	s.note([]byte(fxTurnEndStop))
	if got := s.cleanTurnCount(); got != 1 {
		t.Fatalf("clean turns: got %d, want 1", got)
	}
}

func TestStreamProviderErrorRepeatsAcrossTurns(t *testing.T) {
	s := newStreamState()
	const d1 = "upstream API error (401)"
	const d2 = "upstream API error (429): rate limit exceeded"

	s.note([]byte(fxTurnStart))
	s.note([]byte(fxMsgEndErr(d1)))
	s.note([]byte(fxTurnEndErr(d1)))
	// New turn → the dedup key clears → a DIFFERENT failure reports.
	s.note([]byte(fxTurnStart))
	s.note([]byte(fxMsgEndErr(d2)))
	s.note([]byte(fxTurnEndErr(d2)))

	inc := s.drain()
	if len(inc) != 2 {
		t.Fatalf("incidents: got %d, want 2 (one per turn)", len(inc))
	}
	if inc[0].Detail != d1 || inc[1].Detail != d2 {
		t.Fatalf("incident order/details wrong: %+v", inc)
	}
	if inc[1].Kind != IncidentRetry {
		t.Fatalf("rate-limit detail must classify as retry, got %s", inc[1].Kind)
	}
}

// TestStreamNonRecordNoiseIsNoise pins the F15 ruling that replaced the
// old "any output is life" rule: an unparseable stdout line is NOISE —
// the pilot's wedge weaponized exactly that assumption.
func TestStreamNonRecordNoiseIsNoise(t *testing.T) {
	s := newStreamState()
	s.note([]byte("some non-JSON stdout line\n"))
	hb := s.heartbeat()
	if hb.Noise != 1 || hb.Records != 0 || hb.LastNoiseAt.IsZero() {
		t.Fatalf("non-record output must classify as noise: %+v", hb)
	}
	if !hb.LastProgressAt.IsZero() {
		t.Fatal("non-record output must never set the progress clock")
	}
}

// TestStreamNoiseVsProgressClassification pins the F15 split: only the
// turn-machinery record families prove progress; the extension UI
// chatter stream (and any other unclassified record) is noise and never
// refreshes the progress clock the watchdogs anchor on.
func TestStreamNoiseVsProgressClassification(t *testing.T) {
	s := newStreamState()

	// The pilot's double-blind ingredient: extension_ui_request records
	// streaming continuously out of a background-task extension.
	const fxExtUI = `{"type":"extension_ui_request","request":{"method":"setStatus"},"seq":1}`
	s.note([]byte(fxExtUI))
	hb := s.heartbeat()
	if hb.LastNoiseAt.IsZero() {
		t.Fatal("extension UI chatter must set the noise clock")
	}
	if !hb.LastProgressAt.IsZero() {
		t.Fatal("extension UI chatter must NEVER set the progress clock")
	}
	if hb.Records != 0 || hb.Noise != 1 {
		t.Fatalf("classification counts: records=%d noise=%d, want 0/1", hb.Records, hb.Noise)
	}
	if !hb.Observable {
		t.Fatal("a noise-only stream is still observable")
	}
	// Noise alone never arms the hang criterion — but with a turn in
	// flight it must not hold it off either (the double-blind shape).
	s.note([]byte(fxTurnStart))
	before := s.heartbeat().LastProgressAt
	for i := 0; i < 20; i++ {
		s.note([]byte(fxExtUI))
	}
	hb = s.heartbeat()
	if !hb.InFlight {
		t.Fatal("turn must be in flight")
	}
	if !hb.LastProgressAt.Equal(before) {
		t.Fatal("a flood of UI noise must not refresh lastProgressAt")
	}
	// Silent is time-based; the pin here is that the effective anchor
	// stayed at the turn-open moment, not at the newest noise record
	// (the noise flood landed AFTER the turn opened — if it counted,
	// neither check below would hold).
	if hb.Silent(before.Add(20*time.Minute), 30*time.Minute, time.Time{}) {
		t.Fatal("sanity: within the window from the true anchor it is not silent yet")
	}
	if !hb.Silent(before.Add(time.Hour), 30*time.Minute, time.Time{}) {
		t.Fatal("past the window from the true anchor it must be silent despite the noise flood")
	}

	// Unclassified JSON record types and unparseable lines are noise too.
	for _, raw := range []string{
		`{"type":"some_future_record","x":1}`,
		`not json at all`,
	} {
		s.note([]byte(raw))
	}
	if got := s.heartbeat().Noise; got != 23 {
		t.Fatalf("noise count: got %d, want 23", got)
	}
	if got := s.heartbeat().Records; got != 1 {
		t.Fatalf("progress count: got %d, want 1 (turn_start only)", got)
	}

	// The progress families all move the progress clock.
	for _, rec := range []string{
		fxResponse, fxMsgStart, fxTextDelta, fxMsgEndStop, fxTurnEndStop,
		fxAgentEnd, fxSettled,
		`{"type":"toolResult","id":"t1"}`,
	} {
		s.note([]byte(rec))
	}
	hb = s.heartbeat()
	if hb.Records != 9 {
		t.Fatalf("progress count: got %d, want 9", hb.Records)
	}
	if hb.LastProgressAt.Before(before) || hb.LastProgressAt.IsZero() {
		t.Fatal("progress records must refresh lastProgressAt")
	}
	if hb.InFlight {
		t.Fatal("turn_end/agent_end/agent_settled must close the turn")
	}
}
