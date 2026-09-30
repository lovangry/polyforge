package cli

// migrate_engine_test.go: the migration engine against a REAL in-proc
// aihub (core + memory + server over httptest) — identity resolution,
// the admin import face, the file-wins reconcile backfill, retrieval and
// the idempotent re-run, all over the real wire.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/aihub/git"
	"github.com/silgrid/polyforge/internal/aihub/server"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// migSrv is a real in-proc aihub for the migration tests.
type migSrv struct {
	t        *testing.T
	dataDir  string
	db       *core.DB
	ts       *httptest.Server
	adminID  string
	adminKey string
	client   *aihubclient.Client
}

func newMigSrv(t *testing.T, project string) *migSrv {
	t.Helper()
	dataDir := t.TempDir()
	db, err := core.Open(dataDir)
	if err != nil {
		t.Fatalf("core open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	admin := core.NewAdminManager(db)
	u, err := admin.CreateUser("mig admin", "mig@example.com", "admin", "human")
	if err != nil {
		t.Fatalf("admin user: %v", err)
	}
	issued, err := admin.CreateAPIKey(u.ID, "mig", "")
	if err != nil {
		t.Fatalf("api key: %v", err)
	}
	if _, err := admin.CreateProject(core.CreateProjectRequest{Name: project}); err != nil {
		t.Fatalf("project: %v", err)
	}
	gitMgr, err := git.NewManager(dataDir, core.NewOwnerManager(db))
	if err != nil {
		t.Fatalf("git manager: %v", err)
	}
	ts := httptest.NewServer(server.New(db, dataDir, gitMgr))
	t.Cleanup(ts.Close)
	client := aihubclient.New(ts.URL, issued.Plain)
	return &migSrv{t: t, dataDir: dataDir, db: db, ts: ts, adminID: u.ID, adminKey: issued.Plain, client: client}
}

func (s *migSrv) optsWI(from string) MigWorkItemsOptions {
	return MigWorkItemsOptions{AihubURL: s.ts.URL, APIKey: s.adminKey, FromJSONL: from, Project: "mig"}
}

func (s *migSrv) optsMem() MigMemoriesOptions {
	return MigMemoriesOptions{DataDir: s.dataDir, AihubURL: s.ts.URL, APIKey: s.adminKey, Project: "mig"}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wiLines builds a work-items JSONL covering every importable status.
func wiLines() string {
	var sb strings.Builder
	for _, l := range []string{
		`{"id":"wi_wrap00000001","seq":11,"project":"oldp","goal":"wrapped record","content":"the content",
"status":"wrapped","priority":"high","labels":["pilot-phase-0"],"attrs":{"team":"x"},"reporter":"usr_oldroot",
"created_at":"2026-09-28T08:03:18.565208Z","updated_at":"2026-09-28T10:23:11.131171Z",
"closed_at":"2026-09-28T10:23:11.131171Z","bundle_name":"quick-fix"}`,
		`{"id":"wi_fail00000001","seq":12,"project":"oldp","goal":"failed record","status":"failed",
"labels":[],"attrs":{},"reporter":"usr_oldroot","created_at":"2026-09-28T08:00:00Z",
"updated_at":"2026-09-28T10:00:00Z","closed_at":"2026-09-28T10:00:00Z"}`,
		`{"id":"wi_blocked0001","seq":13,"project":"oldp","goal":"blocked record","status":"blocked",
"labels":[],"attrs":{},"reporter":"usr_oldroot","waiting_on":"runtime:pilot-ops",
"created_at":"2026-09-28T08:00:00Z","updated_at":"2026-09-28T10:00:00Z","closed_at":"2026-09-28T10:00:00Z"}`,
		`{"id":"wi_cancel00001","seq":14,"project":"oldp","goal":"v0-spelled cancelled record","status":"cancelled",
"labels":[],"attrs":{},"reporter":"usr_oldroot","created_at":"2026-09-28T08:00:00Z",
"updated_at":"2026-09-28T10:00:00Z","closed_at":"2026-09-28T10:00:00Z"}`,
	} {
		sb.WriteString(strings.ReplaceAll(strings.ReplaceAll(l, "\n", ""), "\t", ""))
		sb.WriteString("\n")
	}
	return sb.String()
}

func TestMigrateWorkItemsRoundTrip(t *testing.T) {
	s := newMigSrv(t, "mig")
	from := writeTemp(t, wiLines())

	rep, err := RunMigrateWorkItems(s.optsWI(from), s.client)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Imported != 4 || rep.Skipped != 0 {
		t.Fatalf("imported=%d skipped=%d, want 4/0", rep.Imported, rep.Skipped)
	}
	if rep.StatusCounts["canceled"] != 1 || rep.StatusCounts["cancelled"] != 0 {
		t.Errorf("v0 cancelled not normalized: %v", rep.StatusCounts)
	}
	// Terminal field preservation over the ordinary business face.
	wi, err := s.client.GetWorkItem("wi_wrap00000001")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if wi.Status != "wrapped" || wi.Seq != 11 || wi.Priority != "high" || wi.Slug != "mig#11" {
		t.Errorf("wrapped record drifted: %+v", wi)
	}
	if wi.ClosedAt == nil || *wi.ClosedAt != "2026-09-28T10:23:11.131171Z" {
		t.Errorf("closed_at not preserved: %v", wi.ClosedAt)
	}
	if wi.Content == nil || *wi.Content != "the content" {
		t.Errorf("content not preserved: %v", wi.Content)
	}
	if wi.BundleName == nil || *wi.BundleName != "quick-fix" {
		t.Errorf("bundle_name lost: %+v", wi)
	}
	blocked, err := s.client.GetWorkItem("wi_blocked0001")
	if err != nil {
		t.Fatalf("get blocked: %v", err)
	}
	if blocked.Status != "blocked" || blocked.WaitingOn == nil || *blocked.WaitingOn != "runtime:pilot-ops" {
		t.Errorf("blocked record drifted: %+v", blocked)
	}
	if blocked.ClosedAt != nil {
		t.Errorf("blocked record carries closed_at: %v", blocked.ClosedAt)
	}
	// One audit event, no history.
	events, err := core.NewWIManager(s.db).Events("wi_wrap00000001")
	if err != nil || len(events) != 1 || events[0].Type != "work_item_imported" {
		t.Errorf("import audit event wrong: %v (%v)", events, err)
	}

	// Idempotent re-run: 4 skips, zero delta, same record verified.
	rep2, err := RunMigrateWorkItems(s.optsWI(from), s.client)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rep2.Imported != 0 || rep2.Skipped != 4 {
		t.Fatalf("re-run imported=%d skipped=%d, want 0/4", rep2.Imported, rep2.Skipped)
	}
}

func TestMigrateWorkItemsDivergentRecordFails(t *testing.T) {
	s := newMigSrv(t, "mig")
	if _, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, wiLines())), s.client); err != nil {
		t.Fatalf("first run: %v", err)
	}
	diverged := strings.Replace(wiLines(), "wrapped record", "a DIFFERENT goal", 1)
	_, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, diverged)), s.client)
	if err == nil || !strings.Contains(err.Error(), "goal differs") {
		t.Fatalf("divergent re-run must fail, got: %v", err)
	}
}

