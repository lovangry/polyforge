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

func outboxEnv(t *testing.T) (*Daemon, *outboxHub, outboxTarget) {
	t.Helper()
	hub := newFakeHub()
	oh := &outboxHub{}
	hub.memDo = oh.do
	d := &Daemon{hub: hub}
	d.cfg.APIKey = "k_machine_secret_value"
	return d, oh, outboxTarget{wi: "wi_ob", project: "proj", capsuleDir: t.TempDir()}
}

func putOutbox(t *testing.T, tg outboxTarget, name string, m capsule.OutboxMemory) {
	t.Helper()
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
	d.flushOutbox(tg)
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
	d.flushOutbox(tg)
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
	putOutbox(t, tg, "e.md", capsule.OutboxMemory{Type: "experience.env", Title: "key", Body: "api_key = abcdef0123456789"})
	putOutbox(t, tg, "f.md", capsule.OutboxMemory{Type: "experience.env", Title: "machine", Body: "the key k_machine_secret_value"})
	putOutbox(t, tg, "g.md", capsule.OutboxMemory{Type: "experience.env", Title: "tag", Tags: []string{"bad tag"}, Body: "b"})
	writeOutboxRaw(t, tg, "h.md", "no frontmatter\n")
	d.flushOutbox(tg)
	if len(oh.posts) != 1 || oh.posts[0]["title"] != "first" {
		t.Fatalf("posts = %+v", oh.posts)
	}
	rejected := outboxAt(tg, capsule.OutboxRejected)
	if strings.Join(rejected, ",") != "a2.md,b.md,c.md,d.md,e.md,f.md,g.md,h.md" {
		t.Fatalf("rejected = %v", rejected)
	}
	log := memoryLog(t, tg)
	for _, want := range []string{"a2.md: more than one memory in one step", `b.md: type "doc.design" is not experience.<kind>`, "c.md: a title is required",
		"d.md: the body is 2049 bytes", "e.md: the memory looks like it carries a credential value", "f.md: the memory", "g.md: tag \"bad tag\"", "h.md: no frontmatter"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %q:\n%s", want, log)
		}
	}
	// the next step may write its own one memory
	putOutbox(t, tg, "z.md", capsule.OutboxMemory{Type: "experience.env", Title: "next step", Body: "b"})
	d.flushOutbox(tg)
	if len(oh.posts) != 2 {
		t.Fatalf("next step's memory not uploaded: %+v", oh.posts)
	}
}

func TestOutboxDedupesAndOneWrap(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	putOutbox(t, tg, "1.md", capsule.OutboxMemory{Type: "experience.wrap", Title: "proj#1: done", Body: "b"})
	d.flushOutbox(tg)
	// same title again (capsule dedupe) and a second wrap (quota)
	putOutbox(t, tg, "2.md", capsule.OutboxMemory{Type: "experience.wrap", Title: "proj#1: done", Body: "b"})
	putOutbox(t, tg, "3.md", capsule.OutboxMemory{Type: "experience.wrap", Title: "proj#1: other", Body: "b"})
	d.flushOutbox(tg)
	if len(oh.posts) != 1 {
		t.Fatalf("posts = %+v", oh.posts)
	}
	log := memoryLog(t, tg)
	if !strings.Contains(log, "- duplicate experience.wrap proj#1: done") || !strings.Contains(log, "3.md: the work item already has its experience.wrap summary") {
		t.Fatalf("log:\n%s", log)
	}
	// aihub already holds the same title for this work item (another
	// capsule's attempt): a duplicate, nothing posted; another work
	// item's same title is no duplicate
	oh.hits = `{"items":[{"id":"mem_x","title":"Shared lesson","work_item_id":"wi_ob"}]}`
	putOutbox(t, tg, "4.md", capsule.OutboxMemory{Type: "experience.env", Title: "shared lesson", Body: "b"})
	d.flushOutbox(tg)
	if len(oh.posts) != 1 || !strings.Contains(memoryLog(t, tg), "- duplicate of mem_x experience.env shared lesson") {
		t.Fatalf("aihub dedupe: posts %+v log:\n%s", oh.posts, memoryLog(t, tg))
	}
	oh.hits = `{"items":[{"id":"mem_y","title":"Other lesson","work_item_id":"wi_zz"}]}`
	putOutbox(t, tg, "5.md", capsule.OutboxMemory{Type: "experience.env", Title: "other lesson", Body: "b"})
	d.flushOutbox(tg)
	if len(oh.posts) != 2 {
		t.Fatalf("another work item's title deduped: %+v", oh.posts)
	}
}

