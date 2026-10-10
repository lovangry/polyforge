package watch

// transcript.go: the SHARED transcript location resolution (spec: "it is
// resolved by a shared function reusing the existing
// runtime.LocateLiveTranscript(sessionsRoot, capsuleDir, notBefore) /
// sessions-root resolution, not a duplicate implementation"). The
// sessions root comes from the config's harness_sessions_root override,
// else the harness default (runtime.HarnessSessionsRootDefault); the
// capsule directory is <workspace>/capsules/<wi> per the mapping plan —
// the caller resolves it, the watch module consumes it.

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// LocateTranscript resolves the WI's live transcript through the shared
// locator. The zero notBefore means no derivation-age floor: the probe
// narrates the newest transcript of the capsule, whatever derivation
// wrote it (a watch lane outlives derivations and may re-arm across
// them). When the harness session store has none, the capsule's own
// synced session transcripts are the fallback — the daemon copies the
// live transcript onto the capsule's declared session path at every
// commit boundary. "" when neither source has one (the caller renders
// the snapshot without transcript facts; the wake channel must never
// depend on the transcript being found).
func LocateTranscript(sessionsRoot, capsuleDir string) string {
	if p := runtime.LocateLiveTranscript(sessionsRoot, capsuleDir, time.Time{}); p != "" {
		return p
	}
	return newestCapsuleTranscript(capsuleDir)
}

// newestCapsuleTranscript answers the newest .jsonl under the capsule's
// session/ directory (the daemon's synced copies), "" when none.
func newestCapsuleTranscript(capsuleDir string) string {
	dir := filepath.Join(capsuleDir, "session")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var (
		best    string
		bestMod time.Time
	)
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if best == "" || info.ModTime().After(bestMod) {
			best = filepath.Join(dir, e.Name())
			bestMod = info.ModTime()
		}
	}
	return best
}
