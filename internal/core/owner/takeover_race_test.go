package owner

// White-box regression test for the R2C-F9 concurrent-takeover collision
// (02 §2.3/§6.2, INV-2b). ForceTakeover's in-transaction re-check used to
// reread only the WI status; the active generation (g, active) stayed a
// PRE-transaction value. Two concurrent takeovers that both read generation
// g pre-transaction then both end g and both insert g+1 — the loser dying on
// the uq_owner_gen_seq unique index with a raw driver error. forceTakeoverTx
// is the transactional half, so this test drives it directly with exactly
// the state the race produces — a caller whose pre-tx snapshot says
// generation g while the winner has already committed g+1 — and asserts the
// typed CORE-409 TAKEOVER_RACE with nothing written. Mirrors
// endgen_race_test.go / cancel_race_test.go.

import (
	"fmt"
	"sync"
	"testing"

	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
)

func seedTakeoverRace(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc, err := service.Register(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(db, svc)
	for _, q := range []string{
		`INSERT INTO users(id, display_name, kind, role) VALUES ('usr_owner', 'owner', 'human', 'writer')`,
		`INSERT INTO users(id, display_name, kind, role) VALUES ('usr_admin', 'admin', 'human', 'admin')`,
		`INSERT INTO projects(id, name) VALUES ('pj1', 'p1')`,
		`INSERT INTO work_items(id, seq, project_id, goal, status, reporter, bundle_name, bundle_digest,
			workflow_budget_baseline, created_at, updated_at)
			VALUES ('wi_race', 1, 'pj1', 'takeover race fixture', 'running', 'usr_owner', 'demo',
			'` + digest64 + `', '{"requests":100,"tokens":null,"wall_minutes":null}',
			'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO users(id, display_name, kind, role) VALUES ('mac_user_b', 'machine-b', 'machine', 'writer')`,
		`INSERT INTO machines(id, user_id, last_heartbeat_at) VALUES ('mac_b', 'mac_user_b', '2026-01-01T00:00:00Z')`,
		`INSERT INTO owner_generations(work_item_id, generation, owner, started_at)
			VALUES ('wi_race', 1, 'usr_owner', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return m, "wi_race"
}

// TestForceTakeoverTxRefusesStaleGenerationSnapshot: the caller's pre-tx
// snapshot read generation 1 active; the competing takeover committed
// generation 2 before this transaction opened. The in-tx re-read must
// observe generation 2, refuse CORE-409 TAKEOVER_RACE, and write nothing
// (generation 2 stays active and un-ended, no generation 3 exists).
func TestForceTakeoverTxRefusesStaleGenerationSnapshot(t *testing.T) {
	m, wi := seedTakeoverRace(t)
	// The takeover that won the race: generation 1 ended, generation 2
	// active under the new owner.
	if _, err := m.db.Exec(`UPDATE owner_generations SET ended_at='2026-01-01T00:01:00Z', end_reason='takeover'
		WHERE work_item_id=?1 AND generation=1`, wi); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at)
		VALUES (?1, 2, 'usr_admin', '2026-01-01T00:01:00Z')`, wi); err != nil {
		t.Fatal(err)
	}

	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// The loser's transactional half, carrying its stale pre-tx snapshot
	// (generation 1 active — what it read before the winner committed).
	_, err = m.forceTakeoverTx(tx, wi, "usr_admin", "mac_b", 1, true)
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-409" || ae.Reason != "TAKEOVER_RACE" {
		t.Fatalf("stale-snapshot takeover must be refused CORE-409 TAKEOVER_RACE, got %v", ae)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Nothing was written: generation 2 still active, no generation 3.
	var active int
	var maxGen int64
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1 AND ended_at IS NULL`, wi).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := m.db.QueryRow(`SELECT MAX(generation) FROM owner_generations WHERE work_item_id=?1`, wi).Scan(&maxGen); err != nil {
		t.Fatal(err)
	}
	if active != 1 || maxGen != 2 {
		t.Fatalf("refused takeover may write nothing: active=%d maxGen=%d", active, maxGen)
	}

	// Positive control: the SAME transactional half with a FRESH snapshot
	// (generation 2 active) takes over normally — generation 2 ended,
	// generation 3 opened, response carries generation 3's token.
	tx2, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := m.forceTakeoverTx(tx2, wi, "usr_admin", "mac_b", 2, true)
	if err != nil {
		t.Fatalf("fresh-snapshot takeover must succeed: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	if resp.Generation != 3 {
		t.Fatalf("takeover must open generation 3, got %d", resp.Generation)
	}
	if resp.GenerationToken == "" || resp.SecretEpoch == 0 {
		t.Fatalf("takeover response must carry the new token: %+v", resp)
	}
}

// TestForceTakeoverConcurrent: two goroutines race ForceTakeover on the
// same running WI. Both pre-read generation 1 before either transaction
// commits (the store's single connection serializes the rest), so exactly
// one wins and the loser must answer the typed CORE-409 TAKEOVER_RACE —
// never a raw UNIQUE-constraint driver error — and the generation
// sequence must stay strictly sequential (1 ended by takeover, 2 active,
// nothing else).
func TestForceTakeoverConcurrent(t *testing.T) {
	m, wi := seedTakeoverRace(t)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins, races int
	var rawErr []error
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := m.ForceTakeover(wi, "usr_admin", "mac_b")
			mu.Lock()
			defer mu.Unlock()
			switch e := err.(type) {
			case nil:
				wins++
			case *APIError:
				if e.Code == "CORE-409" && e.Reason == "TAKEOVER_RACE" {
					races++
				} else {
					rawErr = append(rawErr, e)
				}
			default:
				rawErr = append(rawErr, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(rawErr) > 0 {
		t.Fatalf("no untyped/refused-differently error may surface: %v", rawErr)
	}
	if wins == 0 {
		t.Fatalf("exactly one takeover must win, got wins=%d races=%d", wins, races)
	}
	// The overwhelmingly common interleave (both pre-read generation 1
	// before the winner's commit) yields wins=1/races=1; the rare
	// fully-serialized chain yields wins=2/races=0. Both keep generations
	// strictly sequential — which is the invariant under test — but a
	// loser that lost MUST have seen the typed refusal.
	if wins+races != 2 {
		t.Fatalf("every goroutine must finish as a win or a typed TAKEOVER_RACE, got wins=%d races=%d", wins, races)
	}

	// Generations strictly sequential: 1..N contiguous, exactly one active.
	rows, err := m.db.Query(`SELECT generation FROM owner_generations WHERE work_item_id=?1 ORDER BY generation`, wi)
	if err != nil {
		t.Fatal(err)
	}
	var gens []int64
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			t.Fatal(err)
		}
		gens = append(gens, g)
	}
	rows.Close()
	for i, g := range gens {
		if g != int64(i+1) {
			t.Fatalf("generations must be strictly sequential 1..N, got %v", gens)
		}
	}
	if len(gens) < 2 {
		t.Fatalf("the winner must have opened a new generation, got %v", gens)
	}
	var active int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1 AND ended_at IS NULL`, wi).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("exactly one active generation must remain, got %d", active)
	}
	var endReasons []string
	rows, err = m.db.Query(`SELECT end_reason FROM owner_generations WHERE work_item_id=?1 AND ended_at IS NOT NULL ORDER BY generation`, wi)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		endReasons = append(endReasons, r)
	}
	rows.Close()
	for i, r := range endReasons {
		if r != "takeover" {
			t.Fatalf("ended generation %d must carry end_reason=takeover, got %q", i+1, r)
		}
	}
	_ = fmt.Sprint
}
