package daemon

// memoutbox.go: the upload half of the step agent's memory write-back.
// Inside a step, `pf remember` writes one file into the capsule's memory
// outbox (capsule/memoryoutbox.go) — the agent holds no aihub credential
// path. The daemon flushes the outbox with its own machine credential.
//
// No flush ever runs under a driver or runtime lock: the driver log only
// REQUESTS one (a goroutine, after step_done / gate_verdict / terminal),
// and the final flush runs in the agent adapter once the driver has
// exited — before the adapter reports the exit, so before the observe
// lane hands the work item to the seal — and in an operator close after
// the agent is stopped and before rt.Stop seals. The final flush is
// bounded (outboxFlushWait); when it returns (or times out) the outbox is
// CLOSED: a flush still in flight makes no further move or log write,
// and no flush starts, until a new driver handle reopens it (a recovery).
//
// One flush, per work item and serialized:
//
//  1. judge each new file: at most OutboxFileMax bytes, type
//     experience.<kind>, a one-line title, a body of at most
//     outboxBodyMax bytes, plain tags, no credential value; queued by a
//     step dispatch of this work item (step + attempt in the frontmatter,
//     checked against the spawn journal), at most ONE memory per step
//     attempt besides the experience.wrap summary, one wrap per work
//     item. A refused file moves to rejected/; an accepted one to pending/.
//  2. upload each pending file, attributed to the job's work item and
//     project: a memory of the same work item with the same type and
//     title is a duplicate (done/, nothing written); a typed 4xx refusal
//     moves the file to rejected/; an outage (transport, 5xx, 408, 429)
//     keeps it in pending/ for the next flush and ends this one.
//
// Every outcome is one line of artifacts/memory-log.md. A failure never
// fails a step. A work item sealed while memories are still pending (aihub
// down at the final flush) keeps them in the sealed capsule's pending/ —
// best effort, there is no flush after the seal. Symlinked outbox,
// artifacts or log paths are refused, never followed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
)

const (
	// outboxBodyMax bounds a memory body (bytes).
	outboxBodyMax = 2048
	// outboxTitleRunes bounds a title.
	outboxTitleRunes = 200
	// outboxMaxTags bounds the tags of one memory.
	outboxMaxTags = 8
	// outboxFlushWait bounds the final flush.
	outboxFlushWait = 20 * time.Second
	// outboxStopWait bounds stopping the agent before an operator
	// close's final flush.
	outboxStopWait = 10 * time.Second
	// wrapType is the work item's one summary memory.
	wrapType = "experience.wrap"
)

var (
	outboxTypeRe = regexp.MustCompile(`^experience\.[a-z0-9][a-z0-9._-]*$`)
	outboxTagRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// outboxTarget is the job an outbox belongs to.
type outboxTarget struct {
	wi, project, capsuleDir string
}

// outboxTargetOf reads the target from the capsule's materialized task
// definition.
func outboxTargetOf(capsuleDir string) (outboxTarget, bool) {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, filepath.FromSlash(capsule.WorkItemJSON)))
	if err != nil {
		return outboxTarget{}, false
	}
	var doc capsule.WorkItemDoc
	if json.Unmarshal(raw, &doc) != nil || doc.ID == "" || doc.Project == "" {
		return outboxTarget{}, false
	}
	return outboxTarget{wi: doc.ID, project: doc.Project, capsuleDir: capsuleDir}, true
}

// outboxState is one work item's flush state.
type outboxState struct {
	run    sync.Mutex // one flush at a time
	mu     sync.Mutex // guards closed; held across each capsule mutation
	closed bool
}

// do runs one capsule mutation unless the outbox is closed; false = closed.
func (st *outboxState) do(fn func()) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return false
	}
	fn()
	return true
}

func (st *outboxState) isClosed() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closed
}

func (d *Daemon) outboxStateOf(wi string) *outboxState {
	v, _ := d.outboxLocks.LoadOrStore(wi, &outboxState{})
	return v.(*outboxState)
}

// requestFlush starts one background flush (never blocks the caller).
func (d *Daemon) requestFlush(t outboxTarget) {
	st := d.outboxStateOf(t.wi)
	if st.isClosed() {
		return
	}
	go d.flushOutbox(t, st)
}

