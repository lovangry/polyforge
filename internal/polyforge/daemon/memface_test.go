package daemon

import (
	"net/url"
	"strings"
	"testing"
)

// The claim-time memory snapshot scopes its search to the work item's
// project (audit B5, daemon half): the request carries project=, and an
// empty project sends none.
func TestRecallSnapshotScopesSearchToProject(t *testing.T) {
	cases := []struct {
		project string
		want    string // expected project= value; "" means absent
	}{
		{project: "prj_alpha", want: "prj_alpha"},
		{project: "", want: ""},
	}
	for _, tc := range cases {
		h := newFakeHub()
		var gotPath string
		h.memDo = func(method, path string, body, out any) error {
			gotPath = path
			return nil
		}
		if _, err := recallSnapshot(h)("goal text", tc.project, 5); err != nil {
			t.Fatalf("project %q: recallSnapshot: %v", tc.project, err)
		}
		if !strings.HasPrefix(gotPath, "/api/v1/memories/search?") {
			t.Fatalf("project %q: search path = %q", tc.project, gotPath)
		}
		q, err := url.ParseQuery(strings.TrimPrefix(gotPath, "/api/v1/memories/search?"))
		if err != nil {
			t.Fatalf("project %q: parse query: %v", tc.project, err)
		}
		if got, has := q.Get("project"), q.Has("project"); got != tc.want || has != (tc.want != "") {
			t.Fatalf("project %q: project= is %q (present=%v), want %q", tc.project, got, has, tc.want)
		}
	}
}
