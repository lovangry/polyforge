package daemon

// takeover_requeue_test.go: R2N-5 regression — an orphaned TAKEOVER intent
// requeued by crash recovery must CONSUME the journaled generation token
// (02 §6.2/04 §7.3: the force-takeover call itself ends the old generation
// and opens generation+1 in one response — re-calling it would open ANOTHER
// generation: churn), never re-call ForceTakeover. Only rows still at
// intent_persisted re-run ForceTakeover (that takeover never landed
// server-side).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/silgrid/polyforge/internal/core/testsupport"
)

// takeoverStub is a scripted Core that COUNTS force-takeover calls and
// answers the first one with a generation; the regression is that the
// crashed-requeue second life adds ZERO calls.
type takeoverStub struct {
	mu        sync.Mutex
	takeovers int
	wi        string
}

func (s *takeoverStub) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/work-items/wi_probe_invalid/head"):
		writeStubErr(w, 404, "404", "", "not found")
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/force-takeover"):
		s.mu.Lock()
		s.takeovers++
		s.mu.Unlock()
		boot := map[string]any{
			"work_item_id": s.wi, "bundle_name": "demo", "bundle_digest": "sha256:demo",
			"repo_inventory": []any{},
		}
		writeStubJSON(w, 200, map[string]any{
			"generation": 7, "generation_token": "tok_g7", "secret_epoch": 1,
			"owner": "usr_worker", "capsule_bootstrap": boot,
		})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/head"):
		writeStubErr(w, 404, "404", "", "no head")
	case r.Method == "PUT" && strings.Contains(r.URL.Path, "/blocks/"):
		writeStubJSON(w, 200, map[string]any{"head_seq": 0})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/manifest"):
		writeStubJSON(w, 200, map[string]any{"head_seq": 1})
	default:
		writeStubErr(w, 404, "404", "", "unscripted "+r.Method+" "+r.URL.Path)
	}
}

func (s *takeoverStub) takeoverCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takeovers
}

