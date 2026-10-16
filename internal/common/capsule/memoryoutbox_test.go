package capsule

import (
	"reflect"
	"strings"
	"testing"
)

func TestOutboxMemoryRoundTrip(t *testing.T) {
	m := OutboxMemory{Type: "experience.debug", Title: "a: b — 中文", Tags: []string{"policy-proposal"}, Body: "line 1\n---\nline 3\n"}
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseOutboxMemory(raw)
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip = %+v, %v\n%s", got, err, raw)
	}
	for raw, want := range map[string]string{
		"no fence\n":                     "no frontmatter",
		"---\ntype: x\n":                 "unterminated",
		"---\ntype: x\nbogus: 1\n---\nb": "frontmatter",
	} {
		if _, err := ParseOutboxMemory([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: err = %v, want %q", raw, err, want)
		}
	}
}
