package daemon

// close_during_claim_test.go — a wrap or pause of a work item whose start
// is still on its claim's hub calls (no CLAIMING placeholder yet) answers
// the in-progress conflict, not "no local job".

import (
	"errors"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

func TestCloseDuringClaimHubCallIsConflict(t *testing.T) {
	td := newTestDaemon(t, nil)
	const wi = "wi_close_claiming"
	release := make(chan struct{})
	entered := make(chan struct{})
	td.hub.mu.Lock()
	td.hub.status[wi] = api.StatusQueued
	// the first claim's answer is lost; the requeued start verifies the
	// journaled claim, and that verify hangs
	td.hub.claimLost = 1
	td.hub.blockVerify = release
	td.hub.verifyEntered = entered
	td.hub.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	mkBare(t, td.cfg.GitRemoteBase, wi)
	if _, err := td.client.ClaimWithModels(wi, "normal", nil); err != nil {
		t.Fatalf("claim: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the requeued start never reached its journal verify")
	}
	for _, close := range []struct {
		name string
		do   func() error
	}{
		{"wrap", func() error { _, err := td.client.Wrap(wi, ""); return err }},
		{"pause", func() error { _, err := td.client.Pause(wi); return err }},
	} {
		err := close.do()
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrDaemonConflict {
			t.Fatalf("%s during the claim's hub call = %v, want DAEMON-409", close.name, err)
		}
	}
	close(release)
	td.waitLive(t, wi)
}
