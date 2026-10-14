// Package spawn carries the process-derivation contract shared by the
// runtime (daemon assembly) and the driver (step-process spawning).
package spawn

import (
	"context"
	"time"
)

// Params describes one agent derivation.
type Params struct {
	WorkItemID     string
	CapsuleDir     string
	SessionHandle  string // PUBLIC fingerprint (B3), never the secret
	SpawnN         int
	TranscriptPath string
	Resume         bool
	InitialPrompt  string
	Model          string
	Journal        func(pid int, startedAt string) error
	// OnRecord, when set, receives every raw stdout record line the
	// process emits (the harness translator's input). It runs on the
	// stdout reader: it must be cheap and must never block.
	OnRecord func(line string)
	// OnExit, when set, is called exactly once when the process is gone
	// (after the stdout reader drained), BEFORE Done() turns true.
	OnExit func(ExitInfo)
}

// ExitInfo describes how a derived process ended.
type ExitInfo struct {
	// Code is the exit status (-1 when the process was killed by a
	// signal).
	Code int
	// Signal names the terminating signal, "" when it exited normally.
	Signal string
	// StderrTail is the tail of the process's stderr (bounded).
	StderrTail string
}

// ExitReporter is the optional face of a Handle that can describe its
// process's exit once Done() is true.
type ExitReporter interface {
	ExitInfo() ExitInfo
}

// Handle is the supervised face over one derived agent process.
type Handle interface {
	PID() int
	StartedAt() string
	Alive() bool
	Done() bool
	Terminate(bound time.Duration) error
}

// Spawner derives agent processes.
type Spawner interface {
	Spawn(ctx context.Context, p Params) (Handle, error)
}