func TestOutboxOutageKeepsPendingAndRetries(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	oh.failPost = errors.New("dial tcp: connection refused")
	putOutbox(t, tg, "1.md", capsule.OutboxMemory{Type: "experience.env", Title: "lesson", Body: "b"})
	d.flushOutbox(tg)
	if got := outboxAt(tg, capsule.OutboxPending); len(got) != 1 || len(oh.posts) != 0 {
		t.Fatalf("pending = %v posts %+v", got, oh.posts)
	}
	// a 5xx answer is an outage too; the next step's own memory is judged
	// on its own quota while the pending one waits
	oh.failPost = &api.Error{Code: "MEM-500", Detail: "store down"}
	putOutbox(t, tg, "2.md", capsule.OutboxMemory{Type: "experience.env", Title: "next", Body: "b"})
	d.flushOutbox(tg)
	if got := outboxAt(tg, capsule.OutboxPending); len(got) != 2 {
		t.Fatalf("pending after 5xx = %v", got)
	}
	oh.failPost = nil
	d.flushOutbox(tg)
	if len(oh.posts) != 2 || len(outboxAt(tg, capsule.OutboxPending)) != 0 || len(outboxAt(tg, capsule.OutboxDone)) != 2 {
		t.Fatalf("retry: posts %+v pending %v", oh.posts, outboxAt(tg, capsule.OutboxPending))
	}
	// a typed 4xx refusal is final: rejected, logged
	oh.failPost = &api.Error{Code: "MEM-403", Detail: "no writer role"}
	putOutbox(t, tg, "3.md", capsule.OutboxMemory{Type: "experience.env", Title: "third", Body: "b"})
	d.flushOutbox(tg)
	if got := outboxAt(tg, capsule.OutboxRejected); len(got) != 1 || !strings.Contains(memoryLog(t, tg), "3.md by aihub: MEM-403") {
		t.Fatalf("4xx: rejected %v log:\n%s", got, memoryLog(t, tg))
	}
	// a search outage defers the upload as well
	oh.failPost = nil
	oh.searchFn = func() error { return errors.New("timeout") }
	putOutbox(t, tg, "4.md", capsule.OutboxMemory{Type: "experience.env", Title: "fourth", Body: "b"})
	d.flushOutbox(tg)
	if len(outboxAt(tg, capsule.OutboxPending)) != 1 {
		t.Fatal("a search outage must keep the file pending")
	}
}

// TestDriverLogFlushesAtTerminal: the per-capsule driver log flushes the
// outbox synchronously at the terminal transition (before the seal).
func TestDriverLogFlushesAtTerminal(t *testing.T) {
	d, oh, tg := outboxEnv(t)
	doc := capsule.WorkItemDoc{ID: tg.wi, Project: tg.project}
	raw, _ := json.Marshal(doc)
	path := filepath.Join(tg.capsuleDir, filepath.FromSlash(capsule.WorkItemJSON))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	putOutbox(t, tg, "1.md", capsule.OutboxMemory{Type: "experience.wrap", Title: "proj#1: x", Body: "b"})
	if err := d.driverLogFor(tg.capsuleDir).Append(tg.wi, "terminal", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if len(oh.posts) != 1 {
		t.Fatalf("terminal did not flush: %+v", oh.posts)
	}
}
