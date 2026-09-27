package cli

import (
	"net/url"
	"strconv"
	"strings"
)

// pathEscape escapes one path segment.
func pathEscape(s string) string {
	return url.PathEscape(s)
}

// joinComma joins for a comma-separated multi-value query parameter.
func joinComma(parts []string) string {
	return strings.Join(parts, ",")
}

// itoa formats an int.
func itoa(i int) string {
	return strconv.Itoa(i)
}

// itoa64 formats an int64.
func itoa64(i int64) string {
	return strconv.FormatInt(i, 10)
}

// strPtr returns a pointer to v.
func strPtr(v string) *string { return &v }
