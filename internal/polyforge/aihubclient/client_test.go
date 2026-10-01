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
