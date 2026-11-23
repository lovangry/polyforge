package cli

// carries: internal/common/api/types.go, internal/polyforge/cli/cli.go, internal/polyforge/cli/verbs_admin.go
//
// repos_branch_test: the branch-pin half of the -repos registration
// (polyforge#45). A repo pins its development branch with a trailing
// "#<branch>" on its URL; the LAST "#" splits (git URLs never use a
// fragment, "," already separates the list, "@" would collide with
// scp-style user@host), an empty branch after "#" is an error (a silent
// drop would land the lane on the server's default — the accident the
// pin exists to prevent), and an unpinned repo keeps branch "". The wire
// half: ProjectRepo marshals "branch" only when set — pf projects get
// prints the server's project JSON verbatim, so an unset pin adds no
// byte — and a legacy record (the field absent) decodes with Branch "".

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

// refs maps the parsed list to the (name, url, branch) triples, so a
// table case asserts every field of every entry, not just the one the
// case is about.
func refs(t *testing.T, s string) [][3]string {
	t.Helper()
	out, err := parseRepoRefs(s)
	if err != nil {
		t.Fatalf("parseRepoRefs(%q): %v", s, err)
	}
	var rows [][3]string
	for _, r := range out {
		rows = append(rows, [3]string{r.Name, r.URL, r.Branch})
	}
	return rows
}

func TestParseRepoRefsPinsBranch(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want [][3]string
	}{
		// the ticket's own mixed spelling: one pinned, one not
		{"r1=https://git.example/r1.git#feat-cloud,r2=https://git.example/r2.git",
			[][3]string{{"r1", "https://git.example/r1.git", "feat-cloud"},
				{"r2", "https://git.example/r2.git", ""}}},
		// no "#": the whole value is the URL, the branch is empty
		{"r1=https://git.example/r1.git",
			[][3]string{{"r1", "https://git.example/r1.git", ""}}},
		// multiple "#": the LAST one splits, an earlier one stays in the URL
		{"r1=https://git.example/a#b#c", [][3]string{{"r1", "https://git.example/a#b", "c"}}},
		// "@" inside the URL stays part of the URL (scp-style)
		{"r1=git@github.com:org/r1.git#feat-cloud", [][3]string{{"r1", "git@github.com:org/r1.git", "feat-cloud"}}},
		// spaces around entries keep today's trimming behavior
		{" r1=https://git.example/r1.git#feat-cloud , r2=file:///x ",
			[][3]string{{"r1", "https://git.example/r1.git", "feat-cloud"}, {"r2", "file:///x", ""}}},
	} {
		if got := refs(t, tc.in); len(got) != len(tc.want) {
			t.Errorf("parseRepoRefs(%q) = %d entries, want %d", tc.in, len(got), len(tc.want))
		} else {
			for i, row := range got {
				if row != tc.want[i] {
					t.Errorf("parseRepoRefs(%q)[%d] = %q (name, url, branch), want %q", tc.in, i, row, tc.want[i])
				}
			}
		}
	}
	// the explicit clear keeps its own shape: an empty string is an empty
	// non-nil list, never an error and never a fabricated entry.
	if out, err := parseRepoRefs(""); err != nil || out == nil || len(out) != 0 {
		t.Fatalf("parseRepoRefs(\"\") = %v, %v; want an empty non-nil list", out, err)
	}
}

func TestParseRepoRefsEmptyBranchRefused(t *testing.T) {
	for _, in := range []string{
		"r1=https://git.example/r1.git#", // the pin's branch is empty
		"r1=https://git.example/r1.git#,r2=file:///x",
	} {
		if _, err := parseRepoRefs(in); err == nil {
			t.Errorf("parseRepoRefs(%q) accepted an empty branch after %q; the pin must be refused, never silently dropped", in, "#")
		} else if !strings.Contains(err.Error(), "-repos entry") {
			t.Errorf("parseRepoRefs(%q) error = %q; it must name the offending entry", in, err)
		}
	}
}

func TestProjectRepoBranchJSON(t *testing.T) {
	// set: the key is printed (pf projects get shows the pin)
	raw, err := json.Marshal(api.ProjectRepo{Name: "r1", URL: "https://git.example/r1.git", Branch: "feat-cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"branch":"feat-cloud"`) {
		t.Fatalf("a pinned ProjectRepo marshals %s; it must print its branch", raw)
	}
	// unset: the key is omitted entirely — the server's JSON for every
	// repo registered before the pin stays byte-identical
	raw, err = json.Marshal(api.ProjectRepo{Name: "r2", URL: "https://git.example/r2.git"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "branch") {
		t.Fatalf("an unpinned ProjectRepo marshals %s; the branch key must be omitted", raw)
	}
	// legacy: a record the server holds from before the field decodes
	// with Branch "" (no migration, no error)
	var back api.ProjectRepo
	if err := json.Unmarshal([]byte(`{"name":"r3","url":"file:///r3"}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != "r3" || back.URL != "file:///r3" || back.Branch != "" {
		t.Fatalf("a legacy ProjectRepo decodes to %+v; the branch must read empty", back)
	}
}
