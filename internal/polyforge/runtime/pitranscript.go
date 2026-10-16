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
// fix: the observe monitor owns the sync (locate after spawn, copy into
// the capsule at every commit boundary of the dispatch, before the
// commit so the transcript rides git history — ruling C1), per dispatch
// onto the path its step_started fact names. This file only holds the
// locator the monitor calls.
//
// The session-directory naming rule is the harness's own, mirrored here
// verbatim from its source: the resolved cwd is stripped of its one
// leading slash, every "/ \ :" becomes "-", and the result is wrapped in
// "--" ... "--". A capsule at /root/ws/capsules/polyforge-140 therefore
// writes under "--root-ws-capsules-polyforge-140--". Path characters the
// rule does NOT fold (e.g. "#") survive into the directory name — one
// more reason the capsule directory never carries a raw slug: it is
// named by api.PathName ("<project>-<seq>", F2 / polyforge#144).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/common/harness"
	"github.com/silgrid/polyforge/internal/polyforge/sessionjsonl"
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
		return "" // no step session store configured: never the operator's ~/.pi
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

// LocateTranscript is the per-harness live-transcript locator both sync
// points share (polyforge#159, dispatched by harness per #171 ruling 7):
// pi → the harness session store's newest session file of the capsule
// since the spawn moment; claude → its native session file under the
// claude-config dir the caller names, by the uuid the daemon minted for
// the dispatch (only a canonical uuid is looked up — the id may come
// from the agent-writable spawn journal).
func LocateTranscript(piSessionsRoot, claudeConfigDir, capsuleDir string, spawnAt time.Time, h, session string) string {
	if harness.Of(h) == harness.Claude {
		return harness.LocateClaudeSession(session, claudeConfigDir, capsuleDir)
	}
	return LocateLiveTranscript(piSessionsRoot, capsuleDir, spawnAt)
}

// syncLiveTranscript copies the CURRENT dispatch's live harness
// transcript onto that dispatch's own capsule session path (the latest
// spawn-journal entry's transcript) so a seal / pause / bounded-fail
// commit carries the tail written since the last commit boundary. It is
// synchronous and best-effort (a missing source or journal is a no-op:
// the commit proceeds without the tail) and touches only the latest
// dispatch's file — an earlier step's transcript is never overwritten.
// A resumed dispatch already appends to the capsule file itself. Callers
// run it after the agent tree is killed and BEFORE the commit and the
// read-only seal.
func (r *Runtime) syncLiveTranscript(capsuleDir string) {
	entries, _ := capsule.ReadSpawnJournal(capsuleDir)
	if len(entries) == 0 {
		return
	}
	last := entries[len(entries)-1]
	claude := harness.Of(last.Harness) == harness.Claude
	// a resumed pi dispatch appends to the capsule file itself; a claude
	// dispatch's native session is always outside the capsule (resumed
	// or not), so it is always copied
	if last.Transcript == "" || (last.Resume && !claude) {
		return
	}
	spawnAt, err := time.Parse(time.RFC3339Nano, last.StartedAt)
	if err != nil && !claude {
		return
	}
	// The journal is agent-writable: the destination is never taken from
	// it verbatim (see trustedTranscriptDest).
	dst, ok := trustedTranscriptDest(capsuleDir, entries)
	if !ok {
		fmt.Fprintf(os.Stderr, "runtime: transcript sync skipped: the latest spawn-journal line names an untrusted destination %q\n", last.Transcript)
		return
	}
	src := LocateTranscript(r.cfg.StepAgentSessionsDir(), r.cfg.ClaudeConfigDir(), capsuleDir, spawnAt, last.Harness, last.HarnessSession)
	if src == "" || src == dst {
		return
	}
	if err := sessionjsonl.CopyReadonly(src, dst); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: transcript sync before commit failed: %v\n", err)
	}
}

// trustedTranscriptDest validates the latest spawn-journal line's
// transcript destination before the runtime writes to it. The agent can
// forge journal lines, so the path must (1) equal the one this runtime
// derives itself for that spawn token (sessionPathFor), (2) resolve inside
// <capsuleDir>/session/ with symlinks on the parent resolved and the file
// itself not a symlink, and (3) not be the destination of any EARLIER
// journal line (a forged line aimed at step 1's file). It answers the
// validated path, or false to skip the sync.
func trustedTranscriptDest(capsuleDir string, entries []capsule.SpawnJournalEntry) (string, bool) {
	last := entries[len(entries)-1]
	if last.SpawnToken == "" || strings.ContainsAny(last.SpawnToken, `/\`) {
		return "", false
	}
	want := sessionPathFor(capsuleDir, last.SpawnToken)
	if filepath.Clean(last.Transcript) != want {
		return "", false
	}
	for _, e := range entries[:len(entries)-1] {
		if e.SpawnToken == last.SpawnToken || (e.Transcript != "" && filepath.Clean(e.Transcript) == want) {
			return "", false
		}
	}
	sessDir, err := filepath.EvalSymlinks(filepath.Join(capsuleDir, capsule.DirSession))
	if err != nil {
		return "", false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(want))
	if err != nil {
		return "", false
	}
	if rel, err := filepath.Rel(sessDir, parent); err != nil || rel != "." {
		return "", false
	}
	if fi, err := os.Lstat(want); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return want, true
}
