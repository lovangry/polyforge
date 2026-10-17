// Package stepmem validates the one memory a workflow step writes — its
// work item's experience.wrap summary — for the CLI and the daemon alike.
package stepmem

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	WrapType   = "experience.wrap" // the only type a step writes
	BodyMax    = 2048              // bytes
	TitleRunes = 200
	MaxTags    = 8
)

var tagRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Check answers why a step memory is refused ("" = fine).
func Check(memType, title string, tags []string, body string) string {
	title = strings.TrimSpace(title)
	switch {
	case memType != WrapType:
		return fmt.Sprintf("a workflow step writes only its %s summary (got type %q)", WrapType, memType)
	case title == "":
		return "a title is required"
	case strings.ContainsAny(title, "\r\n"):
		return "the title must be one line"
	case utf8.RuneCountInString(title) > TitleRunes:
		return fmt.Sprintf("the title exceeds %d characters", TitleRunes)
	case strings.TrimSpace(body) == "":
		return "the body is empty"
	case len(body) > BodyMax:
		return fmt.Sprintf("the body is %d bytes (at most %d)", len(body), BodyMax)
	case len(tags) > MaxTags:
		return fmt.Sprintf("more than %d tags", MaxTags)
	}
	for _, t := range tags {
		if !tagRe.MatchString(t) {
			return fmt.Sprintf("tag %q is not a plain name", t)
		}
	}
	return SecretValueDefect(title + "\n" + body)
}

// IdempotencyKey keys a step dispatch's memory at aihub: a retry after a
// lost answer returns the stored memory.
func IdempotencyKey(wi, step string, attempt int) string {
	return fmt.Sprintf("pf-step-memory:%s:%s:%d", wi, step, attempt)
}

// secretValueRes match credential VALUES, never key names (`the key lives
// in cli.toml`, `api_key=${PF_API_KEY}` pass).
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

var longToken = regexp.MustCompile(`[A-Za-z0-9_+=-]{32,}`)

// SecretValueDefect answers why text looks like a credential value ("" =
// it does not): a known shape, a PEM key, or a 32+ run mixing upper,
// lower and digits (a git sha or uuid does not).
func SecretValueDefect(text string) string {
	for _, re := range secretValueRes {
		if re.MatchString(text) {
			return "the memory looks like it carries a credential value — name where the secret lives, never the value"
		}
	}
	for _, tok := range longToken.FindAllString(text, -1) {
		var up, low, dig bool
		for _, r := range tok {
			up = up || (r >= 'A' && r <= 'Z')
			low = low || (r >= 'a' && r <= 'z')
			dig = dig || (r >= '0' && r <= '9')
		}
		if up && low && dig {
			return "the memory carries a long high-entropy token — name where the secret lives, never the value"
		}
	}
	return ""
}
