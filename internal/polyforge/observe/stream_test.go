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

func TestStreamNonRecordNoiseIsProgress(t *testing.T) {
	s := newStreamState()
	s.note([]byte("some non-JSON stdout line\n"))
	hb := s.heartbeat()
	if hb.Records != 1 || hb.LastRecordAt.IsZero() {
		t.Fatal("unknown stream output still proves agent progress")
	}
}
