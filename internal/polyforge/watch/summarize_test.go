package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingDispatch records every dispatch (argv echoed as one string)
// and answers a fixed line, or fails when failWith is set.
type countingDispatch struct {
	calls    []string
	answers  string
	failWith string
}

func (c *countingDispatch) Dispatch(harness, model, prompt string) (string, error) {
	c.calls = append(c.calls, harness+"|"+model+"|"+fmt.Sprint(len(prompt)))
	if c.failWith != "" {
		return "", fmt.Errorf("%s", c.failWith)
	}
	return c.answers, nil
}

// probeRig is a rig plus a probe wired to a counting dispatcher.
type probeRig struct {
	g    rig
	disp *countingDispatch
	prob Probe
}

func newProbeRig(t *testing.T, models ModelConfig, disp *countingDispatch, src StateSource) probeRig {
	t.Helper()
	g := newRig(t)
	return probeRig{
		g:    g,
		disp: disp,
		prob: Probe{
			Deps:       Deps{WI: "wi_x", Workspace: g.workspace, CapsuleDir: g.capsuleDir, SessionsRoot: g.sessRoot, Source: src},
			Models:     models,
			Harness:    "pi",
			Dispatcher: disp,
		},
	}
}

// TestProbeDeltaTriggeredDispatch pins the delta-trigger: a fresh arm
// with transcript content dispatches EXACTLY once; a silent re-run costs
// ZERO calls; grown records cost exactly ONE further call. The state
// file is persisted either way.
func TestProbeDeltaTriggeredDispatch(t *testing.T) {
	g := newRig(t)
	disp := &countingDispatch{answers: "the agent is grilling the owner"}
	src := fakeSource{}
	prob := Probe{
		Deps:       Deps{WI: "wi_x", Workspace: g.workspace, CapsuleDir: g.capsuleDir, SessionsRoot: g.sessRoot, Source: src},
		Models:     ModelConfig{"pi": "prov/m-1"},
		Harness:    "pi",
		Dispatcher: disp,
	}
	tpath := writeTranscript(t, g.sessRoot, g.capsuleDir, "{\"t\":1}\n")

	snap1, err := prob.Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(disp.calls) != 1 {
		t.Fatalf("a fresh arm with content must dispatch exactly once: %v", disp.calls)
	}
	if snap1.Summary == nil || *snap1.Summary != "the agent is grilling the owner" {
		t.Fatalf("summary must carry the dispatch answer: %v", snap1.Summary)
	}
	if snap1.SummaryError != nil {
		t.Fatalf("summary_error must be null on success: %v", snap1.SummaryError)
	}

	// Silent re-run: zero further calls, summary stays null (no new
	// output is narration the CALLER renders), state untouched.
	snap2, err := prob.Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(disp.calls) != 1 {
		t.Fatalf("a silent transcript must cost ZERO model calls: %v", disp.calls)
	}
	if snap2.Summary != nil || snap2.SummaryError != nil {
		t.Fatalf("a silent run must leave the summary fields null: %v %v", snap2.Summary, snap2.SummaryError)
	}

	// Growth: exactly one further call, tail carries the new record.
	appendTranscript(t, tpath, "{\"t\":2}")
	snap3, err := prob.Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(disp.calls) != 2 {
		t.Fatalf("growth must cost exactly ONE further dispatch: %v", disp.calls)
	}
	if snap3.Summary == nil {
		t.Fatal("a grown run must summarize")
	}
	// State persisted (the crash-recovery contract): a fresh LoadState
	// sees the offset.
	st, err := LoadState(StatePath(g.workspace, "wi_x"))
	if err != nil || st.Size != snap3.TranscriptSize {
		t.Fatalf("state must be persisted after the run: %+v err %v", st, err)
	}
}

// TestProbeMissingModelFailsTyped: the eager typed failure — key named,
// config sample printed, pf doctor pointed at, ZERO dispatch attempts.
func TestProbeMissingModelFailsTyped(t *testing.T) {
	g := newRig(t)
	writeTranscript(t, g.sessRoot, g.capsuleDir, "{\"t\":1}\n")
	disp := &countingDispatch{answers: "should never be called"}
	prob := Probe{
		Deps:       Deps{WI: "wi_x", Workspace: g.workspace, CapsuleDir: g.capsuleDir, SessionsRoot: g.sessRoot, Source: fakeSource{}},
		Models:     ModelConfig{"claude": "prov/other"}, // pi missing
		Harness:    "pi",
		Dispatcher: disp,
	}
	_, err := prob.Run()
	if err == nil {
		t.Fatal("the missing model must fail typed")
	}
	for _, tok := range []string{"watch.summarize_model", "[watch", "doctor"} {
		if !strings.Contains(err.Error(), tok) {
			t.Fatalf("the typed failure must carry %q: %q", tok, err.Error())
		}
	}
	if len(disp.calls) != 0 {
		t.Fatalf("no dispatch may happen without a configured model: %v", disp.calls)
	}
	// And the failure is not conditioned on growth: a silent transcript
	// fails the same way (the spec's WHEN is the probe run itself).
	_, err = prob.Run()
	if err == nil || !strings.Contains(err.Error(), "watch.summarize_model") {
		t.Fatalf("the typed failure must be eager: %v", err)
	}
}

