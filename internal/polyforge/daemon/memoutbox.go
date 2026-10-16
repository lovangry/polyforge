package daemon

// memoutbox.go: the upload half of the step agent's memory write-back.
// Inside a step, `pf remember` writes one file into the capsule's memory
// outbox (capsule/memoryoutbox.go) — the agent holds no aihub credential
// path. The daemon flushes the outbox with its own machine credential:
// after every step result (step_done / gate_verdict, without blocking
// the driver), at the workflow's terminal transition (bounded wait, so
// the last step's memories land before the capsule is sealed) and on
// every operator close (pf wrap / fail / pause, before the teardown).
//
// One flush, per work item and serialized:
//
//  1. judge each new file: type experience.<kind>, a one-line title, a
//     body of at most outboxBodyMax bytes, plain tags, no credential-
//     looking content; at most ONE memory per step (per flush) besides
//     the experience.wrap summary, and one wrap per work item. A refused
//     file moves to rejected/; an accepted one to pending/.
//  2. upload each pending file, attributed to the job's work item and
//     project: a memory of the same work item with the same title is a
//     duplicate (done/, nothing written); a typed 4xx refusal moves the
//     file to rejected/; an outage (transport, 5xx) keeps it in pending/
//     for the next flush and ends this one.
//
// Every outcome is one line of artifacts/memory-log.md. A failure never
// fails a step. A work item wrapped while its memories are still pending
// (aihub down at the last flush) keeps them in the sealed capsule's
// pending/ — best effort, there is no flush after the seal.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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
	// outboxFlushWait bounds how long a terminal transition or a close
	// waits for the flush (the flush itself goes on in the background).
	outboxFlushWait = 20 * time.Second
	// wrapType is the work item's one summary memory.
	wrapType = "experience.wrap"
)

var (
	outboxTypeRe = regexp.MustCompile(`^experience\.[a-z0-9][a-z0-9._-]*$`)
	outboxTagRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// outboxSecretRes are credential-looking contents a memory must never
	// carry (a guard against accidents: the policy says name where a
	// secret lives, never its value).
	outboxSecretRes = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`\b(sk|pk|rk)-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`\b(ghp|gho|ghs|ghu|github_pat)_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password|passwd)\b["']?\s*[:=]\s*["']?[^\s"']{8,}`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}`),
	}
)

// outboxTarget is the job an outbox belongs to.
type outboxTarget struct {
	wi, project, capsuleDir string
}

// outboxTargetOf reads the target from the capsule's materialized task
// definition (no runtime lookup: the driver's event path calls this).
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

// flushOutboxBounded runs one flush and waits at most wait for it.
func (d *Daemon) flushOutboxBounded(t outboxTarget, wait time.Duration) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.flushOutbox(t)
	}()
	select {
	case <-done:
	case <-time.After(wait):
		fmt.Fprintf(os.Stderr, "daemon: %s: memory outbox flush still running after %s; continuing without it\n", t.wi, wait)
	}
}

// outboxFile is one judged outbox file.
type outboxFile struct {
	name string
	mem  capsule.OutboxMemory
}

