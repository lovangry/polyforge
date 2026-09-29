package mainagent

// isolation_test.go: the WI-agent BIRTH ISOLATION PROFILE (polyforge#51).
//
// A spawned WI Agent inherits HOME — 04 §8 sanitizes credentials, not HOME,
// because the harness needs it for its own auth — so without adapter flags
// it loads the OPERATOR's global harness configuration: global skills,
// AGENTS.md/CLAUDE.md context files, prompt templates, extensions. Those
// are v1-era routing instructions and harness-level background-task
// mechanisms that break the Capsule's single-writer domain (03 §7) and
// confuse the executor, whose only instruction of record is the pf-execute
// text injected at derivation (04 §6.1a).
//
// MEASURED against the REAL pi 0.87.1 in a capsule-shaped cwd (rpc
// session; the role=system message record's sections{} + toolsAdded[] is
// the inventory channel, get_commands the command one):
//
//	flags                | ext_ui | tools | subagent | skills sec | ctx sec | commands
//	---------------------|--------|-------|----------|------------|---------|---------
//	(none, baseline)     |   4    |  73   |   yes    |  6705 B    | 1153 B  |   37
//	--no-extensions      |   0    |   4   |   NO     |  6705 B    | 1153 B  |   23
//	--no-skills          |   4    |  73   |   yes    |     0      | 1153 B  |   18
//	--no-context-files   |   4    |  73   |   yes    |  6705 B    |    0    |   37
//	--no-prompt-templates|   4    |  73   |   yes    |  6705 B    | 1153 B  |   34
//	-ns -nc -np (PROFILE)|   4    |  73   |   yes    |     0      |    0    |   15
//	all four             |   0    |   4   |   NO     |     0      |    0    |    1
//
// and the profile row was confirmed end to end: with -ns -nc -np on, the
// capsule's .pi/agents/code.md was discovered AND dispatched by the real
// pi (toolCall subagent{agent:"code",agentScope:"project"} → "OK").
//
// The decisive finding these tests pin: pi 0.87.1 has NO built-in subagent
// facility. The tool that reads the Capsule's materialized
// .pi/agents/<step>.md (03 §4.5) is EXTENSION-provided, so --no-extensions
// removes the step-agent dispatch mechanism of pf-execute §4.3 altogether.
// It is therefore NOT in the profile, and the fake below models exactly
// that coupling so a future "let's also add -ne" fails loudly here.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// isolationFakeBody models the MEASURED pi flag semantics at fake level:
// it parses its own argv, writes one report file describing what the
// session would load, and only then runs the normal RPC loop.
//
//   - extension discovery on  → one extension_ui setWidget record (the
//     background-tasks widget really observed in a capsule-cwd rpc
//     session) AND a working subagent tool, i.e. project agents
//     discovered from ./.pi/agents (the nearest agents dir from cwd,
//     which is the Capsule because BuildCmd pins cmd.Dir there);
//   - extension discovery off → no widget record and NO agent discovery
//     at all, because the discovering tool is the extension;
//   - --no-skills / --no-context-files / --no-prompt-templates → the
//     corresponding operator-global resource counts collapse to 0.
const isolationFakeBody = `ext=1; sk=1; cx=1; tp=1
for a in "$@"; do
  case "$a" in
    --no-extensions|-ne)      ext=0 ;;
    --no-skills|-ns)          sk=0 ;;
    --no-context-files|-nc)   cx=0 ;;
    --no-prompt-templates|-np) tp=0 ;;
  esac
done
agents=none
if [ "$ext" = 1 ]; then
  # The subagent tool exists only while extensions load; it resolves the
  # nearest .pi/agents from the process cwd (the Capsule).
  printf '%s\n' '{"type":"extension_ui_request","id":"ui-bg","method":"setWidget","widgetKey":"background-tasks"}'
  found=$(ls .pi/agents 2>/dev/null | sed 's/\.md$//' | tr '\n' ',')
  [ -n "$found" ] && agents="$found"
fi
{
  printf 'argv=%s\n' "$*"
  printf 'extensions=%s\n' "$ext"
  printf 'agents=%s\n' "$agents"
  if [ "$sk" = 1 ]; then printf 'global_skills=%s\n' "${FAKE_GLOBAL_SKILLS:-19}"; else printf 'global_skills=0\n'; fi
  if [ "$cx" = 1 ]; then printf 'context_files=%s\n' "${FAKE_CONTEXT_FILES:-2}"; else printf 'context_files=0\n'; fi
  if [ "$tp" = 1 ]; then printf 'prompt_templates=%s\n' "${FAKE_PROMPT_TEMPLATES:-3}"; else printf 'prompt_templates=0\n'; fi
} > "$ISOLATION_REPORT"
`

