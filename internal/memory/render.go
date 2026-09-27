package memory

// Markdown→HTML derivation (06 §4.3).
//
// Renderer ruling (recorded): 06 §4.3 pins goldmark (CommonMark 0.31 + GFM
// extensions, unsafe=false, deterministic output). goldmark was NOT yet a
// dependency of this module, but the module proxy is reachable, so the
// mandated renderer is used directly rather than falling back to a
// stdlib-only approximation of its rule set.
//
// Rule set (06 §4.3): GFM tables, task lists, strikethrough and autolink
// enabled; raw HTML and any script injection DISABLED (unsafe=false —
// everything the source could carry as markup is escaped by the renderer).
// Output is deterministic: goldmark emits no timestamps and no random ids
// for equal input bytes.

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

var mdRenderer = goldmark.New(
	goldmark.WithExtensions(
		extension.Table,
		extension.TaskList,
		extension.Strikethrough,
		extension.Linkify,
	),
	// The renderer defaults to the SAFE mode: raw HTML in the source is
	// escaped, not passed through (06 §4.3/§7: unsafe=false — no
	// script-injection surface in the derived artifact). goldmark's
	// html.WithUnsafe() would OPT IN to raw HTML, so it is simply not set.
)

// renderMarkdown converts a Markdown body to an HTML fragment.
//
// Inference (recorded): the derived memory.html holds the rendered BODY of
// the authority document — the YAML frontmatter is metadata (06 §4.2), not
// content, and is not part of the rendered view. goldmark renders a body
// fragment; the consuming UI wraps it in its own document shell.
func renderMarkdown(body string) ([]byte, error) {
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(body), &buf); err != nil {
		return nil, errf("MEM-501", "DERIVE_FAILED", "%v", err)
	}
	return buf.Bytes(), nil
}
