package daemon

// heartbeat_test.go: the 04 §7.2 startup-beat regression — "registration IS
// the first heartbeat" (02 §4.10): RunHeartbeat must beat IMMEDIATELY on
// start, not after a full empty interval, so a freshly started daemon is
// registered and observable without a 60s dead window.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestHeartbeatFirstBeatIsImmediate(t *testing.T) {
	var mu sync.Mutex
	beats := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/machines/heartbeat" {
			mu.Lock()
			beats++
			mu.Unlock()
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"registered":true}`))
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	cfg := Config{
		AIHubURL: ts.URL, APIKey: "pfk_hb", MachineID: "mac_hb",
		DataDir:       filepath.Join(dir, "daemon"),
		WorkspaceRoot: filepath.Join(dir, "ws"),
		Adapter:       "pi", Version: "hb-test",
	}
	d, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A deliberately HUGE interval: any beat observed within the bound can
	// only be the immediate startup beat, never a tick.
	go d.RunHeartbeat(ctx, time.Hour)

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := beats
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first heartbeat must fire immediately on start, not wait for a tick")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The beat was recorded in the tracker (the stale-machines signal):
	// with a zero threshold every observed machine is listed, so the
	// tracker's knowledge of mac_hb IS the record of the startup beat.
	deadline = time.Now().Add(2 * time.Second)
	for {
		found := false
		for _, m := range d.beats.Stale(time.Now(), 0) {
			if m.MachineID == "mac_hb" {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the startup beat must be recorded in the beat tracker")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
