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
			"project":              "p",
			"items":                []map[string]any{{"id": "wi_a", "status": "queued", "goal": "a"}},
			"needs_human_session":  []map[string]any{{"id": "wi_h", "status": "queued", "goal": "h"}},
			"unclassified":         []map[string]any{{"id": "wi_u", "status": "queued", "goal": "u"}},
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