// writeIsolationFake writes the flag-aware fake harness (report file +
// the shared REAL-protocol RPC loop).
func writeIsolationFake(t *testing.T) string {
	t.Helper()
	return writeHarness(t, "isolationfake", "#!/bin/sh\n"+isolationFakeBody+rpcHarnessBody)
}

// readReport parses the fake's key=value report. The report is written at
// harness startup, which races a Spawn that returns as soon as the initial
// prompt is injected, so poll briefly rather than read once.
func readReport(t *testing.T, path string) map[string]string {
	t.Helper()
	var raw []byte
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err = os.ReadFile(path)
		if err == nil && len(raw) > 0 && strings.Contains(string(raw), "prompt_templates=") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake harness must have written its load report: %v (%q)", err, string(raw))
		}
		time.Sleep(20 * time.Millisecond)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		out[k] = v
	}
	return out
}

// capsuleWithStepAgent builds spawn params whose Capsule carries a
// materialized step-agent definition (03 §4.5: agents/<id>.md →
// .pi/agents/<id>.md).
func capsuleWithStepAgent(t *testing.T, sessionID, stepID string) daemon.SpawnParams {
	t.Helper()
	p := newSpawnParams(t, capsule.AdapterPi, sessionID)
	def := "---\nname: " + stepID + "\ndescription: step agent\ntools: bash\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(p.CapsuleDir, ".pi", "agents", stepID+".md"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------------------------------------------------------------------------
// (1) argv pins: the isolation flags ride every pi derivation, and
// --no-extensions never does.

func TestPiAdapterIsolationArgvPinned(t *testing.T) {
	a := &PiAdapter{Bin: "/custom/pi"}
	cmd, err := a.BuildCmd(daemon.SpawnParams{
		CapsuleDir: "/caps", SessionID: "sess_1", WorkItemID: "wi_1", Generation: 2,
	}, "/caps/harness/pi/session-data")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/custom/pi", "--mode", "rpc", "--no-skills", "--no-context-files", "--no-prompt-templates"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("pi argv must be transport + the named isolation profile, got %v", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("pi argv[%d] = %q, want %q (full: %v)", i, cmd.Args[i], want[i], cmd.Args)
		}
	}
	// The measured exclusion: -ne removes the EXTENSION that provides the
	// subagent tool, and with it every .pi/agents/<step>.md dispatch
	// (TestPiIsolationFlagsKeepStepAgentsDiscoverable proves the coupling).
	for _, banned := range []string{"--no-extensions", "-ne"} {
		for _, got := range cmd.Args {
			if got == banned {
				t.Fatalf("%s must never enter the pi profile: it deletes the subagent tool and with it step-agent dispatch (pf-execute §4.3)", banned)
			}
		}
	}
	// The profile is ONE named constant, not flags scattered per call site.
	if len(piIsolationFlags) != 3 {
		t.Fatalf("piIsolationFlags must stay the single documented set, got %v", piIsolationFlags)
	}
}