// takeoverEnv builds the standard offline daemon config over a stub Core.
func takeoverEnv(t *testing.T, wi, url string) Config {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	for _, d := range []string{
		filepath.Join(home, "skills", "pf-execute"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "pf-execute", "SKILL.md"), []byte("# pf-execute\nrun\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The daemon's own workflows checkout must carry the demo bundle: the
	// spawn path materializes the snapshot from it (03 §4.5, fix round 2).
	if err := testsupport.MakeDemoBundle(home); err != nil {
		t.Fatal(err)
	}
	return Config{
		AIHubURL: url, APIKey: "pfk_stub", MachineID: "mac_t",
		DataDir:       filepath.Join(home, "daemon"),
		WorkspaceRoot: filepath.Join(home, "ws"),
		WorkflowsDir:  filepath.Join(home, "workflows"),
		SkillsDir:     filepath.Join(home, "skills"),
		Adapter:       "pi", Version: "takeover-requeue-test",
	}
}

// TestCrashedTakeoverRequeueConsumesJournal: the first life runs the
// takeover (ONE Core call), crashes before derivation; the restarted
// daemon's re-run spawn must proceed from the JOURNALED generation and add
// ZERO further ForceTakeover calls.
func TestCrashedTakeoverRequeueConsumesJournal(t *testing.T) {
	wi := "wi_to01"
	stub := &takeoverStub{wi: wi}
	ts := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(ts.Close)

	// --- first life: the takeover lands, the derivation "crashes" ---
	cfg := takeoverEnv(t, wi, ts.URL)
	d1, err := New(cfg, &failingSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d1.Submit(SubmitIntent{WorkItemID: wi, Intent: "takeover"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d1.SpawnNext(context.Background()); err == nil {
		t.Fatal("the simulated-crash spawn must fail")
	}
	if n := stub.takeoverCalls(); n != 1 {
		t.Fatalf("the first life must have run exactly ONE force-takeover, got %d", n)
	}
	st, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	var orphan *PendingJob
	for i := range st.PendingJobs {
		if st.PendingJobs[i].WorkItemID == wi {
			orphan = &st.PendingJobs[i]
		}
	}
	if orphan == nil || orphan.Status != PendingSessionPersisted {
		t.Fatalf("a session_persisted orphan must survive the crash: %+v", st.PendingJobs)
	}
	if orphan.Generation != 7 || orphan.GenerationToken != "tok_g7" || orphan.CapsuleBootstrap == "" {
		t.Fatalf("the journal must carry the generation, token AND bootstrap for the re-run: %+v", orphan)
	}

	// --- restart: crash recovery requeues the orphan; the re-run spawn
	// consumes the journal and NEVER re-calls ForceTakeover ---
	d2, err := New(cfg, newIntegSpawner())
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := d2.jobq.LenPending(); n != 1 {
		t.Fatalf("the orphan must be requeued exactly once, pending=%d", n)
	}
	if _, err := d2.SpawnNext(context.Background()); err != nil {
		t.Fatalf("the re-run takeover spawn must complete from the journal: %v", err)
	}
	if n := stub.takeoverCalls(); n != 1 {
		t.Fatalf("R2N-5: the crashed takeover requeue must add ZERO force-takeover calls, saw %d total", n)
	}
	// The spawn proceeded with the JOURNALED generation and token.
	d2.mu.Lock()
	jr := d2.jobs[wi]
	d2.mu.Unlock()
	if jr == nil {
		t.Fatal("the re-run job must be live in the runtime")
	}
	if jr.token != "tok_g7" {
		t.Fatalf("the re-run must use the journaled generation token, got %q", jr.token)
	}
	if got := d2.jobGeneration(wi); got != 7 {
		t.Fatalf("the re-run must continue the journaled generation 7, got %d", got)
	}
	// The journal is clean and the formal jobs[] record landed.
	st2, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.PendingJobs) != 0 || st2.FindJob(wi) == nil {
		t.Fatalf("the completed re-run must clear the journal and record the job: %+v", st2)
	}
	if j := st2.FindJob(wi); j.Generation != 7 {
		t.Fatalf("the jobs[] record must carry the journaled generation 7, got %d", j.Generation)
	}
}

// TestTakeoverIntentPersistedStillRunsForceTakeover: an orphaned takeover
// row still at intent_persisted (the takeover NEVER landed server-side)
// MUST re-run ForceTakeover on the requeue — consuming the journal there
// would strand the WI unclaimed.
func TestTakeoverIntentPersistedStillRunsForceTakeover(t *testing.T) {
	wi := "wi_to02"
	stub := &takeoverStub{wi: wi}
	ts := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(ts.Close)

	cfg := takeoverEnv(t, wi, ts.URL)
	d, err := New(cfg, &failingSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between the intent persist and the Core call:
	// the journal holds an intent_persisted row.
	if err := d.state.UpdatePending(d.st, PendingJob{
		WorkItemID: wi, IdempotencyKey: "pf-wi_to02-takeover-1", Intent: "takeover",
		CreatedAt: NowStamp(), Status: PendingIntentPersisted,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.RecoverFromState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SpawnNext(context.Background()); err == nil {
		t.Fatal("the failing spawner must fail the spawn (the Core call is what is asserted)")
	}
	if n := stub.takeoverCalls(); n != 1 {
		t.Fatalf("an intent_persisted takeover orphan MUST re-run ForceTakeover exactly once, got %d", n)
	}
	// The claim_confirmed row the re-run wrote must now journal the
	// bootstrap for a potential next crash.
	st, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	row := st.FindPending(wi, "pf-wi_to02-takeover-1")
	if row == nil || row.Status != PendingSessionPersisted {
		t.Fatalf("the failed re-run must keep the journal row: %+v", st.PendingJobs)
	}
	var boot map[string]any
	if err := json.Unmarshal([]byte(row.CapsuleBootstrap), &boot); err != nil || boot["bundle_name"] != "demo" {
		t.Fatalf("the claim_confirmed row must journal the capsule_bootstrap, got %q", row.CapsuleBootstrap)
	}
}

// TestFreshTakeoverKeyAfterSpawnFailureKeepsStateLoadable (R3N-F7): a
// spawn-failure leftover journal row (old key, session_persisted) followed
// by a FRESH takeover admission (new key) used to APPEND a second
// valid-looking row for the same WI — the next crash then made LoadOrAdopt
// reject the daemon's own state as corrupt. UpdatePending now supersedes
// any existing row for the WI, so the journal stays one-row-per-WI and
// the post-crash load succeeds.
func TestFreshTakeoverKeyAfterSpawnFailureKeepsStateLoadable(t *testing.T) {
	wi := "wi_f7e2e"
	stub := &takeoverStub{wi: wi}
	ts := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(ts.Close)

	cfg := takeoverEnv(t, wi, ts.URL)
	d, err := New(cfg, &failingSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	// First takeover: lands server-side, the derivation "crashes" — the
	// journal keeps a session_persisted row under key K1.
	if err := d.Submit(SubmitIntent{WorkItemID: wi, Intent: "takeover"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SpawnNext(context.Background()); err == nil {
		t.Fatal("the first takeover spawn must fail (simulated crash)")
	}
	// Fresh takeover admission: a NEW idempotency key for the same WI.
	if err := d.Submit(SubmitIntent{WorkItemID: wi, Intent: "takeover"}); err != nil {
		t.Fatalf("the fresh takeover must be admitted: %v", err)
	}
	if _, err := d.SpawnNext(context.Background()); err == nil {
		t.Fatal("the second takeover spawn must fail (simulated crash)")
	}

	// The crash consequence: the next daemon life must be able to LOAD its
	// own state file, with exactly ONE journal row (the fresh key's).
	st, err := NewStateStore(cfg.DataDir).Load()
	if err != nil {
		t.Fatalf("R3N-F7: the state must load after the fresh-key takeover: %v", err)
	}
	rows := 0
	for _, p := range st.PendingJobs {
		if p.WorkItemID == wi {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("exactly one journal row may exist for %s, got %+v", wi, st.PendingJobs)
	}
	if row := st.FindPending(wi, ""); row == nil || row.Status != PendingSessionPersisted {
		t.Fatalf("the surviving row must be the fresh takeover's: %+v", st.PendingJobs)
	}
}
