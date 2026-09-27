package capsule

// Tests for execution.json atomic commit (03 §4.2), the INV-6 permission
// flip (03 §6.2), safety rules (03 §6.3) and session dirs (03 §4.4).

import (
	"os"
	"path/filepath"
	"testing"
)

func saveExec(t *testing.T, c *Capsule, version int64) *Execution {
	t.Helper()
	e := &Execution{
		CapsuleID: c.ID(), WorkItemID: c.WorkItemID, Generation: c.Generation,
		CurrentStep: "code", WorkflowStatus: StatusRunning,
		Steps:   map[string]*Step{"code": {Status: StepInProgress}},
		Version: version,
	}
	if err := c.SaveExecution(e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSaveLoadExecution(t *testing.T) {
	c := newCapsule(t, 3)
	if _, err := c.LoadExecution(); err != ErrNoExecutionState {
		t.Fatalf("expected ErrNoExecutionState, got %v", err)
	}
	saveExec(t, c, 1)
	e, err := c.LoadExecution()
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != 1 || e.CurrentStep != "code" || e.UpdatedAt == "" {
		t.Fatalf("loaded: %+v", e)
	}
}

func TestExecutionVersionMonotonic(t *testing.T) {
	c := newCapsule(t, 3)
	saveExec(t, c, 5)
	err := c.SaveExecution(&Execution{Version: 5, Steps: map[string]*Step{}})
	if err == nil {
		t.Fatal("version regression must be refused")
	}
	saveExec(t, c, 6) // advance works
}

func TestExecutionReadOnlyPermissionFlip(t *testing.T) {
	c := newCapsule(t, 3)
	saveExec(t, c, 1)
	// INV-6 mechanism B: file 0444, dir 0555 after every write.
	fi, err := os.Stat(c.ExecPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o444 {
		t.Fatalf("execution.json perm = %o, want 0444", fi.Mode().Perm())
	}
	dfi, err := os.Stat(filepath.Join(c.Dir, "runtime-state"))
	if err != nil {
		t.Fatal(err)
	}
	if dfi.Mode().Perm() != 0o555 {
		t.Fatalf("runtime-state perm = %o, want 0555", dfi.Mode().Perm())
	}
	// A subagent write hits EACCES (CAP-403 at the runner layer). Permission
	// bits do not bind uid 0, so the behavioral assertion is skipped when
	// tests run as root; the bits themselves are asserted above either way.
	if os.Geteuid() != 0 {
		if err := os.WriteFile(c.ExecPath(), []byte("tamper"), 0o644); err == nil {
			t.Fatal("direct write to execution.json must fail with EACCES")
		}
	}
	// The WI Agent write path reopens writability then writes the next version.
	if err := c.MakeWritable(); err != nil {
		t.Fatal(err)
	}
	saveExec(t, c, 2)
}

func TestNoTornFileOnDisk(t *testing.T) {
	c := newCapsule(t, 3)
	saveExec(t, c, 1)
	// Only the final file exists; no leftover tmp once Save returned.
	if _, err := os.Stat(filepath.Join(c.Dir, "runtime-state", ".execution.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp file must not survive a successful save")
	}
}

func TestSymlinkEscapeParentLevel(t *testing.T) {
	c := newCapsule(t, 3)
	// 03 §6.3 scenario: evil -> ../../runtime-state inside the repo tree;
	// a write targeting evil/execution.json must be refused at the symlink
	// LEVEL, even though the string contains only a legal relative form.
	if err := os.MkdirAll(filepath.Join(c.Dir, "repos", "ieops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../runtime-state", filepath.Join(c.Dir, "repos", "ieops", "evil")); err != nil {
		t.Fatal(err)
	}
	err := c.ValidateWriteTarget("repos/ieops/evil/execution.json", "")
	if err == nil || err.(*Error).Code != ErrSymlinkEscape {
		t.Fatalf("expected CAP-406, got %v", err)
	}
	// runtime-state/execution.json content is unaffected by the refusal;
	// the operation simply never wrote anything.
}

func TestSymlinkTargetValidation(t *testing.T) {
	c := newCapsule(t, 3)
	// Legal in-root relative target is accepted.
	if err := c.ValidateWriteTarget("repos/ieops/vendor/self", "shared/lib"); err != nil {
		t.Fatalf("legal symlink rejected: %v", err)
	}
	// Escaping target (even a "legal" relative form) refused.
	if err := c.ValidateWriteTarget("repos/ieops/evil", "../../runtime-state"); err == nil {
		t.Fatal("escaping symlink target must be refused")
	}
	// Absolute target refused.
	if err := c.ValidateWriteTarget("repos/ieops/abs", "/etc/passwd"); err == nil {
		t.Fatal("absolute symlink target must be refused")
	}
	// Target into the forbidden areas refused.
	if err := c.ValidateWriteTarget("repos/ieops/sneaky", "../../sync"); err == nil {
		t.Fatal("symlink into sync/ must be refused")
	}
}

func TestValidateWriteTargetExclusions(t *testing.T) {
	c := newCapsule(t, 3)
	for _, rel := range []string{"harness/pi/session.json", ".pi/agents/x.md", "workflow.md", "sync/dirty.json", "capsule.json"} {
		if err := c.ValidateWriteTarget(rel, ""); err == nil {
			t.Errorf("recovery write into %s must be refused", rel)
		}
	}
	if err := c.ValidateWriteTarget("artifacts/spec.md", ""); err != nil {
		t.Fatalf("normal artifact write refused: %v", err)
	}
}

func TestIsExcludedFromSync(t *testing.T) {
	cases := map[string]bool{
		"runtime-state/execution.json":           false,
		"artifacts/a.md":                         false,
		"logs/session/3.jsonl":                   false,
		"repos/ieops/internal/x.go":              false,
		"repos/ieops/.git/config":                true,
		"repos/ieops/.git":                       true,
		"repos/inventory.json":                   false,
		"harness/pi/session.json":                false, // optional-sync member
		"harness/pi/session-data/whatever.jsonl": true,
		"capsule.json":                           true,
		"sync/dirty.json":                        true,
		".pi/agents/code.md":                     true,
		".claude/agents/code.md":                 true,
		"workflow.md":                            true,
		"workflow/x.md":                          true,
	}
	for rel, want := range cases {
		if got := IsExcludedFromSync(rel); got != want {
			t.Errorf("IsExcludedFromSync(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestSessionIdentityRoundTrip(t *testing.T) {
	c := newCapsule(t, 3)
	if err := c.EnsureHarnessDir(AdapterPi); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteSessionIdentity(AdapterPi, "sess_wi_test01_3"); err != nil {
		t.Fatal(err)
	}
	idn, err := c.ReadSessionIdentity(AdapterPi)
	if err != nil {
		t.Fatal(err)
	}
	if idn.SessionID != "sess_wi_test01_3" || idn.Adapter != AdapterPi {
		t.Fatalf("identity: %+v", idn)
	}
}

func TestArchiveTranscript(t *testing.T) {
	c := newCapsule(t, 3)
	if err := c.EnsureHarnessDir(AdapterPi); err != nil {
		t.Fatal(err)
	}
	// Empty session-data → no archive (keeps the empty-chain branch sound).
	if archived, err := c.ArchiveTranscript(AdapterPi, 3); err != nil || archived {
		t.Fatalf("empty transcript must not archive: %v %v", archived, err)
	}
	sd := c.SessionDataDir(AdapterPi)
	os.WriteFile(filepath.Join(sd, "a.jsonl"), []byte(""), 0o644) // empty file: still nothing
	if archived, err := c.ArchiveTranscript(AdapterPi, 3); err != nil || archived {
		t.Fatalf("zero-byte transcript must not archive: %v %v", archived, err)
	}
	os.WriteFile(filepath.Join(sd, "b.jsonl"), []byte("{\"e\":1}\n"), 0o644)
	if archived, err := c.ArchiveTranscript(AdapterPi, 3); err != nil || !archived {
		t.Fatalf("non-empty transcript must archive: %v %v", archived, err)
	}
	raw, err := os.ReadFile(filepath.Join(c.Dir, "logs", "session", "3.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{\"e\":1}\n" {
		t.Fatalf("archive content: %q", raw)
	}
}
