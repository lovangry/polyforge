package workitem_test

// R2C-F11 regressions (02 §4.2: goal is "≤500 字符" — CHARACTERS, not UTF-8
// bytes). The old byte-count (len(string)) rejected multilingual goals
// early: 200 CJK characters are 600 bytes but well inside a 500-CHARACTER
// limit. The boundary must be rune-counted identically on create and PATCH.

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/silgrid/polyforge/internal/core/workitem"
)

// TestGoalLimitCountsCharacters: exactly 500 CJK characters pass creation,
// 501 fail — while the byte-counted variants of the same goals (1500/1503
// bytes) prove the limit is no longer byte-based.
func TestGoalLimitCountsCharacters(t *testing.T) {
	h := setup(t)

	exact := strings.Repeat("汉", 500) // 500 chars, 1500 bytes
	if utf8.RuneCountInString(exact) != 500 || len(exact) != 1500 {
		t.Fatalf("fixture: %d runes, %d bytes", utf8.RuneCountInString(exact), len(exact))
	}
	over := strings.Repeat("汉", 501)

	d, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: exact, Reporter: "usr_worker", BundleName: "demo"})
	if err != nil {
		t.Fatalf("exactly 500 CJK characters (1500 bytes) must pass creation: %v", err)
	}
	if d.Goal != exact || utf8.RuneCountInString(d.Goal) != 500 {
		t.Fatalf("goal must round-trip: %d runes", utf8.RuneCountInString(d.Goal))
	}
	_, err = h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: over, Reporter: "usr_worker", BundleName: "demo"})
	e := ae(t, err)
	if e.Code != "CORE-400" || !strings.Contains(e.Detail, "500") {
		t.Fatalf("501 CJK characters must fail creation CORE-400, got %v", e)
	}

	// Multilingual mix at the boundary: 400 CJK + 100 ASCII = 500 chars,
	// 1300 bytes — passes; one more CJK char fails.
	mix := strings.Repeat("汉", 400) + strings.Repeat("a", 100)
	if utf8.RuneCountInString(mix) != 500 {
		t.Fatalf("fixture: %d runes", utf8.RuneCountInString(mix))
	}
	if _, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: mix, Reporter: "usr_worker", BundleName: "demo"}); err != nil {
		t.Fatalf("500-char multilingual mix must pass creation: %v", err)
	}

	// PATCH carries the same rune-counted limit: 500 CJK chars pass, 501
	// fail, and a plain ASCII 501 still fails.
	p, err := h.wi.Patch(d.ID, map[string]json.RawMessage{"goal": json.RawMessage(`"` + exact + `"`)})
	if err != nil || utf8.RuneCountInString(p.Goal) != 500 {
		t.Fatalf("PATCH to exactly 500 CJK characters must pass: %v", err)
	}
	_, err = h.wi.Patch(d.ID, map[string]json.RawMessage{"goal": json.RawMessage(`"` + over + `"`)})
	e = ae(t, err)
	if e.Code != "CORE-400" {
		t.Fatalf("PATCH to 501 CJK characters must fail CORE-400, got %v", e)
	}
	_, err = h.wi.Patch(d.ID, map[string]json.RawMessage{"goal": json.RawMessage(`"` + strings.Repeat("x", 501) + `"`)})
	e = ae(t, err)
	if e.Code != "CORE-400" {
		t.Fatalf("PATCH to 501 ASCII characters must still fail CORE-400, got %v", e)
	}

	// Goal must remain a non-empty string (unchanged rule).
	_, err = h.wi.Patch(d.ID, map[string]json.RawMessage{"goal": json.RawMessage(`""`)})
	if e := ae(t, err); e.Code != "CORE-400" {
		t.Fatalf("empty goal must still be refused: %v", e)
	}
}
