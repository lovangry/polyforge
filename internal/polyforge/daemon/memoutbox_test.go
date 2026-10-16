package daemon

// memoutbox_test.go: the outbox flush — validation, the one-per-step and
// one-wrap quotas, dedupe (capsule and aihub), the attributed upload, the
// outage retry, the 4xx rejection, the file moves and the memory log —
// plus the driver-log trigger.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
)

// outboxHub scripts aihub for the flush: searches answer hits, posts
// store (or fail with failPost while it is non-nil).
type outboxHub struct {
	mu       sync.Mutex
	hits     string // search answer JSON
	failPost error
	searchFn func() error
	posts    []map[string]any
	searches []string
}

func (o *outboxHub) do(method, path string, body, out any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case method == "GET" && strings.HasPrefix(path, "/api/v1/memories/search?"):
		o.searches = append(o.searches, path)
		if o.searchFn != nil {
			if err := o.searchFn(); err != nil {
				return err
			}
		}
		hits := o.hits
		if hits == "" {
			hits = `{"items":[]}`
		}
		return json.Unmarshal([]byte(hits), out)
	case method == "POST" && path == "/api/v1/memories":
		if o.failPost != nil {
			return o.failPost
		}
		o.posts = append(o.posts, body.(map[string]any))
		return json.Unmarshal([]byte(`{"id":"mem_`+string(rune('a'+len(o.posts)-1))+`"}`), out)
	}
	return errors.New("unexpected " + method + " " + path)
}

// postCount reads the post count under the lock (background flushes).
func (o *outboxHub) postCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.posts)
}

func outboxEnv(t *testing.T) (*Daemon, *outboxHub, outboxTarget) {
	t.Helper()
	hub := newFakeHub()
	oh := &outboxHub{}
	hub.memDo = oh.do
	d := &Daemon{hub: hub}
	d.cfg.APIKey = "k_machine_secret_value"
	tg := outboxTarget{wi: "wi_ob", project: "proj", capsuleDir: t.TempDir()}
	writeJournal(t, tg.capsuleDir, []capsule.SpawnJournalEntry{{StepID: "fix", SpawnN: 1}, {StepID: "fix", SpawnN: 2}, {StepID: "deliver", SpawnN: 3}})
	return d, oh, tg
}

// writeJournal writes the capsule's spawn journal.
func writeJournal(t *testing.T, capDir string, entries []capsule.SpawnJournalEntry) {
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		raw, _ := json.Marshal(e)
		b.Write(raw)
		b.WriteString("\n")
	}
	path := filepath.Join(capDir, capsule.DirRuntimeState, capsule.SpawnJournalFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// flush runs one flush on the work item's current state.
func flush(d *Daemon, tg outboxTarget) { d.flushOutbox(tg, d.outboxStateOf(tg.wi)) }

// putOutbox queues m as attempt fix#1 unless it names its own dispatch.
func putOutbox(t *testing.T, tg outboxTarget, name string, m capsule.OutboxMemory) {
	t.Helper()
	if m.Step == "" {
		m.Step, m.Attempt = "fix", 1
	}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeOutboxRaw(t, tg, name, string(data))
}

func writeOutboxRaw(t *testing.T, tg outboxTarget, name, data string) {
	t.Helper()
	dir := filepath.Join(tg.capsuleDir, filepath.FromSlash(capsule.DirMemoryOutbox))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func outboxAt(tg outboxTarget, sub string) []string {
	return outboxNames(filepath.Join(tg.capsuleDir, filepath.FromSlash(capsule.DirMemoryOutbox), sub))
}

func memoryLog(t *testing.T, tg outboxTarget) string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(tg.capsuleDir, filepath.FromSlash(capsule.MemoryLogMD)))
	return string(raw)
}

