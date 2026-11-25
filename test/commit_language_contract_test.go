// carries: workflows/_shared/skills/build.md, workflows/_shared/skills/publish.md, workflows/_shared/scripts/pfstep/commitmsg.py, test/commit_language_contract_test.go
package test

// commit_language_contract_test.go: the commit-language rule is taught
// once, in the skills, and its machinery is the shared pfstep modules —
// a capsule's messages in a task repository follow the work item's own
// goal (the capsule's runtime-state/work-item.json, read by
// commitmsg.goal_language: Simplified Chinese when the goal carries a
// Chinese character, English when it does not), never the repository's
// history: the conventional prefix is kept verbatim and the
// repository's own commit-msg hook stays the judge. This contract pins
// that the build and publish skills keep teaching exactly that, that
// the goal-reading module exists in the repository tree and that the
// retired repository-history reading stays gone.
//
// Run (targeted): GOWORK=off go test ./test/ -run '^TestCommitMessageLanguageFollowsTheWorkItem$' -count=1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitMessageLanguageFollowsTheWorkItem(t *testing.T) {
	root := v1Root(t)
	for _, c := range []struct {
		file, fragment, why string
	}{
		{"workflows/_shared/skills/build.md", "follows the work item's own goal", "the message language is the goal's, never the repository's history"},
		{"workflows/_shared/skills/build.md", "no reason to write this one in English", "an English commit history never decides the message language"},
		{"workflows/_shared/skills/build.md", "the conventional prefix kept verbatim", "the repository's type-enum word is never translated"},
		{"workflows/_shared/skills/build.md", "the header length per the repository's own commitlint", "the repository's own commit-msg rules stay the judge"},
		{"workflows/_shared/skills/build.md", "never routed around", "a refused message is fixed in the message, not routed around the hook"},
		{"workflows/_shared/skills/build.md", "a `core.hooksPath` of your own", "the Never list keeps the repository's commit-msg hook the judge"},
		{"workflows/_shared/skills/publish.md", "is written in Simplified Chinese", "the landing title's one line is Simplified Chinese by default"},
		{"workflows/_shared/skills/publish.md", "the work item's own goal decides the language", "the title's language is the goal's, never the repository's history"},
		{"workflows/_shared/skills/publish.md", "no reason to write this one in English", "an English commit history never decides the title's language"},
		{"workflows/_shared/skills/publish.md", "the `<slug>: ` prefix kept verbatim", "the title keeps the conventional prefix"},
		{"workflows/_shared/skills/publish.md", "lands it verbatim", "the landing squash's message is the title, both languages"},
	} {
		b, err := os.ReadFile(filepath.Join(root, c.file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), c.fragment) {
			t.Errorf("%s does not teach that %s", c.file, c.why)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "workflows/_shared/scripts/pfstep/commitmsg.py")); err != nil {
		t.Errorf("the skill's named machinery workflows/_shared/scripts/pfstep/commitmsg.py is not in the repository tree")
	}
	if _, err := os.Stat(filepath.Join(root, "workflows/_shared/scripts/pfstep/commitlang.py")); err == nil {
		t.Errorf("the retired repository-history reading workflows/_shared/scripts/pfstep/commitlang.py is back in the repository tree")
	}
}