func TestMigrateWorkItemsRejectsBadInput(t *testing.T) {
	s := newMigSrv(t, "mig")
	// malformed JSON
	if _, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, "{not json\n")), s.client); err == nil {
		t.Fatal("malformed line accepted")
	}
	// unknown field
	bad := `{"id":"wi_x0000000001","seq":1,"project":"p","goal":"g","status":"wrapped","reporter":"u",
"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","closed_at":"2026-01-01T00:00:00Z","no_such_key":1}`
	if _, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, bad)), s.client); err == nil {
		t.Fatal("unknown field accepted")
	}
	// live status is never migration input
	live := `{"id":"wi_x0000000001","seq":1,"project":"p","goal":"g","status":"running","reporter":"u",
"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`
	if _, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, live)), s.client); err == nil {
		t.Fatal("live status accepted")
	}
	// terminal status without closed_at
	noClose := `{"id":"wi_x0000000001","seq":1,"project":"p","goal":"g","status":"wrapped","reporter":"u",
"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`
	if _, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, noClose)), s.client); err == nil {
		t.Fatal("terminal without closed_at accepted")
	}
}

// memLines builds a memories JSONL: a resolvable WI ref, an unresolvable
// one, a related pair, and redacted stock.
func memLines() string {
	var sb strings.Builder
	for _, l := range []string{
		`{"id":"mem_keep0000001","type":"experience.debug","title":"kept title","tags":["alpha"],
"visibility":"project","content":"调试记录 body with searchable token ZebraOne","project":"oldp",
"owner":"old-owner@example.com","owner_kind":"human","work_item_id":"wi_wrap00000001",
"related":["mem_keep0000002"],"created_at":"2026-09-27T13:44:52.640956Z",
"updated_at":"2026-09-27T13:45:27.021894Z","status":"active"}`,
		`{"id":"mem_keep0000002","type":"rule.work","tags":[],"visibility":"project",
"content":"rule body plain token ZebraTwo","project":"oldp","owner":"old-owner@example.com",
"owner_kind":"human","work_item_id":"wi_ghost000000","related":["mem_keep0000001"],
"created_at":"2026-09-27T13:44:52.640956Z","updated_at":"2026-09-27T13:45:27.021894Z","status":"active"}`,
		`{"id":"mem_redact0001","type":"fact.note","tags":[],"visibility":"private",
"content":"private redacted body token ZebraThree","project":"oldp","owner":"old-owner@example.com",
"owner_kind":"human","related":[],"created_at":"2026-09-27T13:44:52.640956Z",
"updated_at":"2026-09-27T13:45:27.021894Z","status":"redacted"}`,
	} {
		sb.WriteString(strings.ReplaceAll(l, "\n", ""))
		sb.WriteString("\n")
	}
	return sb.String()
}