// flushOutbox judges the new outbox files and uploads the pending ones.
func (d *Daemon) flushOutbox(t outboxTarget) {
	if t.capsuleDir == "" || t.wi == "" || t.project == "" {
		return
	}
	mu, _ := d.outboxLocks.LoadOrStore(t.wi, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	root := filepath.Join(t.capsuleDir, filepath.FromSlash(capsule.DirMemoryOutbox))
	fresh := outboxNames(root)
	pending := outboxNames(filepath.Join(root, capsule.OutboxPending))
	if len(fresh) == 0 && len(pending) == 0 {
		return
	}
	var log []string
	defer func() { appendMemoryLog(t, log) }()

	known := outboxRead(root, capsule.OutboxDone)
	known = append(known, outboxRead(root, capsule.OutboxPending)...)
	wrapSeen, stepTaken := false, false
	for _, f := range known {
		if f.mem.Type == wrapType {
			wrapSeen = true
		}
	}
	for _, name := range fresh {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		m, perr := capsule.ParseOutboxMemory(raw)
		reason := ""
		switch {
		case perr != nil:
			reason = perr.Error()
		default:
			reason = d.outboxDefect(m)
		}
		if reason == "" && sameTitle(known, m.Title) {
			moveOutbox(filepath.Join(root, name), filepath.Join(root, capsule.OutboxDone))
			log = append(log, fmt.Sprintf("- duplicate %s %s (same title as an earlier memory of this work item)", m.Type, oneLineTitle(m.Title)))
			continue
		}
		switch {
		case reason != "":
		case m.Type == wrapType && wrapSeen:
			reason = "the work item already has its experience.wrap summary"
		case m.Type != wrapType && stepTaken:
			reason = "more than one memory in one step (the policy allows one, plus the wrap summary)"
		}
		if reason != "" {
			moveOutbox(filepath.Join(root, name), filepath.Join(root, capsule.OutboxRejected))
			log = append(log, fmt.Sprintf("- rejected %s: %s", name, reason))
			continue
		}
		if m.Type == wrapType {
			wrapSeen = true
		} else {
			stepTaken = true
		}
		moveOutbox(filepath.Join(root, name), filepath.Join(root, capsule.OutboxPending))
		known = append(known, outboxFile{name, m})
	}

	for _, f := range outboxRead(root, capsule.OutboxPending) {
		line, keep := d.uploadOutbox(t, f)
		if line != "" {
			log = append(log, line)
		}
		if keep {
			return // an outage: the rest waits for the next flush
		}
	}
}

// outboxDefect answers why a parsed outbox memory is refused ("" = fine).
func (d *Daemon) outboxDefect(m capsule.OutboxMemory) string {
	title := strings.TrimSpace(m.Title)
	switch {
	case !outboxTypeRe.MatchString(m.Type):
		return fmt.Sprintf("type %q is not experience.<kind>", m.Type)
	case title == "":
		return "a title is required"
	case strings.ContainsAny(title, "\r\n"):
		return "the title must be one line"
	case utf8.RuneCountInString(title) > outboxTitleRunes:
		return fmt.Sprintf("the title exceeds %d characters", outboxTitleRunes)
	case strings.TrimSpace(m.Body) == "":
		return "the body is empty"
	case len(strings.TrimSuffix(m.Body, "\n")) > outboxBodyMax:
		return fmt.Sprintf("the body is %d bytes (at most %d)", len(strings.TrimSuffix(m.Body, "\n")), outboxBodyMax)
	case len(m.Tags) > outboxMaxTags:
		return fmt.Sprintf("more than %d tags", outboxMaxTags)
	}
	for _, tag := range m.Tags {
		if !outboxTagRe.MatchString(tag) {
			return fmt.Sprintf("tag %q is not a plain name", tag)
		}
	}
	text := title + "\n" + m.Body
	for _, re := range outboxSecretRes {
		if re.MatchString(text) {
			return "the memory looks like it carries a credential value (name where a secret lives, never the value)"
		}
	}
	if k := d.cfg.APIKey; k != "" && strings.Contains(text, k) {
		return "the memory carries a credential value"
	}
	return ""
}

// uploadOutbox uploads one pending file; keep reports an outage (the
// file stays pending and the flush stops).
func (d *Daemon) uploadOutbox(t outboxTarget, f outboxFile) (line string, keep bool) {
	root := filepath.Join(t.capsuleDir, filepath.FromSlash(capsule.DirMemoryOutbox))
	title := strings.TrimSpace(f.mem.Title)
	dup, err := d.sameTitleMemory(t, f.mem.Type, title)
	if err != nil {
		if transient(err) {
			fmt.Fprintf(os.Stderr, "daemon: %s: memory outbox upload deferred: %v\n", t.wi, err)
			return "", true
		}
		dup = ""
	}
	if dup != "" {
		moveOutbox(filepath.Join(root, capsule.OutboxPending, f.name), filepath.Join(root, capsule.OutboxDone))
		return fmt.Sprintf("- duplicate of %s %s %s", dup, f.mem.Type, oneLineTitle(title)), false
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
	var stored api.Memory
	if err := d.hub.Do("POST", "/api/v1/memories", body, &stored); err != nil {
		if transient(err) {
			fmt.Fprintf(os.Stderr, "daemon: %s: memory outbox upload deferred: %v\n", t.wi, err)
			return "", true
		}
		moveOutbox(filepath.Join(root, capsule.OutboxPending, f.name), filepath.Join(root, capsule.OutboxRejected))
		return fmt.Sprintf("- rejected %s by aihub: %v", f.name, err), false
	}
	moveOutbox(filepath.Join(root, capsule.OutboxPending, f.name), filepath.Join(root, capsule.OutboxDone))
	tags := ""
	if len(f.mem.Tags) > 0 {
		tags = " [" + strings.Join(f.mem.Tags, ",") + "]"
	}
	return fmt.Sprintf("- %s %s%s %s", stored.ID, f.mem.Type, tags, oneLineTitle(title)), false
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

// transient reports an outage-class failure (transport, 5xx, 408, 429):
// the upload is retried at the next flush.
func transient(err error) bool {
	var ae *api.Error
	if !errors.As(err, &ae) {
		return true
	}
	s := ae.HTTPStatus()
	return s >= 500 || s == 408 || s == 429
}

func sameTitle(files []outboxFile, title string) bool {
	title = strings.TrimSpace(title)
	for _, f := range files {
		if strings.EqualFold(strings.TrimSpace(f.mem.Title), title) {
			return true
		}
	}
	return false
}

func oneLineTitle(s string) string { return strings.Join(strings.Fields(s), " ") }

// outboxNames lists the *.md files directly in dir, sorted.
func outboxNames(dir string) []string {
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

// outboxRead parses every file of one outbox subdirectory (unparsable
// ones skipped).
func outboxRead(root, sub string) []outboxFile {
	dir := filepath.Join(root, sub)
	var out []outboxFile
	for _, name := range outboxNames(dir) {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if m, err := capsule.ParseOutboxMemory(raw); err == nil {
			out = append(out, outboxFile{name, m})
		}
	}
	return out
}

// moveOutbox moves the file src into dstDir (created on demand).
func moveOutbox(src, dstDir string) {
	err := os.MkdirAll(dstDir, 0o755)
	if err == nil {
		err = os.Rename(src, filepath.Join(dstDir, filepath.Base(src)))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: memory outbox: move %s: %v\n", filepath.Base(src), err)
	}
}

// appendMemoryLog appends lines to the capsule's memory log.
func appendMemoryLog(t outboxTarget, lines []string) {
	if len(lines) == 0 {
		return
	}
	path := filepath.Join(t.capsuleDir, filepath.FromSlash(capsule.MemoryLogMD))
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %s: memory log: %v\n", t.wi, err)
		return
	}
	defer f.Close()
	stamp := time.Now().UTC().Format(time.RFC3339)
	for _, l := range lines {
		fmt.Fprintf(f, "%s (%s)\n", l, stamp)
	}
}
