package test

// w081_agent_role_identity_floor_test.go: the polyforge#81 content-floor
// suite (the plan's final unit, role_contract_floor_tests). Where
// w081_context_identity_acceptance_test.go pins the BEHAVIOR through the
// real e2e surfaces (daemon env face, fakepi journal), this file pins the
// TAUGHT CONTRACT on the document surfaces only — the repo's established
// content-floor pattern (test/w079_*, test/w113_* read the skill surfaces
// via v1Root(t) and assert what the docs must teach):
//
//   - SKILL.md teaches the `pf watch` lane (v2-converge §3): subscription
//     = adoption, the fixed-template decision line the agent must not
//     answer, the Claude Code statusLine + wake loop and the pi adapter,
//     and no longer teaches the retired watch.sh arming / ask-to-adopt;
//   - verbs/submit.md and verbs/resume.md say the chat is subscribed
//     automatically (nothing to arm);
//   - no bootstrap resurrection: skills/polyforge's top level stays
//     SKILL.md + hooks/ + extensions/ + verbs/, and no using-polyforge-shaped file
//     appears anywhere the docs live.
//
// The PF_AGENT_ROLE execution-role marker and the SKILL.md role-detection
// section it routed were deleted (#136: step processes never load the
// skill, so the marker had no reader); their pins were removed with them.

import (
	"os"
	"path/filepath"
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

// TestW081FloorWatchLaneTeaching pins the taught contract of the pf watch
// lane: subscription is the ownership model, a decision line is a fixed
// template the agent never answers, the question text is read only on
// request, and both harness adapters are documented.
func TestW081FloorWatchLaneTeaching(t *testing.T) {
	full := w081FloorSkill(t)
	section := w081FloorSection(t, full, "## Human coordination: `pf watch`", "## Session rules")
	for _, tok := range []string{
		"Subscription is the whole ownership model", "pf watch subscribe", "PF_CHAT_ID",
		"the agent must not answer it", "Never answer a decision line yourself",
		"pf watch <wi> --once", "queued/preparing", "daemon unreachable; progress paused",
		"statusLine", "pf watch --statusline --harness-session -", "--wait-decision",
		"COMMAND AGAIN", "extensions/pf-chat.ts", "triggerTurn",
	} {
		if !strings.Contains(section, tok) {
			t.Fatalf("the pf watch section no longer teaches %q", tok)
		}
	}
}

// TestW081FloorRetiredWatchLaneIsGone pins the removal of the old watcher
// script lane from every document surface.
func TestW081FloorRetiredWatchLaneIsGone(t *testing.T) {
	root := v1Root(t)
	if _, err := os.Stat(filepath.Join(root, "skills", "polyforge", "scripts", "watch.sh")); err == nil {
		t.Fatal("skills/polyforge/scripts/watch.sh must be deleted")
	}
	for _, rel := range []string{"SKILL.md", "verbs/submit.md", "verbs/resume.md", "verbs/watch.md", "verbs/doctor.md", "verbs/help.md", "verbs/init.md"} {
		raw, err := os.ReadFile(filepath.Join(root, "skills", "polyforge", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, stale := range []string{"watch.sh", "watch-probe", "ask-to-adopt", ".polyforge/watchers", "summarize_model"} {
			if strings.Contains(text, stale) {
				t.Fatalf("%s still mentions the retired %q", rel, stale)
			}
		}
	}
}

// TestW081FloorVerbFragmentsSayChatIsSubscribed pins submit's and resume's
// wording: the submitting chat is subscribed by the daemon, nothing to arm.
func TestW081FloorVerbFragmentsSayChatIsSubscribed(t *testing.T) {
	for _, frag := range []string{"submit.md", "resume.md"} {
		raw, err := os.ReadFile(filepath.Join(v1Root(t), "skills", "polyforge", "verbs", frag))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "subscribes") || !strings.Contains(string(raw), "pf watch") {
			t.Fatalf("verbs/%s does not say the chat is subscribed automatically", frag)
		}
	}
}

// TestW081FloorNoBootstrapResurrection pins the documentation confinement
// on the final tree: skills/polyforge's top level stays exactly
// SKILL.md + hooks/ + extensions/ + verbs/ (the role routing lives in SKILL.md and
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
