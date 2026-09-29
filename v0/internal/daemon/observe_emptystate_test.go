package daemon

// observe_emptystate_test.go: the 04 §5.1a read-through observe must
// render the EMPTY revision chain gracefully instead of hard-erroring
// (polyforge#36). Tri-state against the REAL Core:
//   - never claimed        -> 200, head_seq null, "no published revision yet"
//   - claimed, no publish   -> 200, head_seq null, "no published revision yet
//                               (claimed, first publish pending)" — the
//                               minutes-long first-publish window of every
//                               real WI, which used to surface CORE-504 as
//                               a DAEMON-404 hard error
//   - published HEAD        -> unchanged (covered by observe_readthrough_test.go)
// Plus the predicate audit: ONLY the claimed-empty-chain CORE-504 variant
// is softened; the broken-chain variants keep erroring.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/sync/aihub"
)

// TestIsEmptyChainPendingErrVariants audits every CORE-504 origin
// (internal/core/capsule/capsule.go Store.Progress): only the
// claimed-but-no-published-HEAD detail may render instead of error.
func TestIsEmptyChainPendingErrVariants(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"claimed but no published HEAD (first-publish window)",
			&aihub.APIError{Code: "CORE-504", Detail: "work item wi_x was claimed but has no published HEAD", Status: 502}, true},
		{"execution.json not present at HEAD (broken chain)",
			&aihub.APIError{Code: "CORE-504", Detail: "execution.json not present at HEAD", Status: 502}, false},
		{"block undecodable (broken chain)",
			&aihub.APIError{Code: "CORE-504", Detail: "block sha deadbeef undecodable: zstd: bad magic", Status: 502}, false},
		{"manifest unreadable (broken chain)",
			&aihub.APIError{Code: "CORE-504", Detail: "manifest 3 unreadable: open: no such file", Status: 502}, false},
		{"manifest unparsable (broken chain)",
			&aihub.APIError{Code: "CORE-504", Detail: "manifest 3 unparsable: bad JSON", Status: 502}, false},
		{"same detail under a different code",
			&aihub.APIError{Code: "CORE-404", Detail: "work item wi_x was claimed but has no published HEAD"}, false},
		{"plain error", errors.New("work item was claimed but has no published HEAD"), false},
		{"nil error", nil, false},
	}
	for _, c := range cases {
		if got := isEmptyChainPendingErr(c.err); got != c.want {
			t.Errorf("%s: isEmptyChainPendingErr = %v, want %v", c.name, got, c.want)
		}
	}
}

// newRemoteObserver spins a second daemon (different machine id, no local
// capsule) against the same Core plus its UDS API: the cross-machine
// observer that exercises the read-through layer.
func newRemoteObserver(t *testing.T, e *denv, name string) *udsClient {
	t.Helper()
	home := filepath.Join(e.dataDir, "home_"+name)
	if err := os.MkdirAll(filepath.Join(home, "skills", "pf-execute"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "pf-execute", "SKILL.md"), []byte("# pf-execute\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		AIHubURL: e.ts.url, APIKey: "pfk_daemon_it", MachineID: "mac_" + name,
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		Adapter:       "pi", Version: "it-" + name,
	}
	d2, err := New(cfg, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIServer(d2, filepath.Join(e.dataDir, "sock_"+name+".sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	go api.Serve()
	return &udsClient{path: api.path}
}

// TestObserveReadThroughNeverClaimed: a WI nobody has claimed renders the
// null-HEAD view (head_seq null, the Core's own never-claimed note), not
// an error.
func TestObserveReadThroughNeverClaimed(t *testing.T) {
	e := newDaemonEnv(t)
	// e.wi is created but never claimed: no Submit, no spawn.
	obs := newRemoteObserver(t, e, "nc")
	var resp map[string]any
	if code, err := obs.do("GET", "/jobs/"+e.wi+"/observe?after=0", nil, &resp); err != nil || code != 200 {
		t.Fatalf("never-claimed observe: %d %v", code, err)
	}
	if resp["read_through"] != true {
		t.Fatalf("cross-machine observe must be read-through: %v", resp)
	}
	if resp["head_seq"] != nil {
		t.Fatalf("never-claimed head_seq must be null: %v", resp["head_seq"])
	}
	if fn, _ := resp["freshness_note"].(string); fn != freshnessNoteNeverClaimed {
		t.Fatalf("never-claimed freshness note = %q, want %q", fn, freshnessNoteNeverClaimed)
	}
	snap, _ := resp["state_snapshot"].(map[string]any)
	if len(snap) != 0 {
		t.Fatalf("never-claimed snapshot must be empty: %v", snap)
	}
}

// TestObserveReadThroughClaimedFirstPublishPending: a CLAIMED WI with no
// published revision yet — the first-publish window of every real run —
// must render the empty-chain view (exit-grade 200 answer), not the
// typed CORE-504 / DAEMON-404 hard error it used to produce.
func TestObserveReadThroughClaimedFirstPublishPending(t *testing.T) {
	e := newDaemonEnv(t)
	ctx := context.Background()
	// Claim (spawn writes the owner generation server-side) but publish
	// nothing: the revision chain stays empty.
	if err := e.d.Submit(SubmitIntent{WorkItemID: e.wi, Intent: "claim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.SpawnNext(ctx); err != nil {
		t.Fatal(err)
	}

	obs := newRemoteObserver(t, e, "fp")
	var resp map[string]any
	if code, err := obs.do("GET", "/jobs/"+e.wi+"/observe?after=0", nil, &resp); err != nil || code != 200 {
		t.Fatalf("claimed-unpublished observe must answer 200, got %d %v (body %v)", code, err, resp)
	}
	if resp["read_through"] != true {
		t.Fatalf("cross-machine observe must be read-through: %v", resp)
	}
	if resp["head_seq"] != nil {
		t.Fatalf("empty-chain head_seq must be null: %v", resp["head_seq"])
	}
	if fn, _ := resp["freshness_note"].(string); fn != freshnessNoteFirstPublishPending {
		t.Fatalf("first-publish-pending freshness note = %q, want %q", fn, freshnessNoteFirstPublishPending)
	}
	snap, _ := resp["state_snapshot"].(map[string]any)
	if len(snap) != 0 {
		t.Fatalf("empty-chain snapshot must be empty: %v", snap)
	}
	if evs, _ := resp["events"].([]any); len(evs) != 0 {
		t.Fatalf("empty-chain events must be empty: %v", evs)
	}
}