func TestClaudeAdapterIsolationArgvPinned(t *testing.T) {
	a := &ClaudeAdapter{Bin: "/custom/claude"}
	cmd, err := a.BuildCmd(daemon.SpawnParams{
		CapsuleDir: "/caps", SessionID: "sess_1", WorkItemID: "wi_1", Generation: 2,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	// The one VERIFIED cc isolation flag (real CLI 2.1.283: agent list and
	// a live project-agent dispatch both unchanged with it on).
	if !strings.Contains(joined, "--disable-slash-commands") {
		t.Fatalf("claude argv must carry the verified isolation flag --disable-slash-commands, got %v", cmd.Args)
	}
	// The two REFUSED ones: --safe-mode disables "custom commands and
	// agents" (kills .claude/agents/<step>.md); --bare forces
	// ANTHROPIC_API_KEY-only auth (the pilot box is oauth_token) and
	// demands agents be passed explicitly.
	for _, banned := range []string{"--safe-mode", "--bare"} {
		for _, got := range cmd.Args {
			if got == banned {
				t.Fatalf("%s must never enter the claude profile: it breaks step-agent discovery or harness auth", banned)
			}
		}
	}
	if len(claudeIsolationFlags) != 1 {
		t.Fatalf("claudeIsolationFlags must stay the single documented set, got %v", claudeIsolationFlags)
	}
	// The transport contract is untouched by the profile.
	if !strings.Contains(joined, "--input-format stream-json") || !strings.Contains(joined, "--append-system-prompt") {
		t.Fatalf("the isolation profile must not disturb the measured cc transport argv: %v", cmd.Args)
	}
}

// ---------------------------------------------------------------------------
// (2) THE regression: with the isolation flags on, the Capsule's
// .pi/agents/<step>.md definitions are STILL discovered — agents are a
// DIFFERENT resource family from skills, and --no-skills does not touch
// them (measured on the real pi, modelled here by the fake).

func TestPiIsolationFlagsKeepStepAgentsDiscoverable(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("ISOLATION_REPORT", report)
	bin := writeIsolationFake(t)
	s := NewSupervisor(bin, bin, "")
	p := capsuleWithStepAgent(t, "sess_iso1", "code")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)

	got := readReport(t, report)
	if !strings.Contains(got["agents"], "code") {
		t.Fatalf("the capsule's .pi/agents/code.md must still be discovered with the isolation flags on, report: %v", got)
	}
	// ...while the operator-global resource families are shut out.
	for _, k := range []string{"global_skills", "context_files", "prompt_templates"} {
		if got[k] != "0" {
			t.Fatalf("%s must be 0 under the isolation profile, report: %v", k, got)
		}
	}
	// Extension discovery is deliberately still on: that is the price of
	// keeping the subagent tool (see piIsolationFlags).
	if got["extensions"] != "1" {
		t.Fatalf("the pi profile keeps extension discovery on (the subagent tool lives there), report: %v", got)
	}
}

// TestPiNoExtensionsWouldKillStepAgents is the counterfactual that makes
// the dropped flag's reason executable: the SAME fake, the SAME capsule,
// with --no-extensions added on top of the profile. The operator's
// extension widget goes silent (which is what -ne buys) and step-agent
// discovery dies with it (which is what -ne costs).
func TestPiNoExtensionsWouldKillStepAgents(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("ISOLATION_REPORT", report)
	bin := writeIsolationFake(t)
	p := capsuleWithStepAgent(t, "sess_iso2", "code")

	run := func(extra ...string) (map[string]string, string) {
		args := append(append([]string{"--mode", "rpc"}, piIsolationFlags...), extra...)
		cmd := exec.Command(bin, args...)
		cmd.Dir = p.CapsuleDir
		cmd.Stdin = strings.NewReader("") // closed stdin: the loop exits at EOF
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("fake harness run %v: %v", args, err)
		}
		return readReport(t, report), string(out)
	}

	withExt, extOut := run()
	if !strings.Contains(withExt["agents"], "code") {
		t.Fatalf("control run must discover the capsule step agent, report: %v", withExt)
	}
	if !strings.Contains(extOut, `"widgetKey":"background-tasks"`) {
		t.Fatal("control run must show the operator extension emitting its setWidget record on load")
	}

	noExt, noExtOut := run("--no-extensions")
	// The flag DOES shut the extension out at the fake level...
	if strings.Contains(noExtOut, "extension_ui_request") {
		t.Fatalf("--no-extensions must silence extension load records, got %q", noExtOut)
	}
	// ...and that is exactly why it is not in the profile: no subagent
	// tool means no step-agent dispatch (pf-execute §4.3).
	if noExt["agents"] != "none" {
		t.Fatalf("--no-extensions must cost step-agent discovery (the measured coupling), report: %v", noExt)
	}
}

// ---------------------------------------------------------------------------
// (3) the profile is applied at DERIVATION, i.e. it is on the argv the
// supervisor actually spawns (not only on a directly-built cmd), and it
// survives a crash-continuation derivation.

func TestIsolationProfileRidesEverySpawn(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("ISOLATION_REPORT", report)
	bin := writeIsolationFake(t)
	s := NewSupervisor(bin, bin, "")
	p := capsuleWithStepAgent(t, "sess_iso3", "review")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Terminate(5 * time.Second)
	argv := readReport(t, report)["argv"]
	for _, f := range piIsolationFlags {
		if !strings.Contains(argv, f) {
			t.Fatalf("the spawned WI agent's argv must carry %s, got %q", f, argv)
		}
	}
	if !strings.Contains(argv, "--mode rpc") {
		t.Fatalf("the transport flags must still lead the argv, got %q", argv)
	}
}
