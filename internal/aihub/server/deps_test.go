package server

// polyforge#161: the dependency endpoints accept a slug on BOTH sides
// (path and blocking_wi_id), report the other side's slug and status on
// every edge, surface CORE-409 DEPENDENCY_CYCLE / CORE-400 cross-project
// refusals as typed envelopes, and the WI view carries the derived
// waiting_for list.

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

func slugOf(t *testing.T, e *srvEnv, id string) string {
	t.Helper()
	var wi struct {
		Slug string `json:"slug"`
	}
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+id, e.key, nil, &wi); code != 200 || wi.Slug == "" {
		t.Fatalf("get: %d %s", code, raw)
	}
	return wi.Slug
}

func TestDependencyEndpointsBySlug(t *testing.T) {
	e := newSrvEnv(t)
	c := aihubclient.New(e.ts.URL, e.key)
	aID := createExecWIOverHTTP(t, e, "deps blocker")
	bID := createExecWIOverHTTP(t, e, "deps blocked")
	a, b := slugOf(t, e, aID), slugOf(t, e, bID)

	raw, err := c.AddBlocksDependency(a, b)
	if err != nil {
		t.Fatalf("add by slug: %v", err)
	}
	var deps core.Deps
	if err := json.Unmarshal(raw, &deps); err != nil {
		t.Fatal(err)
	}
	if len(deps.BlockedBy) != 1 || deps.BlockedBy[0].WI != aID || deps.BlockedBy[0].Slug != a ||
		deps.BlockedBy[0].Status != api.StatusQueued || deps.BlockedBy[0].Kind != "blocks" {
		t.Fatalf("blocked_by = %+v", deps.BlockedBy)
	}
	raw, err = c.DependenciesRaw(a)
	if err != nil {
		t.Fatal(err)
	}
	deps = core.Deps{}
	if err := json.Unmarshal(raw, &deps); err != nil {
		t.Fatal(err)
	}
	if len(deps.Blocking) != 1 || deps.Blocking[0].Slug != b {
		t.Fatalf("blocking = %+v", deps.Blocking)
	}

	// The WI view (detail and list) carries waiting_for.
	var view core.WIView
	if code, raw := e.do(t, "GET", "/api/v1/work-items/"+url.PathEscape(b), e.key, nil, &view); code != 200 ||
		len(view.WaitingFor) != 1 || view.WaitingFor[0].Slug != a || view.WaitingFor[0].Status != api.StatusQueued {
		t.Fatalf("detail waiting_for: %d %s", code, raw)
	}
	var list struct {
		Items []core.WIView `json:"items"`
	}
	e.do(t, "GET", "/api/v1/work-items?project=alpha", e.key, nil, &list)
	found := false
	for _, it := range list.Items {
		if it.ID == bID {
			found = len(it.WaitingFor) == 1 && it.WaitingFor[0].Slug == a
		}
		if it.ID == aID && len(it.WaitingFor) != 0 {
			t.Fatalf("the blocker itself carries waiting_for: %+v", it.WaitingFor)
		}
	}
	if !found {
		t.Fatalf("list view lacks waiting_for for %s: %+v", b, list.Items)
	}

	// Cycle: CORE-409 DEPENDENCY_CYCLE.
	if _, err := c.AddBlocksDependency(b, a); !isWire(err, "CORE-409", "DEPENDENCY_CYCLE") {
		t.Fatalf("cycle: %v", err)
	}
	// Unknown blocker slug: CORE-404.
	if _, err := c.AddBlocksDependency("alpha#999", b); !isWire(err, "CORE-404", "") {
		t.Fatalf("unknown blocker: %v", err)
	}
	// Cross-project: CORE-400.
	if _, err := e.admin.CreateProject(core.CreateProjectRequest{Name: "beta"}); err != nil {
		t.Fatal(err)
	}
	var other struct {
		Slug string `json:"slug"`
	}
	e.do(t, "POST", "/api/v1/work-items", e.key, map[string]any{"project": "beta", "goal": "elsewhere"}, &other)
	if _, err := c.AddBlocksDependency(other.Slug, b); !isWire(err, "CORE-400", "") {
		t.Fatalf("cross-project: %v", err)
	}

	// Remove by slug on both sides.
	if _, err := c.RemoveBlocksDependency(a, b); err != nil {
		t.Fatalf("rm by slug: %v", err)
	}
	if _, err := c.RemoveBlocksDependency(a, b); !isWire(err, "CORE-404", "") {
		t.Fatalf("second rm: %v", err)
	}
	view = core.WIView{}
	if e.do(t, "GET", "/api/v1/work-items/"+url.PathEscape(b), e.key, nil, &view); len(view.WaitingFor) != 0 {
		t.Fatalf("waiting_for survived the edge removal: %+v", view.WaitingFor)
	}
}

func isWire(err error, code, reason string) bool {
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != code {
		return false
	}
	if reason == "" {
		return true
	}
	var extra struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(ae.Extra, &extra)
	return extra.Reason == reason
}