// finalFlush runs one flush, waits at most wait, then closes the outbox.
func (d *Daemon) finalFlush(t outboxTarget, wait time.Duration) {
	st := d.outboxStateOf(t.wi)
	if st.isClosed() {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.flushOutbox(t, st)
	}()
	select {
	case <-done:
	case <-time.After(wait):
		fmt.Fprintf(os.Stderr, "daemon: %s: memory outbox flush still running after %s; the outbox is closed, pending memories stay in the capsule\n", t.wi, wait)
	}
	st.mu.Lock()
	st.closed = true
	st.mu.Unlock()
}

// reopenOutbox reopens a work item's outbox for a new driver handle.
func (d *Daemon) reopenOutbox(wi string) {
	st := d.outboxStateOf(wi)
	st.mu.Lock()
	st.closed = false
	st.mu.Unlock()
}

// dropOutbox forgets a finished work item's flush state (it stays closed
// for any flush still in flight).
func (d *Daemon) dropOutbox(wi string) {
	if v, ok := d.outboxLocks.LoadAndDelete(wi); ok {
		st := v.(*outboxState)
		st.mu.Lock()
		st.closed = true
		st.mu.Unlock()
	}
}

// outboxFile is one judged outbox file.
type outboxFile struct {
	name string
	mem  capsule.OutboxMemory
}

// realDir reports whether path is a directory that is no symlink
// (absent = false, nil).
func realDir(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return false, fmt.Errorf("%s is not a plain directory", path)
	}
	return true, nil
}

// flushOutbox judges the new outbox files and uploads the pending ones.
func (d *Daemon) flushOutbox(t outboxTarget, st *outboxState) {
	if t.capsuleDir == "" || t.wi == "" || t.project == "" {
		return
	}
	st.run.Lock()
	defer st.run.Unlock()
	if st.isClosed() {
		return
	}
	root := filepath.Join(t.capsuleDir, filepath.FromSlash(capsule.DirMemoryOutbox))
	for _, dir := range []string{filepath.Dir(root), root} {
		if ok, err := realDir(dir); err != nil || !ok {
			if err != nil {
				fmt.Fprintf(os.Stderr, "daemon: %s: memory outbox refused: %v\n", t.wi, err)
			}
			return
		}
	}
	fresh := outboxNames(root)
	if len(fresh) == 0 && len(outboxNames(filepath.Join(root, capsule.OutboxPending))) == 0 {
		return
	}
	logLine := func(line string) bool { return st.do(func() { appendMemoryLog(t, line) }) }
	move := func(src, sub string) bool {
		return st.do(func() { moveOutbox(src, filepath.Join(root, sub)) })
	}

	known := outboxRead(root, capsule.OutboxDone)
	known = append(known, outboxRead(root, capsule.OutboxPending)...)
	wrapSeen := false
	taken := map[string]bool{} // step#attempt that already has its memory
	for _, f := range known {
		if f.mem.Type == wrapType {
			wrapSeen = true
		} else {
			taken[attemptKey(f.mem)] = true
		}
	}
	dispatches := journalDispatches(t.capsuleDir)
	for _, name := range fresh {
		path := filepath.Join(root, name)
		m, reason := readOutboxFile(path)
		if reason == "" {
			reason = d.outboxDefect(m)
		}
		if reason == "" && !dispatches[attemptKey(m)] {
			reason = fmt.Sprintf("step %q attempt %d is no dispatch of this work item (queue memories with `pf remember` inside the step)", m.Step, m.Attempt)
		}
		if reason == "" && sameTitle(known, m.Type, m.Title) {
			if !move(path, capsule.OutboxDone) || !logLine(fmt.Sprintf("- duplicate %s %s (same type and title as an earlier memory of this work item)", m.Type, oneLineTitle(m.Title))) {
				return
			}
			continue
		}
		switch {
		case reason != "":
		case m.Type == wrapType && wrapSeen:
			reason = "the work item already has its experience.wrap summary"
		case m.Type != wrapType && taken[attemptKey(m)]:
			reason = fmt.Sprintf("a second memory from step %s attempt %d (the policy allows one per step, plus the wrap summary)", m.Step, m.Attempt)
		}
		if reason != "" {
			if !move(path, capsule.OutboxRejected) || !logLine(fmt.Sprintf("- rejected %s: %s", name, reason)) {
				return
			}
			continue
		}
		if m.Type == wrapType {
			wrapSeen = true
		} else {
			taken[attemptKey(m)] = true
		}
		if !move(path, capsule.OutboxPending) {
			return
		}
		known = append(known, outboxFile{name, m})
	}

	for _, f := range outboxRead(root, capsule.OutboxPending) {
		line, sub, outage := d.uploadOutbox(t, f, st)
		if outage {
			return // the rest waits for the next flush
		}
		if !move(filepath.Join(root, capsule.OutboxPending, f.name), sub) || !logLine(line) {
			return
		}
	}
}

