package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/silgrid/polyforge/internal/common/api"
)

// readRandom is the single crypto-random entry point (kept local so store.go
// stays import-light).
func readRandom(b []byte) (int, error) { return rand.Read(b) }

// sha256Hex is the storage digest for every secret (api keys, push
// credentials, project identifiers): only hashes are persisted, plaintext
// is handed out exactly once.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Sha256Hex is the exported form for the HTTP auth middleware.
func Sha256Hex(s string) string { return sha256Hex(s) }

// Fail builds the typed wire error. Reason is the machine-readable
// sub-reason carried in Extra (ALREADY_CLAIMED etc.); Detail is the human
// sentence.
func Fail(code, reason, format string, args ...any) *api.Error {
	detail := fmt.Sprintf(format, args...)
	e := &api.Error{Code: code, Detail: detail}
	if reason != "" {
		raw, _ := json.Marshal(map[string]string{"reason": reason})
		e.Extra = raw
	}
	return e
}

// IsCode reports whether err is a typed *api.Error with the given code.
func IsCode(err error, code string) bool {
	ae, ok := err.(*api.Error)
	return ok && ae.Code == code
}

// ErrorReason extracts the machine-readable sub-reason from a typed error.
func ErrorReason(err error) string {
	ae, ok := err.(*api.Error)
	if !ok || len(ae.Extra) == 0 {
		return ""
	}
	var m map[string]string
	if json.Unmarshal(ae.Extra, &m) != nil {
		return ""
	}
	return m["reason"]
}