func TestOutboxUploadsAttributedAndLogs(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	putOutbox(t, tg, "1.md", capsule.OutboxMemory{Type: "experience.debug", Title: "sqlite busy under WAL", Tags: []string{"policy-proposal"}, Body: "现象→原因→解法\n建议新增条目：x\n"})
	putOutbox(t, tg, "2.md", capsule.OutboxMemory{Type: "experience.wrap", Title: "proj#1: did it", Body: "做了什么\n"})
	flush(d, tg)
	if len(oh.posts) != 2 {
		t.Fatalf("posts = %+v", oh.posts)
	}
	p := oh.posts[0]
	if p["project"] != "proj" || p["work_item_id"] != "wi_ob" || p["type"] != "experience.debug" || p["title"] != "sqlite busy under WAL" ||
		p["visibility"] != "project" || !strings.Contains(p["content"].(string), "建议新增条目") {
		t.Fatalf("upload body = %+v", p)
	}
	if got := outboxAt(tg, capsule.OutboxDone); len(got) != 2 || len(outboxAt(tg, "")) != 0 || len(outboxAt(tg, capsule.OutboxPending)) != 0 {
		t.Fatalf("done = %v", got)
	}
	log := memoryLog(t, tg)
	if !strings.Contains(log, "- mem_a experience.debug [policy-proposal] sqlite busy under WAL") || !strings.Contains(log, "- mem_b experience.wrap proj#1: did it") {
		t.Fatalf("log:\n%s", log)
	}
	// a second flush with nothing new is a no-op
	flush(d, tg)
	if len(oh.posts) != 2 {
		t.Fatal("a flush re-uploaded")
	}
}

func TestOutboxRejectsInvalidAndOverQuota(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	ok := capsule.OutboxMemory{Type: "experience.env", Title: "first", Body: "b"}
	putOutbox(t, tg, "a1.md", ok)
	putOutbox(t, tg, "a2.md", capsule.OutboxMemory{Type: "experience.env", Title: "second", Body: "b"})
	putOutbox(t, tg, "b.md", capsule.OutboxMemory{Type: "doc.design", Title: "t", Body: "b"})
	putOutbox(t, tg, "c.md", capsule.OutboxMemory{Type: "experience.env", Title: "", Body: "b"})
	putOutbox(t, tg, "d.md", capsule.OutboxMemory{Type: "experience.env", Title: "big", Body: strings.Repeat("x", outboxBodyMax+1)})
	putOutbox(t, tg, "e.md", capsule.OutboxMemory{Type: "experience.env", Title: "key", Body: "token ghp_abcdefghijklmnopqrstuvwxyz0123"})
	putOutbox(t, tg, "f.md", capsule.OutboxMemory{Type: "experience.env", Title: "machine", Body: "the key k_machine_secret_value"})
	putOutbox(t, tg, "g.md", capsule.OutboxMemory{Type: "experience.env", Title: "tag", Tags: []string{"bad tag"}, Body: "b"})
	writeOutboxRaw(t, tg, "h.md", "no frontmatter\n")
	putOutbox(t, tg, "i.md", capsule.OutboxMemory{Type: "experience.env", Title: "forged", Body: "b", Step: "review", Attempt: 9})
	writeOutboxRaw(t, tg, "j.md", "---\n"+strings.Repeat("x", capsule.OutboxFileMax)+"\n---\n")
	flush(d, tg)
	if len(oh.posts) != 1 || oh.posts[0]["title"] != "first" {
		t.Fatalf("posts = %+v", oh.posts)
	}
	rejected := outboxAt(tg, capsule.OutboxRejected)
	if strings.Join(rejected, ",") != "a2.md,b.md,c.md,d.md,e.md,f.md,g.md,h.md,i.md,j.md" {
		t.Fatalf("rejected = %v", rejected)
	}
	log := memoryLog(t, tg)
	for _, want := range []string{"a2.md: a second memory from step fix attempt 1", `i.md: step "review" attempt 9 is no dispatch`, "j.md: the file is", `b.md: type "doc.design" is not experience.<kind>`, "c.md: a title is required",
		"d.md: the body is 2049 bytes", "e.md: the memory looks like it carries a credential value", "f.md: the memory carries the daemon's credential", "g.md: tag \"bad tag\"", "h.md: no frontmatter"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %q:\n%s", want, log)
		}
	}
	// the next step may write its own one memory
	putOutbox(t, tg, "z.md", capsule.OutboxMemory{Type: "experience.env", Title: "next step", Body: "b", Step: "fix", Attempt: 2})
	flush(d, tg)
	if len(oh.posts) != 2 {
		t.Fatalf("next step's memory not uploaded: %+v", oh.posts)
	}
}