func TestMigrateMemoriesJSONLFullRound(t *testing.T) {
	s := newMigSrv(t, "mig")
	// Work items first: the WI ref then resolves and is PRESERVED.
	if _, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, wiLines())), s.client); err != nil {
		t.Fatalf("work items: %v", err)
	}
	opts := s.optsMem()
	opts.FromJSONL = writeTemp(t, memLines())

	rep, err := RunMigrateMemories(opts, s.client)
	if err != nil {
		t.Fatalf("run: %v (%+v)", err, rep)
	}
	if rep.Migrated != 3 || rep.ActiveCount != 2 || rep.RedactedCount != 1 {
		t.Fatalf("migrated=%d active=%d redacted=%d", rep.Migrated, rep.ActiveCount, rep.RedactedCount)
	}
	if rep.Rebuilt != rep.ActiveCount {
		t.Errorf("rebuilt=%d != active=%d (the reconcile did not backfill from the files)", rep.Rebuilt, rep.ActiveCount)
	}
	if rep.PreservedWIs != 1 {
		t.Errorf("preserved WI refs = %d, want 1", rep.PreservedWIs)
	}
	// one blanked WI ref, no blanked related (both resolve in the batch)
	blanked := 0
	for _, c := range rep.Conversions {
		if c.Field == "work_item_id" && c.Old == "wi_ghost000000" {
			blanked++
		}
	}
	if blanked != 1 {
		t.Errorf("unresolvable WI ref not blanked exactly once: %+v", rep.Conversions)
	}
	// the owner identity was resolved away from the old identifier (a
	// placeholder machine user — the target carried no such email)
	conv := 0
	for _, c := range rep.Conversions {
		if c.Field == "owner_user_id" && c.Old == "old-owner@example.com" && c.New != "" && c.New != c.Old {
			conv++
		}
	}
	if conv != 3 {
		t.Errorf("owner resolution missing: %+v", rep.Conversions)
	}
	// verification: all four steps passed
	for _, st := range rep.Verification {
		if !st.OK {
			t.Errorf("verification %s failed: %s", st.Step, st.Detail)
		}
	}
	if len(rep.Verification) != 4 {
		t.Errorf("expected the four-step verification, got %d", len(rep.Verification))
	}
	// serving through the real INV-7f gate (the reconcile IS the writer
	// here): the active entry reads back over HTTP with its content.
	raw, err := s.client.SearchMemories("mig", "ZebraOne", "", nil, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var hits struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits.Items) != 1 || hits.Items[0].ID != "mem_keep0000001" {
		t.Errorf("retrieval after reconcile: %+v", hits.Items)
	}

	// Idempotent re-run: byte-identical re-placement, zero delta, the
	// same verification passes.
	rep2, err := RunMigrateMemories(opts, s.client)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rep2.Unchanged != 3 {
		t.Errorf("re-run delta: unchanged=%d, want 3 (zero delta)", rep2.Unchanged)
	}
	if rep2.Rebuilt != rep2.ActiveCount {
		t.Errorf("re-run rebuild=%d != active=%d", rep2.Rebuilt, rep2.ActiveCount)
	}
	for _, st := range rep2.Verification {
		if !st.OK {
			t.Errorf("re-run verification %s failed: %s", st.Step, st.Detail)
		}
	}
}

