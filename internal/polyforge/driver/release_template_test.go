package driver

// release_template_test.go: content regression pin for the shared release
// step templates after polyforge#123's dispatcher rework. The three
// templates are project-generic dispatchers; the release procedure itself
// lives in each project's repo (.polyforge/release.sh|.md). These tests
// grep the templates for polyforge-private procedure markers and pin the
// one-dispatcher-body convention and the sanctioned output-schema
// extension (verdict enum [pass, defer, fail]), so a future edit cannot
// quietly re-inline a private procedure or fork the bodies per lane.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseBundles are the three bundles whose shared release template the
// rework rewrote; each is pinned independently (all three must stay
// consistent).
var releaseBundles = []string{"bug-fix", "feature-full", "quick-fix"}

// releaseTemplatePath answers one bundle's shared release template path,
// relative to this package's directory (internal/polyforge/driver → the
// repo root is three levels up).
func releaseTemplatePath(bundle string) string {
	return filepath.Join("..", "..", "..", "workflows", bundle, "steps", "release.md")
}

// readReleaseTemplate reads one shared release template, failing loudly.
func readReleaseTemplate(t *testing.T, bundle string) string {
	t.Helper()
	raw, err := os.ReadFile(releaseTemplatePath(bundle))
	if err != nil {
		t.Fatalf("cannot read the shared %s release template: %v", bundle, err)
	}
	return string(raw)
}

// templateBody returns the template's body lines (everything after the
// closing frontmatter fence), trimmed per line.
func templateBody(t *testing.T, bundle string) []string {
	t.Helper()
	lines := strings.Split(readReleaseTemplate(t, bundle), "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		t.Fatalf("%s: no frontmatter opening fence", releaseTemplatePath(bundle))
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			var out []string
			for _, ln := range lines[i+1:] {
				out = append(out, strings.TrimSpace(ln))
			}
			return out
		}
	}
	t.Fatalf("%s: no frontmatter closing fence", releaseTemplatePath(bundle))
	return nil
}

// requireLine asserts one exact (trimmed) line exists in lines.
func requireLine(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, ln := range lines {
		if strings.TrimSpace(ln) == want {
			return
		}
	}
	t.Fatalf("template frontmatter lost the pinned line %q (the frontmatter contract must stay byte-identical to shipped except the sanctioned verdict enum extension)", want)
}

// privateProcedureMarkers are the polyforge-private procedure markers that
// MUST NOT survive in any of the three shared templates: the procedure
// moved to the repo's .polyforge/release.sh and the dispatcher ships
// project-generic (P0d/P0a). Case-insensitive.
var privateProcedureMarkers = []string{
	"releases_root",               // the intranet publish root variable
	"gowork=off make release",     // the private build line
	"sha256sums",                  // the private checksum manifest
	"polyforge release procedure", // the inlined private procedure section
	`mkdir "$releases_root`,       // the private atomic version claim
	"sort -vu",                    // the private union-floor command
	"git tag -l",                  // the private floor computation
	"intranet",                    // the private distribution story
	"aihub",                       // private build/deploy targets
	"-ldflags",                    // the private ldflags stamp
	"pf upgrade",                  // the private upgrade sequence
	"checksum",                    // the private manifest prose
	"00-architecture.md",          // private design-doc refs
	"07-deployment-pilot.md",
}

// multisetDiff returns the lines of a that are not covered by b (multiset
// semantics: duplicate counts matter).
func multisetDiff(a, b []string) []string {
	counts := map[string]int{}
	for _, l := range b {
		counts[l]++
	}
	var out []string
	for _, l := range a {
		if counts[l] > 0 {
			counts[l]--
		} else {
			out = append(out, l)
		}
	}
	return out
}

// TestReleaseTemplatesCarryNoPrivateProcedure pins the dispatcher posture:
// none of the three shared release templates carries polyforge-private
// procedure content — the procedure lives in the repo's own
// .polyforge/release.sh, and the templates stay project-generic.
func TestReleaseTemplatesCarryNoPrivateProcedure(t *testing.T) {
	for _, bundle := range releaseBundles {
		t.Run(bundle, func(t *testing.T) {
			raw := strings.ToLower(readReleaseTemplate(t, bundle))
			if strings.TrimSpace(raw) == "" {
				t.Fatalf("%s: the shared release template is empty", releaseTemplatePath(bundle))
			}
			for _, marker := range privateProcedureMarkers {
				if strings.Contains(raw, marker) {
					t.Fatalf("the shared %s release template carries polyforge-private procedure text %q — the procedure lives in the repo's .polyforge/release.sh, never in the shared template", bundle, marker)
				}
			}
		})
	}
}

