package cli

// observe_emptystate_test.go: `pf observe` must render the EMPTY revision
// chain gracefully (polyforge#36) at every layer. Tri-state against stub
// faces, all asserting exit 0 (no error exit codes):
//   (a) queued/never-claimed wi  -> "(never claimed)" view (unchanged)
//   (b) claimed, no revisions   -> the new first-publish-pending view,
//                                  mapped from the typed CORE-504
//   (c) published wi            -> the HEAD view (unchanged)
// plus the guard: a DIFFERENT CORE-504 (broken published chain) still
// hard-errors, and the daemon-answered graceful view renders too.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// neverClaimedBody is the Core's row-6 200 answer for a wi nobody claimed
// (head_seq null, internal/server/handlers_wi.go hProgress).
const neverClaimedBody = `{"work_item_id":"wi_a","head_seq":null,"head_generation":null,"progress":null,"head_updated_at":null,"freshness_note":"no published revision yet"}`

// claimedNoHeadEnvelope is the typed row-6 answer for the first-publish
// window: CORE-504 (HTTP 502 via errStatus) with the deterministic detail.
func claimedNoHeadEnvelope() string {
	return envelope("CORE-504", "", "work item wi_a was claimed but has no published HEAD")
}

func TestObserveReadThroughNeverClaimedRendersGracefully(t *testing.T) {
	var aCap captured
	aihub := newCapturingServer(t, 200, neverClaimedBody, &aCap)
	code, out, errOut := runCLI(t, testCtxConfig(aihub.URL), "", "observe", "wi_a", "--read-through")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "no published revision yet") {
		t.Errorf("never-claimed freshness note missing: %q", out)
	}
	if !strings.Contains(out, "head: (never claimed)") {
		t.Errorf("never-claimed head line missing: %q", out)
	}
}

func TestObserveReadThroughClaimedFirstPublishPending(t *testing.T) {
	var aCap captured
	aihub := newCapturingServer(t, 502, claimedNoHeadEnvelope(), &aCap)

	// --read-through: the aihub face answers the typed CORE-504 directly;
	// the CLI must map it to the empty-chain view, exit 0.
	code, out, errOut := runCLI(t, testCtxConfig(aihub.URL), "", "observe", "wi_a", "--read-through")
	if code != 0 {
		t.Fatalf("claimed-unpublished observe must exit 0, got %d (stderr %s)", code, errOut)
	}
	if !strings.Contains(out, "no published revision yet (claimed, first publish pending)") {
		t.Errorf("first-publish-pending note missing: %q", out)
	}
	if !strings.Contains(out, "head: (claimed, first publish pending)") {
		t.Errorf("pending head line missing: %q", out)
	}

	// The default two-layer path (daemon answers DAEMON-404 -> read-through
	// fallback) must degrade the same way.
	var dCap captured
	sockPath := filepath.Join(t.TempDir(), "polyforge.sock")
	startUnixDaemonAtPath(t, sockPath, 404, envelope("DAEMON-404", "", "no local job and no published progress for wi_a"), &dCap)
	cfg := testCtxConfig(aihub.URL)
	cfg.DaemonSocket = sockPath
	code, out, errOut = runCLI(t, cfg, "", "observe", "wi_a")
	if code != 0 {
		t.Fatalf("fallback observe must exit 0, got %d (stderr %s)", code, errOut)
	}
	if dCap.Path != "/jobs/wi_a/observe?after=0" {
		t.Errorf("daemon layer not tried first: %s", dCap.Path)
	}
	if !strings.Contains(out, "no published revision yet (claimed, first publish pending)") {
		t.Errorf("fallback rendering missing the pending view: %q", out)
	}
}

func TestObserveReadThroughClaimedPendingJSONMode(t *testing.T) {
	// --json must emit a payload (the row-6 null-HEAD shape), not the
	// CORE-504 error envelope.
	var aCap captured
	aihub := newCapturingServer(t, 502, claimedNoHeadEnvelope(), &aCap)
	code, out, errOut := runCLI(t, testCtxConfig(aihub.URL), "", "--json", "observe", "wi_a", "--read-through")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	var body struct {
		WorkItemID    string `json:"work_item_id"`
		HeadSeq       *int64 `json:"head_seq"`
		Progress      any    `json:"progress"`
		FreshnessNote string `json:"freshness_note"`
	}
	if err := jsonUnmarshalStrictForObserve(out, &body); err != nil {
		t.Fatalf("--json output must be one JSON object: %v (%q)", err, out)
	}
	if body.WorkItemID != "wi_a" || body.HeadSeq != nil || body.Progress != nil {
		t.Errorf("synthesized body = %q", out)
	}
	if body.FreshnessNote != "no published revision yet (claimed, first publish pending)" {
		t.Errorf("synthesized freshness note = %q", body.FreshnessNote)
	}
}

func TestObserveDaemonAnsweredPendingViewRenders(t *testing.T) {
	// The daemon itself already performed the graceful read-through
	// (internal/daemon/api.go): the CLI renders its answer, exit 0.
	var dCap captured
	sockPath := filepath.Join(t.TempDir(), "polyforge.sock")
	startUnixDaemonAtPath(t, sockPath, 200,
		`{"events":[],"state_snapshot":{},"head_seq":null,"read_through":true,`+
			`"freshness_note":"no published revision yet (claimed, first publish pending)"}`, &dCap)
	cfg := daemonCtxConfig(sockPath)
	code, out, errOut := runCLI(t, cfg, "", "observe", "wi_a")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "no published revision yet (claimed, first publish pending)") {
		t.Errorf("daemon-answered pending view not rendered: %q", out)
	}
}

func TestObserveBrokenChainCORE504StillErrors(t *testing.T) {
	// Guard (polyforge#36): only the claimed-empty-chain variant is
	// softened. A CORE-504 from a genuinely broken published chain keeps
	// the hard error — read-only faces degrade, they do not erase
	// diagnostics.
	var aCap captured
	aihub := newCapturingServer(t, 502,
		envelope("CORE-504", "", "execution.json not present at HEAD"), &aCap)
	code, _, errOut := runCLI(t, testCtxConfig(aihub.URL), "", "observe", "wi_a", "--read-through")
	if code != 1 {
		t.Fatalf("broken-chain CORE-504 must stay exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "CORE-504") || !strings.Contains(errOut, "execution.json not present at HEAD") {
		t.Errorf("typed diagnostic must surface verbatim: %q", errOut)
	}
}

func TestObservePublishedHEADViewUnchanged(t *testing.T) {
	var aCap captured
	aihub := newCapturingServer(t, 200,
		`{"work_item_id":"wi_a","head_seq":5,"head_generation":1,"progress":{"current_step":"code"},"head_updated_at":"t","freshness_note":"data as of last publish"}`, &aCap)
	code, out, errOut := runCLI(t, testCtxConfig(aihub.URL), "", "observe", "wi_a", "--read-through")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "head: seq=5") || !strings.Contains(out, "current_step") {
		t.Errorf("published HEAD view changed: %q", out)
	}
}

// jsonUnmarshalStrictForObserve is the local strict single-value decode
// (the shared helper lives in the e2e test package).
func jsonUnmarshalStrictForObserve(s string, out any) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("empty output")
	}
	return decodeJSON([]byte(s), out)
}
