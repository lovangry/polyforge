package server

// Repro test for the TODO(v1) §4.5 checkout-hash gap named in the WI
// goal (workitem.go ~line 191): the WI repo-reference triple must not be
// persistable half-empty. A repo-reference entry that carries name+URL
// but a missing/blank HEAD sha (base) must be refused at WI create and
// at WI patch with the typed CORE-400 INVENTORY_BASE_MISSING refusal —
// the vocabulary this codebase already uses for exactly this refusal
// (v0 core, 02-aihub-core.md §4.6) — and nothing may be persisted.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

// refuseShape asserts the typed refusal envelope: HTTP 400, CORE-400,
// machine-readable reason INVENTORY_BASE_MISSING.
func refuseShape(t *testing.T, code int, raw []byte) {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("want HTTP 400 (CORE-400 INVENTORY_BASE_MISSING), got %d %s", code, raw)
	}
	var envl api.Error
	if err := json.Unmarshal(raw, &envl); err != nil {
		t.Fatalf("bad error envelope: %s (%v)", raw, err)
	}
	if envl.Code != api.ErrCoreBadInput {
		t.Fatalf("want error code %s, got %q (raw: %s)", api.ErrCoreBadInput, envl.Code, raw)
	}
	var m map[string]string
	if len(envl.Extra) == 0 || json.Unmarshal(envl.Extra, &m) != nil || m["reason"] != "INVENTORY_BASE_MISSING" {
		t.Fatalf("want reason INVENTORY_BASE_MISSING in extra, got %s", raw)
	}
}

// nothingPersisted asserts project alpha holds no work items at all.
func nothingPersisted(t *testing.T, e *srvEnv) {
	t.Helper()
	var list struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	code, raw := e.do(t, "GET", "/api/v1/work-items?project=alpha", e.key, nil, &list)
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, raw)
	}
	if list.Total != 0 || len(list.Items) != 0 {
		t.Fatalf("refused create must not persist; found %d work item(s): %s", list.Total, raw)
	}
}

// TestWIRepoRefTripleBlankBaseRefused pins the desired refusal shape at
// both surfaces the WI goal names.
func TestWIRepoRefTripleBlankBaseRefused(t *testing.T) {
	const repoInv = "https://example.com/polyforge.git"

	// WI create: repo-reference entry with name+URL but a blank base sha.
	t.Run("create/base-blank", func(t *testing.T) {
		e := newSrvEnv(t)
		code, raw := e.do(t, "POST", "/api/v1/work-items", e.key, map[string]any{
			"project": "alpha",
			"goal":    "half-empty repo-reference triple must be refused",
			"repo_inventory": []map[string]any{
				{"name": "polyforge", "url": repoInv},
			},
			"repo_inventory_base": []map[string]any{
				{"name": "polyforge", "base": ""},
			},
		}, nil)
		refuseShape(t, code, raw)
		nothingPersisted(t, e)
	})

	// WI create: same entry with the base list absent entirely.
	t.Run("create/base-absent", func(t *testing.T) {
		e := newSrvEnv(t)
		code, raw := e.do(t, "POST", "/api/v1/work-items", e.key, map[string]any{
			"project": "alpha",
			"goal":    "half-empty repo-reference triple must be refused",
			"repo_inventory": []map[string]any{
				{"name": "polyforge", "url": repoInv},
			},
		}, nil)
		refuseShape(t, code, raw)
		nothingPersisted(t, e)
	})

	// WI patch: patching a repo-reference entry whose base is blank must
	// draw the same typed refusal, not a silent accept and not a
	// different reason.
	t.Run("patch/base-blank", func(t *testing.T) {
		e := newSrvEnv(t)
		var created struct {
			ID string `json:"id"`
		}
		if code, raw := e.do(t, "POST", "/api/v1/work-items", e.key,
			map[string]any{"project": "alpha", "goal": "patch carrier"}, &created); code != http.StatusCreated || created.ID == "" {
			t.Fatalf("seed create: %d %s", code, raw)
		}
		code, raw := e.do(t, "PATCH", "/api/v1/work-items/"+created.ID, e.key, map[string]any{
			"repo_inventory": []map[string]any{
				{"name": "polyforge", "url": repoInv},
			},
			"repo_inventory_base": []map[string]any{
				{"name": "polyforge", "base": ""},
			},
		}, nil)
		refuseShape(t, code, raw)
		// Nothing from the refused patch may be applied: the carrier WI
		// keeps its original goal.
		var wi struct {
			Goal string `json:"goal"`
		}
		if code, raw := e.do(t, "GET", "/api/v1/work-items/"+created.ID, e.key, nil, &wi); code != http.StatusOK {
			t.Fatalf("get after refused patch: %d %s", code, raw)
		}
		if wi.Goal != "patch carrier" {
			t.Fatalf("refused patch must not mutate the WI; goal=%q", wi.Goal)
		}
	})
}
