package runtime

// procargs.go: the kern.procargs2 parser (polyforge#218). Only
// procinfo_darwin.go feeds it macos blobs; this file carries no build
// tag so its unit tests run on Linux with synthetic blobs.
//
// kern.procargs2 layout (XNU, kern_proc.c): the first four bytes are
// the argument count (little-endian; darwin is little-endian), then
// one string per buffer region — the exec path first, then the argv
// strings (pad NULs may follow the exec path), then the environment
// strings NUL-terminated up to the buffer's end (trailing pad NULs).

import (
	"encoding/binary"
	"strings"
)

// procargsEnviron parses one kern.procargs2 blob into its environment
// strings (KEY=VALUE form, never empty). Unparsable blobs answer what
// they parse up to that point; the exact-token marker matching in the
// callers then simply finds nothing (fail-closed on the guard's side).
func procargsEnviron(raw []byte) []string {
	if len(raw) < 4 {
		return nil
	}
	argc := binary.LittleEndian.Uint32(raw)
	lines := strings.Split(string(raw[4:]), "\x00")
	// lines[0] is the exec path; skip the pad NULs that may follow it
	lines = lines[1:]
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	// the next argc strings are argv
	if c := min(int(argc), len(lines)); c > 0 {
		lines = lines[c:]
	}
	// the environment: every string up to the first empty one (the
	// buffer's trailing pad; env strings are never empty)
	env := make([]string, 0, len(lines))
	for _, l := range lines {
		if l == "" {
			break
		}
		env = append(env, l)
	}
	return env
}
