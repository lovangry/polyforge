package test

// w081_agent_role_identity_floor_test.go: the polyforge#81 content-floor
// suite (the plan's final unit, role_contract_floor_tests). Where
// w081_context_identity_acceptance_test.go pins the BEHAVIOR through the
// real e2e surfaces (daemon env face, fakepi journal), this file pins the
// TAUGHT CONTRACT on the document surfaces only — the repo's established
// content-floor pattern (test/w079_*, test/w113_* read the skill surfaces
// via v1Root(t) and assert what the docs must teach):
//
//   - SKILL.md teaches BOUND-ONLY needs_input and LAST-PROGRESS
//     presentation, the watchers-directory bound set, name-only
//     machine-global listing on an explicit ask, and the unchanged daemon
//     watchdog recovery;
//   - SKILL.md teaches the ask-to-adopt channel (batched ask, explicit
//     consent, header-rewrite adoption with the armed_at refresh, the
//     adopted binding's answer-presentation duty, concurrent-session
//     header authority, legacy #76-header scripts) and does NOT teach
//     automatic same-workspace adoption;
//   - verbs/submit.md and verbs/resume.md carry the explicit-adoption
//     wording in the fragments' own words;
//   - no bootstrap resurrection: skills/polyforge's top level stays
//     SKILL.md + scripts/ + verbs/, and no using-polyforge-shaped file
//     appears anywhere the docs live.
//
// The PF_AGENT_ROLE execution-role marker and the SKILL.md role-detection
// section it routed were deleted (#136: step processes never load the
// skill, so the marker had no reader); their pins were removed with them.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// w081FloorSkill loads SKILL.md and returns its full text.
func w081FloorSkill(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(v1Root(t), "skills", "polyforge", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// w081FloorSection returns the text between two section headings.
func w081FloorSection(t *testing.T, full, start, end string) string {
	t.Helper()
	i, j := strings.Index(full, start), strings.Index(full, end)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("SKILL.md lost the section anchors %q / %q — the floor lost its subject", start, end)
	}
	return full[i:j]
}

// w081FloorWatchLane returns the watch-lane text (from its heading to the
// end of the file).
func w081FloorWatchLane(t *testing.T, full string) string {
	t.Helper()
	i := strings.Index(full, "## The watch lane")
	if i < 0 {
		t.Fatal("SKILL.md lost the watch-lane anchor — the adoption floor lost its subject")
	}
	return full[i:]
}

// w081FloorHasWord answers whether text contains tok as a whole word
// (CJK tokens fall back to plain containment).
func w081FloorHasWord(text, tok string) bool {
	if !regexp.MustCompile(`^[A-Za-z]+$`).MatchString(tok) {
		return strings.Contains(text, tok)
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(tok) + `\b`)
	if err != nil {
		return strings.Contains(text, tok)
	}
	return re.MatchString(text)
}

// TestW081FloorTurnStartDutyIsBoundOnly pins the #117 correction's taught
// contract: the turn-start obligation section derives a BOUND set from the
// watchers directory (own session_id headers + explicit adoption), gates
// needs_input AND LAST-PROGRESS presentation on it, never surfaces
// unbound jobs proactively (name-only on an explicit machine-global ask),
// and leaves machine-level stall recovery with the daemon's watchdogs.
func TestW081FloorTurnStartDutyIsBoundOnly(t *testing.T) {
	full := w081FloorSkill(t)
	section := w081FloorSection(t, full, "## Turn-start obligation", "## The watch lane")
	// The bound set: derived from the watchers directory (session_id
	// headers), plus explicit adoption — and `pf ops status` alone
	// binds nothing.
	for _, tok := range []string{"bound", "watchers/", "session_id", "adopt"} {
		if !strings.Contains(section, tok) {
			t.Fatalf("the turn-start obligation section does not derive the bound set via %q", tok)
		}
	}
	if !strings.Contains(section, "machine-global") || !strings.Contains(section, "pf ops status") {
		t.Fatal("the turn-start obligation section does not contrast the bound set with the machine-global pf ops status sweep")
	}
	// Both presentation duties ride the bound gate.
	for _, tok := range []string{"needs_input", "LAST-PROGRESS"} {
		if !strings.Contains(section, tok) {
			t.Fatalf("the bound-only gate does not cover %q presentation", tok)
		}
	}
	// Unbound jobs: never proactive; name-only on an explicit
	// machine-global ask.
	if !strings.Contains(section, "BY NAME ONLY") {
		t.Fatal("the turn-start obligation section does not teach name-only listing for unbound WIs on an explicit machine-global status ask")
	}
	// Machine-level stall recovery stays with the daemon's watchdogs.
	if !strings.Contains(section, "watchdogs") || !strings.Contains(section, "unchanged") {
		t.Fatal("the turn-start obligation section does not teach that the daemon's watchdogs keep machine-level stall recovery unchanged")
	}
	// The incident this gating forbids stays named.
	if !strings.Contains(section, "117") {
		t.Fatal("the turn-start obligation section no longer names the #117 cross-session hijack this gating forbids")
	}
}

// TestW081FloorAdoptionIsAskToAdoptWithHeaderRewrite pins the adoption
// channel's taught contract on the watch lane: predecessor (non-live-
// session) scripts go through a batched ask-the-owner prompt; consent
// adopts by REWRITING the session_id header and refreshing armed_at; the
// adopted binding carries the answer-presentation duty; concurrent
// sessions keep the header authoritative; legacy #76-header scripts
// follow the identical rule.
func TestW081FloorAdoptionIsAskToAdoptWithHeaderRewrite(t *testing.T) {
	full := w081FloorSkill(t)
	lane := w081FloorWatchLane(t, full)
	if !strings.Contains(lane, "adopt") {
		t.Fatal("the watch-lane teaching never mentions adoption — the ask-to-adopt channel for predecessor scripts is unspecified")
	}
	asked := false
	for _, tok := range []string{"ask", "consent", "explicit", "确认", "征求"} {
		if w081FloorHasWord(lane, tok) {
			asked = true
			break
		}
	}
	if !asked {
		t.Fatal("the watch-lane adoption teaching carries no ask-the-owner consent step")
	}
	// The batched ask for predecessor scripts.
	for _, tok := range []string{"Predecessor", "batched"} {
		if !strings.Contains(lane, tok) {
			t.Fatalf("the watch lane does not teach the batched ask for %q (non-live-session) scripts", tok)
		}
	}
	// Adoption is a header rewrite: session_id line rewritten to the
	// adopting session, armed_at refreshed.
	for _, tok := range []string{"rewrite", "armed_at"} {
		if !strings.Contains(lane, tok) {
			t.Fatalf("the watch lane does not teach header-rewrite adoption via %q", tok)
		}
	}
	// An adopted binding carries the answer-presentation duty.
	if !strings.Contains(lane, "presentation duty") {
		t.Fatal("the watch lane does not teach that an adopted binding carries the answer-presentation duty")
	}
	// Concurrent sessions: the header is authoritative; the only path
	// to ownership is ask-to-adopt.
	if !strings.Contains(lane, "authoritative") {
		t.Fatal("the watch lane does not teach that the pf-watcher session_id: header stays authoritative among concurrent sessions")
	}
	// Legacy #76-header scripts follow the identical ask-to-adopt rule.
	if !strings.Contains(lane, "#76") {
		t.Fatal("the watch lane does not teach the legacy #76-header script rule")
	}
}

// TestW081FloorNoAutomaticAdoptionTeaching re-pins the absent-behavior
// guard on the floor suite's own terms: no skill/verb/workflow document
// may pair adoption with an automatic / silent mechanism (a line carrying
// both). Green before AND after — this floor exists so the adoption
// teaching can never grow the WI §3 behavior back.
func TestW081FloorNoAutomaticAdoptionTeaching(t *testing.T) {
	root := v1Root(t)
	autos := []string{"automatic", "automatically", "silently", "silent"}
	var hits []string
	for _, r := range []string{filepath.Join(root, "skills"), filepath.Join(root, "workflows")} {
		err := filepath.WalkDir(r, func(path string, de os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if de.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, line := range strings.Split(string(raw), "\n") {
				low := strings.ToLower(line)
				if !strings.Contains(low, "adopt") {
					continue
				}
				for _, a := range autos {
					if strings.Contains(low, a) {
						hits = append(hits, path+": "+strings.TrimSpace(line))
						break
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("docs teach automatic adoption — adoption is ask-only, the WI §3 behavior must stay absent (%d hits):\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

// TestW081FloorVerbFragmentsCarryAdoptionRule pins the touched verb
// fragments' contract in the fragments' own words: submit's arm step and
// resume's re-arm lane state the ownership rule — another session's
// script is reachable only through ask-to-adopt, with resume carrying the
// explicit consent + header-rewrite wording.
func TestW081FloorVerbFragmentsCarryAdoptionRule(t *testing.T) {
	for _, frag := range []string{"submit.md", "resume.md"} {
		raw, err := os.ReadFile(filepath.Join(v1Root(t), "skills", "polyforge", "verbs", frag))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		// The ownership/adoption wording in the fragment's own words.
		for _, tok := range []string{"session_id", "ask-to-adopt"} {
			if !strings.Contains(text, tok) {
				t.Fatalf("verbs/%s does not carry the ownership rule token %q", frag, tok)
			}
		}
	}
	// resume.md additionally carries the explicit-consent +
	// header-rewrite adoption mechanics (the re-arm lane is where a
	// session meets predecessor scripts).
	raw, err := os.ReadFile(filepath.Join(v1Root(t), "skills", "polyforge", "verbs", "resume.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"explicit consent", "armed_at", "Predecessor"} {
		if !strings.Contains(string(raw), tok) {
			t.Fatalf("verbs/resume.md does not carry the explicit-adoption mechanic %q", tok)
		}
	}
}

// TestW081FloorNoBootstrapResurrection pins the documentation confinement
// on the final tree: skills/polyforge's top level stays exactly
// SKILL.md + scripts/ + verbs/ (the role routing lives in SKILL.md and
// the touched verb fragments, never a new document), and no
// using-polyforge-shaped file appears anywhere the docs live. Green
// before AND after.
func TestW081FloorNoBootstrapResurrection(t *testing.T) {
	skillDir := filepath.Join(v1Root(t), "skills", "polyforge")
	ents, err := os.ReadDir(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".md") && e.Name() != "SKILL.md" {
			t.Fatalf("skills/polyforge gained a top-level doc %q — role routing must live in SKILL.md and the verb fragments, no bootstrap resurrection", e.Name())
		}
	}
	for _, base := range []string{"skills", "docs", "workflows"} {
		err := filepath.WalkDir(filepath.Join(v1Root(t), base), func(path string, de os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if de.IsDir() {
				return nil
			}
			if strings.Contains(strings.ToLower(de.Name()), "using-polyforge") {
				return os.ErrInvalid
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bootstrap resurrection under %s/: %v", base, err)
		}
	}
}
