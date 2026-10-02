package runtime

// pitranscript.go: the pi-harness live-transcript face (F3 of the pilot
// fix lane). The real harness ignores the PI_SESSION_DIR env the v0
// supervisor trusted (the variable the harness actually honors is its
// own <APP>_CODING_AGENT_SESSION_DIR; v0's port blind-carried the dead
// name), so a fresh derivation's transcript lands at the harness's
// DEFAULT session store —
//
//	~/.pi/agent/sessions/<dashified-capsule-dir>--/<ts>-<uuid>.jsonl
//
// — and nothing ever reaches the capsule's declared session/ path. The
// fix follows the v0 late conclusion: the DAEMON owns the sync (locate
// after spawn, copy into the capsule at every commit boundary, before
// the commit so the transcript rides git history — ruling C1), and the
// recovery engine resumes from the capsule copy with the live file as
// the fallback. The spawn-side env additions are deliberately untouched
// (the isolation flags and the sanitize point are pinned by v0's
// measured baseline); this file is the daemon-side compensation.
//
// The session-directory naming rule is the harness's own, mirrored here
// verbatim from its source: the resolved cwd is stripped of its one
// leading slash, every "/ \ :" becomes "-", and the result is wrapped in
// "--" ... "--". A capsule at /root/ws/capsules/wi_x therefore writes
// under "--root-ws-capsules-wi_x--". Path characters the rule does NOT
// fold (e.g. "#") survive into the directory name — one more reason the
// capsule directory itself must carry the canonical work-item id (F2).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// locateSlack is how far before the spawn moment a live transcript's
// mtime may sit: mtime is only file-resolution precise and the file is
// created moments after process birth. The slack stays smaller than any
// honest predecessor's last write (the old tree is fully collected
// before a re-derivation begins), so it can never re-admit a dead
// instance's transcript.
const locateSlack = 2 * time.Second

// Transcript FILE naming — deliberately NOT renamed with the v1 fossil
// cleanup: the capsule-side transcript copies are named
// session-<fingerprint>#N.jsonl, where the "#N" suffix is spawn_n's
// file-level expression — it carries the "which derivation wrote this"
// semantics (a re-derivation gets its own #-suffixed file and never
// clobbers its predecessor's; see sessionPathFor and the spawn token in
// recover.go). LocateLiveTranscript relies on that shape when it scans
// the harness session store for a live transcript. Renaming the pattern
// would orphan every existing sealed capsule's transcript files (the
// resume path re-reads the declared path from the capsule), so the
// naming stays frozen even though the spawn-journal line's display-key
// fossil (instance → spawn_token) is cleaned up.

// HarnessSessionsRootDefault is the harness's default session store.
// A Config override exists for tests and relocated installs.
func HarnessSessionsRootDefault() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".pi", "agent", "sessions")
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// piSessionDirName derives the harness's per-cwd session directory name
// (the dashify rule above). capsuleDir is resolved through symlinks
// first: the harness derives the name from the process's real cwd.
func piSessionDirName(capsuleDir string) string {
	real := capsuleDir
	if resolved, err := filepath.EvalSymlinks(capsuleDir); err == nil {
		real = resolved
	}
	trimmed := real
	// strip exactly one leading separator (the rule's ^[/\\] anchor)
	trimmed = strings.TrimPrefix(trimmed, "/")
	if len(trimmed) > 0 && trimmed[0] == '\\' {
		trimmed = trimmed[1:]
	}
	dashed := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, trimmed)
	return "--" + dashed + "--"
}

// LocateLiveTranscript scans the harness session store for the live
// transcript of a derivation whose process was born at notBefore: the
// newest .jsonl under the capsule's dashified session directory whose
// mtime is at/after the spawn moment (minus the locate slack). "" when
// the directory, or any qualifying file, does not exist — the callers
// treat that as the typed transcript_missing condition, never an error.
func LocateLiveTranscript(sessionsRoot, capsuleDir string, notBefore time.Time) string {
	if sessionsRoot == "" {
		sessionsRoot = HarnessSessionsRootDefault()
	}
	dir := filepath.Join(sessionsRoot, piSessionDirName(capsuleDir))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	floor := notBefore.Add(-locateSlack)
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
		if info.ModTime().Before(floor) {
			continue // an earlier derivation's file (or unrelated residue)
		}
		if best == "" || info.ModTime().After(bestMod) {
			best = filepath.Join(dir, e.Name())
			bestMod = info.ModTime()
		}
	}
	return best
}

// copyTranscriptReadonly copies the live transcript onto the capsule's
// declared session path. "Read-only" is about the SOURCE: it is opened
// for reading only, never truncated, never renamed. The destination is
// written atomically (tmp+rename) with 0644 — deliberately writable,
// because the resume path hands the very same file back to the harness
// as its --session continuation target, and the harness must be able to
// append to it.
func copyTranscriptReadonly(src, dst string) error {
	if src == "" || dst == "" {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return atomicWrite(dst, data, 0o644)
}

// syncTranscriptBeforeSeal is the RUNTIME half of the F3 transcript
// sync: the two commit points the observe lane cannot see — the seal
// push of a CLI-driven Stop (pause/wrap) and the commit-dirty-on-recover
// — copy the dead derivation's live transcript into the capsule before
// their commit, so the transcript rides the chain (ruling C1) even when
// the last step boundary is long past. Best-effort and quiet: the commit
// proceeds without the transcript when the source cannot be read.
func (r *Runtime) syncTranscriptBeforeSeal(j *job) {
	j.mu.Lock()
	src := j.liveTranscript
	startedAt := j.agentStartedAt
	dst := j.transcript
	capsuleDir := j.capsuleDir
	j.mu.Unlock()
	if dst == "" {
		return
	}
	if src == "" {
		if spawnAt, err := time.Parse(time.RFC3339Nano, startedAt); err == nil {
			src = r.locateLive(capsuleDir, spawnAt)
		}
	}
	if src == "" || src == dst {
		return
	}
	if err := copyTranscriptReadonly(src, dst); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: %s: seal transcript sync skipped: %v\n", j.wi, err)
	}
}