func TestMigrateMemoriesRejectsBadInput(t *testing.T) {
	s := newMigSrv(t, "mig")
	opts := s.optsMem()
	// malformed
	opts.FromJSONL = writeTemp(t, "{not json\n")
	if _, err := RunMigrateMemories(opts, s.client); err == nil {
		t.Fatal("malformed memory line accepted")
	}
	// duplicate id
	dup := strings.ReplaceAll(memLines(), "\n", "\n") + strings.Split(memLines(), "\n")[0] + "\n"
	opts.FromJSONL = writeTemp(t, dup)
	if _, err := RunMigrateMemories(opts, s.client); err == nil {
		t.Fatal("duplicate id accepted")
	}
	// bad visibility
	opts.FromJSONL = writeTemp(t, `{"id":"mem_bad00000001","type":"rule.work","visibility":"group",
"content":"x","project":"p","owner":"o@example.com","created_at":"2026-01-01T00:00:00Z",
"updated_at":"2026-01-01T00:00:00Z","status":"active"}`)
	if _, err := RunMigrateMemories(opts, s.client); err == nil {
		t.Fatal("bad visibility accepted")
	}
	// both and neither input path
	opts = s.optsMem()
	opts.FromDir = "x"
	opts.FromJSONL = "y"
	if _, err := RunMigrateMemories(opts, s.client); err == nil {
		t.Fatal("both input paths accepted")
	}
	opts.FromDir, opts.FromJSONL = "", ""
	if _, err := RunMigrateMemories(opts, s.client); err == nil {
		t.Fatal("no input path accepted")
	}
}

func TestMigrateMemoriesDryRunPlacesNothing(t *testing.T) {
	s := newMigSrv(t, "mig")
	opts := s.optsMem()
	opts.FromJSONL = writeTemp(t, memLines())
	opts.DryRun = true
	rep, err := RunMigrateMemories(opts, s.client)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if rep.Migrated != 3 {
		t.Fatalf("dry-run migrated=%d", rep.Migrated)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "memory")); !os.IsNotExist(err) {
		t.Fatal("dry-run placed files")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("dry-run created rows: n=%d err=%v", n, err)
	}
}

