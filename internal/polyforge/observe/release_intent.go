package observe

// release_intent.go: the seal-time release-intent gate's fragment type and
// its shape-only validation (polyforge#123, P1a+P1c).
//
// When a WI's per-repo base→worktree diff touches anything outside the
// pure docs/tests exemption, wrap requires an entry for that repo in the
// capsule's artifacts/release-intent.json. The fragment is SELF-REPORTED
// and trusted as to content (the bump type, the note — never re-derived or
// second-guessed by the daemon); the gate validates SHAPE only: which
// repos are named, one closed vocabulary for the bump, note type. This is
// the same trust line the release templates draw — the schema admits the
// agent's own declaration, the daemon refuses only its absence or
// malformation.
//
// Fragment shape (one JSON object):
//
//	{ "repos": [ { "repo": "<repo name>", "bump": "patch|minor|major", "note": "<one line>" } ] }
//
// `repo` names the changed repository exactly as capsule.json's repos map
// spells it; `name` is accepted as a spelling alias for the same field
// (the schema-shape latitude the spec's own prose allows — field names
// were plan latitude, and both spellings appear in the wild). `note` is
// optional but must be a string when present.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReleaseIntentFile is the capsule-relative path of the release-intent
// fragment the wrap gate reads.
const ReleaseIntentFile = "artifacts/release-intent.json"

// ReleaseIntentEntry is one repo's self-reported release intent.
type ReleaseIntentEntry struct {
	// Repo names the changed repository (capsule.json repos-map spelling).
	// `name` is accepted as an alias on read; Repo is always populated.
	Repo string `json:"repo"`
	// Bump is the self-reported version impact: patch, minor or major.
	Bump string `json:"bump"`
	// Note is the one-line human note (what the change is, for the
	// release train's record). Optional.
	Note string `json:"note,omitempty"`
}

// UnmarshalJSON reads one entry, accepting `name` as the spelling alias
// for `repo` (the schema-shape latitude the spec allows — the entry's
// one non-negotiable duty is to name the repo it covers).
func (e *ReleaseIntentEntry) UnmarshalJSON(b []byte) error {
	var raw struct {
		Repo *string `json:"repo"`
		Name *string `json:"name"`
		Bump string  `json:"bump"`
		Note string  `json:"note"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch {
	case raw.Repo != nil:
		e.Repo = *raw.Repo
	case raw.Name != nil:
		e.Repo = *raw.Name
	}
	e.Bump, e.Note = raw.Bump, raw.Note
	return nil
}

// ReleaseIntent is the fragment's whole shape: the repos this WI intends
// to release, with their self-reported bump and note.
type ReleaseIntent struct {
	Repos []ReleaseIntentEntry `json:"repos"`
}

// ValidBumps is the closed bump vocabulary.
var ValidBumps = []string{"patch", "minor", "major"}

// validBump answers whether s belongs to the closed bump set.
func validBump(s string) bool {
	for _, v := range ValidBumps {
		if s == v {
			return true
		}
	}
	return false
}

// loadReleaseIntent reads and shape-validates the fragment at
// <capsuleDir>/artifacts/release-intent.json. An ABSENT file answers
// (nil, nil) — the gate treats a nil fragment as "no entries" and judges
// the repos against an empty set. A PRESENT file must parse and validate
// strictly: any shape violation is a typed error naming the offending
// entry (the same refusal path as a missing entry — re-flush after
// fixing the fragment).
func loadReleaseIntent(capsuleDir string) (*ReleaseIntent, error) {
	raw, err := os.ReadFile(filepath.Join(capsuleDir, filepath.FromSlash(ReleaseIntentFile)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s unreadable: %w", ReleaseIntentFile, err)
	}
	return parseReleaseIntent(raw)
}

// parseReleaseIntent shape-validates one fragment document. Split from
// loadReleaseIntent so tests can feed bytes directly.
func parseReleaseIntent(raw []byte) (*ReleaseIntent, error) {
	var frag ReleaseIntent
	if err := json.Unmarshal(raw, &frag); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %v", ReleaseIntentFile, err)
	}
	seen := map[string]bool{}
	for i, e := range frag.Repos {
		name := strings.TrimSpace(e.Repo)
		if name == "" {
			return nil, fmt.Errorf("%s: repos[%d] carries no repo name (field \"repo\"; \"name\" accepted as the alias)", ReleaseIntentFile, i)
		}
		if !validBump(e.Bump) {
			return nil, fmt.Errorf("%s: repo %q carries bump %q — the vocabulary is (%s)", ReleaseIntentFile, name, e.Bump, strings.Join(ValidBumps, ", "))
		}
		if seen[name] {
			return nil, fmt.Errorf("%s: repo %q is listed twice — one entry per repo", ReleaseIntentFile, name)
		}
		seen[name] = true
	}
	return &frag, nil
}

// covers answers whether the fragment carries an entry for the named repo.
func (r *ReleaseIntent) covers(repo string) bool {
	if r == nil {
		return false
	}
	for _, e := range r.Repos {
		if e.Repo == repo {
			return true
		}
	}
	return false
}

// ReleaseIntentMissingFor is the wrap gate's predicate, exported for the
// runtime's driver-lane terminal wrap close (recover.go): the driver era
// has no flush(wrap) call — Run returning nil IS the completion signal —
// so the driver lane's close is the face that must refuse. The monitor's
// flush lane (flushWrap) runs the same predicate for agent-driven seals.
func ReleaseIntentMissingFor(capsuleDir, wi string) []string {
	return releaseIntentMissingFor(capsuleDir, wi)
}

// ReleaseIntentRefusal is the typed refusal text for repos the fragment
// does not cover, exported alongside the predicate for the same caller.
func ReleaseIntentRefusal(wi string, missing []string) string {
	return releaseIntentRefusal(wi, missing)
}

// releaseIntentRefusal builds the typed refusal text for repos the
// fragment does not cover (or mis-declared): the DAEMON-400 code, the
// release-intent name, every uncovered repo, and the re-flush recovery —
// the same non-terminal shape as VERIFICATION_EVIDENCE_MISSING.
func releaseIntentRefusal(wi string, missing []string) string {
	return fmt.Sprintf(
		"work item %s: releaseable code changed with no release-intent entry for %s — write %s with a {repo, bump, note} entry per changed repo (bump: patch|minor|major), then re-flush",
		wi, strings.Join(missing, ", "), ReleaseIntentFile)
}
