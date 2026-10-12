package test

// w079_workspace_yaml_acceptance_test.go: the polyforge#79 acceptance
// suite — authored against the SPEC (the capsule's artifacts/spec.md:
// its `## Acceptance criteria` and `#### Scenario` blocks), BEFORE the
// implementation lands, mirroring the w113 authoring convention. It
// pins the workspace-`.polyforge.yaml` contract through the project's
// real e2e surfaces only: the pf/aihub/fakepi binaries, the `pf` CLI,
// the daemon's UDS face, cli.toml, real git and the real filesystem —
// never implementation internals.
//
// Suite posture (see artifacts/e2e-scenarios.md for the full mapping):
//   - New-behavior bars are RED on the pre-implementation tree
//     (e2e-baseline) — assertion-level reds, never build reds: this file
//     compiles and vets against the tree as it stands today. They must
//     go green when the change lands the spec.
//   - Kept-face guards (absent-yaml behavior at HEAD, bundle step-level
//     candidate precedence, rotation semantics, registry sourcing when
//     no projects section applies, the #113 required-project error,
//     placement via the cli.toml mapping, the installer's workspace
//     blindness) are green BOTH before and after the change.
//   - Document-only scenarios (the agent-driven workspace-init flow's
//     judgment halves, drift surfacing teaching quality, the
//     no-candidate-chain adapter default) live in
//     artifacts/e2e-scenarios.md; the greppable floors below pin their
//     machine-checkable halves.
//
// Daemon argv discipline: every `pf daemon` this suite launches rides
// the w113 fixture's surviving flag surface only (--socket, --api-key,
// --machine-id, --data-dir, --workflows-dir, --pi-bin, --config). The
// suite never touches the not-yet-existing yaml surface through
// anything but the yaml FILE at the workspace root and the real claim
// lane.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// The quick-fix bundle's pinned heads (frontmatter at HEAD): the
// defaults head and the fix step's step-level head. Kept-face guards
// assert these stay in effect wherever the yaml layer is silent.
const (
	w079DefaultsHead = "sub2api-glm/glm-5.3"
	w079FixStepHead  = "sub2api-glm/glm-5.3-flash"
)

// ---------------------------------------------------------------------------
// Fixtures: yaml files, local checkouts, registry remotes, the pf CLI
// with a working directory, capsule/execution readers.
// ---------------------------------------------------------------------------

// w079Yaml writes one .polyforge.yaml at the workspace root.
func w079Yaml(t *testing.T, ws, content string) string {
	t.Helper()
	p := filepath.Join(ws, ".polyforge.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// w079GitRepo creates a real git repo (one commit carrying markerName
// with markerContent) at dir and answers its HEAD SHA.
func w079GitRepo(t *testing.T, dir, markerName, markerContent string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, markerName), []byte(markerContent), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "w079 init")
	return strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
}

// w079Remote is one local fake registry remote: a real git repo served
// over file://, carrying its own marker commit.
func w079Remote(t *testing.T, markerName, markerContent string) (url, head string) {
	t.Helper()
	dir := t.TempDir()
	head = w079GitRepo(t, dir, markerName, markerContent)
	return "file://" + dir, head
}

// w079Env is the environment one w079 CLI run gets: a scratch HOME and
// XDG_CONFIG_HOME, the connection env, an optional --config global
// flag, and a working directory (the inference scenarios need the CLI
// to run INSIDE a mapped workspace).
type w079Env struct {
	home   string
	config string
	cwd    string
	noConn bool
}

// w079Pf runs the real pf binary once with a working directory and
// returns (combined output, exit code), asserting nothing.
func w079Pf(t *testing.T, inst *v1Install, e w079Env, args ...string) (string, int) {
	t.Helper()
	pfPath := filepath.Join(v1Bins(t), "pf")
	if inst != nil && inst.pfPath != "" {
		pfPath = inst.pfPath
	}
	if e.home == "" {
		e.home = t.TempDir()
	}
	xdg := filepath.Join(e.home, ".config")
	if err := os.MkdirAll(xdg, 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.home,
		"XDG_CONFIG_HOME=" + xdg,
	}
	if !e.noConn && inst != nil {
		env = append(env,
			"PF_SERVER="+inst.url,
			"PF_API_KEY="+inst.adminKey,
			"PF_USER_ID=usr_root",
		)
	}
	argv := make([]string, 0, len(args)+2)
	if e.config != "" {
		argv = append(argv, "--config", e.config)
	}
	argv = append(argv, args...)
	cmd := exec.Command(pfPath, argv...)
	if e.cwd != "" {
		cmd.Dir = e.cwd
	}
	cmd.Env = env
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("pf %v (cwd %s): %v", argv, e.cwd, err)
		}
		code = ee.ExitCode()
	}
	return out.String(), code
}

// w079PfWant asserts one w079 CLI run's exit code and answers the
// combined output (wantExit < 0 = must succeed).
func w079PfWant(t *testing.T, inst *v1Install, wantExit int, e w079Env, args ...string) string {
	t.Helper()
	out, code := w079Pf(t, inst, e, args...)
	if wantExit < 0 && code != 0 {
		t.Fatalf("pf %v (cwd %s): exit %d, want success (output %q)", args, e.cwd, code, out)
	}
	if wantExit >= 0 && code != wantExit {
		t.Fatalf("pf %v (cwd %s): exit %d, want %d (output %q)", args, e.cwd, code, wantExit, out)
	}
	return out
}

// w079Capsule is the capsule dir of wi under workspace ws.
func w079Capsule(ws, wi string) string {
	return filepath.Join(ws, "capsules", wi)
}

// w079CapsuleRaw answers the raw capsule.json bytes of one capsule.
func w079CapsuleRaw(t *testing.T, capDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capDir, "capsule.json"))
	if err != nil {
		t.Fatalf("capsule.json of %s: %v", capDir, err)
	}
	return string(raw)
}

// w079Identity is the loose read of the capsule.json legacy fields the
// spec pins as unchanged (work_item_id / project / bundle_name / the
// Repos name→SHA baselines).
type w079Identity struct {
	WorkItemID string            `json:"work_item_id"`
	Project    string            `json:"project"`
	BundleName string            `json:"bundle_name"`
	Repos      map[string]string `json:"repos"`
}

// w079IdentityOf decodes one capsule.json's legacy identity.
func w079IdentityOf(t *testing.T, capDir string) *w079Identity {
	t.Helper()
	var id w079Identity
	if err := json.Unmarshal([]byte(w079CapsuleRaw(t, capDir)), &id); err != nil {
		t.Fatalf("capsule.json of %s does not decode: %v", capDir, err)
	}
	return &id
}

// w079Model answers the recorded effective model of one step out of
// execution.json (the driver writes the pinned candidate on every
// step-starting write; "" = no switchable chain).
func w079Model(t *testing.T, capDir, step string) string {
	t.Helper()
	x := readExecution(t, capDir)
	st := x.Steps[step]
	if st == nil {
		t.Fatalf("no step %q in execution.json of %s (steps %v)", step, capDir, x.Steps)
	}
	return st.Model
}

// w079AttemptStats counts one step's archived attempts by cause: the
// provider-class infra failures (the only class that advances the
// candidate pointer) and the gate-fail rewind archives.
func w079AttemptStats(t *testing.T, capDir, step string) (providerInfra, gateRewind int) {
	t.Helper()
	x := readExecution(t, capDir)
	st := x.Steps[step]
	if st == nil {
		t.Fatalf("no step %q in execution.json of %s", step, capDir)
	}
	for _, a := range st.Attempts {
		cause, _ := a["cause"].(string)
		fc, _ := a["fault_class"].(string)
		switch {
		case cause == "gate_fail_rewind":
			gateRewind++
		case cause == "infra_failure" && fc == "provider":
			providerInfra++
		}
	}
	return
}

