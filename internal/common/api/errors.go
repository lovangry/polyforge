package api

import (
	"encoding/json"
	"strconv"
)

// Error is the typed wire error shared by aihub and pf (02 §6.7 errStatus).
// HTTP status is derived from the numeric suffix of the code.
type Error struct {
	Code   string          `json:"code"`
	Detail string          `json:"detail,omitempty"`
	Extra  json.RawMessage `json:"extra,omitempty"`
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Detail
	}
	return e.Code + ": " + e.Detail
}

// HTTPStatus maps an error code (e.g. "CORE-409") to its HTTP status by
// parsing the numeric suffix. Unknown shapes map to 500.
func (e *Error) HTTPStatus() int {
	if len(e.Code) < 4 || e.Code[len(e.Code)-4] != '-' {
		return 500
	}
	n, err := strconv.Atoi(e.Code[len(e.Code)-3:])
	if err != nil || n < 400 || n > 599 {
		return 500
	}
	return n
}

// Error code registry. v1 surface: CORE/MEM/AUTH/GIT/DAEMON/CAP.
// SYNC-* codes are deleted (git replaces the revision chain).
const (
	// CORE — work item / project / user business errors (02 §6)
	ErrCoreBadInput     = "CORE-400"
	ErrCoreUnauthorized = "CORE-401"
	ErrCoreForbidden    = "CORE-403"
	ErrCoreNotFound     = "CORE-404"
	ErrCoreConflict     = "CORE-409"
	ErrCoreGone         = "CORE-410"
	ErrCoreTooLarge     = "CORE-413"
	ErrCoreRateLimit    = "CORE-425"
	ErrCoreInternal     = "CORE-500"
	ErrCoreUnavailable  = "CORE-503"
	// CORE-504 is the §5.2 read-through miss: progress unobtainable
	// (no readable object at the WI's published HEAD).
	ErrCoreProgressUnavailable = "CORE-504"
	ErrCoreLockHeld            = "CORE-508"
	ErrCoreStaleService        = "CORE-509"

	// MEM — memory errors (06 §5)
	ErrMemBadInput       = "MEM-400"
	ErrMemForbidden      = "MEM-403"
	ErrMemNotFound       = "MEM-404"
	ErrMemConflict       = "MEM-409"
	ErrMemGone           = "MEM-410"
	ErrMemTooLarge       = "MEM-413"
	ErrMemInternal       = "MEM-500"
	ErrMemNotImplemented = "MEM-501"
	ErrMemUpstream       = "MEM-502"
	ErrMemUnavailable    = "MEM-503"

	// AUTH — authentication/authorization at the HTTP boundary
	ErrAuthUnauthorized = "AUTH-401"
	ErrAuthForbidden    = "AUTH-403"

	// GIT — v1 git proxy surface (pre-receive rejects, auth, protocol)
	ErrGitUnauthorized = "GIT-401" // attempt-scoped credential rejected (takeover fencing)
	ErrGitForbidden    = "GIT-403"
	ErrGitNotFound     = "GIT-404"
	ErrGitNonFF        = "GIT-409" // non-fast-forward push
	ErrGitRefDeleted   = "GIT-410" // ref deletion attempt
	ErrGitGitlink      = "GIT-412" // gitlink (mode 160000) in pushed tree
	ErrGitTooLarge     = "GIT-413" // blob size limit
	ErrGitSeqViolation = "GIT-422" // seq trailer not monotonically increasing
	ErrGitInternal     = "GIT-500"

	// DAEMON — pf daemon UDS surface (04 §5)
	ErrDaemonBadInput            = "DAEMON-400"
	ErrDaemonForbidden           = "DAEMON-403"
	ErrDaemonNotFound            = "DAEMON-404"
	ErrDaemonConflict            = "DAEMON-409"
	ErrDaemonGone                = "DAEMON-410"
	ErrDaemonShuttingDown        = "DAEMON-503"
	ErrDaemonSpawnFail           = "DAEMON-507"
	ErrDaemonWatchdog            = "DAEMON-511"
	ErrDaemonShipPreflight       = "DAEMON-512"
	ErrDaemonSessionJSONLMissing = "DAEMON-513"
	ErrDaemonInternal            = "DAEMON-500"

	// CAP — capsule lifecycle errors (03 §5.3)
	ErrCapBadInput      = "CAP-400"
	ErrCapNotFound      = "CAP-404"
	ErrCapCheckoutDiver = "CAP-405"
	ErrCapSymlinkEscape = "CAP-406"
	ErrCapFenced        = "CAP-409"
	ErrCapRunnerCrash   = "CAP-500"
)
