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
//	step: fix          # the step and dispatch (spawn_n) that wrote it
//	attempt: 3
//	---
//	<body>

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"

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
	// OutboxFileMax bounds an outbox file read (bytes): a larger one is
	// refused unread.
	OutboxFileMax = 64 << 10
)

// OutboxMemory is one outbox file.
type OutboxMemory struct {
	Type  string   `yaml:"type"`
	Title string   `yaml:"title"`
	Tags  []string `yaml:"tags,omitempty"`
	// Step and Attempt name the step and its dispatch ordinal (spawn_n)
	// that queued the memory: one memory per step attempt.
	Step    string `yaml:"step"`
	Attempt int    `yaml:"attempt"`
	Body    string `yaml:"-"`
}

// secretValueRes match likely credential VALUES (never key names: "the
// key lives in ~/.config/polyforge/cli.toml" or `api_key=${PF_API_KEY}`
// are what the memory policy asks for, and pass).
var secretValueRes = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\b(sk|rk)-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\b(sk|pk|rk)_(live|test)_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`\b(ghp|gho|ghs|ghu|ghr|github_pat)_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
}

// longToken is a run that may be a high-entropy token.
var longToken = regexp.MustCompile(`[A-Za-z0-9_+=-]{32,}`)

// SecretValueDefect answers why text looks like it carries a credential
// value ("" = it does not): a known key/token shape, a PEM private key,
// or a long run mixing upper case, lower case and digits (a git sha or a
// uuid does not mix cases).
func SecretValueDefect(text string) string {
	for _, re := range secretValueRes {
		if re.MatchString(text) {
			return "looks like it carries a credential value (" + re.String() + ") — name where the secret lives, never the value"
		}
	}
	for _, tok := range longToken.FindAllString(text, -1) {
		if mixedCase(tok) {
			return "carries a long high-entropy token — name where the secret lives, never the value"
		}
	}
	return ""
}

func mixedCase(s string) bool {
	var up, low, dig bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			up = true
		case r >= 'a' && r <= 'z':
			low = true
		case r >= '0' && r <= '9':
			dig = true
		}
	}
	return up && low && dig
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

// ReadOutboxFile reads one outbox file safely: opened without following
// a symlink (and without blocking on a fifo), it must be a regular file
// of at most OutboxFileMax bytes — the size is judged on the open file
// and the read is capped, so a swap between check and read gains nothing.
func ReadOutboxFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, OutboxFileMax+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > OutboxFileMax {
		return nil, fmt.Errorf("the file exceeds %d bytes", OutboxFileMax)
	}
	return raw, nil
}