// v0-style authority dir import: frontmatter conversion + format diff
// reporting.
func TestMigrateMemoriesFromDir(t *testing.T) {
	s := newMigSrv(t, "mig")
	src := t.TempDir()
	projDir := filepath.Join(src, "pj_oldworld01")
	// two active files (one carries an out-of-set key → format diff), one
	// redacted file.
	files := []struct {
		rel   string
		fm    string
		extra string
	}{
		{"mem_dir00000001/mem0001.md", "id: mem_dir00000001\ntype: experience.debug\nowner_user_id: usr_old0000001\nvisibility: project\nproject_id: pj_oldworld01\ncreated_at: \"2026-09-27T13:44:52.640956Z\"\nupdated_at: \"2026-09-27T13:45:27.021894Z\"\n", ""},
		{"mem_dir00000002/mem0002.md", "id: mem_dir00000002\ntype: rule.work\ntags: [alpha, beta]\nowner_user_id: usr_old0000001\nvisibility: project\nproject_id: pj_oldworld01\nrelated: [mem_dir00000001]\ncreated_at: \"2026-09-27T13:44:52.640956Z\"\nupdated_at: \"2026-09-27T13:45:27.021894Z\"\n", "legacy_extra: whatever\n"},
	}
	for _, f := range files {
		dir := filepath.Join(projDir, filepath.Dir(f.rel))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		body := "# body of " + f.fm[strings.Index(f.fm, "id: ")+4:strings.Index(f.fm, "id: ")+15] + "\nsearchable token DirZebra\n"
		if err := os.WriteFile(filepath.Join(dir, "memory.md"),
			[]byte("---\n"+f.fm+f.extra+"---\n"+body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	redDir := filepath.Join(projDir, "_redacted", "mem_dir00000003")
	if err := os.MkdirAll(redDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(redDir, "memory.md"), []byte(
		"---\nid: mem_dir00000003\ntype: fact.note\nowner_user_id: usr_old0000001\nvisibility: project\nproject_id: pj_oldworld01\ncreated_at: \"2026-09-27T13:44:52.640956Z\"\nupdated_at: \"2026-09-27T13:45:27.021894Z\"\n---\nredacted body\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	opts := s.optsMem()
	opts.FromDir = src
	opts.Owner = s.adminID // resolved target user
	rep, err := RunMigrateMemories(opts, s.client)
	if err != nil {
		t.Fatalf("from-dir run: %v", err)
	}
	if rep.Migrated != 3 || rep.ActiveCount != 2 || rep.RedactedCount != 1 {
		t.Fatalf("dir import: %+v", rep)
	}
	// format diff: the out-of-set key was found and reported
	foundDiff := false
	for _, d := range rep.FormatDiffs {
		if d.Field == "legacy_extra" {
			foundDiff = true
		}
	}
	if !foundDiff {
		t.Errorf("out-of-set frontmatter key not reported: %+v", rep.FormatDiffs)
	}
	// the owner override converted every owner_user_id
	conv := 0
	for _, c := range rep.Conversions {
		if c.Field == "owner_user_id" && c.New == s.adminID {
			conv++
		}
	}
	if conv != 3 {
		t.Errorf("owner override conversions = %d, want 3", conv)
	}
	// timestamps from the file survive verbatim (byte-identical bytes to
	// the source apart from the identity fields)
	for _, memID := range []string{"mem_dir00000001", "mem_dir00000002"} {
		raw, err := os.ReadFile(filepath.Join(s.dataDir, "memory", projIDOf(t, s), memID, "memory.md"))
		if err != nil {
			t.Fatalf("placed file: %v", err)
		}
		if !strings.Contains(string(raw), `created_at: "2026-09-27T13:44:52.640956Z"`) {
			t.Errorf("%s: file timestamps drifted: %s", memID, raw)
		}
		if !strings.Contains(string(raw), "DirZebra") {
			t.Errorf("%s: body lost: %s", memID, raw)
		}
		if strings.Contains(string(raw), "legacy_extra") {
			t.Errorf("%s: out-of-set key survived conversion", memID)
		}
	}
	// all four steps green
	for _, st := range rep.Verification {
		if !st.OK {
			t.Errorf("verification %s: %s", st.Step, st.Detail)
		}
	}
	// idempotent re-run: zero delta
	rep2, err := RunMigrateMemories(opts, s.client)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rep2.Unchanged != 3 {
		t.Errorf("re-run unchanged=%d, want 3", rep2.Unchanged)
	}
}

func projIDOf(t *testing.T, s *migSrv) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM projects WHERE name='mig'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The CLI verbs wire the engine over the real wire path end to end
// (flags → client → report).
func TestMigrateVerbsOverRealServer(t *testing.T) {
	s := newMigSrv(t, "mig")
	from := writeTemp(t, wiLines())
	env := map[string]string{
		"PF_SERVER":  s.ts.URL,
		"PF_API_KEY": s.adminKey,
	}
	code, out, stderr := runPf(t, env, "migrate-work-items", "--from", from,
		"--project", "mig", "--reporter", s.adminID)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, stderr, out)
	}
	if !strings.Contains(out, "imported 4") {
		t.Errorf("report: %s", out)
	}
	// the --json mode emits the machine report
	code, out, stderr = runPf(t, env, "--json", "migrate-work-items", "--from", from,
		"--project", "mig", "--reporter", s.adminID)
	if code != 0 {
		t.Fatalf("json re-run code=%d stderr=%s out=%s", code, stderr, out)
	}
	if !strings.Contains(out, `"skipped": 4`) && !strings.Contains(out, `"skipped":4`) {
		t.Errorf("json report: %s", out)
	}
	memJSONL := writeTemp(t, memLines())
	_, out, stderr = runPf(t, env, "migrate-memories", "--from-jsonl", memJSONL,
		"--data-dir", s.dataDir, "--project", "mig", "--owner", s.adminID)
	if strings.Contains(stderr, "pf:") {
		t.Fatalf("memories verb stderr: %s (out %s)", stderr, out)
	}
	if !strings.Contains(out, "verification") || strings.Contains(out, "FAIL") {
		t.Errorf("memories report: %s", out)
	}
}

// The all-field skip-check: a divergence in a field the old check never
// looked at (priority — previously a silent skip) now hard-fails the run
// with a per-field diff.
func TestMigrateWorkItemsAllFieldSkipCheck(t *testing.T) {
	s := newMigSrv(t, "mig")
	if _, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, wiLines())), s.client); err != nil {
		t.Fatalf("first run: %v", err)
	}
	diverged := strings.Replace(wiLines(), `"priority":"high"`, `"priority":"urgent"`, 1)
	_, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, diverged)), s.client)
	if err == nil {
		t.Fatal("a changed priority silently skipped")
	}
	if !strings.Contains(err.Error(), "priority") || !strings.Contains(err.Error(), `"urgent"`) {
		t.Fatalf("per-field diff missing the divergent field: %v", err)
	}
	if !strings.Contains(err.Error(), "--accept-changed") {
		t.Errorf("the failure does not point at --accept-changed: %v", err)
	}
}