// TestSummarizeFailureKeepsSnapshot: a dispatch failure never fails the
// probe — the snapshot stays intact, summary null, summary_error carries
// the RAW error text, and the delta state still advances.
func TestSummarizeFailureKeepsSnapshot(t *testing.T) {
	g := newRig(t)
	tpath := writeTranscript(t, g.sessRoot, g.capsuleDir, "{\"t\":1}\n")
	disp := &countingDispatch{failWith: "pi -p dispatch failed: exit status 1: the configured model refused the request"}
	prob := Probe{
		Deps:       Deps{WI: "wi_x", Workspace: g.workspace, CapsuleDir: g.capsuleDir, SessionsRoot: g.sessRoot, Source: fakeSource{}},
		Models:     ModelConfig{"pi": "prov/m-1"},
		Harness:    "pi",
		Dispatcher: disp,
	}
	snap, err := prob.Run()
	if err != nil {
		t.Fatalf("a summarize failure must NOT fail the probe: %v", err)
	}
	if snap.Summary != nil {
		t.Fatalf("summary must be null on a dispatch failure: %v", *snap.Summary)
	}
	if snap.SummaryError == nil || !strings.Contains(*snap.SummaryError, "the configured model refused the request") {
		t.Fatalf("summary_error must carry the RAW error text: %v", snap.SummaryError)
	}
	if snap.TranscriptSize <= 0 || len(snap.TranscriptTail) != 1 {
		t.Fatalf("the snapshot must stay intact: %+v", snap)
	}
	st, err := LoadState(StatePath(g.workspace, "wi_x"))
	if err != nil || st.Size != snap.TranscriptSize || st.TranscriptPath != tpath {
		t.Fatalf("the delta state must advance despite the failure: %+v err %v", st, err)
	}
}

// TestSummarizeModelAndNormalize pins the config access and the harness
// vocabulary.
func TestSummarizeModelAndNormalize(t *testing.T) {
	m, err := SummarizeModel(ModelConfig{"pi": "prov/m"}, "pi")
	if err != nil || m != "prov/m" {
		t.Fatalf("SummarizeModel = %q, %v", m, err)
	}
	if _, err := SummarizeModel(ModelConfig{"pi": "  "}, "pi"); err == nil {
		t.Fatal("an empty entry must fail typed")
	}
	for in, want := range map[string]string{"pi": "pi", "claude": "claude", "cc": "claude", "PI": "pi"} {
		got, err := NormalizeHarness(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeHarness(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeHarness(""); err == nil {
		t.Fatal("an empty harness must fail typed")
	}
	if _, err := NormalizeHarness("cursor"); err == nil {
		t.Fatal("an unknown harness must fail typed")
	}
}

// TestDispatchArgsShape pins the one-shot argv: print mode, exactly the
// configured model, then the prompt — no model selection in the adapter.
func TestDispatchArgsShape(t *testing.T) {
	got := DispatchArgs("prov/m-1", "summarize this")
	want := []string{"-p", "--model", "prov/m-1", "summarize this"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("DispatchArgs = %v, want %v", got, want)
	}
	if _, err := dispatchBinary("wat"); err == nil {
		t.Fatal("an unknown harness must not dispatch")
	}
}

// TestExecDispatcherAgainstStub drives the real exec path against stub
// binaries on PATH (the same shape the e2e lane uses): success answers
// stdout; failure carries the raw stderr.
func TestExecDispatcherAgainstStub(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls.log")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\necho \"stub summary line\"\n"
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	failStub := "#!/bin/sh\necho \"stub boom: model refused\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(failStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	d := ExecDispatcher{}
	out, err := d.Dispatch("pi", "prov/wl-model", "what is it doing")
	if err != nil || !strings.Contains(out, "stub summary line") {
		t.Fatalf("Dispatch = %q, %v", out, err)
	}
	raw, _ := os.ReadFile(log)
	if !strings.Contains(string(raw), "-p") || !strings.Contains(string(raw), "prov/wl-model") {
		t.Fatalf("the stub must record `pi -p --model <model> <prompt>`: %q", string(raw))
	}
	_, err = d.Dispatch("claude", "prov/wl-model", "what")
	if err == nil || !strings.Contains(err.Error(), "stub boom: model refused") {
		t.Fatalf("a failing dispatch must carry the raw stderr: %v", err)
	}
}
