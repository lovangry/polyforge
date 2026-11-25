// carries: workflows/_shared/skills/build.md, workflows/_shared/skills/publish.md, workflows/_shared/skills/review.md, workflows/_shared/skills/verify.md, test/owner_language_contract_test.go
package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerFacingRequestsUseSimplifiedChineseWithoutTranslatingTokens(t *testing.T) {
	root := v1Root(t)
	files := []string{
		"workflows/_shared/skills/build.md",
		"workflows/_shared/skills/publish.md",
		"workflows/_shared/skills/review.md",
		"workflows/_shared/skills/verify.md",
	}
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		required := map[string]string{
			"workflows/_shared/skills/build.md":   "Write all owner-facing explanatory prose in Simplified Chinese.",
			"workflows/_shared/skills/publish.md": "Write all owner-facing explanatory prose in Simplified Chinese;",
			"workflows/_shared/skills/review.md":  "Write all owner-facing explanatory prose in Simplified Chinese.",
			"workflows/_shared/skills/verify.md":  "Write all owner-facing explanatory prose in Simplified Chinese",
		}[rel]
		if !strings.Contains(s, required) {
			t.Errorf("%s does not require English instruction for Simplified Chinese owner-facing prose", rel)
		}
		if strings.Contains(s, "使用简体中文") || strings.Contains(s, "解释 使用简体中文") {
			t.Errorf("%s puts mixed-language wording into the English step-agent instructions", rel)
		}
	}
	// publish's tokens below are read by name, never by the files slice's
	// order (build.md sorts first; polyforge#48)
	publish, err := os.ReadFile(filepath.Join(root, "workflows/_shared/skills/publish.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{
		"owner-review:", "「过目通过」", "「不通过:",
		// the landing title's one line is Simplified Chinese by default, the
		// work item's own goal deciding the language (polyforge#47)
		"the work item's own goal decides the language",
		"no reason to write this one in English",
	} {
		if !strings.Contains(string(publish), token) {
			t.Errorf("publish lost structured token %q", token)
		}
	}
}
