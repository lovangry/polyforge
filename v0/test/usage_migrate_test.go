package test

// usage_migrate_test.go: the `pf migrate-memories` verb (polyforge#25)
// against a REAL aihub process and the REAL pf binary — the §10.1
// one-time memory migration: full-fixture import with the four-step
// verification, --since delta mode, idempotent re-runs (no duplicate
// projects/users/rows), redacted stock staying soft-deleted, and
// placeholder identity creation for unresolvable old projects/users.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// migRecord builds one JSONL line. nil optional fields are omitted
// (json.Marshal drops nil pointers/slices), matching the psql export.
func migRecord(id, project, owner, ownerKind, vis, status, content string,
	workItem *string, related []string, created, updated string) string {
	m := map[string]any{
		"id": id, "type": "experience.debug", "visibility": vis,
		"content": content, "project": project, "owner": owner,
		"created_at": created, "updated_at": updated, "status": status,
	}
	if ownerKind != "" {
		m["owner_kind"] = ownerKind
	}
	if workItem != nil {
		m["work_item_id"] = *workItem
	}
	if related != nil {
		m["related"] = related
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

// migWriteInput writes JSONL lines to a file and returns its path.
func migWriteInput(t *testing.T, dir string, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// migrateRun execs the real pf binary with the install's config (admin
// key) and returns exit code + stdout + stderr: the report prints to
// stdout even on a verification failure, so both streams matter.
func migrateRun(inst *usageInstall, args ...string) (int, string, string) {
	cmd := exec.Command(inst.pfPath, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+inst.xdgHome)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			return -1, stdout.String(), err.Error()
		}
		code = ee.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

// migHTTP is one authenticated request against the real server's HTTP
// face (the tests assert redacted 404s and detail reads the CLI has no
// verb for).
func migHTTP(t *testing.T, inst *usageInstall, method, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, inst.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+inst.adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("%s %s: decode: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// migQueryCount reads the Core SQLite file read-only and returns one
// COUNT(*) scalar — the strongest no-duplicates pin for idempotent
// re-runs.
func migQueryCount(t *testing.T, inst *usageInstall, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(inst.dataDir, "aihub.db")+
		"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open core db: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// migID extracts "id" from a `pf --json users create`-style answer.
func migID(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := jsonUnmarshalStrict(out, &v); err != nil || v.ID == "" {
		t.Fatalf("no id in %q (%v)", out, err)
	}
	return v.ID
}

// TestUsageMigrateFullFixture: the §10.1 happy path end to end — mapped
// and placeholder identities, blanked WI/related references, redacted
// placement, rebuild over HTTP, and the four verification steps PASS.
func TestUsageMigrateFullFixture(t *testing.T) {
	inst := newUsageInstall(t)

	// Pre-create real identities the maps bind to: a human user (old
	// email) and a real project (old project name).
	alice := migID(t, inst.pf(t, "--json", "users", "create", "--display-name", "Alice", "--kind", "human"))
	realProj := migID(t, inst.pf(t, "--json", "projects", "create", "--name", "alpha-real"))

	dir := t.TempDir()
	userMap := migWriteInput(t, dir, "user-map.json", []string{
		`{"alice@example.com": ` + jsonQuote(alice) + `}`,
	})
	projMap := migWriteInput(t, dir, "proj-map.json", []string{
		`{"alpha": ` + jsonQuote(realProj) + `}`,
	})

	// The export fixture: 20 active entries under mapped project "alpha"
	// (mixed owners/visibilities, one legacy work_item_id, one
	// in-batch related ref, one dangling), plus beta (unmapped →
	// placeholder visible=false) with actives and one redacted entry.
	var lines []string
	for i := 0; i < 20; i++ {
		owner, kind := "alice@example.com", "human"
		vis := "project"
		switch {
		case i%7 == 3:
			owner, kind, vis = "ci-agent", "machine", "team"
		case i%7 == 5:
			owner, vis = "ghost@example.com", "private"
		case i%7 == 6:
			vis = "public"
		}
		line := migRecord(fmt.Sprintf("mem_full%04d", i), "alpha", owner, kind, vis, "active",
			fmt.Sprintf("migration smoke body %02d zebra-unique-token alpha", i),
			nil, nil, "2026-03-01T00:00:00Z", "2026-03-02T00:00:00Z")
		if i == 0 {
			wi := "wi_legacy_9"
			line = migRecord("mem_full0000", "alpha", owner, kind, vis, "active",
				"migration smoke body 00 zebra-unique-token alpha", &wi,
				[]string{"mem_full0001", "mem_dangling"}, "2026-03-01T00:00:00Z", "2026-03-02T00:00:00Z")
		}
		lines = append(lines, line)
	}
	lines = append(lines,
		migRecord("mem_beta0001", "beta", "ci-agent", "machine", "public", "active",
			"beta project body one", nil, nil, "2026-03-01T00:00:00Z", "2026-03-02T00:00:00Z"),
		migRecord("mem_beta0002", "beta", "alice@example.com", "human", "project", "active",
			"beta project body two", nil, nil, "2026-03-01T00:00:00Z", "2026-03-02T00:00:00Z"),
		migRecord("mem_betared01", "beta", "alice@example.com", "human", "project", "redacted",
			"beta redacted body three", nil, nil, "2026-03-01T00:00:00Z", "2026-03-02T00:00:00Z"),
	)
	input := migWriteInput(t, dir, "input.jsonl", lines)

	code, stdout, stderr := migrateRun(inst,
		"migrate-memories",
		"--data-dir", inst.dataDir,
		"--aihub-url", inst.url,
		"--project-map", projMap,
		"--user-map", userMap,
		input)
	if code != 0 {
		t.Fatalf("migrate exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	// The four verification steps all PASS.
	for _, step := range []string{"count-reconciliation", "content-sha-sampling",
		"retrieval-smoke", "blanked-list-reconciliation"} {
		if !strings.Contains(stdout, "[PASS] "+step) {
			t.Fatalf("step %s not PASS:\n%s", step, stdout)
		}
	}
	if strings.Contains(stdout, "[FAIL]") {
		t.Fatalf("unexpected FAIL:\n%s", stdout)
	}

	// Placeholder identities created and logged: beta (project,
	// visible=false), ci-agent and ghost@example.com (machine users).
	if !strings.Contains(stdout, "beta -> pj_") || !strings.Contains(stdout, "[placeholder created, visible=false]") {
		t.Fatalf("beta placeholder not reported:\n%s", stdout)
	}
	if strings.Contains(stdout, "alpha -> "+realProj) == false {
		t.Fatalf("alpha mapping missing:\n%s", stdout)
	}
	for _, ph := range []string{"ci-agent (imported)", "ghost@example.com (imported)"} {
		if !strings.Contains(stdout, ph) {
			t.Fatalf("placeholder %q not in report:\n%s", ph, stdout)
		}
	}

	// Blanked lists: the legacy WI id and the dangling related ref.
	if !strings.Contains(stdout, "blanked work_item_id list (1):") ||
		!strings.Contains(stdout, "mem_full0000: wi_legacy_9") {
		t.Fatalf("blanked WI list wrong:\n%s", stdout)
	}
	if !strings.Contains(stdout, "blanked related list (1):") ||
		!strings.Contains(stdout, "mem_full0000: mem_dangling") {
		t.Fatalf("blanked related list wrong:\n%s", stdout)
	}

	// Server-side truth: the placeholder project exists (visible=false)
	// and the alpha memories landed in the MAPPED project. The 3 private
	// entries (i%7==5: i=5,12,19) are excluded from the list face (06 §7:
	// private is owner-only, for admins too).
	st, body := migHTTP(t, inst, "GET", "/api/v1/memories?project=alpha-real")
	if st != 200 {
		t.Fatalf("list alpha-real: %d %v", st, body)
	}
	if items := body["items"].([]any); len(items) != 17 {
		t.Fatalf("alpha-real: %d items, want 17 (20 actives minus 3 private)", len(items))
	}
	// A private entry is invisible even to the admin key.
	st, _ = migHTTP(t, inst, "GET", "/api/v1/memories/mem_full0005")
	if st != 404 {
		t.Fatalf("private entry visible to admin: %d, want 404", st)
	}
	st, body = migHTTP(t, inst, "GET", "/api/v1/memories?project=beta")
	if st != 200 {
		t.Fatalf("list beta: %d %v", st, body)
	}
	if items := body["items"].([]any); len(items) != 2 {
		t.Fatalf("beta list: %d items, want 2 (redacted excluded, both actives are non-private): %v", len(items), body)
	}

	// Redacted entry: detail read 404s (terminal soft-deleted state).
	st, _ = migHTTP(t, inst, "GET", "/api/v1/memories/mem_betared01")
	if st != 404 {
		t.Fatalf("redacted detail read: %d, want 404", st)
	}

	// Active entry: authority markdown served verbatim, no work_item_id,
	// and the related ref survived while the dangling one is gone.
	st, body = migHTTP(t, inst, "GET", "/api/v1/memories/mem_full0000")
	if st != 200 {
		t.Fatalf("active detail: %d", st)
	}
	entry := body["entry"].(map[string]any)
	if entry["work_item_id"] != nil {
		t.Fatalf("work_item_id not blanked: %v", entry["work_item_id"])
	}
	if rel, _ := entry["related"].([]any); len(rel) != 1 || rel[0] != "mem_full0001" {
		t.Fatalf("related wrong: %v", entry["related"])
	}
	if !strings.Contains(body["content"].(string), "migration smoke body 00") {
		t.Fatalf("content lost: %v", body["content"])
	}

	// The HTML face re-derives lazily for migrated files.
	st, body = migHTTP(t, inst, "GET", "/api/v1/memories/mem_full0000?format=html")
	if st != 200 || !strings.Contains(body["html"].(string), "zebra-unique-token") {
		t.Fatalf("html derivation for migrated file: %d", st)
	}

	// Real-time retrieval over the CLI (the recall verb, row 23).
	out := inst.pf(t, "recall", "--project", "alpha-real", "zebra-unique-token")
	if !strings.Contains(out, "hit(s)") || strings.Contains(out, "0 hit(s)") {
		t.Fatalf("recall after migration:\n%s", out)
	}

	// The mapping tables are archived under <data-dir>/migration/.
	for _, name := range []string{"project-map.json", "user-map.json"} {
		if _, err := os.Stat(filepath.Join(inst.dataDir, "migration", name)); err != nil {
			t.Fatalf("mapping archive %s: %v", name, err)
		}
	}
}

// TestUsageMigrateDeltaMode: --since imports only the changed records and
// keeps references into already-migrated stock resolvable.
func TestUsageMigrateDeltaMode(t *testing.T) {
	inst := newUsageInstall(t)
	dir := t.TempDir()

	base := []string{
		migRecord("mem_delta0001", "dproj", "alice@example.com", "human", "project", "active",
			"delta base body one", nil, nil, "2026-04-01T00:00:00Z", "2026-04-01T00:00:00Z"),
		migRecord("mem_delta0002", "dproj", "ci-agent", "machine", "project", "active",
			"delta base body two", nil, nil, "2026-04-01T00:00:00Z", "2026-04-01T00:00:00Z"),
	}
	input := migWriteInput(t, dir, "base.jsonl", base)
	code, stdout, stderr := migrateRun(inst, "migrate-memories",
		"--data-dir", inst.dataDir, "--aihub-url", inst.url, input)
	if code != 0 {
		t.Fatalf("base migrate: %d\n%s\n%s", code, stdout, stderr)
	}

	// The delta export repeats the unchanged records (older updated_at,
	// filtered out) and adds one NEW record whose related points into the
	// already-placed stock — the reference must survive.
	delta := append(append([]string{}, base...),
		migRecord("mem_delta0003", "dproj", "alice@example.com", "human", "project", "active",
			"delta new body three", nil, []string{"mem_delta0001"},
			"2026-04-01T00:00:00Z", "2026-04-10T00:00:00Z"),
	)
	dinput := migWriteInput(t, dir, "delta.jsonl", delta)
	code, stdout, stderr = migrateRun(inst, "migrate-memories",
		"--data-dir", inst.dataDir, "--aihub-url", inst.url,
		"--since", "2026-04-05T00:00:00Z", dinput)
	if code != 0 {
		t.Fatalf("delta migrate: %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "2 dropped by --since") {
		t.Fatalf("delta filter not reported:\n%s", stdout)
	}
	for _, step := range []string{"count-reconciliation", "content-sha-sampling",
		"retrieval-smoke", "blanked-list-reconciliation"} {
		if !strings.Contains(stdout, "[PASS] "+step) {
			t.Fatalf("delta step %s not PASS:\n%s", step, stdout)
		}
	}

	// The new record is live; its related ref into prior stock survived.
	st, body := migHTTP(t, inst, "GET", "/api/v1/memories/mem_delta0003")
	if st != 200 {
		t.Fatalf("delta new record: %d", st)
	}
	entry := body["entry"].(map[string]any)
	if rel, _ := entry["related"].([]any); len(rel) != 1 || rel[0] != "mem_delta0001" {
		t.Fatalf("delta related into placed stock was blanked: %v", entry["related"])
	}
	// No duplicate rows: 3 rows total, not 5.
	if n := migQueryCount(t, inst, "SELECT COUNT(*) FROM memories"); n != 3 {
		t.Fatalf("rows after delta: %d, want 3", n)
	}
}

// TestUsageMigrateIdempotentReRun: the same export imported twice creates
// no duplicate projects, users or memory rows, and the redacted entry
// stays soft-deleted after both runs.
func TestUsageMigrateIdempotentReRun(t *testing.T) {
	inst := newUsageInstall(t)
	dir := t.TempDir()
	input := migWriteInput(t, dir, "input.jsonl", []string{
		migRecord("mem_idem0001", "idemproj", "alice@example.com", "human", "project", "active",
			"idem body one", nil, nil, "2026-05-01T00:00:00Z", "2026-05-01T00:00:00Z"),
		migRecord("mem_idem0002", "idemproj", "ci-agent", "machine", "project", "active",
			"idem body two", nil, nil, "2026-05-01T00:00:00Z", "2026-05-01T00:00:00Z"),
		migRecord("mem_idemred01", "idemproj", "alice@example.com", "human", "project", "redacted",
			"idem redacted body", nil, nil, "2026-05-01T00:00:00Z", "2026-05-01T00:00:00Z"),
	})

	run := func() int {
		code, stdout, stderr := migrateRun(inst, "migrate-memories",
			"--data-dir", inst.dataDir, "--aihub-url", inst.url, input)
		if code != 0 {
			t.Fatalf("migrate run: %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if strings.Contains(stdout, "[FAIL]") {
			t.Fatalf("verification FAIL on re-run:\n%s", stdout)
		}
		return migQueryCount(t, inst, "SELECT COUNT(*) FROM memories")
	}

	run()
	usersBefore := inst.pf(t, "--json", "users", "list")
	projectsBefore := inst.pf(t, "--json", "projects", "list")
	rows := run()

	if rows != 3 {
		t.Fatalf("rows after re-run: %d, want 3 (no duplicates)", rows)
	}
	if out := inst.pf(t, "--json", "users", "list"); out != usersBefore {
		t.Fatalf("user list changed after re-run:\n%s\nvs\n%s", out, usersBefore)
	}
	if out := inst.pf(t, "--json", "projects", "list"); out != projectsBefore {
		t.Fatalf("project list changed after re-run:\n%s\nvs\n%s", out, projectsBefore)
	}
	// Redacted stays terminal after both runs.
	st, _ := migHTTP(t, inst, "GET", "/api/v1/memories/mem_idemred01")
	if st != 404 {
		t.Fatalf("redacted after re-run: %d, want 404", st)
	}
	// The stock is still fully retrievable.
	if n := migQueryCount(t, inst, "SELECT COUNT(*) FROM memory_fts"); n != 2 {
		t.Fatalf("fts rows after re-run: %d, want 2 (active entries)", n)
	}
}

// TestUsageMigrateRedactedOnly: a redacted-only export migrates into
// _redacted/, the rebuild statistic is n=0 (no ACTIVE entries), every
// entry stays soft-deleted (Get 404) and a re-run adds nothing.
func TestUsageMigrateRedactedOnly(t *testing.T) {
	inst := newUsageInstall(t)
	dir := t.TempDir()
	input := migWriteInput(t, dir, "input.jsonl", []string{
		migRecord("mem_redonly01", "rproj", "alice@example.com", "human", "project", "redacted",
			"redonly body one", nil, nil, "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z"),
		migRecord("mem_redonly02", "rproj", "ci-agent", "machine", "project", "redacted",
			"redonly body two", nil, nil, "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z"),
	})

	code, stdout, stderr := migrateRun(inst, "migrate-memories",
		"--data-dir", inst.dataDir, "--aihub-url", inst.url, input)
	if code != 0 {
		t.Fatalf("redonly migrate: %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "rebuilt 0 active") {
		t.Fatalf("rebuild statistic not 0 for redacted-only stock:\n%s", stdout)
	}
	if n := migQueryCount(t, inst, "SELECT COUNT(*) FROM memories WHERE status='redacted'"); n != 2 {
		t.Fatalf("redacted rows: %d, want 2", n)
	}
	if n := migQueryCount(t, inst, "SELECT COUNT(*) FROM memory_fts"); n != 0 {
		t.Fatalf("redacted stock must not be indexed, fts rows: %d", n)
	}
	for _, id := range []string{"mem_redonly01", "mem_redonly02"} {
		st, _ := migHTTP(t, inst, "GET", "/api/v1/memories/"+id)
		if st != 404 {
			t.Fatalf("%s: %d, want 404", id, st)
		}
	}
	// Redacted stock sits under _redacted/.
	for _, id := range []string{"mem_redonly01", "mem_redonly02"} {
		found := false
		err := filepath.WalkDir(filepath.Join(inst.dataDir, "memory"), func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && d.Name() == id && strings.Contains(p, "_redacted") {
				found = true
			}
			return nil
		})
		if err != nil || !found {
			t.Fatalf("%s not placed under _redacted/: found=%v err=%v", id, found, err)
		}
	}
	// Re-run: idempotent, still soft-deleted.
	code, stdout, stderr = migrateRun(inst, "migrate-memories",
		"--data-dir", inst.dataDir, "--aihub-url", inst.url, input)
	if code != 0 {
		t.Fatalf("redonly re-run: %d\n%s\n%s", code, stdout, stderr)
	}
	if n := migQueryCount(t, inst, "SELECT COUNT(*) FROM memories"); n != 2 {
		t.Fatalf("rows after redonly re-run: %d, want 2", n)
	}
	st, _ := migHTTP(t, inst, "GET", "/api/v1/memories/mem_redonly01")
	if st != 404 {
		t.Fatalf("redacted resurrected by re-run: %d", st)
	}
}

// TestUsageMigrateStatusFlip: a re-import whose export now marks a
// previously-active entry redacted moves the placement to _redacted/ and
// leaves no stale active-path file behind (the import's status IS the
// placement authority; a stale active file would keep the row alive).
func TestUsageMigrateStatusFlip(t *testing.T) {
	inst := newUsageInstall(t)
	dir := t.TempDir()
	makeInput := func(aStatus string) string {
		return migWriteInput(t, dir, "input-"+aStatus+".jsonl", []string{
			migRecord("mem_flip0001", "flipproj", "alice@example.com", "human", "project", aStatus,
				"flip body one", nil, nil, "2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z"),
			migRecord("mem_flip0002", "flipproj", "alice@example.com", "human", "project", "active",
				"flip body two", nil, nil, "2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z"),
		})
	}

	code, stdout, stderr := migrateRun(inst, "migrate-memories",
		"--data-dir", inst.dataDir, "--aihub-url", inst.url, makeInput("active"))
	if code != 0 {
		t.Fatalf("first migrate: %d\n%s\n%s", code, stdout, stderr)
	}
	if st, _ := migHTTP(t, inst, "GET", "/api/v1/memories/mem_flip0001"); st != 200 {
		t.Fatalf("first run: mem_flip0001 must be active, got %d", st)
	}

	// Re-import with the entry now redacted in the old world.
	code, stdout, stderr = migrateRun(inst, "migrate-memories",
		"--data-dir", inst.dataDir, "--aihub-url", inst.url, makeInput("redacted"))
	if code != 0 {
		t.Fatalf("flip migrate: %d\n%s\n%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "[FAIL]") {
		t.Fatalf("flip run verification failed:\n%s", stdout)
	}
	// The flipped entry is terminal; the other one untouched.
	if st, _ := migHTTP(t, inst, "GET", "/api/v1/memories/mem_flip0001"); st != 404 {
		t.Fatalf("flipped entry must be soft-deleted, got %d", st)
	}
	if st, _ := migHTTP(t, inst, "GET", "/api/v1/memories/mem_flip0002"); st != 200 {
		t.Fatalf("unflipped entry must stay active, got %d", st)
	}
	// No stale active-path placement survives for the flipped id, and the
	// _redacted placement exists.
	var activeFile, redactedFile bool
	err := filepath.WalkDir(filepath.Join(inst.dataDir, "memory"), func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || d.Name() != "mem_flip0001" {
			return nil
		}
		if strings.Contains(p, "_redacted") {
			redactedFile = true
		} else {
			activeFile = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if activeFile || !redactedFile {
		t.Fatalf("placement wrong: active=%v redacted=%v", activeFile, redactedFile)
	}
	if n := migQueryCount(t, inst, "SELECT COUNT(*) FROM memories WHERE status='active'"); n != 1 {
		t.Fatalf("active rows after flip: %d, want 1", n)
	}
	if n := migQueryCount(t, inst, "SELECT COUNT(*) FROM memories WHERE status='redacted'"); n != 1 {
		t.Fatalf("redacted rows after flip: %d, want 1", n)
	}
}

// TestUsageMigrateVerificationFailure: the four-step verification is a
// gate, not a decoration — when the files never reach the server's data
// dir (wrong --data-dir), count reconciliation fails and the verb exits
// non-zero with the report still printed.
func TestUsageMigrateVerificationFailure(t *testing.T) {
	inst := newUsageInstall(t)
	dir := t.TempDir()
	input := migWriteInput(t, dir, "input.jsonl", []string{
		migRecord("mem_fail0001", "fproj", "alice@example.com", "human", "project", "active",
			"failure gate body", nil, nil, "2026-07-01T00:00:00Z", "2026-07-01T00:00:00Z"),
	})
	wrongDir := t.TempDir()
	code, stdout, stderr := migrateRun(inst, "migrate-memories",
		"--data-dir", wrongDir, "--aihub-url", inst.url, input)
	if code == 0 {
		t.Fatalf("verification must fail (files landed outside the server's data dir):\n%s", stdout)
	}
	if !strings.Contains(stdout, "[FAIL] count-reconciliation") {
		t.Fatalf("count step must FAIL:\n%s", stdout)
	}
	if !strings.Contains(stderr, "verification failed") {
		t.Fatalf("stderr must name the failure:\n%s", stderr)
	}
}

// TestUsageMigrateBadMapIsFatal: a mapping table naming a nonexistent
// target row is a fatal config error — never silently demoted to a
// placeholder identity.
func TestUsageMigrateBadMapIsFatal(t *testing.T) {
	inst := newUsageInstall(t)
	dir := t.TempDir()
	input := migWriteInput(t, dir, "input.jsonl", []string{
		migRecord("mem_badmap001", "bproj", "alice@example.com", "human", "project", "active",
			"bad map body", nil, nil, "2026-08-01T00:00:00Z", "2026-08-01T00:00:00Z"),
	})
	badUserMap := migWriteInput(t, dir, "bad-user-map.json", []string{
		`{"alice@example.com": "usr_nosuchrow"}`,
	})
	code, _, stderr := migrateRun(inst, "migrate-memories",
		"--data-dir", inst.dataDir, "--aihub-url", inst.url,
		"--user-map", badUserMap, input)
	if code == 0 || !strings.Contains(stderr, "user map") {
		t.Fatalf("dangling user map must be fatal, got exit %d:\n%s", code, stderr)
	}

	badProjMap := migWriteInput(t, dir, "bad-proj-map.json", []string{
		`{"bproj": "pj_nosuchrow"}`,
	})
	code, _, stderr = migrateRun(inst, "migrate-memories",
		"--data-dir", inst.dataDir, "--aihub-url", inst.url,
		"--project-map", badProjMap, input)
	if code == 0 || !strings.Contains(stderr, "project map") {
		t.Fatalf("dangling project map must be fatal, got exit %d:\n%s", code, stderr)
	}
}

func jsonQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
