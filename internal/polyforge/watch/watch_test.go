package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSource is the faked execution-state face (the CLI wires the daemon
// observe face; unit tests wire this).
type fakeSource struct {
	es  ExecState
	err error
}

func (f fakeSource) ObserveExecState(string) (ExecState, error) { return f.es, f.err }

// sessionStoreDir mirrors the harness's own session-directory naming rule
// (documented in internal/polyforge/runtime/pitranscript.go) so the test
// can PRE-PLACE a live transcript the shared locator finds.
func sessionStoreDir(t *testing.T, capDir string) string {
	t.Helper()
	real := capDir
	if resolved, err := filepath.EvalSymlinks(capDir); err == nil {
		real = resolved
	}
	trimmed := strings.TrimPrefix(real, "/")
	if len(trimmed) > 0 && trimmed[0] == '\\' {
		trimmed = trimmed[1:]
	}
	dashed := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, trimmed)
	return "--" + dashed + "--"
}

// writeTranscript pre-places a live transcript for capsuleDir under
// sessionsRoot and returns its path.
func writeTranscript(t *testing.T, sessionsRoot, capDir, body string) string {
	t.Helper()
	dir := filepath.Join(sessionsRoot, sessionStoreDir(t, capDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "live.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendTranscript(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// rig builds one probe fixture: workspace, capsule dir, sessions root.
type rig struct {
	workspace  string
	capsuleDir string
	sessRoot   string
}

func newRig(t *testing.T) rig {
	t.Helper()
	return rig{
		workspace:  t.TempDir(),
		capsuleDir: filepath.Join(t.TempDir(), "capsules", "wi_x"),
		sessRoot:   t.TempDir(),
	}
}

// TestAssembleSnapshotFields drives the snapshot assembly: the exact
// field wiring from the faked execution state and the pre-placed
// transcript, the bounded tail, the growth verdict against a fresh arm.
func TestAssembleSnapshotFields(t *testing.T) {
	g := newRig(t)
	live := writeTranscript(t, g.sessRoot, g.capsuleDir,
		"{\"type\":\"turn\",\"text\":\"seed one\"}\n{\"type\":\"turn\",\"text\":\"seed two\"}\n")
	deps := Deps{
		WI:           "wi_x",
		Workspace:    g.workspace,
		CapsuleDir:   g.capsuleDir,
		SessionsRoot: g.sessRoot,
		Source: fakeSource{es: ExecState{
			Status:      "needs_input",
			Step:        "grill",
			StepStatus:  "needs_input",
			LatestEvent: "step_needs_input",
			LatestSeq:   7,
			Pending:     &PendingQuestion{ID: "q_wl", Step: "grill", Question: "which lane?"},
		}},
	}
	snap, delta, err := Assemble(deps)
	if err != nil {
		t.Fatal(err)
	}
	if snap.WI != "wi_x" || snap.Step != "grill" || snap.StepStatus != "needs_input" {
		t.Fatalf("identity fields not wired: %+v", snap)
	}
	if snap.LatestEvent != "step_needs_input" {
		t.Fatalf("latest event not wired: %+v", snap.LatestEvent)
	}
	if snap.Pending == nil || snap.Pending.ID != "q_wl" {
		t.Fatalf("pending question not wired: %+v", snap.Pending)
	}
	if snap.TranscriptPath != live {
		t.Fatalf("transcript path = %q, want the pre-placed %q (the shared locator must find it)", snap.TranscriptPath, live)
	}
	if snap.TranscriptSize <= 0 {
		t.Fatalf("transcript size must be the real file size: %d", snap.TranscriptSize)
	}
	if len(snap.TranscriptTail) != 2 || snap.TranscriptTail[1] != "{\"type\":\"turn\",\"text\":\"seed two\"}" {
		t.Fatalf("tail must carry the raw records: %v", snap.TranscriptTail)
	}
	if snap.TranscriptAge == "" || snap.TranscriptAge == "-" {
		t.Fatalf("age must render for an existing transcript: %q", snap.TranscriptAge)
	}
	if snap.Timestamp == "" {
		t.Fatal("timestamp must render")
	}
	// Fresh arm: no state file, non-empty transcript -> growth.
	if !delta.Grew {
		t.Fatalf("a fresh arm with transcript content must count as growth: %+v", delta)
	}
	if delta.StatePath != filepath.Join(g.workspace, ".polyforge", "watchers", "wi_x.state") {
		t.Fatalf("state path must be <workspace>/.polyforge/watchers/<wi>.state: %q", delta.StatePath)
	}
	if delta.Next.TranscriptPath != live || delta.Next.Size != snap.TranscriptSize {
		t.Fatalf("next state must snapshot the transcript facts: %+v", delta.Next)
	}
}

// TestAssembleDeltaLifecycle drives the state-file lifecycle: fresh arm
// grows, a silent re-read does not, appended records grow again — the
// crash-recovery property (the state lives on disk across Assemble calls,
// exactly what a re-armed watcher sees).
func TestAssembleDeltaLifecycle(t *testing.T) {
	g := newRig(t)
	live := writeTranscript(t, g.sessRoot, g.capsuleDir, "{\"t\":1}\n{\"t\":2}\n")
	deps := Deps{WI: "wi_x", Workspace: g.workspace, CapsuleDir: g.capsuleDir, SessionsRoot: g.sessRoot, Source: fakeSource{}}

	snap1, d1, err := Assemble(deps)
	if err != nil || !d1.Grew {
		t.Fatalf("fresh arm must grow: %v %+v", err, d1)
	}
	if err := SaveState(d1.StatePath, d1.Next); err != nil {
		t.Fatal(err)
	}
	snap2, d2, err := Assemble(deps)
	if err != nil || d2.Grew {
		t.Fatalf("a silent transcript must not grow: %v grew=%v", err, d2.Grew)
	}
	if snap2.TranscriptSize != snap1.TranscriptSize {
		t.Fatalf("size must be stable on silence: %d -> %d", snap1.TranscriptSize, snap2.TranscriptSize)
	}
	appendTranscript(t, live, "{\"t\":3}")
	_, d3, err := Assemble(deps)
	if err != nil || !d3.Grew {
		t.Fatalf("appended records must grow: %v grew=%v", err, d3.Grew)
	}
	if d3.Next.Size <= d3.Prev.Size {
		t.Fatalf("the persisted offset must climb: %d -> %d", d3.Prev.Size, d3.Next.Size)
	}
	// Crash recovery: the state file alone re-derives the delta.
	if d3.Prev.Size != d1.Next.Size {
		t.Fatalf("the previous state must come from the file: %+v", d3.Prev)
	}
}

// TestAssembleNoTranscript: an unresolvable transcript degrades the
// snapshot (never errors) and never counts as growth; the capsule's own
// synced session transcripts are the fallback.
func TestAssembleNoTranscript(t *testing.T) {
	g := newRig(t)
	deps := Deps{WI: "wi_x", Workspace: g.workspace, CapsuleDir: g.capsuleDir, SessionsRoot: g.sessRoot, Source: fakeSource{}}
	snap, d, err := Assemble(deps)
	if err != nil {
		t.Fatal(err)
	}
	if snap.TranscriptPath != "" || snap.TranscriptSize != 0 || snap.TranscriptAge != "-" {
		t.Fatalf("no transcript must degrade, not fail: %+v", snap)
	}
	if d.Grew {
		t.Fatal("a missing transcript must never count as growth")
	}
	// Fallback: the daemon-synced capsule copy.
	if err := os.MkdirAll(filepath.Join(g.capsuleDir, "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	synced := filepath.Join(g.capsuleDir, "session", "session-wi_x.jsonl")
	if err := os.WriteFile(synced, []byte("{\"t\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap2, d2, err := Assemble(deps)
	if err != nil {
		t.Fatal(err)
	}
	if snap2.TranscriptPath != synced {
		t.Fatalf("the capsule copy must be the fallback: %q", snap2.TranscriptPath)
	}
	if !d2.Grew {
		t.Fatal("finding the transcript (fresh arm) must grow")
	}
}

// TestAssembleSourceError surfaces a source failure typed, not panicked.
func TestAssembleSourceError(t *testing.T) {
	g := newRig(t)
	deps := Deps{WI: "wi_x", Workspace: g.workspace, CapsuleDir: g.capsuleDir,
		Source: fakeSource{err: fmt.Errorf("daemon down")}}
	if _, _, err := Assemble(deps); err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("source failure must surface: %v", err)
	}
}

// TestFormatDuration pins the renderer the human line and the cadence
// face share.
func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "0s",
		-time.Second:     "0s",
		3 * time.Second:  "3s",
		45 * time.Second: "45s",
		time.Minute:      "1m",
		11 * time.Minute: "11m",
		90 * time.Second: "90s",
		61 * time.Second: "61s",
	}
	for d, want := range cases {
		if got := FormatDuration(d); got != want {
			t.Fatalf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