// w079WaitRefusal polls until the claim of wi is refused: the combined
// submit output + daemon log names every needle, the aihub-side status
// settles into queued or blocked (never running — pre-claim refusals
// leave the WI queued, abandonClaim-shaped ones leave it blocked), and
// no capsule dir for wi exists under ws. A status that goes running /
// wrapped / failed fails fast — the claim was accepted, which is the
// red bar of the pre-implementation tree.
func w079WaitRefusal(t *testing.T, d *w113Daemon, inst *v1Install, ws, wi, submitOut string, bound time.Duration, needles ...string) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		hay := submitOut + "\n" + d.logTail()
		matched := true
		for _, n := range needles {
			if !strings.Contains(hay, n) {
				matched = false
				break
			}
		}
		status := inst.wiStatus(t, wi)
		if matched && (status == "blocked" || status == "queued") {
			if _, err := os.Stat(w079Capsule(ws, wi)); err == nil {
				t.Fatalf("refused claim of %s still materialized a capsule at %s", wi, w079Capsule(ws, wi))
			}
			return
		}
		switch status {
		case "running", "wrapped", "failed":
			t.Fatalf("claim of %s was ACCEPTED (status %q) — the yaml did not refuse it (needles %v; submit output %q; log tail %s)",
				wi, status, needles, submitOut, d.logTail())
		}
		if time.Now().After(deadline) {
			t.Fatalf("claim of %s was never refused within %v (status %q; needles %v; submit output %q; log tail %s)",
				wi, bound, status, needles, submitOut, d.logTail())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Read/failure semantics.
// ---------------------------------------------------------------------------

// TestW079AbsentAndEmptyYamlRunExactlyAsHead pins the kept faces of the
// read semantics: a workspace with NO .polyforge.yaml, and one whose
// file is comments-only (parses to zero sections, [spec-decided] #3),
// both behave exactly as at HEAD — the repo inventory comes from the
// aihub registry, the models come from bundle frontmatter alone, the
// legacy capsule.json fields (Repos name→SHA) are unchanged, and NO
// yaml audit content is fabricated. Green before AND after the change.
func TestW079AbsentAndEmptyYamlRunExactlyAsHead(t *testing.T) {
	remoteURL, remoteHead := w079Remote(t, "w079reg.txt", "registry marker\n")
	inst := newV1InstallRepos(t, []api.ProjectRepo{{Name: "w079reg", URL: remoteURL}})
	ws := t.TempDir()
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	// (a) no yaml at all.
	wiA := w113NewWI(t, inst, "e2e", "w079 absent-yaml run exactly as head", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wiA)
	capA := w079Capsule(ws, wiA)
	w113WaitCapsule(t, capA, 90*time.Second)
	w113WaitCompleted(t, capA, 240*time.Second)
	inst.waitWIStatus(t, wiA, "wrapped", 90*time.Second)

	// (b) a comments-only yaml: zero sections, valid, a no-op.
	w079Yaml(t, ws, "# w079: comments only — no sections at all.\n# a second comment line.\n")
	wiB := w113NewWI(t, inst, "e2e", "w079 comments-only yaml is a no-op", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wiB)
	capB := w079Capsule(ws, wiB)
	w113WaitCapsule(t, capB, 90*time.Second)
	w113WaitCompleted(t, capB, 240*time.Second)
	inst.waitWIStatus(t, wiB, "wrapped", 90*time.Second)

	for name, capDir := range map[string]string{"absent yaml": capA, "comments-only yaml": capB} {
		raw := w079CapsuleRaw(t, capDir)
		if strings.Contains(raw, "yaml") {
			t.Fatalf("%s: capsule.json fabricated yaml audit content (raw %q)", name, raw)
		}
		id := w079IdentityOf(t, capDir)
		if len(id.Repos) != 1 || id.Repos["w079reg"] != remoteHead {
			t.Fatalf("%s: legacy Repos baselines changed: %v (want w079reg=%s)", name, id.Repos, remoteHead)
		}
		if id.Project != "e2e" || id.BundleName != "quick-fix" {
			t.Fatalf("%s: legacy identity fields changed: %+v", name, id)
		}
		marker, err := os.ReadFile(filepath.Join(capDir, "repos", "w079reg", "w079reg.txt"))
		if err != nil || string(marker) != "registry marker\n" {
			t.Fatalf("%s: the registry clone did not land as at HEAD: %q, %v", name, marker, err)
		}
		if got := w079Model(t, capDir, "fix"); got != w079FixStepHead {
			t.Fatalf("%s: fix model %q, want the bundle step-level head %q", name, got, w079FixStepHead)
		}
		if got := w079Model(t, capDir, "grill"); got != w079DefaultsHead {
			t.Fatalf("%s: grill model %q, want the bundle defaults head %q", name, got, w079DefaultsHead)
		}
	}
}

// TestW079OneDaemonTwoWorkspacesTwoYamls pins the multi-workspace
// contract AND the WI's integration criterion: one daemon, two
// workspaces, two different yamls, the SAME bundle — each claim applies
// only its own workspace's yaml (models, repo sets, resolved source
// paths, audit content), never a cross-workspace leak. RED
// pre-implementation (the yaml governs nothing yet).
func TestW079OneDaemonTwoWorkspacesTwoYamls(t *testing.T) {
	inst := newV1Install(t)
	w113EnsureProject(t, inst, "ieops")
	wsA := filepath.Join(t.TempDir(), "wsA")
	wsB := filepath.Join(t.TempDir(), "wsB")
	localA := w079GitRepo(t, filepath.Join(wsA, ".repo", "w79a"), "w79a-local.txt", "w79a local checkout\n")
	localB := w079GitRepo(t, filepath.Join(wsB, ".repo", "w79b"), "w79b-local.txt", "w79b local checkout\n")
	w079Yaml(t, wsA, "projects:\n  e2e:\n    repos:\n      - name: w79a\n        path: .repo/w79a\nworkflow_defaults:\n  model_candidates: [w079a-flow]\n")
	w079Yaml(t, wsB, "projects:\n  ieops:\n    repos:\n      - name: w79b\n        path: .repo/w79b\nworkflow_defaults:\n  step_kinds:\n    code: [w079b-code]\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{
			{key: wsA, projects: []string{"e2e"}},
			{key: wsB, projects: []string{"ieops"}},
		})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wiA := w113NewWI(t, inst, "e2e", "w079 two-workspace claim A", envelopeQuickFix())
	wiB := w113NewWI(t, inst, "ieops", "w079 two-workspace claim B", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wiA)
	d.pfWant(inst, -1, "submit", wiB)
	capA, capB := w079Capsule(wsA, wiA), w079Capsule(wsB, wiB)
	w113WaitCapsule(t, capA, 90*time.Second)
	w113WaitCapsule(t, capB, 90*time.Second)
	w113WaitCompleted(t, capA, 240*time.Second)
	w113WaitCompleted(t, capB, 240*time.Second)
	inst.waitWIStatus(t, wiA, "wrapped", 90*time.Second)
	inst.waitWIStatus(t, wiB, "wrapped", 90*time.Second)

	// Each workspace's own yaml governs its claim — models.
	if got := w079Model(t, capA, "grill"); got != "w079a-flow" {
		t.Fatalf("A grill model %q, want the yaml flow default head w079a-flow", got)
	}
	if got := w079Model(t, capA, "fix"); got != w079FixStepHead {
		t.Fatalf("A fix model %q, want the bundle step-level head %q (the flow default never beats a step-level list)", got, w079FixStepHead)
	}
	if got := w079Model(t, capB, "fix"); got != "w079b-code" {
		t.Fatalf("B fix model %q, want the yaml step_kinds pin w079b-code", got)
	}
	if got := w079Model(t, capB, "release"); got != "w079b-code" {
		t.Fatalf("B release model %q, want the yaml step_kinds pin w079b-code (every kind:code step)", got)
	}
	if got := w079Model(t, capB, "grill"); got != w079DefaultsHead {
		t.Fatalf("B grill model %q, want the bundle defaults head %q (B's yaml is silent for kind spec)", got, w079DefaultsHead)
	}

	// Each workspace's own repo set, cloned from its own local checkout.
	markerA, err := os.ReadFile(filepath.Join(capA, "repos", "w79a", "w79a-local.txt"))
	if err != nil || string(markerA) != "w79a local checkout\n" {
		t.Fatalf("A did not clone w79a from its local checkout: %q, %v", markerA, err)
	}
	markerB, err := os.ReadFile(filepath.Join(capB, "repos", "w79b", "w79b-local.txt"))
	if err != nil || string(markerB) != "w79b local checkout\n" {
		t.Fatalf("B did not clone w79b from its local checkout: %q, %v", markerB, err)
	}
	if _, err := os.Stat(filepath.Join(capA, "repos", "w79b")); err == nil {
		t.Fatal("A's capsule carries B's repo — a cross-workspace leak")
	}
	if _, err := os.Stat(filepath.Join(capB, "repos", "w79a")); err == nil {
		t.Fatal("B's capsule carries A's repo — a cross-workspace leak")
	}
	idA, idB := w079IdentityOf(t, capA), w079IdentityOf(t, capB)
	if idA.Repos["w79a"] != localA {
		t.Fatalf("A's w79a baseline %q, want the local checkout HEAD %s", idA.Repos["w79a"], localA)
	}
	if idB.Repos["w79b"] != localB {
		t.Fatalf("B's w79b baseline %q, want the local checkout HEAD %s", idB.Repos["w79b"], localB)
	}

	// The audit record, per capsule, with no cross-workspace leak.
	rawA, rawB := w079CapsuleRaw(t, capA), w079CapsuleRaw(t, capB)
	for _, tok := range []string{"w079a-flow", filepath.Join(wsA, ".repo", "w79a"), "w79a"} {
		if !strings.Contains(rawA, tok) {
			t.Fatalf("A's capsule.json audit misses %q (raw %s)", tok, rawA)
		}
		if strings.Contains(rawB, tok) {
			t.Fatalf("B's capsule.json audit leaks A's token %q — a cross-workspace leak", tok)
		}
	}
	for _, tok := range []string{"w079b-code", filepath.Join(wsB, ".repo", "w79b"), "w79b"} {
		if !strings.Contains(rawB, tok) {
			t.Fatalf("B's capsule.json audit misses %q (raw %s)", tok, rawB)
		}
		if strings.Contains(rawA, tok) {
			t.Fatalf("A's capsule.json audit leaks B's token %q — a cross-workspace leak", tok)
		}
	}
}

