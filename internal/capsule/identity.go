package capsule

// identity.go: the Capsule instance and its create/reuse/quarantine
// lifecycle (03 §2.1/§2.2/§6.1).
//
// capsule_id = "cap_" + wi_id, one-to-one with the WI for life. The on-disk
// directory is named by wi_id. An instance is (capsule_id, generation,
// machine_id); only one instance may be active at a time (03 §2.4, enforced
// upstream by owner generations + publish fencing).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Identity is the capsule.json self-description (03 §2.2).
type Identity struct {
	CapsuleID    string `json:"capsule_id"`
	WorkItemID   string `json:"work_item_id"`
	Generation   int64  `json:"generation"`
	MachineID    string `json:"machine_id"`
	BundleDigest string `json:"bundle_digest,omitempty"`
	Status       string `json:"status"` // initializing | ready
	CreatedAt    string `json:"created_at"`
}

// Status values (03 §2.2).
const (
	StatusInitializing = "initializing"
	StatusReady        = "ready"
)

// Capsule is a handle on one WI's execution site on this machine.
type Capsule struct {
	Dir        string
	WorkItemID string
	Generation int64
}

// ID returns the capsule id ("cap_" + wi id, 03 §2.1).
func (c *Capsule) ID() string { return "cap_" + c.WorkItemID }

// layout is the authoritative directory set (03 §3.1). The .pi/agents dir is
// created even when empty: pi resolves the nearest .pi/agents from the
// subagent cwd upward, so a missing one leaks the resolution face outside
// the capsule (03 §4.5 anti-conflict layer 3).
func (c *Capsule) layoutDirs() []string {
	return []string{
		"repos",
		"runtime-state",
		"artifacts",
		"logs", "logs/session",
		".pi/agents",
		"harness",
		"sync",
	}
}

// SyncRootRelPaths are the top-level sync roots (05 §4.1): runtime-state
// (execution.json only), artifacts/, logs/ (incl. logs/session/) and
// repos/<name>/ working trees (excluding .git). harness/<adapter>/session.json
// is optional-sync and handled separately; everything else (capsule.json,
// sync/, .pi/agents/, .claude/agents/, workflow.md, harness/<adapter>/)
// is excluded from the sync surface.
const (
	RelExecutionJSON   = "runtime-state/execution.json"
	RelRuntimeStateDir = "runtime-state"
	RelArtifactsDir    = "artifacts"
	RelLogsDir         = "logs"
	RelReposDir        = "repos"
	RelSyncDir         = "sync"
	RelHarnessDir      = "harness"
	RelSessionArchive  = "logs/session"
)

// Path returns the absolute path of a capsule-relative path (slash-separated).
// The path is validated: it must stay inside the capsule (no "..", no
// absolute, no backslash, no NUL). This is the entry-level string check;
// symlink-level checks for recovery writes live in safety.go (03 §6.3).
func (c *Capsule) Path(rel string) (string, error) {
	if err := validateRelPath(rel); err != nil {
		return "", err
	}
	return filepath.Join(c.Dir, filepath.FromSlash(rel)), nil
}

// CapsuleRoot returns the capsules root for a workspace.
func CapsuleRoot(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, "capsules")
}

func capsuleDirFor(workspaceRoot, wiID string) (string, error) {
	if err := validateWiID(wiID); err != nil {
		return "", err
	}
	return filepath.Join(CapsuleRoot(workspaceRoot), wiID), nil
}

func validateWiID(id string) error {
	if id == "" || len(id) > 128 {
		return errf("CAP-400", "invalid work item id")
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return errf("CAP-400", "invalid work item id %q", id)
		}
	}
	return nil
}

func validateRelPath(p string) error {
	if p == "" || p[0] == '/' {
		return errf("CAP-400", "invalid relative path %q", p)
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' || p[i] == 0 {
			return errf("CAP-400", "invalid relative path %q", p)
		}
	}
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if p[start:i] == ".." {
				return errf(ErrSymlinkEscape, "path escape %q", p)
			}
			start = i + 1
		}
	}
	return nil
}

// Open loads the identity of an existing capsule directory. Errors if the
// directory or capsule.json is missing (a caller wanting create semantics
// should use Create).
func Open(workspaceRoot, wiID string) (*Capsule, *Identity, error) {
	dir, err := capsuleDirFor(workspaceRoot, wiID)
	if err != nil {
		return nil, nil, err
	}
	idn, err := readIdentity(dir)
	if err != nil {
		return nil, nil, err
	}
	return &Capsule{Dir: dir, WorkItemID: wiID, Generation: idn.Generation}, idn, nil
}

func readIdentity(dir string) (*Identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "capsule.json"))
	if err != nil {
		return nil, err
	}
	var idn Identity
	if err := json.Unmarshal(raw, &idn); err != nil {
		return nil, fmt.Errorf("capsule.json unparsable: %w", err)
	}
	return &idn, nil
}

// CreateResult reports what a Create call did.
type CreateResult struct {
	Reused      bool // existing ready capsule for the same generation was reused
	Quarantined string
}

