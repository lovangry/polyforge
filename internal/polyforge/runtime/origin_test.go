package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestOriginPersists(t *testing.T) {
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
}

func TestNoteOriginIsConsumedOnce(t *testing.T) {
	r := &Runtime{origins: map[string]Origin{}}
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

// TestOriginDecodeDefaultUnknownKind (C8): a persisted origin whose kind
// the current code does not recognize (a record written by an older
// build) is read back as a chat-less manual submit — kind chat, an empty
// ChatID, the owner as recorded — through the decoder's plain
// unknown-kind default.
func TestOriginDecodeDefaultUnknownKind(t *testing.T) {
	old := &jobRecord{Origin: &Origin{Kind: "legacy_kind", ChatID: "chat_old", Owner: "usr_1"}}
	normalizeOrigin(old)
	if old.Origin.Kind != OriginChat || old.Origin.ChatID != "" || old.Origin.Owner != "usr_1" {
		t.Fatalf("unknown kind decoded as %+v, want chat-less manual with owner kept", old.Origin)
	}
	ok := &jobRecord{Origin: &Origin{Kind: OriginAuto}}
	normalizeOrigin(ok)
	if ok.Origin.Kind != OriginAuto {
		t.Fatalf("recognized kind rewritten: %+v", ok.Origin)
	}
	chat := &jobRecord{Origin: &Origin{Kind: OriginChat, ChatID: "chat_9", Owner: "u"}}
	normalizeOrigin(chat)
	if chat.Origin.ChatID != "chat_9" || chat.Origin.Owner != "u" {
		t.Fatalf("chat origin rewritten: %+v", chat.Origin)
	}
}