// TestW079YamlEditGovernsNextClaimNoRestart pins the fresh-per-claim
// read: an edit to the workspace yaml takes effect on the NEXT claim,
// with no daemon restart (the same daemon process serves both claims —
// its pid is asserted alive throughout). RED pre-implementation.
func TestW079YamlEditGovernsNextClaimNoRestart(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	localV1 := w079GitRepo(t, filepath.Join(ws, ".repo", "w79v1"), "w79v1.txt", "yaml v1 source\n")
	localV2 := w079GitRepo(t, filepath.Join(ws, ".repo", "w79v2"), "w79v2.txt", "yaml v2 source\n")
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos:\n      - name: w79src\n        path: .repo/w79v1\nworkflow_defaults:\n  model_candidates: [w079-edit-v1]\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()
	pid := d.cmd.Process.Pid

	// Claim 1 under yaml v1.
	wiA := w113NewWI(t, inst, "e2e", "w079 edit-next-claim first claim", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wiA)
	capA := w079Capsule(ws, wiA)
	w113WaitCapsule(t, capA, 90*time.Second)
	w113WaitCompleted(t, capA, 240*time.Second)
	inst.waitWIStatus(t, wiA, "wrapped", 90*time.Second)
	if got := w079Model(t, capA, "grill"); got != "w079-edit-v1" {
		t.Fatalf("claim 1 grill model %q, want the v1 flow default w079-edit-v1", got)
	}
	if id := w079IdentityOf(t, capA); id.Repos["w79src"] != localV1 {
		t.Fatalf("claim 1 w79src baseline %q, want the v1 local HEAD %s", id.Repos["w79src"], localV1)
	}
	if b, err := os.ReadFile(filepath.Join(capA, "repos", "w79src", "w79v1.txt")); err != nil || string(b) != "yaml v1 source\n" {
		t.Fatalf("claim 1 did not clone from the v1 local checkout: %q, %v", b, err)
	}

	// The edit — the daemon keeps running, untouched.
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos:\n      - name: w79src\n        path: .repo/w79v2\nworkflow_defaults:\n  model_candidates: [w079-edit-v2]\n")

	// Claim 2, same daemon, same socket, same mapping: the NEW yaml
	// governs.
	wiB := w113NewWI(t, inst, "e2e", "w079 edit-next-claim second claim", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wiB)
	capB := w079Capsule(ws, wiB)
	w113WaitCapsule(t, capB, 90*time.Second)
	w113WaitCompleted(t, capB, 240*time.Second)
	inst.waitWIStatus(t, wiB, "wrapped", 90*time.Second)
	if got := w079Model(t, capB, "grill"); got != "w079-edit-v2" {
		t.Fatalf("claim 2 grill model %q, want the edited v2 flow default w079-edit-v2 — the edit must govern the next claim with no restart", got)
	}
	if b, err := os.ReadFile(filepath.Join(capB, "repos", "w79src", "w79v2.txt")); err != nil || string(b) != "yaml v2 source\n" {
		t.Fatalf("claim 2 did not clone from the edited v2 local checkout: %q, %v", b, err)
	}
	if id := w079IdentityOf(t, capB); id.Repos["w79src"] != localV2 {
		t.Fatalf("claim 2 w79src baseline %q, want the v2 local HEAD %s", id.Repos["w79src"], localV2)
	}
	rawB := w079CapsuleRaw(t, capB)
	for _, tok := range []string{"w079-edit-v2", filepath.Join(ws, ".repo", "w79v2")} {
		if !strings.Contains(rawB, tok) {
			t.Fatalf("claim 2's audit record misses the edited-yaml token %q (raw %s)", tok, rawB)
		}
	}
	if strings.Contains(rawB, "w079-edit-v1") {
		t.Fatalf("claim 2's audit record still carries the pre-edit yaml token (raw %s)", rawB)
	}

	// The same daemon process served both claims — never restarted.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the daemon process %d did not survive the yaml edit (restart required): %v", pid, err)
	}
}

// TestW079InvalidYamlRefusesClaimBeforeMaterialization pins the strict
// schema: every invalid shape refuses the claim with a typed error
// naming .polyforge.yaml (and, where the spec pins it, the offending
// key), the WI stays queued/blocked, nothing materializes, and the
// daemon survives. RED pre-implementation (every case is accepted at
// HEAD).
func TestW079InvalidYamlRefusesClaimBeforeMaterialization(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	cases := []struct {
		name    string
		yaml    string
		needles []string
	}{
		{
			name:    "torn yaml",
			yaml:    "projects:\n  e2e:\n    repos: [\n      - name: w79src\n",
			needles: []string{".polyforge.yaml"},
		},
		{
			name:    "unknown top-level key project",
			yaml:    "project: e2e\nprojects:\n  e2e:\n    repos: []\n",
			needles: []string{".polyforge.yaml", "project"},
		},
		{
			name:    "unknown top-level key workflow_default",
			yaml:    "workflow_default:\n  model_candidates: [a]\n",
			needles: []string{".polyforge.yaml", "workflow_default"},
		},
		{
			name:    "unknown nested key on a repo entry",
			yaml:    "projects:\n  e2e:\n    repos:\n      - name: w79src\n        paths: .repo/w79src\n",
			needles: []string{".polyforge.yaml", "paths"},
		},
		{
			name:    "unknown nested key under workflow_defaults",
			yaml:    "workflow_defaults:\n  model_candidate: [a]\n",
			needles: []string{".polyforge.yaml", "model_candidate"},
		},
		{
			name:    "step_kinds key outside the V8 vocabulary",
			yaml:    "workflow_defaults:\n  step_kinds:\n    w079boguskind: [a]\n",
			needles: []string{".polyforge.yaml", "w079boguskind"},
		},
		{
			name:    "project entry without repos",
			yaml:    "projects:\n  e2e: {}\n",
			needles: []string{".polyforge.yaml"},
		},
		{
			name:    "empty model-id string",
			yaml:    "workflow_defaults:\n  model_candidates: [\"\"]\n",
			needles: []string{".polyforge.yaml"},
		},
		{
			name:    "type mismatch on projects",
			yaml:    "projects: 42\n",
			needles: []string{".polyforge.yaml"},
		},
		{
			name:    "duplicate key",
			yaml:    "projects:\n  e2e:\n    repos: []\n  w079dup:\n    repos: []\n  w079dup:\n    repos: []\n",
			needles: []string{".polyforge.yaml", "w079dup"},
		},
		{
			name:    "unsafe repo name",
			yaml:    "projects:\n  e2e:\n    repos:\n      - name: ../w079escape\n        path: .repo/w79src\n",
			needles: []string{".polyforge.yaml"},
		},
		{
			name:    "absolute repo path",
			yaml:    "projects:\n  e2e:\n    repos:\n      - name: w79src\n        path: /tmp/w079absolute\n",
			needles: []string{".polyforge.yaml"},
		},
		{
			name:    "parent-escaping repo path",
			yaml:    "projects:\n  e2e:\n    repos:\n      - name: w79src\n        path: ../w079esc\n",
			needles: []string{".polyforge.yaml"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w079Yaml(t, ws, c.yaml)
			wi := w113NewWI(t, inst, "e2e", "w079 invalid yaml: "+c.name, envelopeQuickFix())
			out, _ := d.pf(inst, "submit", wi)
			w079WaitRefusal(t, d, inst, ws, wi, out, 60*time.Second, c.needles...)
			// The daemon survives the refusal and keeps serving.
			d.pfWant(inst, -1, "ops", "status")
		})
	}
}

