package server_test

// R3C test-gap regression (06 §5.1 row 22/24, R2C-F3): the memory detail
// route (hGetMemory) and the anonymous share-consume route (hConsumeShare)
// must serve the entry metadata and its bytes from ONE snapshot, captured
// inside a single hold of the entry's per-mem write mutex (Mems.ReadView).
// The pre-R2C-F3 shape authorized against Get's metadata and then read
// Content under a separate lock window: an owner PATCH flipping the entry
// private with a new body in between disclosed the NEW private body to a
// non-owner whose authorization judged the OLD metadata.
//
// Three legs, one per observable:
//
//  1. TestGetMemoryJudgesVisibilityInsideReadViewWindow — deterministic
//     choreography through the real HTTP face: the read is parked on the
//     entry lock (held through the store's own public ReadView), the
//     private-flip a finished PATCH would leave behind commits while it is
//     parked, and the resumed request must judge the POST-flip state and
//     refuse — never serve the flipped body (the handler-level analogue of
//     internal/memory's TestReadViewAuthorizesAndServesOneSnapshot).
//
//  2. TestDetailAndShareReadsPairEntryWithBytes — concurrent PATCH/read
//     loop: every 200 response must pair the entry row with the exact
//     bytes it certifies (content_len/content_sha), on both the detail GET
//     (with the derived html from the same hold) and the share consume.
//
//  3. TestMemoryDetailHandlersStayOnReadView — the shape pin. The
//     two-window race lives in the gap between two lock acquisitions,
//     which is externally unobservable: no black-box choreography can park
//     a reverted request inside that gap, so the deterministic
//     "reverting the handler to Get+Content fails the test" guarantee is
//     the source-shape pin: both handlers must call Mems.ReadView and must
//     not call Mems.Get / Mems.Content / memForRead.

import (
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/memory"
	"github.com/silgrid/polyforge/internal/server"
)

// newEnvWithHandle is newEnv plus the *server.Server handle, so the test
// can hold the per-mem entry lock through the store's public ReadView (its
// authorized predicate runs INSIDE the lock) while the real mux serves
// requests in-process.
func newEnvWithHandle(t *testing.T) (*env, *server.Server) {
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
	if err := testsupport.SeedBase(db); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.MakeDemoBundle(dir); err != nil {
		t.Fatal(err)
	}
	srv := server.New(db, dir, filepath.Join(dir, "workflows"), svc)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	e := &env{ts: ts, client: ts.Client(), db: db, dir: dir}
	e.writer = e.mintKey(t, db, "usr_worker", nil)
	e.admin = e.mintKey(t, db, "usr_admin", nil)
	return e, srv
}

// holdEntryLock takes the entry's per-mem write mutex through the store's
// public ReadView (the authorized predicate runs inside the lock) and
// returns only once the lock is actually held. The release function is
// idempotent. This is the same critical section the handlers' own
// ReadView calls must queue behind.
func holdEntryLock(srv *server.Server, memID string) (release func()) {
	entered := make(chan struct{})
	released := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, _, err := srv.Mems.ReadView(memID, func(*memory.Entry) (bool, error) {
			close(entered)
			<-released
			return true, nil
		}, false)
		done <- err
	}()
	<-entered // the predicate is running: the lock is ours
	var once sync.Once
	return func() {
		once.Do(func() {
			close(released)
			<-done
		})
	}
}

