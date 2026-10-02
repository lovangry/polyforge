package aihubclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestQueueMergesUnclassified pins the F8 semantics: both executable
// segments (items + unclassified) are takeable by drain; the
// needs_human_session segment is never included.
func TestQueueMergesUnclassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"project":             "p",
			"items":               []map[string]any{{"id": "wi_a", "status": "queued", "goal": "a"}},
			"needs_human_session": []map[string]any{{"id": "wi_h", "status": "queued", "goal": "h"}},
			"unclassified":        []map[string]any{{"id": "wi_u", "status": "queued", "goal": "u"}},
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	got, err := c.Queue("p")
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 takeable (items+unclassified), got %d: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, wi := range got {
		ids[wi.ID] = true
	}
	if !ids["wi_a"] || !ids["wi_u"] || ids["wi_h"] {
		t.Fatalf("wrong segment membership: %+v", ids)
	}
}

// TestListMachinesPinsRouteAndDecoding: the machines list client call
// hits GET /api/v1/machines with the bearer key; the typed decode covers
// runtime_version / live_wi_ids / stale / heartbeat_age_seconds; and the
// raw variant passes the server body through byte-for-byte.
func TestListMachinesPinsRouteAndDecoding(t *testing.T) {
	// Rows with and without runtime_version, live_wi_ids empty and
	// non-empty, plus an unknown field and non-canonical key order — a
	// body the raw variant must hand through untouched.
	const body = `{"machines":[` +
		`{"stale":false,"live_wi_ids":["wi_a","wi_b"],"mode":"agent","id":"m_1",` +
		`"heartbeat_age_seconds":42,"last_heartbeat_at":"t1",` +
		`"runtime_version":"go1.99","user_id":"usr_1","future_field":true},` +
		`{"id":"m_2","user_id":"usr_2","live_wi_ids":[],` +
		`"last_heartbeat_at":"t2","stale":true}` +
		`],"count":2,"unknown_top":"kept","stale_after_seconds":120}`
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")

	out, err := c.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/api/v1/machines" || gotAuth != "Bearer k" {
		t.Fatalf("request shape: %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if out.Count != 2 || out.StaleAfterSeconds != 120 || len(out.Machines) != 2 {
		t.Fatalf("list decode: %+v", out)
	}
	m0 := out.Machines[0]
	if m0.ID != "m_1" || m0.RuntimeVersion != "go1.99" || m0.Mode != "agent" ||
		m0.UserID != "usr_1" || len(m0.LiveWIIDs) != 2 || m0.LiveWIIDs[0] != "wi_a" ||
		m0.LiveWIIDs[1] != "wi_b" || m0.HeartbeatAgeSeconds == nil ||
		*m0.HeartbeatAgeSeconds != 42 || m0.LastHeartbeatAt != "t1" || m0.Stale {
		t.Fatalf("m_1 decode: %+v", m0)
	}
	m1 := out.Machines[1]
	if m1.ID != "m_2" || m1.RuntimeVersion != "" || m1.Mode != "" ||
		len(m1.LiveWIIDs) != 0 || m1.HeartbeatAgeSeconds != nil || !m1.Stale {
		t.Fatalf("m_2 decode: %+v", m1)
	}

	raw, err := c.ListMachinesRaw()
	if err != nil {
		t.Fatalf("ListMachinesRaw: %v", err)
	}
	if string(raw) != body {
		t.Fatalf("raw passthrough altered the body:\n got: %s\nwant: %s", raw, body)
	}
}

// TestSessionHolderPinsRouteAndDecoding (§1.7): the client method hits
// GET /api/v1/work-items/{wi}/session with the bearer key and decodes
// both the held body and the unheld (flat-omitted) body.
func TestSessionHolderPinsRouteAndDecoding(t *testing.T) {
	held := `{"work_item_id":"wi_x","status":"running",` +
		`"session_fingerprint":"cafe0123abcd4567","machine_id":"m1",` +
		`"session_owner":"op","session_started_at":"2026-10-01T00:00:00Z",` +
		`"clean_pause":false}`
	unheld := `{"work_item_id":"wi_x","status":"queued","clean_pause":false}`
	body := held
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")

	resp, err := c.SessionHolder("wi_x")
	if err != nil {
		t.Fatalf("held: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/api/v1/work-items/wi_x/session" || gotAuth != "Bearer k" {
		t.Fatalf("request shape: %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if resp.WorkItemID != "wi_x" || resp.Status != "running" ||
		resp.SessionFingerprint != "cafe0123abcd4567" || resp.MachineID != "m1" ||
		resp.SessionOwner != "op" || resp.SessionStartedAt != "2026-10-01T00:00:00Z" || resp.CleanPause {
		t.Fatalf("held decode: %+v", resp)
	}

	body = unheld
	resp, err = c.SessionHolder("wi_x")
	if err != nil {
		t.Fatalf("unheld: %v", err)
	}
	if resp.WorkItemID != "wi_x" || resp.Status != "queued" || resp.CleanPause ||
		resp.SessionFingerprint != "" || resp.MachineID != "" ||
		resp.SessionOwner != "" || resp.SessionStartedAt != "" {
		t.Fatalf("unheld decode: %+v", resp)
	}
}