func TestOutboxDedupesAndOneWrap(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	putOutbox(t, tg, "1.md", capsule.OutboxMemory{Type: "experience.wrap", Title: "proj#1: done", Body: "b"})
	flush(d, tg)
	// same title again (capsule dedupe) and a second wrap (quota)
	putOutbox(t, tg, "2.md", capsule.OutboxMemory{Type: "experience.wrap", Title: "proj#1: done", Body: "b"})
	putOutbox(t, tg, "3.md", capsule.OutboxMemory{Type: "experience.wrap", Title: "proj#1: other", Body: "b", Step: "deliver", Attempt: 3})
	flush(d, tg)
	if len(oh.posts) != 1 {
		t.Fatalf("posts = %+v", oh.posts)
	}
	log := memoryLog(t, tg)
	if !strings.Contains(log, "- duplicate experience.wrap proj#1: done (same type and title") || !strings.Contains(log, "3.md: the work item already has its experience.wrap summary") {
		t.Fatalf("log:\n%s", log)
	}
	// aihub already holds the same title for this work item (another
	// capsule's attempt): a duplicate, nothing posted; another work
	// item's same title is no duplicate
	oh.hits = `{"items":[{"id":"mem_x","title":"Shared lesson","work_item_id":"wi_ob"}]}`
	putOutbox(t, tg, "4.md", capsule.OutboxMemory{Type: "experience.env", Title: "shared lesson", Body: "b", Step: "fix", Attempt: 2})
	flush(d, tg)
	if len(oh.posts) != 1 || !strings.Contains(memoryLog(t, tg), "- duplicate of mem_x experience.env shared lesson") {
		t.Fatalf("aihub dedupe: posts %+v log:\n%s", oh.posts, memoryLog(t, tg))
	}
	oh.hits = `{"items":[{"id":"mem_y","title":"Other lesson","work_item_id":"wi_zz"}]}`
	putOutbox(t, tg, "5.md", capsule.OutboxMemory{Type: "experience.env", Title: "other lesson", Body: "b", Step: "deliver", Attempt: 3})
	flush(d, tg)
	if len(oh.posts) != 2 {
		t.Fatalf("another work item's title deduped: %+v", oh.posts)
	}
}

func TestOutboxOutageKeepsPendingAndRetries(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	oh.failPost = errors.New("dial tcp: connection refused")
	putOutbox(t, tg, "1.md", capsule.OutboxMemory{Type: "experience.env", Title: "lesson", Body: "b"})
	flush(d, tg)
	if got := outboxAt(tg, capsule.OutboxPending); len(got) != 1 || len(oh.posts) != 0 {
		t.Fatalf("pending = %v posts %+v", got, oh.posts)
	}
	// a 5xx answer is an outage too; the next step's own memory is judged
	// on its own quota while the pending one waits
	oh.failPost = &api.Error{Code: "MEM-500", Detail: "store down"}
	putOutbox(t, tg, "2.md", capsule.OutboxMemory{Type: "experience.env", Title: "next", Body: "b", Step: "fix", Attempt: 2})
	flush(d, tg)
	if got := outboxAt(tg, capsule.OutboxPending); len(got) != 2 {
		t.Fatalf("pending after 5xx = %v", got)
	}
	oh.failPost = nil
	flush(d, tg)
	if len(oh.posts) != 2 || len(outboxAt(tg, capsule.OutboxPending)) != 0 || len(outboxAt(tg, capsule.OutboxDone)) != 2 {
		t.Fatalf("retry: posts %+v pending %v", oh.posts, outboxAt(tg, capsule.OutboxPending))
	}
	// a typed 4xx refusal is final: rejected, logged
	oh.failPost = &api.Error{Code: "MEM-403", Detail: "no writer role"}
	putOutbox(t, tg, "3.md", capsule.OutboxMemory{Type: "experience.env", Title: "third", Body: "b", Step: "deliver", Attempt: 3})
	flush(d, tg)
	if got := outboxAt(tg, capsule.OutboxRejected); len(got) != 1 || !strings.Contains(memoryLog(t, tg), "3.md by aihub: MEM-403") {
		t.Fatalf("4xx: rejected %v log:\n%s", got, memoryLog(t, tg))
	}
	// a search outage defers the upload as well
	oh.failPost = nil
	oh.searchFn = func() error { return errors.New("timeout") }
	writeJournal(t, tg.capsuleDir, []capsule.SpawnJournalEntry{{StepID: "fix", SpawnN: 1}, {StepID: "fix", SpawnN: 2}, {StepID: "deliver", SpawnN: 3}, {StepID: "deliver", SpawnN: 4}})
	putOutbox(t, tg, "4.md", capsule.OutboxMemory{Type: "experience.env", Title: "fourth", Body: "b", Step: "deliver", Attempt: 4})
	flush(d, tg)
	if len(outboxAt(tg, capsule.OutboxPending)) != 1 {
		t.Fatal("a search outage must keep the file pending")
	}
}

