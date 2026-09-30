package v0export

// delta.go: the top-up (delta) mode. A full export emits every record; a
// delta export emits only records that are NEW or CHANGED since the last
// successful seeding run, judged by the sha256 of each emitted JSONL
// line — no updated_at watermark (orphan authority files have no row;
// the row's content_sha can diverge from the live file the exporter
// already reads; the emitted line hash catches file drift, orphan stock
// and row changes uniformly, all under the read-only source constraint).
//
// The baseline is the seed-v1 state file the last successful run
// promoted (see tools/seed-v1.sh): per record id, the sha256 of the
// emitted line. The exporter never writes the state file itself — it
// writes the candidate FULL hash set (candidate-state.json) into the
// export dir, and the script promotes it to the state file only after
// all four steps pass, so a failed delta run keeps its changes visible
// to the next one.
//
// Capsule snapshot export is skipped in delta mode (reported as
// skipped; nothing in the seed pipeline consumes the snapshot tree).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// hashStateVersion is the state-file format version.
const hashStateVersion = 1

// CandidateStateFile is the candidate full hash set the exporter writes
// into the export dir (promoted to the state file by seed-v1.sh on
// success, in both full and delta mode).
const CandidateStateFile = "candidate-state.json"

// HashState is the persistent seed-v1 state file shape: per record id,
// the sha256 hex of the emitted JSONL line from the last successful run.
type HashState struct {
	Version   int               `json:"version"`
	WorkItems map[string]string `json:"work_items"`
	Memories  map[string]string `json:"memories"`
}

// DeltaCounts are the "since last run" counts in export-report.json.
type DeltaCounts struct {
	NewWorkItems     int  `json:"new_work_items"`
	ChangedWorkItems int  `json:"changed_work_items"`
	NewMemories      int  `json:"new_memories"`
	ChangedMemories  int  `json:"changed_memories"`
	CapsulesSkipped  bool `json:"capsules_skipped"`
}

// loadHashState reads the baseline state file (delta mode input).
func loadHashState(path string) (*HashState, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("baseline state file not found at %s: %v (run a full seed first — a delta run never silently degrades to a full export)", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st HashState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("baseline state file %s unparsable: %v", path, err)
	}
	if st.WorkItems == nil {
		st.WorkItems = map[string]string{}
	}
	if st.Memories == nil {
		st.Memories = map[string]string{}
	}
	return &st, nil
}

// writeHashState writes a hash state file (the export-dir candidate).
func writeHashState(path string, st *HashState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o640)
}

// lineHash is the per-record change-detection hash: the sha256 of the
// exact emitted JSONL line bytes (no trailing newline). The struct
// marshal is deterministic, so line bytes are a stable hash input.
func lineHash(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

// emittedLine is one record's emitted JSONL line plus its identity and
// content hash (the delta filter and the candidate set both key on it).
type emittedLine struct {
	id   string
	hash string
	line string
}

// deltaFilter keeps the new (id absent from baseline) and changed (hash
// differs) records, counting both. Unchanged records drop out of the
// emitted delta files but stay in the candidate set.
func deltaFilter(lines []emittedLine, base map[string]string, newN, changedN *int) []emittedLine {
	var keep []emittedLine
	for _, l := range lines {
		old, ok := base[l.id]
		if !ok {
			*newN++
			keep = append(keep, l)
			continue
		}
		if old != l.hash {
			*changedN++
			keep = append(keep, l)
		}
	}
	return keep
}

// linesOf strips the emittedLine wrapper for writing.
func linesOf(lines []emittedLine) []string {
	out := make([]string, len(lines))
	for i := range lines {
		out[i] = lines[i].line
	}
	return out
}