// TestW079FixYamlAndReclaimIsClean pins the fix-and-retry cycle: a
// claim refused for an invalid yaml leaves no half-materialized state,
// and after the operator fixes the file the re-claim succeeds cleanly —
// exactly one capsule, no husks, a coherent audit record. RED
// pre-implementation (the torn yaml is ignored and the claim runs).
func TestW079FixYamlAndReclaimIsClean(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	localHead := w079GitRepo(t, filepath.Join(ws, ".repo", "w79src"), "w79src.txt", "w079 local source\n")
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos:\n      - name: w79src\n        path: .repo/w79src\n   BAD KEY\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wi := w113NewWI(t, inst, "e2e", "w079 fix-and-reclaim cycle", envelopeQuickFix())
	out, _ := d.pf(inst, "submit", wi)
	w079WaitRefusal(t, d, inst, ws, wi, out, 60*time.Second, ".polyforge.yaml")
	if ents, err := os.ReadDir(filepath.Join(ws, "capsules")); err == nil && len(ents) != 0 {
		t.Fatalf("the refused claim left capsule husks: %v", ents)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", filepath.Join(ws, "capsules"), err)
	}

	// The operator fixes the yaml; the WI is re-claimed.
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos:\n      - name: w79src\n        path: .repo/w79src\nworkflow_defaults:\n  model_candidates: [w079-reclaimed]\n")
	if status := inst.wiStatus(t, wi); status == "blocked" {
		// continue = `pf resume` (the blocked door: hub resume-blocked +
		// the daemon's synchronous re-claim; v2-converge §7).
		if out := d.pfWant(inst, -1, "resume", wi); !strings.Contains(out, "resumed") {
			t.Fatalf("resume answer: %q", out)
		}
	} else {
		inst.waitWIStatus(t, wi, "queued", 30*time.Second)
		d.pfWant(inst, -1, "submit", wi)
	}
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113WaitCompleted(t, capDir, 240*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	// Exactly one capsule — no husk, no half-written record.
	if got := w113TopLevel(t, filepath.Join(ws, "capsules")); len(got) != 1 || got[0] != wi {
		t.Fatalf("capsules dir holds %v, want exactly [%s]", got, wi)
	}
	id := w079IdentityOf(t, capDir)
	if id.Repos["w79src"] != localHead {
		t.Fatalf("post-fix w79src baseline %q, want the local HEAD %s", id.Repos["w79src"], localHead)
	}
	if got := w079Model(t, capDir, "grill"); got != "w079-reclaimed" {
		t.Fatalf("post-fix grill model %q, want the fixed yaml's w079-reclaimed", got)
	}
	if raw := w079CapsuleRaw(t, capDir); !strings.Contains(raw, filepath.Join(ws, ".repo", "w79src")) {
		t.Fatalf("post-fix audit record misses the resolved repo source path (raw %s)", raw)
	}
}

// TestW079UnlistedProjectRefusesClaim pins [spec-decided] #5: a
// projects section that does not list the claimed project refuses the
// claim before materialization with a typed error naming the drift —
// no silent registry fallback. RED pre-implementation (the yaml is
// ignored and the claim runs off the registry).
func TestW079UnlistedProjectRefusesClaim(t *testing.T) {
	remoteURL, _ := w079Remote(t, "w079reg.txt", "registry marker\n")
	inst := newV1InstallRepos(t, []api.ProjectRepo{{Name: "w079reg", URL: remoteURL}})
	ws := t.TempDir()
	w079Yaml(t, ws, "projects:\n  w079other:\n    repos: []\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wi := w113NewWI(t, inst, "e2e", "w079 unlisted project refusal", envelopeQuickFix())
	out, _ := d.pf(inst, "submit", wi)
	w079WaitRefusal(t, d, inst, ws, wi, out, 60*time.Second, ".polyforge.yaml", "e2e")
	// No registry fallback filled the gap.
	if _, err := os.Stat(filepath.Join(w079Capsule(ws, wi), "repos", "w079reg")); err == nil {
		t.Fatal("the unlisted-project refusal fell back to cloning the registry repo")
	}
}

// ---------------------------------------------------------------------------
// Model candidates — the four-level precedence chain.
// ---------------------------------------------------------------------------

// TestW079StepKindsReplacesEveryCodeKindStep pins the outermost
// precedence level: yaml step_kinds[code] wholly replaces the bundle's
// step-level list for EVERY kind:code step of the bundle (quick-fix's
// fix AND release — the same closed kind vocabulary feature-full
// uses), while unlisted kinds keep the bundle's own resolution. RED
// pre-implementation.
func TestW079StepKindsReplacesEveryCodeKindStep(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, "workflow_defaults:\n  step_kinds:\n    code: [w079-code-a, w079-code-b]\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wi := w113NewWI(t, inst, "e2e", "w079 step_kinds wholly replaces code-kind steps", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113WaitCompleted(t, capDir, 240*time.Second)

	// Both kind:code steps run under the per-kind pin.
	for _, step := range []string{"fix", "release"} {
		if got := w079Model(t, capDir, step); got != "w079-code-a" {
			t.Fatalf("%s model %q, want the yaml step_kinds head w079-code-a (the per-kind pin wholly replaces the bundle's step-level list)", step, got)
		}
	}
	// Unlisted kinds resolve from the bundle exactly as today.
	if got := w079Model(t, capDir, "grill"); got != w079DefaultsHead {
		t.Fatalf("grill model %q, want the bundle defaults head %q (kind spec is unlisted)", got, w079DefaultsHead)
	}
	if got := w079Model(t, capDir, "verification"); got != w079DefaultsHead {
		t.Fatalf("verification model %q, want the bundle defaults head %q (kind gate is unlisted)", got, w079DefaultsHead)
	}
}

// TestW079StepKindsOtherKindAndStepLevelFallThrough pins the kind
// scoping: step_kinds is keyed by the step's V8 KIND, not its id — a
// step_kinds entry for kind spec hits the grill step while the
// kind:code fix step keeps its own bundle step-level list. RED
// pre-implementation.
func TestW079StepKindsOtherKindAndStepLevelFallThrough(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, "workflow_defaults:\n  step_kinds:\n    spec: [w079-spec-x]\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wi := w113NewWI(t, inst, "e2e", "w079 step_kinds kind scoping", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113WaitCompleted(t, capDir, 240*time.Second)

	if got := w079Model(t, capDir, "grill"); got != "w079-spec-x" {
		t.Fatalf("grill (kind spec) model %q, want the yaml step_kinds pin w079-spec-x", got)
	}
	if got := w079Model(t, capDir, "fix"); got != w079FixStepHead {
		t.Fatalf("fix (kind code, unlisted) model %q, want its own bundle step-level head %q", got, w079FixStepHead)
	}
	if got := w079Model(t, capDir, "release"); got != w079DefaultsHead {
		t.Fatalf("release (kind code, unlisted) model %q, want the bundle step-level head %q", got, w079DefaultsHead)
	}
}

// TestW079FlowDefaultReplacesWeakDefaultStepLevelWins pins the two
// middle levels: the yaml's workflow_defaults.model_candidates
// replaces the bundle's WEAK defaults head for steps without a
// step-level list, and never beats a step-level list the bundle author
// pinned. The yaml is workflow-only (no projects section), so the repo
// inventory still comes from the registry — and the audit records the
// flow default and the verbatim yaml. RED pre-implementation (the
// yaml governs nothing yet).
func TestW079FlowDefaultReplacesWeakDefaultStepLevelWins(t *testing.T) {
	remoteURL, remoteHead := w079Remote(t, "w079reg.txt", "registry marker\n")
	inst := newV1InstallRepos(t, []api.ProjectRepo{{Name: "w079reg", URL: remoteURL}})
	ws := t.TempDir()
	w079Yaml(t, ws, "workflow_defaults:\n  model_candidates: [w079-flow-a, w079-flow-b]\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wi := w113NewWI(t, inst, "e2e", "w079 flow default vs step-level list", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113WaitCompleted(t, capDir, 240*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	// The flow default replaces the bundle's weak default…
	if got := w079Model(t, capDir, "grill"); got != "w079-flow-a" {
		t.Fatalf("grill model %q, want the yaml flow default w079-flow-a", got)
	}
	if got := w079Model(t, capDir, "verification"); got != "w079-flow-a" {
		t.Fatalf("verification model %q, want the yaml flow default w079-flow-a", got)
	}
	// …and never a step-specific list.
	if got := w079Model(t, capDir, "fix"); got != w079FixStepHead {
		t.Fatalf("fix model %q, want its own bundle step-level head %q", got, w079FixStepHead)
	}
	if got := w079Model(t, capDir, "release"); got != w079DefaultsHead {
		t.Fatalf("release model %q, want its own bundle step-level head %q", got, w079DefaultsHead)
	}

	// No projects section: the registry stays the repo source, exactly
	// as at HEAD, and the audit carries the yaml-governed decisions.
	if id := w079IdentityOf(t, capDir); id.Repos["w079reg"] != remoteHead {
		t.Fatalf("registry baseline %q, want the remote HEAD %s (a workflow-only yaml takes over nothing on the repo side)", id.Repos["w079reg"], remoteHead)
	}
	raw := w079CapsuleRaw(t, capDir)
	for _, tok := range []string{"w079-flow-a", "w079-flow-b", "model_candidates"} {
		if !strings.Contains(raw, tok) {
			t.Fatalf("the audit record of a yaml-governed claim misses %q (raw %s)", tok, raw)
		}
	}
}

// w079ProviderErr is the stderr text a crashing fake harness leaves behind:
// a MODEL-class reason, so the driver's exit classification advances the
// candidate chain (v2-converge 2.3 — a bare crash is an unclassified exit
// and retries the same model).
const w079ProviderErr = "upstream provider error: 529 overloaded (fake harness injected)"

// TestW079RotationAdvancesAndCapsOnEffectiveList pins the rotation
// composition on the post-precedence list: two provider-class infra
// failures advance the pointer along the yaml step_kinds list (two
// crashes are inside the recovery budget; a third would trip the storm
// guard), and the pin is capped at the list end — never beyond, never
// wrapping. RED pre-implementation (the effective list is the bundle's).
func TestW079RotationAdvancesAndCapsOnEffectiveList(t *testing.T) {
	cases := []struct {
		name     string
		stepKind string
		want     string
	}{
		{"two failures pin the third entry", "code: [w079r-a, w079r-b, w079r-c]", "w079r-c"},
		{"two failures on a two-entry list cap at the end", "code: [w079r-a, w079r-b]", "w079r-b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst := newV1Install(t)
			ws := t.TempDir()
			w079Yaml(t, ws, "workflow_defaults:\n  step_kinds:\n    "+c.stepKind+"\n")
			cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
				inst.url, inst.adminKey, "",
				[]w113WS{{key: ws, projects: []string{"e2e"}}})
			d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
			d.start()
			defer d.stop()

			wi := w113NewWI(t, inst, "e2e", "w079 rotation on the effective list: "+c.name,
				[]map[string]any{
					{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
					{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "crash": "before", "crash_stderr": w079ProviderErr},
					{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "crash": "before", "crash_stderr": w079ProviderErr},
					{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
					{"do": "envelope", "id": "verification", "outcome": "completed"},
					{"do": "envelope", "id": "release", "outcome": "completed"},
				})
			d.pfWant(inst, -1, "submit", wi)
			capDir := w079Capsule(ws, wi)
			w113WaitCapsule(t, capDir, 90*time.Second)
			w113WaitCompleted(t, capDir, 300*time.Second)
			inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

			providerInfra, _ := w079AttemptStats(t, capDir, "fix")
			if providerInfra != 2 {
				t.Fatalf("fix archived %d provider-class infra failures, want 2 (the two crash-before dispatches)", providerInfra)
			}
			if got := w079Model(t, capDir, "fix"); got != c.want {
				t.Fatalf("fix model %q, want %q — rotation must advance along the yaml-derived effective list and cap at its end", got, c.want)
			}
		})
	}
}

// TestW079GateFailRewindResetsEffectiveHead pins INV-1g's composition
// with the yaml-derived list: one provider failure advances the pin to
// the list's second entry, then a gate FAIL on verification rewinds to
// fix and RESETS the pin to the effective list's head. RED
// pre-implementation.
func TestW079GateFailRewindResetsEffectiveHead(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, "workflow_defaults:\n  step_kinds:\n    code: [w079g-a, w079g-b]\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	verdict := map[string]any{
		"error":   "w079 injected gate verdict (fake harness)",
		"verdict": "FAIL",
	}
	wi := w113NewWI(t, inst, "e2e", "w079 gate-fail rewind resets the effective head",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "crash": "before", "crash_stderr": w079ProviderErr},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "gate_failed", "output": verdict},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		})
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113WaitCompleted(t, capDir, 300*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	providerInfra, gateRewind := w079AttemptStats(t, capDir, "fix")
	if providerInfra != 1 || gateRewind != 1 {
		t.Fatalf("fix attempts: %d provider infra, %d gate-fail rewinds; want 1/1 — the archived history the recomputation indexes", providerInfra, gateRewind)
	}
	if got := w079Model(t, capDir, "fix"); got != "w079g-a" {
		t.Fatalf("fix model after the rewind %q, want the effective list head w079g-a (a gate FAIL rewind resets the pointer)", got)
	}
}

