// carries: workflows/_shared/skills/publish.md, workflows/_shared/skills/review.md, workflows/_shared/skills/verify.md, test/owner_language_contract_test.go
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
			"workflows/_shared/skills/publish.md": "The explanatory prose must be written in Simplified Chinese",
			"workflows/_shared/skills/review.md":  "Write the explanatory prose in Simplified Chinese",
			"workflows/_shared/skills/verify.md":  "Write the explanation in Simplified Chinese",
		}[rel]
		if !strings.Contains(s, required) {
			t.Errorf("%s does not require English instruction for Simplified Chinese owner-facing prose", rel)
		}
		if strings.Contains(s, "使用简体中文") || strings.Contains(s, "解释 使用简体中文") {
			t.Errorf("%s puts mixed-language wording into the English step-agent instructions", rel)
		}
	}
	publish, err := os.ReadFile(filepath.Join(root, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"owner-review:", "「过目通过」", "「不通过:"} {
		if !strings.Contains(string(publish), token) {
			t.Errorf("publish lost structured token %q", token)
		}
	}
}
