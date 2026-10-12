package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestOriginPersistsAndLegacyRecordsLoad(t *testing.T) {
	rec := &jobRecord{WorkItemID: "wi_o", State: string(StateRunning), Origin: &Origin{Kind: OriginChat, ChatID: "chat_1", Owner: "usr_1"}}
	raw, _ := json.Marshal(rec)
	var back jobRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	got := jobFromRecord(&back).record().Origin
	if got == nil || *got != *rec.Origin {
		t.Fatalf("origin roundtrip: %+v", got)
	}
	// a record written before origin existed loads with origin absent
	var legacy jobRecord
	if err := json.Unmarshal([]byte(`{"work_item_id":"wi_old","state":"running"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if j := jobFromRecord(&legacy); j.origin != nil || j.record().Origin != nil {
		t.Fatal("legacy record must keep origin absent")
	}
	if out, _ := json.Marshal(jobFromRecord(&legacy).record()); json.Valid(out) && string(out) != "" {
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		if _, ok := m["origin"]; ok {
			t.Fatal("absent origin must be omitted from state.json")
		}
	}
}

func TestNoteOriginIsConsumedOnce(t *testing.T) {
	r := &Runtime{}
	r.NoteOrigin("wi_a", Origin{Kind: OriginAuto})
	if o := r.takeOrigin("wi_a"); o == nil || o.Kind != OriginAuto {
		t.Fatalf("take: %+v", o)
	}
	if o := r.takeOrigin("wi_a"); o != nil {
		t.Fatal("origin must be consumed once")
	}
}

func TestStartRecordsNotedOrigin(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	r, toOB, _ := newTestRuntime(t, hub, &scriptedSpawner{})
	base := t.TempDir()
	r.cfg.GitRemoteBase = base
	wi := "wi_origin"
	if out, err := run(t, "git", "init", "--bare", filepath.Join(base, wi+".git")); err != nil {
		t.Fatalf("bare init: %s", out)
	}
	r.NoteOrigin(wi, Origin{Kind: OriginChat, ChatID: "chat_9", Owner: "usr_1"})
	if err := r.Start(context.Background(), wi); err != nil {
		t.Fatalf("Start: %v", err)
	}
	recvRT(t, toOB)
	ji, ok := r.Info(wi)
	if !ok || ji.Origin == nil || ji.Origin.ChatID != "chat_9" || ji.Origin.Owner != "usr_1" {
		t.Fatalf("job origin = %+v", ji.Origin)
	}
	st, err := loadState(r.statePath)
	if err != nil || len(st.Jobs) != 1 || st.Jobs[0].Origin == nil || st.Jobs[0].Origin.Kind != OriginChat {
		t.Fatalf("persisted origin: %v %+v", err, st)
	}
}
