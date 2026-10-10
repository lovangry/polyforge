package cli

// verbs_watch_test.go: the `pf watch-probe` unit pins (wi_LMSxPTQTtz5x
// step cli_watch_probe_verb): the exact --json field set, the human line
// shape (one line, fixed order, the silent-transcript verdict), the
// missing-model typed failure with zero dispatch attempts, the cadence
// resolution through the --print-cadence face (flag > env > config,
// defaults 3s / 1m / off), the usage face without any --since-style
// flag, and the workspace resolution. The daemon observe face and the
// dispatch adapter ride the verb's seams; the rest is the real CLI
// (cli.Run) against a real scratch workspace, capsule and sessions store.

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/watch"
)

// wpRig builds one probe fixture: a mapped workspace (its [workspace]
// config written to a scratch XDG), a capsule with session.json, and a
// sessions store with a pre-placed live transcript.
type wpRig struct {
	workspace string
	capsule   string
	cfgPath   string
	xdg       string
	live      string
}

func newWPRig(t *testing.T, withModels bool) *wpRig {
	t.Helper()
	g := wpRig{workspace: t.TempDir()}
	g.capsule = filepath.Join(g.workspace, "capsules", "wi_x")
	if err := os.MkdirAll(g.capsule, 0o755); err != nil {
		t.Fatal(err)
	}
	sess := "{\"harness\":\"pi\",\"session_id\":\"fp_x\",\"transcript_path\":\"\",\"updated_at\":\"2026-01-01T00:00:00Z\"}\n"
	if err := os.WriteFile(filepath.Join(g.capsule, "session.json"), []byte(sess), 0o644); err != nil {
		t.Fatal(err)
	}
	// The sessions store: the harness's dashified per-capsule directory
	// (the rule runtime/pitranscript.go documents) with one live file.
	sessRoot := t.TempDir()
	real, err := filepath.EvalSymlinks(g.capsule)
	if err != nil {
		real = g.capsule
	}
	dashed := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, strings.TrimPrefix(strings.TrimPrefix(real, "/"), "\\"))
	store := filepath.Join(sessRoot, "--"+dashed+"--")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	g.live = filepath.Join(store, "live.jsonl")
	if err := os.WriteFile(g.live, []byte("{\"t\":\"spawn\"}\n{\"t\":\"turn\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The config: mapping + sessions root + (optional) the summarize
	// models, at a scratch XDG location.
	g.xdg = filepath.Join(t.TempDir(), "xdg")
	if err := os.MkdirAll(filepath.Join(g.xdg, "polyforge"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("[workspace.\"" + g.workspace + "\"]\nprojects = [\"e2e\"]\n\n")
	b.WriteString("harness_sessions_root = \"" + sessRoot + "\"\n")
	if withModels {
		b.WriteString("\n[watch.summarize_model.pi]\nmodel = \"prov/wl-model\"\n\n[watch.summarize_model.claude]\nmodel = \"prov/wl-claude\"\n")
	}
	g.cfgPath = filepath.Join(t.TempDir(), "cli.toml")
	if err := os.WriteFile(g.cfgPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return &g
}

// fakeWatchSource is the faked daemon observe face.
type fakeWatchSource struct{ es watch.ExecState }

func (f fakeWatchSource) ObserveExecState(wi string) (watch.ExecState, error) {
	f.es.Step = "grill"
	f.es.Status = "needs_input"
	f.es.StepStatus = "needs_input"
	f.es.LatestEvent = "step_needs_input"
	f.es.Pending = &watch.PendingQuestion{ID: "q_wl", Step: "grill", Question: "which lane?"}
	return f.es, nil
}

// fakeVerbDispatch counts dispatches and answers a fixed line.
type fakeVerbDispatch struct{ calls int }

func (f *fakeVerbDispatch) Dispatch(harness, model, prompt string) (string, error) {
	f.calls++
	return "verb fake summary line", nil
}

// withSeams swaps the verb's production seams for fakes and restores
// them at cleanup; it also isolates the process environment.
func withSeams(t *testing.T, src watch.StateSource, disp *fakeVerbDispatch) *fakeVerbDispatch {
	t.Helper()
	prevSrc, prevDisp := watchStateSource, watchNewDispatcher
	watchStateSource = func(*Context) watch.StateSource { return src }
	watchNewDispatcher = func() watch.Dispatcher { return disp }
	t.Cleanup(func() { watchStateSource, watchNewDispatcher = prevSrc, prevDisp })
	clearEnv(t)
	return disp
}

func wpRun(t *testing.T, g *wpRig, args ...string) (int, string, string) {
	t.Helper()
	t.Chdir(g.workspace)
	argv := append([]string{"--config", g.cfgPath}, args...)
	var stdout, stderr strings.Builder
	code := Run(argv, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestWatchProbeJSONExactFieldSet: the --json document carries EXACTLY
// the documented field set (spec decision 4) with the wired values.
func TestWatchProbeJSONExactFieldSet(t *testing.T) {
	g := newWPRig(t, true)
	disp := withSeams(t, fakeWatchSource{}, &fakeVerbDispatch{})
	code, out, errOut := wpRun(t, g, "--json", "watch-probe", "wi_x")
	if code != 0 {
		t.Fatalf("watch-probe --json: exit %d (stderr %q)", code, errOut)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("output is not one JSON object: %v (out %q)", err, out)
	}
	want := []string{
		"wi", "step", "step_status", "latest_event", "pending_question",
		"transcript_path", "transcript_tail", "transcript_size",
		"transcript_age", "summary", "summary_error", "timestamp",
	}
	got := map[string]bool{}
	for k := range doc {
		got[k] = true
	}
	for _, f := range want {
		if !got[f] {
			t.Fatalf("the document misses the documented field %q (fields %v)", f, doc)
		}
		delete(got, f)
	}
	if len(got) > 0 {
		t.Fatalf("the document carries undocumented fields: %v", got)
	}
	if doc["wi"] != "wi_x" || doc["step"] != "grill" || doc["step_status"] != "needs_input" {
		t.Fatalf("identity fields not wired: %v", doc)
	}
	pq, ok := doc["pending_question"].(map[string]any)
	if !ok || pq["id"] != "q_wl" {
		t.Fatalf("pending_question must carry the id: %v", doc["pending_question"])
	}
	if sz, ok := doc["transcript_size"].(float64); !ok || sz <= 0 {
		t.Fatalf("transcript_size must be the real size: %v", doc["transcript_size"])
	}
	if doc["summary"] == nil || doc["summary"] == "" {
		t.Fatalf("growth must summarize (the fake dispatch answers): %v", doc["summary"])
	}
	if doc["summary_error"] != nil {
		t.Fatalf("summary_error must be null on the healthy lane: %v", doc["summary_error"])
	}
	if disp.calls != 1 {
		t.Fatalf("the fresh-arm growth must dispatch exactly once: %d", disp.calls)
	}
	// The delta state lives where the spec pins it.
	if _, err := os.Stat(filepath.Join(g.workspace, ".polyforge", "watchers", "wi_x.state")); err != nil {
		t.Fatalf("the probe must persist the delta state: %v", err)
	}
}

// TestWatchProbeHumanSilentLine: the second run over a silent transcript
// renders ONE line, fixed-order metadata and the "no new output, age"
// verdict, with ZERO model calls.
func TestWatchProbeHumanSilentLine(t *testing.T) {
	g := newWPRig(t, true)
	disp := withSeams(t, fakeWatchSource{}, &fakeVerbDispatch{})
	if code, _, err := wpRun(t, g, "watch-probe", "wi_x"); code != 0 {
		t.Fatalf("first probe: exit %d (%q)", code, err)
	}
	calls := disp.calls
	code, out, errOut := wpRun(t, g, "watch-probe", "wi_x")
	if code != 0 {
		t.Fatalf("second probe: exit %d (%q)", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("the human output must be ONE line: %q", out)
	}
	line := lines[0]
	for _, tok := range []string{"wi=wi_x", "status=needs_input", "step=grill", "step_status=needs_input", "latest_event=step_needs_input", "pending_question=q_wl", "no new output, age"} {
		if !strings.Contains(line, tok) {
			t.Fatalf("the human line must carry %q: %q", tok, line)
		}
	}
	if disp.calls != calls {
		t.Fatalf("a silent transcript must cost ZERO model calls: %d -> %d", calls, disp.calls)
	}
}

// TestWatchProbeMissingModelTypedExit1: no [watch.summarize_model] for
// the harness → exit 1, the key named, the config sample printed, pf
// doctor pointed at, ZERO dispatch attempts.
func TestWatchProbeMissingModelTypedExit1(t *testing.T) {
	g := newWPRig(t, false)
	disp := withSeams(t, fakeWatchSource{}, &fakeVerbDispatch{})
	code, stdout, stderr := wpRun(t, g, "watch-probe", "wi_x")
	if code != 1 {
		t.Fatalf("the missing model must exit 1, got %d (out %q err %q)", code, stdout, stderr)
	}
	combined := stdout + stderr
	for _, tok := range []string{"watch.summarize_model", "[watch", "doctor"} {
		if !strings.Contains(combined, tok) {
			t.Fatalf("the typed failure must carry %q: %q", tok, combined)
		}
	}
	if disp.calls != 0 {
		t.Fatalf("no dispatch may happen without a configured model: %d", disp.calls)
	}
}

// TestWatchProbePrintCadencePrecedence drives the resolved-cadence face
// through the real CLI: defaults, env fill-in, config-beats-env,
// flag-beats-env, the off literal.
func TestWatchProbePrintCadencePrecedence(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		g := newWPRig(t, true)
		withSeams(t, fakeWatchSource{}, &fakeVerbDispatch{})
		code, out, errOut := wpRun(t, g, "watch-probe", "wi_x", "--print-cadence")
		if code != 0 {
			t.Fatalf("exit %d (%q)", code, errOut)
		}
		for _, tok := range []string{"watch_poll=3s", "watch_log=1m", "watch_heartbeat=off"} {
			if !strings.Contains(out, tok) {
				t.Fatalf("defaults missing %q: %q", tok, out)
			}
		}
	})
	t.Run("env fills absent keys", func(t *testing.T) {
		g := newWPRig(t, true)
		withSeams(t, fakeWatchSource{}, &fakeVerbDispatch{})
		t.Setenv("PF_WATCH_POLL", "6s")
		t.Setenv("PF_WATCH_LOG", "9m")
		_, out, _ := wpRun(t, g, "watch-probe", "wi_x", "--print-cadence")
		for _, tok := range []string{"watch_poll=6s", "watch_log=9m"} {
			if !strings.Contains(out, tok) {
				t.Fatalf("env fill-in missing %q: %q", tok, out)
			}
		}
		if strings.Contains(out, "3s") {
			t.Fatalf("an env-set poll must override the default: %q", out)
		}
	})
	t.Run("config beats env", func(t *testing.T) {
		g := newWPRig(t, true)
		withSeams(t, fakeWatchSource{}, &fakeVerbDispatch{})
		body, err := os.ReadFile(g.cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(g.cfgPath, []byte(string(body)+"\nwatch_poll = \"7s\"\nwatch_log = \"11m\"\nwatch_heartbeat = \"13m\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PF_WATCH_POLL", "6s")
		t.Setenv("PF_WATCH_LOG", "9m")
		t.Setenv("PF_WATCH_HEARTBEAT", "17m")
		_, out, _ := wpRun(t, g, "watch-probe", "wi_x", "--print-cadence")
		for _, tok := range []string{"watch_poll=7s", "watch_log=11m", "watch_heartbeat=13m"} {
			if !strings.Contains(out, tok) {
				t.Fatalf("config must beat env, missing %q: %q", tok, out)
			}
		}
		for _, bad := range []string{"6s", "9m", "17m"} {
			if strings.Contains(out, bad) {
				t.Fatalf("PF_WATCH_* must not leak (%q): %q", bad, out)
			}
		}
	})
	t.Run("flag beats env", func(t *testing.T) {
		g := newWPRig(t, true)
		withSeams(t, fakeWatchSource{}, &fakeVerbDispatch{})
		t.Setenv("PF_WATCH_POLL", "6s")
		_, out, _ := wpRun(t, g, "watch-probe", "wi_x", "--print-cadence", "--poll", "4s")
		if !strings.Contains(out, "watch_poll=4s") || strings.Contains(out, "6s") {
			t.Fatalf("the flag layer must beat the env: %q", out)
		}
	})
	t.Run("heartbeat off literal beats env", func(t *testing.T) {
		g := newWPRig(t, true)
		withSeams(t, fakeWatchSource{}, &fakeVerbDispatch{})
		body, err := os.ReadFile(g.cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(g.cfgPath, []byte(string(body)+"\nwatch_heartbeat = \"off\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PF_WATCH_HEARTBEAT", "13m")
		_, out, _ := wpRun(t, g, "watch-probe", "wi_x", "--print-cadence")
		if !strings.Contains(out, "watch_heartbeat=off") || strings.Contains(out, "13m") {
			t.Fatalf("the off literal must win over the env: %q", out)
		}
	})
}

// TestWatchProbeHelpHasNoSinceFlag: the probe owns the delta state; no
// script-passed --since-size-style flag exists anywhere on its surface.
func TestWatchProbeHelpHasNoSinceFlag(t *testing.T) {
	var out strings.Builder
	if code := printCommandUsage(&out, "watch-probe"); code != 0 {
		t.Fatalf("help watch-probe: exit %d", code)
	}
	if strings.Contains(out.String(), "--since") || strings.Contains(out.String(), "since-size") {
		t.Fatalf("the usage must not carry a --since-size-style flag: %q", out.String())
	}
}

// TestWatchProbeWorkspaceResolution: the working directory decides the
// workspace; outside any mapped workspace the probe refuses typed.
func TestWatchProbeWorkspaceResolution(t *testing.T) {
	g := newWPRig(t, true)
	withSeams(t, fakeWatchSource{}, &fakeVerbDispatch{})
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)
	var stdout, stderr strings.Builder
	code := Run([]string{"--config", g.cfgPath, "watch-probe", "wi_x"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("outside a mapped workspace: exit %d, want 1 (out %q err %q)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "mapped workspace") {
		t.Fatalf("the refusal must name the mapping: %q", stderr.String())
	}
}

// TestWatchProbeFlagsRegistered: the documented flag surface exists and
// nothing --since-shaped does.
func TestWatchProbeFlagsRegistered(t *testing.T) {
	cmd := lookupCommand("watch-probe")
	if cmd == nil {
		t.Fatal("watch-probe must be registered")
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cmd.Setup(fs)
	for _, name := range []string{"poll", "log", "heartbeat", "print-cadence"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("flag --%s must be registered", name)
		}
	}
	if fs.Lookup("since") != nil || fs.Lookup("since-size") != nil {
		t.Fatal("no --since-style flag may exist")
	}
}

func TestConfigWatchPrintCadenceLine(t *testing.T) {
	// The rendered line carries the resolved values in the shell-source
	// spelling, including the heartbeat off literal and the watch
	// module's log-tail constant.
	cad := WatchCadence{Poll: 3 * time.Second, Log: time.Minute, Heartbeat: 0}
	line := renderCadence(cad)
	for _, tok := range []string{"watch_poll=3s", "watch_log=1m", "watch_heartbeat=off", "watch_log_tail_lines=50", "watch_poll_seconds=3", "watch_log_seconds=60"} {
		if !strings.Contains(line, tok) {
			t.Fatalf("the cadence line must carry %q: %q", tok, line)
		}
	}
}
