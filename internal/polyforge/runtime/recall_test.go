package runtime

// recall_test.go: the F6-A claim-time memory snapshot — the shape it
// writes on a populated recall, and the two degradation paths (an
// unreachable aihub and a zero-hit query), plus the nil-seam no-op.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/protocol"
)

func recallItem(id, typ, excerpt string) RecalledMemory {
	return RecalledMemory{ID: id, Type: typ, Title: "t-" + id, Excerpt: excerpt}
}

// startWithRecall wires a fake recall seam plus a task definition and
// runs one Start.
func startWithRecall(t *testing.T, wi string, seam func(query string, topK int) ([]RecalledMemory, error)) (*Runtime, string) {
	t.Helper()
	setGitIdentity(t)
	hub := newFakeHub()
	content := "background about the flaky provider retry ladder"
	hub.items = map[string]*api.WorkItem{wi: {
		ID: wi, ProjectName: "proj", Goal: "fix the flaky retry ladder",
		Content: &content, Status: api.StatusRunning,
	}}
	r, _, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	r.cfg.MemoryRecall = seam
	if err := r.Start(context.Background(), wi); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background(), wi, protocol.FinishWrapped) })
	return r, filepath.Join(testCapsuleDir(r, wi), filepath.FromSlash(relRecalledMemories))
}

func readSnapshot(t *testing.T, path string) recalledSnapshot {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap recalledSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("snapshot unparsable: %v", err)
	}
	return snap
}

// A populated recall writes the query (goal+content), the top_k, the
// hits with bounded excerpts, and no notice.
func TestRecalledMemoriesSnapshotShape(t *testing.T) {
	var gotQuery string
	var gotTopK int
	_, path := startWithRecall(t, "wi_rc1", func(query string, topK int) ([]RecalledMemory, error) {
		gotQuery, gotTopK = query, topK
		return []RecalledMemory{
			recallItem("mem_1", "experience.debug", "first lesson text"),
			recallItem("mem_2", "rule.process", "second lesson text"),
		}, nil
	})
	snap := readSnapshot(t, path)
	if snap.WorkItemID != "wi_rc1" {
		t.Fatalf("work_item_id = %q", snap.WorkItemID)
	}
	if !strings.Contains(gotQuery, "fix the flaky retry ladder") ||
		!strings.Contains(gotQuery, "background about the flaky provider retry ladder") {
		t.Fatalf("recall query must carry goal+content: %q", gotQuery)
	}
	if snap.Query != gotQuery {
		t.Fatalf("snapshot query %q != seam query %q", snap.Query, gotQuery)
	}
	if gotTopK != 10 || snap.TopK != 10 {
		t.Fatalf("top_k: seam saw %d, snapshot %d (default must be 10)", gotTopK, snap.TopK)
	}
	if len(snap.Items) != 2 || snap.Items[0].ID != "mem_1" ||
		snap.Items[0].Type != "experience.debug" || snap.Items[0].Excerpt != "first lesson text" {
		t.Fatalf("items = %+v", snap.Items)
	}
	if snap.Notice != "" {
		t.Fatalf("populated recall must carry no notice: %q", snap.Notice)
	}
	if snap.CreatedAt == "" {
		t.Fatal("created_at missing")
	}
}

// An unreachable aihub and a zero-hit query both degrade to an EMPTY
// list plus a notice — never a failed spawn, never a missing file.
func TestRecalledMemoriesDegradations(t *testing.T) {
	// (a) search error → empty items + failure notice.
	_, path := startWithRecall(t, "wi_rc2", func(string, int) ([]RecalledMemory, error) {
		return nil, errors.New("aihub unreachable")
	})
	snap := readSnapshot(t, path)
	if len(snap.Items) != 0 || snap.Items == nil {
		t.Fatalf("failed recall must answer an empty (non-nil) list: %+v", snap.Items)
	}
	if !strings.Contains(snap.Notice, "memory search failed") {
		t.Fatalf("notice = %q", snap.Notice)
	}

	// (b) zero hits → empty items + the no-hit notice.
	_, path = startWithRecall(t, "wi_rc3", func(string, int) ([]RecalledMemory, error) {
		return []RecalledMemory{}, nil
	})
	snap = readSnapshot(t, path)
	if len(snap.Items) != 0 {
		t.Fatalf("zero-hit recall must answer an empty list: %+v", snap.Items)
	}
	if snap.Notice != "no memories matched the claim query" {
		t.Fatalf("notice = %q", snap.Notice)
	}
}

// A nil seam (slim installs, most unit tests) writes no snapshot at all.
func TestRecalledMemoriesNilSeamWritesNothing(t *testing.T) {
	_, path := startWithRecall(t, "wi_rc4", nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("nil seam must not write a snapshot: %v", err)
	}
}