// --accept-changed: the changed same-id record is applied in place (new
// version wins) and counted as changed; the follow-up re-run without the
// flag is then a pure skip — the rows now carry the record.
func TestMigrateWorkItemsAcceptChanged(t *testing.T) {
	s := newMigSrv(t, "mig")
	if _, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, wiLines())), s.client); err != nil {
		t.Fatalf("first run: %v", err)
	}
	diverged := strings.Replace(wiLines(), "wrapped record", "wrapped record EDITED", 1)
	opts := s.optsWI(writeTemp(t, diverged))
	opts.AcceptChanged = true
	rep, err := RunMigrateWorkItems(opts, s.client)
	if err != nil {
		t.Fatalf("accept-changed run: %v", err)
	}
	// With the flag the admin face rewrites EVERY same-id record in place
	// and answers overwritten — in the seeded flow the input file carries
	// only genuinely changed records, so every overwritten row is a real
	// change counted as changed in the report.
	if rep.Overwritten != 4 || rep.Skipped != 0 || rep.Imported != 0 {
		t.Fatalf("overwritten=%d skipped=%d imported=%d, want 4/0/0", rep.Overwritten, rep.Skipped, rep.Imported)
	}
	wi, err := s.client.GetWorkItem("wi_wrap00000001")
	if err != nil {
		t.Fatal(err)
	}
	if wi.Goal != "wrapped record EDITED" {
		t.Errorf("the new version did not win: %q", wi.Goal)
	}
	// one audit event per overwritten row, with the old/new summary
	events, err := core.NewWIManager(s.db).Events("wi_wrap00000001")
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %v (%v), want imported + overwritten", events, err)
	}
	if events[1].Type != "work_item_import_overwritten" ||
		!strings.Contains(string(events[1].Payload), "wrapped record") {
		t.Errorf("overwritten audit event wrong: %+v", events[1])
	}
	// D5: after --accept-changed, the plain re-run's all-field skip-check
	// passes — a pure skip, zero delta.
	rep2, err := RunMigrateWorkItems(s.optsWI(writeTemp(t, diverged)), s.client)
	if err != nil {
		t.Fatalf("post-accept re-run: %v", err)
	}
	if rep2.Imported != 0 || rep2.Overwritten != 0 || rep2.Skipped != 4 {
		t.Fatalf("re-run imported=%d overwritten=%d skipped=%d, want 0/0/4",
			rep2.Imported, rep2.Overwritten, rep2.Skipped)
	}
}
