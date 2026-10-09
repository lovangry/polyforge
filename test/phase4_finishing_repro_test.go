package test

// phase4_finishing_repro_test.go: the polyforge#111 (phase-4 finishing pass)
// REPRO artifact, authored by the repro step of work item wi_rg7HTzrbfC7X.
//
// These tests encode the work item's acceptance criteria as executable
// assertions. At authoring time (base commit 3b21e32) every test here
// FAILS for the reported reason: the legacy-engine deletion pass
// (polyforge#106) left (B1) the `--answer-ack-window` knob parsed,
// threaded and advertised, and (B2) design docs 02/03/05 untouched with
// stale WI-Agent references, 03 still teaching the deleted executor.
//
// After the phase-4 finishing pass lands, all three must pass:
//
//   - TestAnswerAckWindowKnobIsGone: acceptance "…and its plumbing gone
//     (grep-clean)". Scope: the live tree excluding .git, the v0 legacy
//     snapshot, the historical pilot registers (docs/pilot/**, kept
//     verbatim by WI B2), and this file (it names the knob, by
//     necessity).
//   - TestStaleAnswerAckWindowUsageFailsLoudly: acceptance "a stale
//     usage fails loudly" — `pf daemon --answer-ack-window 2m` must be
//     rejected at flag parse (exit 2, flag-not-defined), not silently
//     accepted.
//   - TestDocsDeletionPassApplied: acceptance "`git diff -- docs/
//     design/polyforge-vnext/02*03*05*` non-empty" — the three docs
//     must no longer be byte-identical to the pre-pass baseline digests
//     recorded below.
//
// The in-process daemon.Main call in the stale-usage test passes NO
// --workspace and scrubs POLYFORGE_WORKSPACE, so on the current tree the
// call deterministically exits 1 at the workspace guard long before any
// listener, socket or process is created — the test observes flag-parse
// behavior only, cheaply and hermetically.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/polyforge/daemon"
)

// answerAckNeedles are the identifier spellings of the knob and its
// plumbing (flag name, Go identifier, and the var spelling used by the
// harness tests).
var answerAckNeedles = []string{
	"answer-ack-window",
	"AnswerAckWindow",
	"answerAckWindow",
}

// answerAckGrepExempt paths (relative to module root, slash-separated)
// that legitimately mention the knob and are out of the deletion's
// scope: this repro file itself, and the verbatim-kept historical pilot
// registers.
var answerAckGrepExempt = map[string]bool{
	"test/phase4_finishing_repro_test.go": true,
	"docs/pilot/v1-wrapped-register.md":   true,
}

func TestAnswerAckWindowKnobIsGone(t *testing.T) {
	root := v1Root(t)
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "v0" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		relSlash := filepath.ToSlash(rel)
		if answerAckGrepExempt[relSlash] {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".go" && ext != ".md" && ext != ".sh" && ext != ".toml" && ext != ".json" && ext != "Makefile" && filepath.Base(path) != "Makefile" && filepath.Base(path) != "README" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, needle := range answerAckNeedles {
			if strings.Contains(string(data), needle) {
				hits = append(hits, relSlash+" ("+needle+")")
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(hits)
	if len(hits) > 0 {
		t.Errorf("--answer-ack-window plumbing is still grep-visible in the live tree (%d files):\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}

func TestStaleAnswerAckWindowUsageFailsLoudly(t *testing.T) {
	t.Setenv("POLYFORGE_WORKSPACE", "")
	var stdout, stderr bytes.Buffer
	code := daemon.Main([]string{"--answer-ack-window", "2m"}, strings.NewReader(""), &stdout, &stderr)
	errOut := stderr.String()
	t.Logf("exit=%d stderr=%q", code, errOut)
	if code != 2 {
		t.Errorf("stale flag `--answer-ack-window 2m` must be rejected at flag parse (exit 2), got exit %d — the knob is silently accepted", code)
	}
	if !strings.Contains(errOut, "flag provided but not defined") || !strings.Contains(errOut, "answer-ack-window") {
		t.Errorf("stderr must name the stale flag as undefined; got: %q", errOut)
	}
	if strings.Contains(errOut, "no workspace selected") {
		t.Errorf("parse must fail BEFORE the workspace guard; stderr reveals the flag was accepted and execution continued: %q", errOut)
	}
}

// docsDeletionPassBaseline records the sha256 of the three design docs
// at the pre-pass base (commit 3b21e32, phase-4 repro authoring time).
// The deletion pass must change all three (non-empty diff), so the
// live digest must differ from the baseline for every file.
var docsDeletionPassBaseline = map[string]string{
	"02-aihub-core.md":             "30c18041919293c03fcccf1b7fae998d6278399db989c81189afc84ad3550c9a",
	"03-execution-capsule.md":      "99cf430c2b55f4706b09761c6d294a8395aec7c0f70dc575a922ef6c1a54ec93",
	"05-sync-revision-recovery.md": "e546421f7c4ad7a0dcd921a27579354967bc7701556517ac716899c029ec623f",
}

func TestDocsDeletionPassApplied(t *testing.T) {
	dir := filepath.Join(v1Root(t), "docs", "design", "polyforge-vnext")
	for name, base := range docsDeletionPassBaseline {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got == base {
			t.Errorf("%s is byte-identical to the pre-pass baseline — the B2 deletion pass (rewrite of the stale WI-Agent refs, 03's executor teaching) has not touched it", name)
		}
	}
}
