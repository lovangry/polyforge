package cli

// util.go: small shared helpers. The former trivial wrappers
// (pathEscape/joinComma/itoa/itoa64) were deleted: callers now use
// url.PathEscape / strings.Join / strconv directly.

// strPtr returns a pointer to v.
func strPtr(v string) *string { return &v }
