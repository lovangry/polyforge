// carries: workflows/_shared/skills/build.md, workflows/_shared/skills/publish.md, workflows/_shared/scripts/pfstep/commitlang.py, workflows/_shared/scripts/pfstep/commitmsg.py, test/commit_language_contract_test.go
package test

// commit_language_contract_test.go: the commit-language rule is taught
// once, in the skills, and its machinery is the shared pfstep modules —
// a capsule's messages in a task repository follow the repository's own
// language, read from the repository's own facts (the last 50 commit
// bodies on the default branch), never a configured choice: the
// conventional prefix is kept verbatim and the repository's own
// commit-msg hook stays the judge. This contract pins that the build
// and publish skills keep teaching exactly that, and that the pfstep
// modules they name exist in the repository tree.
//
// Run (targeted): GOWORK=off go test ./test/ -run '^TestCommitMessageLanguageFollowsTheRepository$' -count=1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitMessageLanguageFollowsTheRepository(t *testing.T) {
	root := v1Root(t)
	for _, c := range []struct {
		file, fragment, why string
	}{
		{"workflows/_shared/skills/build.md", "the last 50 commit bodies on the default branch", "the reading is the repository's own history, not a configured choice"},
		{"workflows/_shared/skills/build.md", "the conventional prefix kept verbatim", "the repository's type-enum word is never translated"},
		{"workflows/_shared/skills/build.md", "the header length per the repository's own commitlint", "the repository's own commit-msg rules stay the judge"},
		{"workflows/_shared/skills/build.md", "never routed around", "a refused message is fixed in the message, not routed around the hook"},
		{"workflows/_shared/skills/build.md", "a `core.hooksPath` of your own", "the Never list keeps the repository's commit-msg hook the judge"},
		{"workflows/_shared/skills/publish.md", "the same reading as the capsule's commit bodies", "the PR title follows the repository's language"},
		{"workflows/_shared/skills/publish.md", "`workflows/_shared/scripts/pfstep/commitlang.py`", "the one reading is named"},
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
	for _, rel := range []string{
		"workflows/_shared/scripts/pfstep/commitlang.py",
		"workflows/_shared/scripts/pfstep/commitmsg.py",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("the skill's named machinery %s is not in the repository tree", rel)
		}
	}
}
