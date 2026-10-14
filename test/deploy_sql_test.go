package test

// deploy_sql_test.go — the in-window upgrade SQL of .polyforge/deploy.sh
// (polyforge#154 / wi_xKcIlUlXhPSX): the switch-window step order (old
// container stop → transactional SQL → new container up → HTTP poll), the
// SQL-failure and post-swap-failure rollback paths (the latter must run the
// rollback SQL BEFORE the old image starts), the run-once probe skip, and
// the SQLite < 3.35 preflight refusal.
//
// HOW THE PATHS ARE SIMULATED LOCALLY (the written note the memo asks for):
//
//   - The fake environment is the shared deploy_image_test.go world: ssh,
//     docker, gh, scp, curl are stubs on PATH and the "server" is a local
//     temp tree. `rssh python3 - <db> <file>` therefore executes the REAL
//     local python3 (stdlib sqlite3) against the REAL sqlite fixture db —
//     the upgrade/rollback/probe SQL genuinely executes, transactionally,
//     on a real file, through the exact code path production uses.
//   - The "old schema" shape is simulated by ALTER TABLE ADD COLUMN
//     identifier_hash on the fixture's projects table (the column the
//     pre-#149 schema carried, the probe looks for, and the window's
//     upgrade.sql drops); the test rollback.sql re-adds it with a default,
//     exactly like the real v2.4.0 files.
//   - The deployer-side git repo is a throwaway repo (newSQLWorld): deploy.sh
//     is copied verbatim from the repo, the test SQL files are committed and
//     tagged v2.3.0 and then REMOVED from the working tree, so the
//     `git show <TAG>:deploy/sql/<TAG>/...` sourcing path is what runs — the
//     working-tree fallback is never hit.
//   - The server's python3 sqlite library version (the ≥ 3.35 gate) is
//     faked by FAKE_SQLITE_VERSION: the ssh stub intercepts exactly the
//     gate's one-line python program; every other python3 program runs for
//     real.
//   - Failure injection is the harness's own: FAKE_BAD_IMG (the new
//     container never becomes healthy → post-swap verification failure) and
//     a deliberately broken upgrade.sql (DROP COLUMN of a nonexistent
//     column → the transaction fails inside the window).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test instance of deploy/sql/v2.3.0/ — one column stands in for the
// whole #149 dead-shape set (the real v2.4.0 files are pinned by the core
// suite in internal/aihub/core/upgrade_test.go).
const (
	testProbeSQL = `SELECT COUNT(*) FROM pragma_table_info('projects') WHERE name = 'identifier_hash';
`
	testUpgradeSQL = `ALTER TABLE projects DROP COLUMN identifier_hash;
`
	testRollbackSQL = `ALTER TABLE projects ADD COLUMN identifier_hash TEXT NOT NULL DEFAULT '';
`
)