// TestW079RotationUntouchedWithoutYaml pins the kept face of the
// rotation criterion: with NO yaml, the exact same crash + gate-fail
// rewind ladder behaves exactly as at HEAD — the pointer advances on
// the provider failure, the rewind resets to the bundle step-level
// list's head. Green before AND after the change.
func TestW079RotationUntouchedWithoutYaml(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	verdict := map[string]any{
		"error":   "w079 injected gate verdict (fake harness)",
		"verdict": "FAIL",
	}
	wi := w113NewWI(t, inst, "e2e", "w079 rotation untouched without a yaml",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "crash": "before", "crash_stderr": w079ProviderErr},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "gate_failed", "output": verdict},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		})
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113WaitCompleted(t, capDir, 300*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	providerInfra, gateRewind := w079AttemptStats(t, capDir, "fix")
	if providerInfra != 1 || gateRewind != 1 {
		t.Fatalf("fix attempts: %d provider infra, %d gate-fail rewinds; want 1/1", providerInfra, gateRewind)
	}
	if got := w079Model(t, capDir, "fix"); got != w079FixStepHead {
		t.Fatalf("fix model after the rewind %q, want the bundle step-level head %q — rotation semantics must stay exactly as today", got, w079FixStepHead)
	}
}

// TestW079MidflightYamlEditReindexesOnRestart pins the archived-attempt
// recomputation across a mid-flight yaml edit: a work item in flight
// under one step_kinds list, a daemon crash, a yaml edit, and the
// recovery restart — the re-derivation resolves the step's model from
// the NEWLY effective list (the archived provider failure indexes the
// re-derived chain), never from the pre-edit one. RED
// pre-implementation.
func TestW079MidflightYamlEditReindexesOnRestart(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, "workflow_defaults:\n  step_kinds:\n    code: [w079-m1, w079-m2]\n")
	sock := filepath.Join(shortTmp("/tmp", "w079sock"), "d.sock")
	dataDir := t.TempDir()
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})

	wi := w113NewWI(t, inst, "e2e", "w079 mid-flight yaml edit re-indexes the chain",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "ms": 8000},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg), w113DOptSock(sock), w113DOptDataDir(dataDir))
	d.start()
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113StepStatus(t, capDir, "fix", "in_progress", 90*time.Second)
	time.Sleep(2 * time.Second) // into the delayed fix dispatch

	// Crash the daemon mid-flight, edit the yaml, restart on the SAME
	// socket/data/mapping — the recovery re-derives under the new list.
	d.crash()
	w079Yaml(t, ws, "workflow_defaults:\n  step_kinds:\n    code: [w079-n1, w079-n2]\n")
	d2 := w113NewDaemon(t, inst, w113DOptConfig(cfg), w113DOptSock(sock), w113DOptDataDir(dataDir))
	d2.start()
	defer d2.stop()
	w113WaitCompleted(t, capDir, 300*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

	got := w079Model(t, capDir, "fix")
	if got != "w079-n1" && got != "w079-n2" {
		t.Fatalf("fix model after the recovery %q, want one of the EDITED list [w079-n1 w079-n2] — the recomputation must index the re-derived effective list", got)
	}
	// v2-converge 2.3: a daemon restart is NOT a model verdict — the killed
	// in-flight dispatch advances nothing, so the re-derived effective list
	// is indexed from its HEAD.
	if providerInfra, _ := w079AttemptStats(t, capDir, "fix"); providerInfra != 0 {
		t.Fatalf("fix archived %d provider-class infra failures, want 0 (a daemon restart never advances the pointer)", providerInfra)
	}
	if got != "w079-n1" {
		t.Fatalf("fix model after the restart %q, want the head of the edited list w079-n1 (restart re-dispatch: same pointer)", got)
	}
}