// TestReleaseDispatcherBodySharedAcrossBundles pins the one-dispatcher
// convention: bug-fix and feature-full share one dispatcher body
// byte-for-byte; quick-fix is that body plus exactly ONE extra pinned line
// stating this bundle always defers; the shared body never inspects ticket
// type (no bundle name appears in it); the procedure-file contract (both
// file spellings, defer-on-absent, the one-owner rule) is stated; and no
// bundle's dispatcher ever references the deploy procedure files (deploy is
// never triggered by an automated workflow step).
func TestReleaseDispatcherBodySharedAcrossBundles(t *testing.T) {
	ff := templateBody(t, "feature-full")
	bf := templateBody(t, "bug-fix")
	qf := templateBody(t, "quick-fix")

	if strings.Join(bf, "\n") != strings.Join(ff, "\n") {
		t.Fatalf("the bug-fix and feature-full release bodies differ — the three shared templates must share ONE dispatcher body (bug-fix keeps full dispatcher semantics)")
	}
	extra := multisetDiff(qf, ff)
	if len(extra) != 1 {
		t.Fatalf("the quick-fix release body differs from the shared dispatcher by %d lines, want exactly one pinned always-defer line (diff: %q)", len(extra), extra)
	}
	if !strings.Contains(strings.ToLower(extra[0]), "defer") {
		t.Fatalf("the quick-fix body's single extra line is not the always-defer pin: %q", extra[0])
	}

	shared := strings.Join(ff, "\n")
	for _, bundle := range releaseBundles {
		if strings.Contains(shared, bundle) {
			t.Fatalf("the shared dispatcher body names the bundle %q — the dispatcher must not inspect ticket type", bundle)
		}
	}
	for _, needle := range []string{".polyforge/release.sh", ".polyforge/release.md", "defer", "owner"} {
		if !strings.Contains(shared, needle) {
			t.Fatalf("the shared dispatcher body does not state the procedure-file contract (missing %q)", needle)
		}
	}
	for _, bundle := range releaseBundles {
		body := strings.Join(templateBody(t, bundle), "\n")
		if strings.Contains(body, ".polyforge/deploy") {
			t.Fatalf("the shared %s release dispatcher references .polyforge/deploy — deploy must never be triggered by an automated workflow step", bundle)
		}
	}
}

// TestReleaseSchemaAdmitsDeferVerdict pins the output-schema half of the
// verdict-enum extension: each shared release template's frontmatter keeps
// name/description/tools byte-identical to shipped, carries the extended
// top-level verdict enum [pass, defer, fail], keeps the pass and fail oneOf
// branches exactly as shipped (fail still requires fail_reason), and gains
// exactly one defer branch that requires neither version nor
// artifacts_published.
func TestReleaseSchemaAdmitsDeferVerdict(t *testing.T) {
	shippedDescription := map[string]string{
		"feature-full": "description: Project-specific release hook — cut a version, build/tag/publish per this project's own convention, after review has passed. Template; each project fills in its own procedure.",
		"bug-fix":      "description: Project-specific release hook - cut a version, build/tag/publish per this project's own convention, after review has passed. Template; each project fills in its own procedure.",
		"quick-fix":    "description: Project-specific release hook - cut a version, build/tag/publish per this project's own convention, after verification has passed. Template; each project fills in its own procedure.",
	}
	for _, bundle := range releaseBundles {
		t.Run(bundle, func(t *testing.T) {
			lines := strings.Split(readReleaseTemplate(t, bundle), "\n")
			requireLine(t, lines, "name: release")
			requireLine(t, lines, shippedDescription[bundle])
			requireLine(t, lines, "tools: [read, bash]")
			// The sanctioned extension, and only it.
			requireLine(t, lines, "enum: [pass, defer, fail]")
			// The pass branch stays as shipped.
			requireLine(t, lines, "verdict: { type: string, enum: [pass] }")
			requireLine(t, lines, "required: [verdict, version, artifacts_published]")
			// The fail branch stays as shipped (fail still rewinds, with a
			// required fail_reason).
			requireLine(t, lines, "verdict: { type: string, enum: [fail] }")
			requireLine(t, lines, "fail_reason: { type: string, minLength: 1 }")
			requireLine(t, lines, "required: [verdict, fail_reason]")
			// Exactly one defer branch, owing neither version nor
			// artifacts_published.
			deferIdx, count := -1, 0
			for i, ln := range lines {
				if strings.Contains(ln, "enum: [defer]") {
					count++
					deferIdx = i
				}
			}
			if count != 1 {
				t.Fatalf("the %s release schema carries %d defer oneOf branches, want exactly 1", bundle, count)
			}
			var req string
			for j := deferIdx + 1; j < len(lines); j++ {
				ln := lines[j]
				if strings.TrimSpace(ln) == "---" || strings.HasPrefix(ln, "    - ") {
					break
				}
				if strings.Contains(ln, "required:") {
					req = strings.TrimSpace(ln)
				}
			}
			if req == "" {
				t.Fatalf("the %s defer oneOf branch has no required: line", bundle)
			}
			if !strings.Contains(req, "verdict") {
				t.Fatalf("the %s defer branch's required line does not pin the verdict: %q", bundle, req)
			}
			if strings.Contains(req, "version") || strings.Contains(req, "artifacts_published") {
				t.Fatalf("the %s defer branch must not require version/artifacts_published: %q", bundle, req)
			}
		})
	}
}