// TestGetMemoryJudgesVisibilityInsideReadViewWindow: the member's GET is
// parked on the entry lock (the state a PATCH holding the mutex leaves it
// in); the visibility flip a finished owner PATCH would commit lands while
// it is parked; the resumed read must judge the POST-flip row and refuse —
// the flipped body is never served to the non-owner.
func TestGetMemoryJudgesVisibilityInsideReadViewWindow(t *testing.T) {
	e, srv := newEnvWithHandle(t)
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": "snapshot window body windowprobe",
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	memID := m["id"].(string)
	other := e.mintKey(t, e.db, "usr_other", nil)

	release := holdEntryLock(srv, memID)
	defer release()

	type resp struct {
		st int
		m  map[string]any
	}
	got := make(chan resp, 1)
	go func() {
		st, m, _ := e.do(t, "GET", "/api/v1/memories/"+memID, other, nil)
		got <- resp{st, m}
	}()
	select {
	case r := <-got:
		t.Fatalf("GET completed while the entry lock was held (authorize/read not one lock window): st=%d", r.st)
	case <-time.After(200 * time.Millisecond):
		// Parked on the lock: the handler's decision cannot run past a
		// writer holding the entry's critical section.
	}

	// The finished PATCH's committed state, minus the lock: the row is now
	// private (the file/body is untouched, so the row stays INV-7f
	// consistent and the resumed read genuinely re-validates it).
	if _, err := e.db.Exec(`UPDATE memories SET visibility='private' WHERE id=?1`, memID); err != nil {
		t.Fatal(err)
	}
	release()

	select {
	case r := <-got:
		if r.st != 404 {
			t.Fatalf("read judging the post-flip row must refuse the non-owner: st=%d", r.st)
		}
		if body, _ := r.m["content"].(string); strings.Contains(body, "windowprobe") {
			t.Fatal("refused read still served the flipped body")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GET did not resume after the lock was released")
	}
	// The owner still reads the same committed state fine.
	st, m, _ = e.do(t, "GET", "/api/v1/memories/"+memID, e.writer, nil)
	if st != 200 || !strings.Contains(m["content"].(string), "windowprobe") {
		t.Fatalf("owner read of the flipped state: st=%d", st)
	}
}

// TestDetailAndShareReadsPairEntryWithBytes: under a concurrent PATCH loop,
// every 200 detail GET (markdown + derived html) and every anonymous share
// consume must pair the entry row with the exact bytes it certifies —
// content_len matches the served bytes, content_sha matches their digest,
// and the html derives from the same hold. A Get+Content revert straddles
// PATCH commits and pairs an old row with new bytes.
func TestDetailAndShareReadsPairEntryWithBytes(t *testing.T) {
	e, _ := newEnvWithHandle(t)
	st, m, _ := e.do(t, "POST", "/api/v1/memories", e.writer, map[string]any{
		"project": "p1", "type": "fact.note", "visibility": "project",
		"content": "stress body stressmarker-0",
	})
	if st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	memID := m["id"].(string)
	st, m, _ = e.do(t, "POST", "/api/v1/memories/"+memID+"/share", e.writer, nil)
	if st != 200 {
		t.Fatalf("share: %d %v", st, m)
	}
	shareTok := m["share_token"].(string)
	other := e.mintKey(t, e.db, "usr_other", nil)

	// pad widens the PATCH/ReadView work inside each lock hold, so a
	// straddling revert cannot slip between the cracks of tiny bodies.
	pad := strings.Repeat("x", 32*1024)
	const iters = 30
	for i := 1; i <= iters; i++ {
		marker := fmt.Sprintf("stressmarker-%d", i)
		body := "stress body " + marker + "\n" + pad

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { // the owner PATCH racing the reads
			defer wg.Done()
			<-start
			if st, m, _ := e.do(t, "PATCH", "/api/v1/memories/"+memID, e.writer, map[string]any{"content": body}); st != 200 {
				t.Errorf("patch %d: %d %v", i, st, m)
			}
		}()
		go func() { // the reads racing the PATCH
			defer wg.Done()
			<-start
			if st, m, _ := e.do(t, "GET", "/api/v1/memories/"+memID+"?format=html", other, nil); st == 200 {
				assertSnapshotPairs(t, "detail GET", m)
				content, _ := m["content"].(string)
				html, _ := m["html"].(string)
				markerInContent := strings.Contains(content, "stressmarker-")
				markerInHTML := strings.Contains(html, "stressmarker-")
				if markerInContent != markerInHTML {
					t.Errorf("detail GET %d: html derives from a different snapshot than the served markdown", i)
				}
			}
			if st, m, _ := e.do(t, "GET", "/api/v1/share/"+shareTok, "", nil); st == 200 {
				assertSnapshotPairs(t, "share consume", m)
			}
		}()
		close(start)
		wg.Wait()
	}
}

// assertSnapshotPairs checks the single-snapshot invariant on one 200
// response: the entry row and the served bytes are one committed state.
func assertSnapshotPairs(t *testing.T, what string, m map[string]any) {
	t.Helper()
	content, _ := m["content"].(string)
	entry, _ := m["entry"].(map[string]any)
	if entry == nil {
		t.Errorf("%s: response carries no entry view", what)
		return
	}
	if ln, ok := entry["content_len"].(float64); !ok || int64(ln) != int64(len(content)) {
		t.Errorf("%s: entry and bytes straddle a snapshot (content_len=%v, len(content)=%d)", what, entry["content_len"], len(content))
	}
	sum := sha256.Sum256([]byte(content))
	if sha, _ := entry["content_sha"].(string); sha != fmt.Sprintf("%x", sum) {
		t.Errorf("%s: entry content_sha does not certify the served bytes", what)
	}
}

// TestMemoryDetailHandlersStayOnReadView: the shape pin. The two-window
// disclosure race of a Get+Content revert lives in the gap between two lock
// acquisitions, which no external choreography can park a request inside,
// so the deterministic "reverting fails the test" guarantee pins the
// handler shape itself: hGetMemory and hConsumeShare must take the
// single-snapshot path (Mems.ReadView) and must not fall back to the
// two-window Get/Content shape (nor route the detail read through
// memForRead, whose metadata read is a separate lock window from the byte
// read).
func TestMemoryDetailHandlersStayOnReadView(t *testing.T) {
	raw, err := os.ReadFile("handlers_memory.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, tc := range []struct {
		name   string
		decl   string
		must   []string
		forbid []string
	}{
		{name: "hGetMemory", decl: "func (s *Server) hGetMemory(",
			must:   []string{"s.Mems.ReadView("},
			forbid: []string{"s.Mems.Content(", "s.Mems.Get(", "s.memForRead("}},
		{name: "hConsumeShare", decl: "func (s *Server) hConsumeShare(",
			must:   []string{"s.Mems.ReadView("},
			forbid: []string{"s.Mems.Content(", "s.Mems.Get("}},
	} {
		i := strings.Index(src, tc.decl)
		if i < 0 {
			t.Fatalf("%s: handler not found in handlers_memory.go", tc.name)
		}
		rest := src[i:]
		j := strings.Index(rest, "\nfunc ")
		if j < 0 {
			t.Fatalf("%s: handler body boundary not found", tc.name)
		}
		body := rest[:j]
		for _, need := range tc.must {
			if !strings.Contains(body, need) {
				t.Errorf("%s must take the single-snapshot path (%s missing) — a Get+Content revert reopens the R2C-F3 disclosure window", tc.name, need)
			}
		}
		for _, banned := range tc.forbid {
			if strings.Contains(body, banned) {
				t.Errorf("%s must not call %s — that is the two-window shape whose inter-lock gap disclosed private bodies (R2C-F3/R3C-F6)", tc.name, banned)
			}
		}
	}
}