// ---------------------------------------------------------------------------
// Repo source — the yaml projects section replaces the aihub registry.
// ---------------------------------------------------------------------------

// TestW079ListedReposCloneFromLocalCheckout pins the local-source
// contract: for a project listed in the yaml, each listed repo clones
// into repos/<name> FROM the resolved local checkout (workspace root +
// path:), the registry URL is ignored, the baseline SHA is recorded as
// today, and the audit carries the resolved source path and the
// verbatim yaml. The yaml is model-silent, so candidates stay
// bundle-only. RED pre-implementation (the registry clone wins).
func TestW079ListedReposCloneFromLocalCheckout(t *testing.T) {
	remoteURL, remoteHead := w079Remote(t, "w079registry.txt", "registry source\n")
	inst := newV1InstallRepos(t, []api.ProjectRepo{{Name: "w079src", URL: remoteURL}})
	ws := t.TempDir()
	localHead := w079GitRepo(t, filepath.Join(ws, ".repo", "w079src"), "w079local.txt", "local source\n")
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos:\n      - name: w079src\n        path: .repo/w079src\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wi := w113NewWI(t, inst, "e2e", "w079 listed repos clone from the local checkout", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113WaitCompleted(t, capDir, 240*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	// Cloned FROM the local checkout, not the registry URL.
	if b, err := os.ReadFile(filepath.Join(capDir, "repos", "w079src", "w079local.txt")); err != nil || string(b) != "local source\n" {
		t.Fatalf("repos/w079src was not cloned from the local checkout: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(capDir, "repos", "w079src", "w079registry.txt")); err == nil {
		t.Fatal("repos/w079src carries the registry remote's marker — the registry URL was used, not the local checkout")
	}
	// The claim-time baseline is the local checkout's HEAD, recorded as
	// today — and the registry's HEAD is NOT what got recorded.
	id := w079IdentityOf(t, capDir)
	if id.Repos["w079src"] != localHead {
		t.Fatalf("w079src baseline %q, want the local checkout HEAD %s", id.Repos["w079src"], localHead)
	}
	if localHead == remoteHead {
		t.Fatal("fixture defect: the local and registry HEADs are identical; the baseline assertion is vacuous")
	}
	// Models stay bundle-only (the yaml is model-silent).
	if got := w079Model(t, capDir, "fix"); got != w079FixStepHead {
		t.Fatalf("fix model %q, want the bundle step-level head %q (a projects-only yaml is model-silent)", got, w079FixStepHead)
	}
	// The audit: the resolved source path and the verbatim yaml.
	raw := w079CapsuleRaw(t, capDir)
	for _, tok := range []string{filepath.Join(ws, ".repo", "w079src"), "w079src", ".repo/w079src"} {
		if !strings.Contains(raw, tok) {
			t.Fatalf("the audit record misses %q (raw %s)", tok, raw)
		}
	}
}

// TestW079EmptyReposListReplacesRegistryWholly pins the empty-list
// rule: a listed project with repos: [] clones NOTHING — the empty
// list is the authoritative inventory and the registry entry is not
// consulted, no fallback fills the gap. RED pre-implementation (the
// two registry repos are cloned at HEAD).
func TestW079EmptyReposListReplacesRegistryWholly(t *testing.T) {
	urlA, _ := w079Remote(t, "w079ra.txt", "registry A\n")
	urlB, _ := w079Remote(t, "w079rb.txt", "registry B\n")
	inst := newV1InstallRepos(t, []api.ProjectRepo{
		{Name: "w079one", URL: urlA},
		{Name: "w079two", URL: urlB},
	})
	ws := t.TempDir()
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos: []\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wi := w113NewWI(t, inst, "e2e", "w079 empty repos list replaces the registry wholly", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113WaitCompleted(t, capDir, 240*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	// Nothing cloned: repos/ is absent or empty.
	reposDir := filepath.Join(capDir, "repos")
	if ents, err := os.ReadDir(reposDir); err == nil && len(ents) != 0 {
		t.Fatalf("repos/ holds %v, want nothing — the empty list is the authoritative inventory", ents)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", reposDir, err)
	}
	if id := w079IdentityOf(t, capDir); len(id.Repos) != 0 {
		t.Fatalf("capsule.json Repos %v, want empty", id.Repos)
	}
}

// TestW079RecoveryContractForLocallySourcedClones pins the recovery
// contract for local-source clones: a repos/<name> that already exists
// at materialize time is never re-cloned, re-fetched or reset from the
// local checkout — presence is verified, the recorded baseline carries
// forward. The local checkout itself is MUTATED after the claim, so a
// re-clone would be detectable in both the baseline and the worktree
// HEAD. RED pre-implementation (no local clone happens at all).
func TestW079RecoveryContractForLocallySourcedClones(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	localDir := filepath.Join(ws, ".repo", "w079src")
	origHead := w079GitRepo(t, localDir, "w079src.txt", "original scene\n")
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos:\n      - name: w079src\n        path: .repo/w079src\n")
	sock := filepath.Join(shortTmp("/tmp", "w079sock"), "d.sock")
	dataDir := t.TempDir()
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})

	wi := w113NewWI(t, inst, "e2e", "w079 recovery contract for locally-sourced clones",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true, "ms": 12000},
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg), w113DOptSock(sock), w113DOptDataDir(dataDir))
	d.start()
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(ws, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113StepStatus(t, capDir, "fix", "in_progress", 90*time.Second)

	// The local-source clone landed and the baseline was recorded.
	reposDir := filepath.Join(capDir, "repos", "w079src")
	if b, err := os.ReadFile(filepath.Join(reposDir, "w079src.txt")); err != nil || string(b) != "original scene\n" {
		t.Fatalf("repos/w079src was not cloned from the local checkout: %q, %v", b, err)
	}
	if id := w079IdentityOf(t, capDir); id.Repos["w079src"] != origHead {
		t.Fatalf("claim-time baseline %q, want the local HEAD %s", id.Repos["w079src"], origHead)
	}
	// The agent's uncommitted edits in the worktree are the live scene.
	// The marker exists to detect a re-clone/reset: any surviving file
	// proves that, whatever its extension.
	if err := os.WriteFile(filepath.Join(reposDir, "w079-keep.md"), []byte("in flight\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The local checkout MOVES ON (the operator commits there).
	if err := os.WriteFile(filepath.Join(localDir, "w079-moved.txt"), []byte("moved on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, localDir, "add", ".")
	gitRun(t, localDir, "commit", "-m", "w079 the local checkout moves on")
	mutatedHead := strings.TrimSpace(gitRun(t, localDir, "rev-parse", "HEAD"))

	// Crash and restart on the same scene: recovery reconciles in place.
	d.crash()
	d2 := w113NewDaemon(t, inst, w113DOptConfig(cfg), w113DOptSock(sock), w113DOptDataDir(dataDir))
	d2.start()
	defer d2.stop()
	w113WaitCompleted(t, capDir, 300*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 120*time.Second)

	// Never re-cloned / re-fetched / reset: the marker survives, the
	// worktree HEAD and the recorded baseline are the claim-time facts.
	if b, err := os.ReadFile(filepath.Join(reposDir, "w079-keep.md")); err != nil || string(b) != "in flight\n" {
		t.Fatalf("the in-flight marker was lost — repos/w079src was re-cloned or reset: %q, %v", b, err)
	}
	if head := strings.TrimSpace(gitRun(t, reposDir, "rev-parse", "HEAD")); head != origHead {
		t.Fatalf("repos/w079src HEAD %q after recovery, want the claim-time %q (the mutated local HEAD is %q)", head, origHead, mutatedHead)
	}
	if id := w079IdentityOf(t, capDir); id.Repos["w079src"] != origHead {
		t.Fatalf("the recorded baseline moved to %q after recovery, want the claim-time %q", id.Repos["w079src"], origHead)
	}
}

// TestW079MissingLocalCheckoutBlocksNeverFallsBack pins [spec-decided]
// #6: a listed repo whose resolved local checkout is missing on disk
// fails through the typed materialization-failure lane — the attempt
// closes blocked, the daemon never crashes, and the registry URL is
// NEVER used as a fallback. RED pre-implementation (the registry clone
// for the same name succeeds at HEAD).
func TestW079MissingLocalCheckoutBlocksNeverFallsBack(t *testing.T) {
	remoteURL, _ := w079Remote(t, "w079gone.txt", "registry fallback\n")
	inst := newV1InstallRepos(t, []api.ProjectRepo{{Name: "w079gone", URL: remoteURL}})
	ws := t.TempDir()
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos:\n      - name: w079gone\n        path: .repo/absent\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wi := w113NewWI(t, inst, "e2e", "w079 missing local checkout", envelopeQuickFix())
	out, _ := d.pf(inst, "submit", wi)
	// The clone-failure lane: the attempt closes blocked (the log names
	// the repo), never a daemon crash.
	waitUntil(t, 90*time.Second, "the missing-local-checkout claim never settled into blocked", func() bool {
		return inst.wiStatus(t, wi) == "blocked"
	})
	if !strings.Contains(out+"\n"+d.logTail(), "w079gone") {
		t.Fatalf("the clone-failure diagnostics never named the repo w079gone (submit output %q, log tail %s)", out, d.logTail())
	}
	// NEVER a registry-URL fallback: no clone of w079gone anywhere.
	if b, err := os.ReadFile(filepath.Join(w079Capsule(ws, wi), "repos", "w079gone", "w079gone.txt")); err == nil {
		t.Fatalf("the claim fell back to cloning the registry URL: %q", b)
	}
	if _, err := os.Stat(filepath.Join(w079Capsule(ws, wi), "repos", "w079gone")); err == nil {
		t.Fatal("repos/w079gone exists — the registry URL was consulted for a yaml-listed repo")
	}
	// The daemon survives and keeps serving.
	d.pfWant(inst, -1, "ops", "status")
}

// ---------------------------------------------------------------------------
// pf file create — single-project-yaml inference.
// ---------------------------------------------------------------------------

// TestW079SingleProjectYamlInfersDefault pins the inference: a cwd
// inside a mapped workspace whose yaml lists exactly ONE project gets
// that project as the create's default — the verb proceeds exactly as
// if --project had been passed. RED pre-implementation (the #113
// required-project error fires).
func TestW079SingleProjectYamlInfersDefault(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos: []\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})

	goal := "w079 inferred default project goal"
	out := w079PfWant(t, inst, -1, w079Env{config: cfg, cwd: ws},
		"file", "create", "--goal", goal)
	if !strings.Contains(out, goal) {
		t.Fatalf("pf file create (inferred): output does not carry the created item (output %q)", out)
	}
	items, err := inst.client.ListWorkItems("e2e", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.Goal == goal {
			found = true
			if it.ProjectName != "e2e" {
				t.Fatalf("the inferred create landed in project %q, want e2e", it.ProjectName)
			}
		}
	}
	if !found {
		t.Fatalf("the inferred create produced no work item in e2e (items %+v, output %q)", items, out)
	}
}

// TestW079MultiProjectYamlAsksForProject pins the ambiguity guard: a
// workspace yaml listing two or more projects, `pf file create` without
// --project errors asking for --project — it never silently picks an
// entry. Kept face: green before AND after (the #113 error asks for
// --project today; the change must not make it ambient).
func TestW079MultiProjectYamlAsksForProject(t *testing.T) {
	inst := newV1Install(t)
	w113EnsureProject(t, inst, "w079second")
	ws := t.TempDir()
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos: []\n  w079second:\n    repos: []\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e", "w079second"}}})

	out, code := w079Pf(t, inst, w079Env{config: cfg, cwd: ws},
		"file", "create", "--goal", "w079 multi-project no pick")
	if code != 1 {
		t.Fatalf("pf file create (multi-project yaml, no --project): exit %d, want 1 (output %q)", code, out)
	}
	if !strings.Contains(out, "--project") {
		t.Fatalf("the error does not ask for --project (output %q)", out)
	}
	for _, project := range []string{"e2e", "w079second"} {
		items, err := inst.client.ListWorkItems(project, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Fatalf("the errored create still produced work items in %s: %+v", project, items)
		}
	}
}

// TestW079NoYamlRequiredProjectErrorStands pins the kept face: a
// matched workspace WITHOUT a yaml (or a cwd matching no workspace)
// keeps today's #113 error — --project required, pointing at the
// [workspace] mapping and pf doctor. Green before AND after.
func TestW079NoYamlRequiredProjectErrorStands(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})

	out, code := w079Pf(t, inst, w079Env{config: cfg, cwd: ws},
		"file", "create", "--goal", "w079 no yaml no default")
	if code != 1 {
		t.Fatalf("pf file create (no yaml): exit %d, want 1 (output %q)", code, out)
	}
	for _, tok := range []string{"--project", "required", "[workspace", "doctor"} {
		if !strings.Contains(out, tok) {
			t.Fatalf("pf file create (no yaml): the #113 error lost the %q guidance token (output %q)", tok, out)
		}
	}

	// A cwd matching no mapped workspace keeps the same error.
	out2, code2 := w079Pf(t, inst, w079Env{config: cfg, cwd: t.TempDir()},
		"file", "create", "--goal", "w079 unmatched cwd no default")
	if code2 != 1 {
		t.Fatalf("pf file create (unmatched cwd): exit %d, want 1 (output %q)", code2, out2)
	}
	if !strings.Contains(out2, "required") {
		t.Fatalf("pf file create (unmatched cwd): no required-project error (output %q)", out2)
	}
}

// TestW079ExplicitProjectBypassesInference pins the bypass: an explicit
// --project is byte-for-byte today's behavior — inference is not
// consulted (an explicit e2e wins over a yaml whose single listed
// project is a different one) and an INVALID yaml never breaks an
// explicit create. Kept face: green before AND after.
func TestW079ExplicitProjectBypassesInference(t *testing.T) {
	inst := newV1Install(t)
	w113EnsureProject(t, inst, "w079second")
	ws := t.TempDir()
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e", "w079second"}}})

	// A valid yaml whose single listed project is NOT the explicit one.
	w079Yaml(t, ws, "projects:\n  w079second:\n    repos: []\n")
	goal := "w079 explicit project wins over inference"
	w079PfWant(t, inst, -1, w079Env{config: cfg, cwd: ws},
		"file", "create", "--project", "e2e", "--goal", goal)
	items, err := inst.client.ListWorkItems("e2e", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.Goal == goal && it.ProjectName == "e2e" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the explicit --project e2e create did not land in e2e (items %+v)", items)
	}

	// An invalid yaml never breaks an explicit create.
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos: [\n")
	goal2 := "w079 explicit project over invalid yaml"
	w079PfWant(t, inst, -1, w079Env{config: cfg, cwd: ws},
		"file", "create", "--project", "e2e", "--goal", goal2)
	items2, err := inst.client.ListWorkItems("e2e", "")
	if err != nil {
		t.Fatal(err)
	}
	found2 := false
	for _, it := range items2 {
		if it.Goal == goal2 && it.ProjectName == "e2e" {
			found2 = true
		}
	}
	if !found2 {
		t.Fatalf("the explicit --project create under an invalid yaml did not land in e2e (items %+v)", items2)
	}
}

// TestW079InvalidYamlUnderInferenceFailsLoudly pins [spec-decided] #7's
// failure posture: an invalid yaml under inference surfaces the typed
// yaml error (naming the file and the reason), never a silent skip to
// the generic required-flag error. RED pre-implementation (the generic
// #113 error fires and does not name the file).
func TestW079InvalidYamlUnderInferenceFailsLoudly(t *testing.T) {
	inst := newV1Install(t)
	ws := t.TempDir()
	w079Yaml(t, ws, "projects:\n  e2e:\n    repos: [\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{{key: ws, projects: []string{"e2e"}}})

	out, code := w079Pf(t, inst, w079Env{config: cfg, cwd: ws},
		"file", "create", "--goal", "w079 invalid yaml under inference")
	if code != 1 {
		t.Fatalf("pf file create (invalid yaml, no --project): exit %d, want 1 (output %q)", code, out)
	}
	if !strings.Contains(out, ".polyforge.yaml") {
		t.Fatalf("the inference failure does not name .polyforge.yaml — the typed yaml error must surface, not a silent skip to the required-flag error (output %q)", out)
	}
}

// ---------------------------------------------------------------------------
// Skill documentation — the greppable floors.
// ---------------------------------------------------------------------------

// TestW079InitFragmentPointerAndWorkspaceInit pins the init rewrite's
// machine-checkable floor: verbs/init.md carries the machine-level
// POINTER (the README install section and tools/install.sh) AND the
// workspace-level init content (the .polyforge.yaml flow: scan,
// selective sync, the [workspace] mapping re-derivation). RED
// pre-implementation (the fragment knows nothing of the yaml).
func TestW079InitFragmentPointerAndWorkspaceInit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(v1Root(t), "skills", "polyforge", "verbs", "init.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, tok := range []string{"README", "install.sh", ".polyforge.yaml", "[workspace", "scan", "sync"} {
		if !strings.Contains(text, tok) {
			t.Fatalf("verbs/init.md does not carry the %q half of the rewrite (file must teach the machine pointer AND the workspace-init flow)", tok)
		}
	}
}

// TestW079SkillTeachesYamlTopologyAndDrift pins the routing-skill
// floor: SKILL.md's session rules teach the session-start
// .polyforge.yaml topology read and the yaml/cli.toml drift surfacing
// (the relocations that supersede the deleted bootstrap.md). RED
// pre-implementation.
func TestW079SkillTeachesYamlTopologyAndDrift(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(v1Root(t), "skills", "polyforge", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, ".polyforge.yaml") {
		t.Fatal("SKILL.md's session rules do not teach the session-start .polyforge.yaml topology read")
	}
	if !strings.Contains(text, "drift") && !strings.Contains(text, "mismatch") {
		t.Fatal("SKILL.md does not teach the yaml/cli.toml drift surfacing")
	}
}

// TestW079ShipFragmentRoutesRepoPathsViaYaml pins the repo-path routing
// floor: the verb fragments that point at repo locations (ship.md, the
// fragment that names repos/<name> worktrees at HEAD) route through the
// yaml projects section. RED pre-implementation.
func TestW079ShipFragmentRoutesRepoPathsViaYaml(t *testing.T) {
	verbsDir := filepath.Join(v1Root(t), "skills", "polyforge", "verbs")
	ents, err := os.ReadDir(verbsDir)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(verbsDir, e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if !strings.Contains(string(raw), "repos/") {
			continue // this fragment does not route repo paths
		}
		checked++
		if !strings.Contains(string(raw), ".polyforge.yaml") {
			t.Fatalf("%s routes repo paths but does not teach the yaml projects section as their source", e.Name())
		}
	}
	if checked == 0 {
		t.Fatal("no verb fragment routes repo paths — the floor lost its subject (ship.md named repos/<name> at HEAD)")
	}
}

// TestW079InstallScriptStaysWorkspaceBlind pins the installer half of
// the init split: tools/install.sh never learns about workspaces — no
// .polyforge.yaml mention anywhere in the machine-install path. Green
// before AND after.
func TestW079InstallScriptStaysWorkspaceBlind(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(v1Root(t), "tools", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "polyforge.yaml") {
		t.Fatal("tools/install.sh learned about workspaces — machine install must stay workspace-blind")
	}
}

// TestW079NoRestartTeachingNearYamlMentions pins the deletion half of
// the fresh-read criterion: no doc that mentions the yaml teaches that
// a daemon restart is needed for an edit to take effect (a line naming
// both the yaml and a restart must carry a negation). Green before AND
// after (nothing mentions the yaml at HEAD).
func TestW079NoRestartTeachingNearYamlMentions(t *testing.T) {
	root := v1Root(t)
	roots := []string{
		filepath.Join(root, "skills"),
		filepath.Join(root, "workflows"),
		filepath.Join(root, "docs", "design", "polyforge-vnext"),
		filepath.Join(root, "README.md"),
	}
	negations := []string{" no ", "not ", "never ", "without", "无需", "不用", "不需要", " without "}
	restarts := []string{"restart", "重启"}
	var hits []string
	for _, r := range roots {
		info, err := os.Stat(r)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			raw, rerr := os.ReadFile(r)
			if rerr != nil {
				t.Fatal(rerr)
			}
			w079ScanRestartLines(r, string(raw), restarts, negations, &hits)
			continue
		}
		err = filepath.WalkDir(r, func(path string, de os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if de.IsDir() || (!strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".sh")) {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			w079ScanRestartLines(path, string(raw), restarts, negations, &hits)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("docs teach a restart after yaml edits — the note must be deleted, not honored (%d hits):\n%s", len(hits), strings.Join(hits, "\n"))
	}
}

// w079ScanRestartLines flags every line of one file that mentions the
// yaml AND a restart without carrying a negation.
func w079ScanRestartLines(path, text string, restarts, negations []string, hits *[]string) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "polyforge.yaml") {
			continue
		}
		hasRestart := false
		for _, r := range restarts {
			if strings.Contains(strings.ToLower(line), strings.ToLower(r)) {
				hasRestart = true
				break
			}
		}
		if !hasRestart {
			continue
		}
		negated := false
		for _, n := range negations {
			if strings.Contains(strings.ToLower(line), strings.ToLower(n)) {
				negated = true
				break
			}
		}
		if !negated {
			*hits = append(*hits, path+": "+strings.TrimSpace(line))
		}
	}
}

// ---------------------------------------------------------------------------
// Placement invariants.
// ---------------------------------------------------------------------------

// TestW079PlacementFollowsCliTomlOnlyUnderYamlDrift pins the placement
// invariant under drift: a workspace whose yaml claims a project the
// cli.toml mapping routes ELSEWHERE has no effect — the claim lands in
// the mapped workspace, the yaml is not a placement source. Kept face:
// green before AND after (placement reads cli.toml only).
func TestW079PlacementFollowsCliTomlOnlyUnderYamlDrift(t *testing.T) {
	inst := newV1Install(t)
	w113EnsureProject(t, inst, "ieops")
	wsX := filepath.Join(t.TempDir(), "wsX") // its yaml claims e2e
	wsY := t.TempDir()                       // the mapping routes e2e here
	if err := os.MkdirAll(wsX, 0o755); err != nil {
		t.Fatal(err)
	}
	w079Yaml(t, wsX, "projects:\n  e2e:\n    repos: []\nworkflow_defaults:\n  model_candidates: [w079-drift-model]\n")
	cfg := w113ConfigFile(t, filepath.Join(t.TempDir(), "cli.toml"),
		inst.url, inst.adminKey, "",
		[]w113WS{
			{key: wsX, projects: []string{"ieops"}},
			{key: wsY, projects: []string{"e2e"}},
		})
	d := w113NewDaemon(t, inst, w113DOptConfig(cfg))
	d.start()
	defer d.stop()

	wi := w113NewWI(t, inst, "e2e", "w079 placement follows cli.toml only", envelopeQuickFix())
	d.pfWant(inst, -1, "submit", wi)
	capDir := w079Capsule(wsY, wi)
	w113WaitCapsule(t, capDir, 90*time.Second)
	w113WaitCompleted(t, capDir, 240*time.Second)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	// Placement went through the mapping; wsX's yaml never moved it.
	if _, err := os.Stat(w079Capsule(wsX, wi)); err == nil {
		t.Fatalf("the claim landed in %s — the yaml moved capsule placement", w079Capsule(wsX, wi))
	}
	// The read yaml is the MAPPED workspace's (wsY's, absent): the
	// models stay bundle-only, wsX's flow default governs nothing.
	if got := w079Model(t, capDir, "grill"); got != w079DefaultsHead {
		t.Fatalf("grill model %q, want the bundle defaults head %q (wsX's yaml must not govern wsY's claim)", got, w079DefaultsHead)
	}
}
