// Package spawn carries the process-derivation contract shared by the
// runtime (daemon assembly) and the driver (step-process spawning).
package spawn

import (
	"context"
	"time"

	"github.com/silgrid/polyforge/internal/common/protocol"
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
	CgroupPath     string
	Model          string
	Journal        func(pid int, startedAt string) error
}

// Handle is the supervised face over one derived agent process.
type Handle interface {
	PID() int
	StartedAt() string
	Alive() bool
	Done() bool
	Terminate(bound time.Duration) error
	SendControl(m protocol.ControlMsg) error
	WaitControlAck(ctx context.Context, m protocol.ControlMsg, bound time.Duration) error
}

// Spawner derives agent processes.
type Spawner interface {
	Spawn(ctx context.Context, p Params) (Handle, error)
}