// Create drives the 03 §6.1 create/reuse/quarantine sequence for a freshly
// claimed generation:
//
//  1. dir = <workspace>/capsules/<wi_id>
//  2. if capsule.json exists:
//     a. same generation + ready  → REUSE (idempotent retry) — done.
//     b. same generation + initializing → quarantine (half-initialized husk).
//     c. recorded generation > requested → CAP-409 CAPSULE_FENCED.
//     d. recorded generation < requested → quarantine (takeover norm).
//  3. quarantine: move the old dir under capsules/_quarantine/<wi>/<gen>-<status>-<ts>/.
//  4. create the layout directories + capsule.json status=initializing.
//
// Steps 5–6 of 03 §6.1 (inventory checkout, remote recovery, harness/pi
// agent dirs) are orchestrated by the caller between Create and MarkReady so
// each stage is independently injectable; MarkReady flips status to ready and
// only a ready capsule is ever reused.
func Create(workspaceRoot, wiID string, gen int64, machineID, bundleDigest string) (*Capsule, *CreateResult, error) {
	dir, err := capsuleDirFor(workspaceRoot, wiID)
	if err != nil {
		return nil, nil, err
	}
	c := &Capsule{Dir: dir, WorkItemID: wiID, Generation: gen}
	res := &CreateResult{}

	if idn, rerr := readIdentity(dir); rerr == nil {
		switch {
		case idn.Generation == gen && idn.Status == StatusReady:
			res.Reused = true
			return c, res, nil
		case idn.Generation == gen && idn.Status == StatusInitializing:
			// fall through to quarantine + rebuild
		case idn.Generation > gen:
			return nil, nil, errf(ErrCapsuleFenced, "local capsule generation %d > requested %d", idn.Generation, gen)
		default:
			// idn.Generation < gen: takeover — quarantine below.
		}
		q, qerr := c.quarantine(idn.Generation, idn.Status)
		if qerr != nil {
			return nil, nil, qerr
		}
		res.Quarantined = q
	} else if !os.IsNotExist(rerr) && !os.IsNotExist(osErrCause(rerr)) {
		// A readable-but-broken capsule.json is treated as a husk too.
		q, qerr := c.quarantine(0, StatusInitializing)
		if qerr != nil {
			return nil, nil, qerr
		}
		res.Quarantined = q
	}

	for _, d := range c.layoutDirs() {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o755); err != nil {
			return nil, nil, err
		}
	}
	idn := Identity{
		CapsuleID:  c.ID(),
		WorkItemID: wiID,
		Generation: gen,
		MachineID:  machineID,
		Status:     StatusInitializing,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	if bundleDigest != "" {
		idn.BundleDigest = bundleDigest
	}
	blob, _ := json.MarshalIndent(idn, "", "  ")
	if err := AtomicWrite(filepath.Join(dir, "capsule.json"), blob, 0o644); err != nil {
		return nil, nil, err
	}
	return c, res, nil
}

// osErrCause unwraps *fs.PathError to test for the underlying ENOENT.
func osErrCause(err error) error {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err
	}
	return err
}

// quarantine moves the whole capsule dir to
// <root>/capsules/_quarantine/<wi_id>/<gen>-<status>-<ts>/ (03 §2.4/§6.1):
// never reuse in place, never resume in place.
func (c *Capsule) quarantine(oldGen int64, oldStatus string) (string, error) {
	qdir := filepath.Join(CapsuleRoot(filepath.Dir(c.Dir)), "_quarantine", c.WorkItemID)
	name := fmt.Sprintf("%d-%s-%d", oldGen, oldStatus, time.Now().UnixNano())
	dst := filepath.Join(qdir, name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(c.Dir, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// MarkReady atomically flips capsule.json.status to ready (03 §6.1 step 6).
// Between Create and MarkReady any crash leaves an `initializing` husk that
// the next Create call quarantines instead of reusing.
func (c *Capsule) MarkReady() error {
	idn, err := readIdentity(c.Dir)
	if err != nil {
		return err
	}
	idn.Status = StatusReady
	blob, _ := json.MarshalIndent(idn, "", "  ")
	return AtomicWrite(filepath.Join(c.Dir, "capsule.json"), blob, 0o644)
}

// SealReadOnly seals the capsule as a read-only scene after a normal job
// end (03 §2.1: the directory is kept, never disk-deleted here; deletion
// goes through the Core deletion-protection flow only).
func (c *Capsule) SealReadOnly() error {
	return os.Chmod(c.Dir, 0o555)
}

// ValidateGeneration checks a caller-presented generation against the local
// capsule.json (the CAP-409 second line of defense, 03 §5.3).
func (c *Capsule) ValidateGeneration(gen int64) error {
	idn, err := readIdentity(c.Dir)
	if err != nil {
		return err
	}
	if idn.Generation != gen {
		return errf(ErrCapsuleFenced, "capsule generation %d ≠ requested %d", idn.Generation, gen)
	}
	return nil
}
