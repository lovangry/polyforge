package runtime

// v15_session_test.go: the v1.5 session-model regressions the lane was
// asked to add — the B1 journal-replay compensation branch, spawn_n
// monotonicity across the recovery ladder, and the OBSuperseded
// kill+quarantine response (M4/m3).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

// An ADOPTED journal replay (verify says the session is ours) whose start
// cannot finish is compensated with SessionEnd(blocked): the session is
// cleared server-side, the journal settles, no job is left behind — the
// D5 rule carried into the session model.
func TestJournalReplayUnstartableCompensatesBlocked(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	sp := &scriptedSpawner{}
	sp.failFresh = 99 // no transcript on a replayed start: every fresh spawn fails
	dataDir := filepath.Join(t.TempDir(), "data")
	r, _, _ := newTestRuntimeAt(t, hub, sp, dataDir)

	session := fakeSession("wi_adoptfail")
	key := randomKey()
	if err := r.noteClaiming("wi_adoptfail", session, key); err != nil {
		t.Fatal(err)
	}
	hub.hold("wi_adoptfail", session, api.StatusRunning)

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	// verify matched (adopted) and the replay issued exactly one claim
	if n := hub.claimCount(); n != 1 {
		t.Fatalf("the adopted entry must be replayed exactly once, got %d claims", n)
	}
	if got := hub.claims[0].SessionID; got != session {
		t.Fatalf("the replay must carry the journaled secret, got %q", got)
	}
	// the unstartable adoption is compensated: session-end blocked,
	// fenced on OUR secret
	if hub.endCount() != 1 {
		t.Fatalf("the failed replay must end the session exactly once, got %d", hub.endCount())
	}
	end := hub.lastEnd()
	if end.Status != api.StatusBlocked || end.SessionID != session {
		t.Fatalf("the compensation must be SessionEnd(blocked) on our secret, got %+v", end)
	}
	// the journal settled and no job dangles
	if items := r.loadClaiming(); len(items) != 0 {
		t.Fatalf("claiming journal must be empty after the compensation, got %+v", items)
	}
	if r.jobOf("wi_adoptfail") != nil {
		t.Fatal("the compensated start must leave no live job")
	}
}

// spawn_n is the per-WI monotonic derivation counter: every recovery
// increments it by EXACTLY one (no new attempt, no generation), the
// announcements carry it, and it survives persistence.
func TestSpawnNMonotonicAcrossRecoveries(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	hub.hold("wi_mono", fakeSession("wi_mono"), api.StatusRunning)
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, hub, sp)
	wi := "wi_mono"
	j := seedJob(r, wi) // spawn_n 1 on the seed

	seen := []int{}
	for i := 0; i < 2; i++ {
		r.handleOB(context.Background(), recoveryMsg(j, protocol.RecoverDeath))
		msg := recvRT(t, toOB)
		if msg.Kind != protocol.RTRecovered {
			t.Fatalf("recovery %d: expected RTRecovered, got %+v", i, msg)
		}
		seen = append(seen, msg.SpawnN)
	}
	// 1 (seed) → 2 → 3: strictly monotonic, one step per recovery
	if len(seen) != 2 || seen[0] != 2 || seen[1] != 3 {
		t.Fatalf("spawn_n must advance one step per recovery, saw %v", seen)
	}
	j.mu.Lock()
	got := j.spawnN
	j.mu.Unlock()
	if got != 3 {
		t.Fatalf("job spawn_n = %d, want 3", got)
	}
	if recs := loadRecords(t, r); len(recs) != 1 || recs[0].SpawnN != 3 {
		t.Fatalf("state.json must persist spawn_n 3, got %+v", recs)
	}
	// the capsule's spawn journal carries the ordinals (the B2 fresh
	// clone source): lines 1..3
	raw, err := os.ReadFile(filepath.Join(j.capsuleDir, "runtime-state", "spawn-journal.jsonl"))
	if err != nil {
		t.Fatalf("spawn journal: %v", err)
	}
	var ordinals []int
	for _, line := range splitJournalLines(string(raw)) {
		var e struct {
			SpawnN int `json:"spawn_n"`
		}
		if json.Unmarshal([]byte(line), &e) == nil {
			ordinals = append(ordinals, e.SpawnN)
		}
	}
	// the seed's installOldAgent bypasses the journal (a raw spawner
	// call), so the journal holds exactly the two recovery derivations.
	if len(ordinals) != 2 || ordinals[0] != 2 || ordinals[1] != 3 {
		t.Fatalf("journal ordinals wrong, want [2 3]: %v", ordinals)
	}
	if h := j.agent; h != nil {
		_ = h.Terminate(2e9)
	}
}

func splitJournalLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			ln := s[start:i]
			if ln != "" {
				out = append(out, ln)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// OBSuperseded (M4): the observe lane's periodic session revalidation
// learned the WI is held elsewhere. The response is NOT a recovery —
// kill the tree, quarantine the capsule in place, drop the record, emit
// the m3 event, and make NO hub transition.
func TestOBSupersededKillsAndQuarantines(t *testing.T) {
	sp := &scriptedSpawner{}
	r, toOB, _ := newTestRuntime(t, newFakeHub(), sp)
	wi := "wi_sup"
	j := seedJob(r, wi)
	old := installOldAgent(t, sp, j)
	oldPID := old.PID()

	var events []struct {
		wi   string
		kind string
	}
	r.cfg.EventTap = func(w, kind string, body map[string]any) {
		events = append(events, struct {
			wi   string
			kind string
		}{w, kind})
	}

	r.handleOB(context.Background(), protocol.OBMsg{
		Kind: protocol.OBSuperseded, SpawnN: j.spawnN,
		WorkItem: wi, Detail: "verify mismatch: held by fp abc123",
	})

	waitDead(t, oldPID)
	if r.jobOf(wi) != nil {
		t.Fatal("the superseded job must leave the live set")
	}
	if recs := loadRecords(t, r); len(recs) != 0 {
		t.Fatalf("the superseded record must be dropped, got %+v", recs)
	}
	// the capsule is quarantined under the contract's sibling naming
	ents, err := os.ReadDir(filepath.Join(r.cfg.WorkspaceRoot, "capsules"))
	if err != nil {
		t.Fatal(err)
	}
	quarantined := false
	for _, e := range ents {
		if e.Name() == wi {
			t.Fatal("the capsule must not remain at its live path")
		}
		if len(e.Name()) > len(wi) && e.Name()[:len(wi)] == wi && e.Name()[len(wi)] == '.' {
			quarantined = true
		}
	}
	if !quarantined {
		t.Fatalf("no quarantined sibling of %s found: %v", wi, ents)
	}
	// the m3 owner-visible event landed
	if len(events) != 1 || events[0].wi != wi || events[0].kind != "session_superseded" {
		t.Fatalf("expected one session_superseded event, got %+v", events)
	}
	// no recovery, no revival, no terminal announcement
	if sp.callCount() != 1 { // only the install derivation
		t.Fatalf("superseded must not re-derive, got %d calls", sp.callCount())
	}
	assertNoRT(t, toOB)
}
