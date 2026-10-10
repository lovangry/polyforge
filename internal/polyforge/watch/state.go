package watch

// state.go: the delta state the PROBE owns (spec: the script stays
// stateless; the probe owns delta state). One file per watched work item
// at <workspace>/.polyforge/watchers/<wi>.state, next to the log the
// script appends to. The probe reads it before writing it — a crash,
// a killed watcher or a session re-arm re-derives growth from the file
// alone; no script-passed --since-size-style flag exists, by construction.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is one persisted delta-state document: which transcript the
// probe read and the byte size it summarized through.
type State struct {
	TranscriptPath string `json:"transcript_path"`
	Size           int64  `json:"size"`
	UpdatedAt      string `json:"updated_at"`
}

// StatePath is the probe's delta-state file for wi under the machine
// workspace: <workspace>/.polyforge/watchers/<wi>.state.
func StatePath(workspace, wi string) string {
	return filepath.Join(workspace, ".polyforge", "watchers", wi+".state")
}

// LoadState reads the persisted delta state. A missing file is the fresh
// arm (zero State, no error); a corrupt file is loud — silent
// misconfiguration is worse than a wrong delta.
func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("watch: read delta state %s: %w", path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("watch: delta state %s is corrupt: %v — delete it to re-arm fresh", path, err)
	}
	return st, nil
}

// SaveState persists the delta state atomically (tmp+rename, 0644),
// creating the watchers directory on demand. The probe — the session
// side — is the only writer.
func SaveState(path string, st State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("watch: create watchers dir: %w", err)
	}
	blob, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return fmt.Errorf("watch: write delta state %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("watch: rename delta state %s: %w", path, err)
	}
	return nil
}

// Now stamps a State's UpdatedAt (seam for tests).
func Now() string { return time.Now().UTC().Format(time.RFC3339) }