// newSQLWorld builds the deploy world whose deployer machine is a
// throwaway git repo: the real deploy.sh plus a tagged deploy/sql/v2.3.0
// whose files are removed from the working tree afterwards (git-tag
// sourcing). oldShape adds the pre-#149 column to the fixture db so the
// probe reports "not yet applied"; without it the probe skips.
func newSQLWorld(t *testing.T, upgradeSQL string, oldShape bool) *deployWorld {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available")
	}
	dr := t.TempDir()
	realScript, err := os.ReadFile(filepath.Join("..", ".polyforge", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	wf(t, filepath.Join(dr, ".polyforge", "deploy.sh"), string(realScript), 0o755)
	sqlDir := filepath.Join(dr, "deploy", "sql", "v2.3.0")
	wf(t, filepath.Join(sqlDir, "upgrade.sql"), upgradeSQL, 0o644)
	wf(t, filepath.Join(sqlDir, "rollback.sql"), testRollbackSQL, 0o644)
	wf(t, filepath.Join(sqlDir, "probe.sql"), testProbeSQL, 0o644)
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.email=deploy@test", "-c", "user.name=deploy", "commit", "-q", "-m", "sql"},
		{"tag", "v2.3.0"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dr
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// the working tree no longer carries the SQL: only the tag does
	if err := os.RemoveAll(sqlDir); err != nil {
		t.Fatal(err)
	}

	d := newDeployWorld(t, "v2.2.1")
	d.release("v2.2.1", true, digOld, digOld)
	d.release("v2.3.0", true, digNew, digNew)
	d.script = filepath.Join(dr, ".polyforge", "deploy.sh")
	if oldShape {
		py := `import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
c.execute("ALTER TABLE projects ADD COLUMN identifier_hash TEXT")
c.commit()`
		if out, err := exec.Command("python3", "-I", "-c", py, d.dbPath()).CombinedOutput(); err != nil {
			t.Fatalf("old-shape fixture: %v\n%s", err, out)
		}
	}
	return d
}

// dbPath is the fixture db the fake "server" serves.
func (d *deployWorld) dbPath() string {
	return filepath.Join(d.w, "srv", "data", "aihub", "aihub.db")
}

// dbHasColumn: does the fixture db still carry the simulated pre-#149
// column (i.e. is the schema in the old shape)?
func dbHasColumn(t *testing.T, dbPath string) bool {
	t.Helper()
	py := `import sqlite3, sys
cols = [r[1] for r in sqlite3.connect(sys.argv[1]).execute("pragma table_info(projects)")]
print("yes" if "identifier_hash" in cols else "no")`
	out, err := exec.Command("python3", "-I", "-c", py, dbPath).CombinedOutput()
	if err != nil {
		t.Fatalf("column probe: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out)) == "yes"
}

// sqlRuns counts how often deploy.sh EXECUTED (not scp'd) one staged SQL
// file on the "server" — the ssh log's `python3 - <db> <file>` lines.
func sqlRuns(d *deployWorld, file string) int {
	n := 0
	for _, ln := range strings.Split(d.logOf("log-ssh"), "\n") {
		if strings.HasPrefix(ln, "ssh python3 - ") && strings.Contains(ln, file) {
			n++
		}
	}
	return n
}

// sqlRunIndex: position in the ssh log of the first EXECUTION (python3 -)
// of <file>; -1 when it never ran. The stop/up window snippets run through
// `sh -s --` and are logged there too, so one log carries the whole order.
func sqlRunIndex(sshLog, file string) int {
	for _, ln := range strings.Split(sshLog, "\n") {
		if strings.HasPrefix(ln, "ssh python3 - ") && strings.Contains(ln, file) {
			return strings.Index(sshLog, ln)
		}
	}
	return -1
}

func TestDeployWindowAppliesUpgradeSQLInOrder(t *testing.T) {
	d := newSQLWorld(t, testUpgradeSQL, true)
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if err != nil {
		t.Fatalf("deploy failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"server python3 sqlite3: 3.5", // >= 3.35 gate passed
		"upgrade SQL v2.3.0: not yet applied (probe: 1) - runs in the window",
		"upgrade SQL of v2.3.0 applied: SQL-OK",
		"downtime: ", "upgrade SQL:    applied v2.3.0",
		"deployed=v2.3.0 previous=v2.2.1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	if dbHasColumn(t, d.dbPath()) {
		t.Error("identifier_hash still present after the window's upgrade SQL")
	}
	if n := sqlRuns(d, "v2.3.0-rollback.sql"); n != 0 {
		t.Errorf("rollback SQL executed on the happy path (%d)\n%s", n, d.logOf("log-ssh"))
	}
	// the window step order (one timeline: the ssh log also records the
	// stop/up `sh -s --` snippets): probe (read-only, pre-window) -> old
	// container stop -> upgrade SQL -> new container up
	sshLog := d.logOf("log-ssh")
	iProbe := sqlRunIndex(sshLog, "v2.3.0-probe.sql")
	iStop := strings.Index(sshLog, " aihub-app aihub 5\n") // stop snippet: compose project service drain
	iSQL := sqlRunIndex(sshLog, "v2.3.0-upgrade.sql")
	iUp := strings.Index(sshLog, " aihub-app aihub 8080 5") // up snippet: compose project service port wait
	if !(iProbe >= 0 && iProbe < iStop && iStop < iSQL && iSQL < iUp) {
		t.Errorf("wrong window order probe=%d stop=%d sql=%d up=%d\n%s", iProbe, iStop, iSQL, iUp, sshLog)
	}
}

func TestDeploySQLFailureRollsBackWithoutRollbackSQL(t *testing.T) {
	// a broken upgrade.sql: DROP COLUMN of a column that does not exist.
	// The transaction fails -> NOTHING of this script applied -> the
	// rollback is exactly today's (compose back to the old image), no
	// rollback.sql runs, and the db is unchanged.
	d := newSQLWorld(t, "ALTER TABLE projects DROP COLUMN no_such_column;\n", true)
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if exitCode(err) != 10 {
		t.Fatalf("want exit 10, got %v\n%s", err, out)
	}
	for _, want := range []string{
		"upgrade SQL of v2.3.0 failed (its transaction rolled back",
		"AUTOMATIC ROLLBACK to v2.2.1",
		"schema: untouched by the rollback (no upgrade SQL was applied this run)",
		"deployed=ROLLED_BACK previous=v2.2.1 attempted=v2.3.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	if n := sqlRuns(d, "v2.3.0-rollback.sql"); n != 0 {
		t.Errorf("rollback SQL ran although nothing was applied (%d)\n%s", n, d.logOf("log-ssh"))
	}
	if !dbHasColumn(t, d.dbPath()) {
		t.Error("the db changed despite the failed transaction")
	}
	if !strings.Contains(d.composeText(), "image: polyforge/aihub:v2.2.1") {
		t.Errorf("compose not rolled back:\n%s", d.composeText())
	}
	// the new container was never started: the only `up` is the rollback's
	if n := strings.Count(d.logOf("log-docker"), " up -d --no-deps aihub"); n != 1 {
		t.Errorf("want exactly 1 compose up (the rollback's), got %d\n%s", n, d.logOf("log-docker"))
	}
	if !d.lockFree() {
		t.Errorf("the lock is still held after the script exited")
	}
}

func TestDeployPostSwapFailureRunsRollbackSQLBeforeOldImage(t *testing.T) {
	d := newSQLWorld(t, testUpgradeSQL, true)
	d.env = append(d.env, "FAKE_BAD_IMG=polyforge/aihub:v2.3.0") // new image: verification fails after the swap
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if exitCode(err) != 10 {
		t.Fatalf("want exit 10, got %v\n%s", err, out)
	}
	for _, want := range []string{
		"upgrade SQL of v2.3.0 applied: SQL-OK",
		"rollback SQL: applying deploy/sql/v2.3.0/rollback.sql (one transaction)",
		"schema: rolled back by the rollback.sql of v2.3.0",
		"deployed=ROLLED_BACK previous=v2.2.1 attempted=v2.3.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	// the schema is restored: the old shape (column back, with the default)
	if !dbHasColumn(t, d.dbPath()) {
		t.Error("identifier_hash missing after the rollback SQL - the old binary would not run")
	}
	// order (one timeline, the ssh log): upgrade SQL -> rollback SQL ->
	// compose back to the old image -> the old image's `up`. The old binary
	// must never see the new schema.
	sshLog := d.logOf("log-ssh")
	iSQL := sqlRunIndex(sshLog, "v2.3.0-upgrade.sql")
	iRb := sqlRunIndex(sshLog, "v2.3.0-rollback.sql")
	iSed := strings.Index(sshLog, "sed -i 's|image: polyforge/aihub:v2.3.0|image: polyforge/aihub:v2.2.1|'")
	iUpOld := strings.LastIndex(sshLog, " aihub-app aihub 8080 5")
	if !(iSQL >= 0 && iSQL < iRb && iRb < iSed && iSed < iUpOld) {
		t.Errorf("wrong rollback order sql=%d rollbacksql=%d sed=%d up=%d\n%s", iSQL, iRb, iSed, iUpOld, sshLog)
	}
	if !strings.Contains(d.composeText(), "image: polyforge/aihub:v2.2.1") {
		t.Errorf("compose not rolled back:\n%s", d.composeText())
	}
}

func TestDeployRefusesOldSQLiteBeforeWindow(t *testing.T) {
	d := newSQLWorld(t, testUpgradeSQL, true)
	d.env = append(d.env, "FAKE_SQLITE_VERSION=3.31.1")
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if exitCode(err) != 1 || !strings.Contains(out, "3.35") || !strings.Contains(out, "refusing BEFORE the window") {
		t.Fatalf("want exit 1 + the >= 3.35 refusal, got %v\n%s", err, out)
	}
	d.assertNothingMutated("v2.2.1")
	if m := d.serverDockerMutations(); len(m) != 0 {
		t.Errorf("server docker was changed: %v", m)
	}
	if strings.Contains(d.logOf("log-docker"), " stop -t ") {
		t.Errorf("the switch window was entered despite the refusal:\n%s", d.logOf("log-docker"))
	}
	if n := sqlRuns(d, "v2.3.0-upgrade.sql"); n != 0 {
		t.Errorf("upgrade SQL executed despite the refusal (%d)\n%s", n, d.logOf("log-ssh"))
	}
	if !dbHasColumn(t, d.dbPath()) {
		t.Error("the db changed despite the refusal")
	}
}

func TestDeployProbeSkipsAlreadyAppliedSQL(t *testing.T) {
	// no old-shape column: the probe answers 0 -> the SQL is skipped even
	// though the target version ships a deploy/sql dir.
	d := newSQLWorld(t, testUpgradeSQL, false)
	out, err := d.run("", "v2.3.0", "--confirm", "v2.3.0")
	if err != nil {
		t.Fatalf("deploy failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "upgrade SQL v2.3.0: already applied (probe) - skipped") {
		t.Errorf("no probe skip note\n%s", out)
	}
	if n := sqlRuns(d, "v2.3.0-upgrade.sql"); n != 0 {
		t.Errorf("upgrade SQL executed although the probe reported applied (%d)\n%s", n, d.logOf("log-ssh"))
	}
	if dbHasColumn(t, d.dbPath()) {
		t.Error("the db changed although the SQL was skipped")
	}
	// a --force re-deploy of the SAME version is a no-op for the SQL too:
	// the candidates set is {v2.3.0} and the probe still says applied.
	out2, err2 := d.run("", "v2.3.0", "--confirm", "v2.3.0", "--force")
	if err2 != nil {
		t.Fatalf("forced re-deploy failed: %v\n%s", err2, out2)
	}
	if n := sqlRuns(d, "v2.3.0-upgrade.sql"); n != 0 {
		t.Errorf("the --force re-deploy re-ran the upgrade SQL (%d)\n%s", n, d.logOf("log-ssh"))
	}
	if dbHasColumn(t, d.dbPath()) {
		t.Error("the db changed during the --force re-deploy")
	}
}

func TestDeployDryRunStagesAndRunsNoSQL(t *testing.T) {
	d := newSQLWorld(t, testUpgradeSQL, true)
	out, err := d.run("", "v2.3.0", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "upgrade SQL candidates (dry-run - the probes run at deploy time): v2.3.0") {
		t.Errorf("no dry-run SQL candidates note\n%s", out)
	}
	// strictly read-only: no staging dir, no probe execution, no SQL
	if strings.Contains(d.logOf("log-ssh"), "aihub-sql-") {
		t.Errorf("the dry-run staged SQL files on the server:\n%s", d.logOf("log-ssh"))
	}
	if n := sqlRuns(d, "v2.3.0-probe.sql"); n != 0 {
		t.Errorf("the dry-run executed a probe (%d)\n%s", n, d.logOf("log-ssh"))
	}
	d.assertNothingMutated("v2.2.1")
}