// TestDriverLogOnlyRequestsAFlush: the driver log (called under the
// driver's lock) never waits: with aihub hung, Append returns at once;
// the flush runs in the background.
func TestDriverLogOnlyRequestsAFlush(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	doc := capsule.WorkItemDoc{ID: tg.wi, Project: tg.project}
	raw, _ := json.Marshal(doc)
	path := filepath.Join(tg.capsuleDir, filepath.FromSlash(capsule.WorkItemJSON))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	oh.searchFn = func() error { <-release; return nil }
	putOutbox(t, tg, "1.md", capsule.OutboxMemory{Type: "experience.wrap", Title: "proj#1: x", Body: "b"})
	start := time.Now()
	for _, kind := range []string{"step_done", "terminal"} {
		if err := d.driverLogFor(tg.capsuleDir).Append(tg.wi, kind, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Append blocked %s on a hung aihub", el)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for len(outboxAt(tg, capsule.OutboxDone)) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the requested flush never uploaded")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFinalFlushBoundedAndClosed: a hung aihub at the final flush costs
// at most the bound; once it returns the outbox is closed — the late
// flush makes no move and no log write, and no new flush starts — until
// a new driver handle reopens it.
func TestFinalFlushBoundedAndClosed(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	release := make(chan struct{})
	oh.searchFn = func() error { <-release; return nil }
	putOutbox(t, tg, "1.md", capsule.OutboxMemory{Type: "experience.env", Title: "late", Body: "b"})
	start := time.Now()
	d.finalFlush(tg, 100*time.Millisecond)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("final flush took %s", el)
	}
	close(release) // the hung call answers after the seal began
	time.Sleep(100 * time.Millisecond)
	if got := outboxAt(tg, capsule.OutboxPending); len(got) != 1 || oh.postCount() != 0 || len(outboxAt(tg, capsule.OutboxDone)) != 0 {
		t.Fatalf("a late flush acted after the close: pending %v posts %d", got, oh.postCount())
	}
	logBefore := memoryLog(t, tg)
	oh.mu.Lock()
	oh.searchFn = nil
	oh.mu.Unlock()
	d.requestFlush(tg)
	d.finalFlush(tg, time.Second)
	time.Sleep(50 * time.Millisecond)
	if oh.postCount() != 0 || memoryLog(t, tg) != logBefore {
		t.Fatal("a flush started on a closed outbox")
	}
	d.reopenOutbox(tg.wi)
	flush(d, tg)
	if oh.postCount() != 1 {
		t.Fatalf("reopened outbox did not flush: %d", oh.postCount())
	}
	d.dropOutbox(tg.wi)
	if _, ok := d.outboxLocks.Load(tg.wi); ok {
		t.Fatal("dropOutbox kept the state")
	}
}

// TestOutboxRefusesSymlinks: a symlinked outbox (or artifacts) dir is
// never read or written through, and the memory log is never followed.
func TestOutboxRefusesSymlinks(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	elsewhere := t.TempDir()
	putOutbox(t, outboxTarget{capsuleDir: elsewhere}, "1.md", capsule.OutboxMemory{Type: "experience.env", Title: "t", Body: "b"})
	if err := os.MkdirAll(filepath.Join(tg.capsuleDir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, filepath.FromSlash(capsule.DirMemoryOutbox)), filepath.Join(tg.capsuleDir, filepath.FromSlash(capsule.DirMemoryOutbox))); err != nil {
		t.Fatal(err)
	}
	flush(d, tg)
	if len(oh.posts) != 0 || len(outboxAt(outboxTarget{capsuleDir: elsewhere}, "")) != 1 {
		t.Fatal("a symlinked outbox was flushed")
	}
	// the log: a symlink at its path is not followed
	_, oh2, tg2 := outboxEnv(t)
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tg2.capsuleDir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(tg2.capsuleDir, filepath.FromSlash(capsule.MemoryLogMD))); err != nil {
		t.Fatal(err)
	}
	putOutbox(t, tg2, "1.md", capsule.OutboxMemory{Type: "experience.env", Title: "t", Body: "b"})
	d2 := &Daemon{hub: d.hub}
	d2.hub.(*fakeHub).memDo = oh2.do
	flush(d2, tg2)
	if raw, _ := os.ReadFile(target); string(raw) != "keep\n" {
		t.Fatalf("the log followed a symlink: %q", raw)
	}
}