// readOutboxFile reads and parses one outbox file (capsule.ReadOutboxFile:
// no symlink, regular, capped).
func readOutboxFile(path string) (capsule.OutboxMemory, string) {
	raw, err := capsule.ReadOutboxFile(path)
	if err != nil {
		return capsule.OutboxMemory{}, err.Error()
	}
	m, err := capsule.ParseOutboxMemory(raw)
	if err != nil {
		return m, err.Error()
	}
	return m, ""
}

func attemptKey(m capsule.OutboxMemory) string { return fmt.Sprintf("%s#%d", m.Step, m.Attempt) }

// journalDispatches answers the step#attempt keys of the capsule's spawn
// journal.
func journalDispatches(capsuleDir string) map[string]bool {
	out := map[string]bool{}
	entries, _ := capsule.ReadSpawnJournal(capsuleDir)
	for _, e := range entries {
		out[fmt.Sprintf("%s#%d", e.StepID, e.SpawnN)] = true
	}
	return out
}

// outboxDefect answers why a parsed outbox memory is refused ("" = fine).
func (d *Daemon) outboxDefect(m capsule.OutboxMemory) string {
	title := strings.TrimSpace(m.Title)
	body := strings.TrimSuffix(m.Body, "\n")
	switch {
	case !outboxTypeRe.MatchString(m.Type):
		return fmt.Sprintf("type %q is not experience.<kind>", m.Type)
	case title == "":
		return "a title is required"
	case strings.ContainsAny(title, "\r\n"):
		return "the title must be one line"
	case utf8.RuneCountInString(title) > outboxTitleRunes:
		return fmt.Sprintf("the title exceeds %d characters", outboxTitleRunes)
	case strings.TrimSpace(body) == "":
		return "the body is empty"
	case len(body) > outboxBodyMax:
		return fmt.Sprintf("the body is %d bytes (at most %d)", len(body), outboxBodyMax)
	case len(m.Tags) > outboxMaxTags:
		return fmt.Sprintf("more than %d tags", outboxMaxTags)
	}
	for _, tag := range m.Tags {
		if !outboxTagRe.MatchString(tag) {
			return fmt.Sprintf("tag %q is not a plain name", tag)
		}
	}
	text := title + "\n" + m.Body
	if why := capsule.SecretValueDefect(text); why != "" {
		return "the memory " + why
	}
	if k := d.cfg.APIKey; k != "" && strings.Contains(text, k) {
		return "the memory carries the daemon's credential value"
	}
	return ""
}

