package test

// polyforge#161 acceptance: WI dependencies are usable end to end with
// the real pf binary against the real aihub — `pf file create --after`,
// `pf file dep add|rm|list`, the waiting_for rendering, and the owner
// ruling "only a wrapped blocker releases".

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

func w161Slug(t *testing.T, inst *v1Install, id string) string {
	t.Helper()
	slug, _ := w140View(t, inst, id)["slug"].(string)
	if slug == "" {
		t.Fatalf("work item %s has no slug", id)
	}
	return slug
}

func w161InItems(t *testing.T, inst *v1Install, project, id string) bool {
	t.Helper()
	for _, got := range w140SegIDs(w140Queue(t, inst, project), "items") {
		if got == id {
			return true
		}
	}
	return false
}

func TestW161DependenciesEndToEnd(t *testing.T) {
	const project = "w161-deps"
	inst := w140Setup(t, project)
	home := t.TempDir()
	ws := filepath.Join(home, "ws")
	cfg := w113ConfigFile(t, filepath.Join(home, "cli.toml"), "", "", "", []w113WS{{key: ws, projects: []string{project}}})
	env := w113Env{home: home, config: cfg}
	jsonEnv := env
	jsonEnv.json = true

	a := w140Create(t, inst, project, "w161 blocker", strPtr("w161-b"), nil)
	aSlug := w161Slug(t, inst, a)

	// pf file create --after <slug>: B waits for A.
	out := w113PfWant(t, inst, -1, jsonEnv, "file", "create", "--project", project,
		"--goal", "w161 after the blocker", "--bundle", "w161-b", "--after", aSlug)
	var b struct {
		ID         string `json:"id"`
		Slug       string `json:"slug"`
		WaitingFor []struct {
			Slug, Status string
		} `json:"waiting_for"`
	}
	if err := json.Unmarshal([]byte(w113JSONDocOf(out)), &b); err != nil || b.ID == "" {
		t.Fatalf("create --after output: %v\n%s", err, out)
	}
	if len(b.WaitingFor) != 1 || b.WaitingFor[0].Slug != aSlug || b.WaitingFor[0].Status != "queued" {
		t.Fatalf("created record waiting_for = %+v\n%s", b.WaitingFor, out)
	}
	if w161InItems(t, inst, project, b.ID) {
		t.Fatalf("a dependency-blocked work item is in the ready queue")
	}
	out = w113PfWant(t, inst, -1, env, "queue", "--project", project)
	if !strings.Contains(out, "WAITING-FOR") || !strings.Contains(out, aSlug+" (queued)") {
		t.Fatalf("pf queue does not show who B waits for:\n%s", out)
	}
	out = w113PfWant(t, inst, -1, env, "file", "show", b.Slug)
	if !strings.Contains(out, "waiting-for: "+aSlug+" (queued)") || !strings.Contains(out, "after:  "+aSlug+" (queued)") {
		t.Fatalf("pf file show lacks the dependency block:\n%s", out)
	}

	// Cycle and cross-project refusals surface the server code.
	out = w113PfWant(t, inst, 1, env, "file", "dep", "add", b.Slug, aSlug)
	if !strings.Contains(out, "CORE-409: DEPENDENCY_CYCLE") {
		t.Fatalf("cycle refusal:\n%s", out)
	}
	if _, err := inst.client.CreateProject(aihubclient.ProjectCreateRequest{Name: "w161-elsewhere"}); err != nil {
		t.Fatal(err)
	}
	e := w140Create(t, inst, "w161-elsewhere", "w161 other project", nil, nil)
	out = w113PfWant(t, inst, 1, env, "file", "dep", "add", w161Slug(t, inst, e), b.Slug)
	if !strings.Contains(out, "CORE-400") {
		t.Fatalf("cross-project refusal:\n%s", out)
	}

	// A fails → B keeps waiting (owner ruling); dep rm releases it.
	w140ClaimEnd(t, inst, a, "failed")
	if w161InItems(t, inst, project, b.ID) {
		t.Fatalf("a failed blocker released its dependent")
	}
	out = w113PfWant(t, inst, -1, env, "file", "dep", "list", b.Slug)
	if !strings.Contains(out, "after:  "+aSlug+" (failed)") {
		t.Fatalf("dep list:\n%s", out)
	}
	w113PfWant(t, inst, -1, env, "file", "dep", "rm", aSlug, b.Slug)
	if !w161InItems(t, inst, project, b.ID) {
		t.Fatalf("removing the edge did not release the dependent")
	}

	// A wrapped blocker releases on its own.
	c := w140Create(t, inst, project, "w161 second blocker", strPtr("w161-b"), nil)
	d := w140Create(t, inst, project, "w161 after the second", strPtr("w161-b"), nil)
	w113PfWant(t, inst, -1, env, "file", "dep", "add", w161Slug(t, inst, c), w161Slug(t, inst, d))
	if w161InItems(t, inst, project, d) {
		t.Fatalf("dependent of a queued blocker is in the ready queue")
	}
	w140ClaimEnd(t, inst, c, "wrapped")
	if !w161InItems(t, inst, project, d) {
		t.Fatalf("a wrapped blocker did not release its dependent")
	}
}
