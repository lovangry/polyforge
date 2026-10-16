package capsule

// memoryoutbox.go: the step agent's memory outbox. A step agent holds no
// aihub credential path: `pf remember` inside a step writes one file
// here, and the daemon validates and uploads it with its own credential
// after the step (daemon/memoutbox.go), recording every outcome in
// MemoryLogMD.
//
//	artifacts/memory-outbox/<name>.md           new, not yet judged
//	artifacts/memory-outbox/pending/<name>.md   accepted, upload not yet done
//	artifacts/memory-outbox/done/<name>.md      uploaded, or a duplicate
//	artifacts/memory-outbox/rejected/<name>.md  refused (the log says why)
//
// A file is YAML frontmatter (type, title, tags) plus the body:
//
//	---
//	type: experience.debug
//	title: sqlite busy under WAL
//	tags: [policy-proposal]
//	---
//	<body>

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Memory outbox layout (capsule-relative).
const (
	DirMemoryOutbox = "artifacts/memory-outbox"
	MemoryLogMD     = "artifacts/memory-log.md"
	// OutboxPending, OutboxDone and OutboxRejected are the subdirectories
	// of DirMemoryOutbox a judged file moves into.
	OutboxPending  = "pending"
	OutboxDone     = "done"
	OutboxRejected = "rejected"
)

// OutboxMemory is one outbox file.
type OutboxMemory struct {
	Type  string   `yaml:"type"`
	Title string   `yaml:"title"`
	Tags  []string `yaml:"tags,omitempty"`
	Body  string   `yaml:"-"`
}

// Marshal renders the outbox file.
func (m OutboxMemory) Marshal() ([]byte, error) {
	front, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(front)
	b.WriteString("---\n")
	b.WriteString(m.Body)
	if !strings.HasSuffix(m.Body, "\n") {
		b.WriteString("\n")
	}
	return b.Bytes(), nil
}

// ParseOutboxMemory reads one outbox file (frontmatter + body). Unknown
// frontmatter keys are refused.
func ParseOutboxMemory(raw []byte) (OutboxMemory, error) {
	var m OutboxMemory
	s := string(raw)
	if !strings.HasPrefix(s, "---\n") {
		return m, fmt.Errorf("no frontmatter (the file must start with a `---` line)")
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		return m, fmt.Errorf("unterminated frontmatter (no closing `---` line)")
	}
	dec := yaml.NewDecoder(strings.NewReader(s[4 : 4+end+1]))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("frontmatter: %s", strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	m.Body = s[4+end+5:]
	return m, nil
}