// uploadOutbox uploads one pending file and answers its log line and the
// subdirectory it moves to; outage = keep it pending, stop the flush.
func (d *Daemon) uploadOutbox(t outboxTarget, f outboxFile, st *outboxState) (line, sub string, outage bool) {
	if st.isClosed() {
		return "", "", true
	}
	title := strings.TrimSpace(f.mem.Title)
	dup, err := d.sameTitleMemory(t, f.mem.Type, title)
	if err != nil {
		if transient(err) {
			fmt.Fprintf(os.Stderr, "daemon: %s: memory outbox upload deferred: %v\n", t.wi, err)
			return "", "", true
		}
		dup = ""
	}
	if dup != "" {
		return fmt.Sprintf("- duplicate of %s %s %s", dup, f.mem.Type, oneLineTitle(title)), capsule.OutboxDone, false
	}
	body := map[string]any{
		"project":      t.project,
		"type":         f.mem.Type,
		"title":        title,
		"content":      f.mem.Body,
		"visibility":   "project",
		"work_item_id": t.wi,
	}
	if len(f.mem.Tags) > 0 {
		body["tags"] = f.mem.Tags
	}
	if st.isClosed() {
		return "", "", true // the outbox closed while the search ran: nothing is written
	}
	var stored api.Memory
	if err := d.hub.Do("POST", "/api/v1/memories", body, &stored); err != nil {
		if transient(err) {
			fmt.Fprintf(os.Stderr, "daemon: %s: memory outbox upload deferred: %v\n", t.wi, err)
			return "", "", true
		}
		return fmt.Sprintf("- rejected %s by aihub: %v", f.name, err), capsule.OutboxRejected, false
	}
	tags := ""
	if len(f.mem.Tags) > 0 {
		tags = " [" + strings.Join(f.mem.Tags, ",") + "]"
	}
	return fmt.Sprintf("- %s %s%s %s", stored.ID, f.mem.Type, tags, oneLineTitle(title)), capsule.OutboxDone, false
}

// sameTitleMemory answers the id of a memory of the same work item, type
// and title already in aihub ("" = none).
func (d *Daemon) sameTitleMemory(t outboxTarget, memType, title string) (string, error) {
	v := url.Values{}
	v.Set("project", t.project)
	v.Set("q", title)
	v.Set("type", memType)
	v.Set("limit", "50")
	var resp struct {
		Items []struct {
			ID         string  `json:"id"`
			Title      *string `json:"title"`
			WorkItemID *string `json:"work_item_id"`
		} `json:"items"`
	}
	if err := d.hub.Do("GET", "/api/v1/memories/search?"+v.Encode(), nil, &resp); err != nil {
		return "", err
	}
	for _, h := range resp.Items {
		if h.WorkItemID != nil && *h.WorkItemID == t.wi && h.Title != nil && strings.EqualFold(strings.TrimSpace(*h.Title), title) {
			return h.ID, nil
		}
	}
	return "", nil
}

// transient reports an outage-class failure (transport, 5xx, 408, 429).
func transient(err error) bool {
	var ae *api.Error
	if !errors.As(err, &ae) {
		return true
	}
	s := ae.HTTPStatus()
	return s >= 500 || s == 408 || s == 429
}

func sameTitle(files []outboxFile, memType, title string) bool {
	title = strings.TrimSpace(title)
	for _, f := range files {
		if f.mem.Type == memType && strings.EqualFold(strings.TrimSpace(f.mem.Title), title) {
			return true
		}
	}
	return false
}

func oneLineTitle(s string) string { return strings.Join(strings.Fields(s), " ") }

// outboxNames lists the regular *.md files directly in dir, sorted
// (symlinks are no regular files).
func outboxNames(dir string) []string {
	if ok, _ := realDir(dir); !ok {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// outboxRead parses every file of one outbox subdirectory (unreadable
// ones skipped).
func outboxRead(root, sub string) []outboxFile {
	dir := filepath.Join(root, sub)
	var out []outboxFile
	for _, name := range outboxNames(dir) {
		if m, reason := readOutboxFile(filepath.Join(dir, name)); reason == "" {
			out = append(out, outboxFile{name, m})
		}
	}
	return out
}

// moveOutbox moves the file src into dstDir (created on demand; a
// symlinked dstDir is refused).
func moveOutbox(src, dstDir string) {
	ok, err := realDir(dstDir)
	if err == nil && !ok {
		err = os.Mkdir(dstDir, 0o755)
	}
	if err == nil {
		err = os.Rename(src, filepath.Join(dstDir, filepath.Base(src)))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: memory outbox: move %s: %v\n", filepath.Base(src), err)
	}
}

// appendMemoryLog appends one line to the capsule's memory log (never
// through a symlink).
func appendMemoryLog(t outboxTarget, line string) {
	path := filepath.Join(t.capsuleDir, filepath.FromSlash(capsule.MemoryLogMD))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %s: memory log: %v\n", t.wi, err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s (%s)\n", line, time.Now().UTC().Format(time.RFC3339))
}
