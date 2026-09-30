package git

// The §5.2 progress read-through (02-aihub-core.md): the server-side
// reader of a WI's bare-repo HEAD. Progress is NOT a Core-side datum —
// it is the runtime-state/execution.json content of the HEAD revision,
// passed through verbatim; no materialized cache, no per-WI progress
// file on disk (INV-1). The git HTTP face and its credential model are
// not involved: this is a plain read of the local bare repo the server
// already owns.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/capsule"
)

// ErrNoPublishedProgress reports that the WI's bare repo holds no
// readable progress object: no repo, an unborn HEAD, or a HEAD tree
// without runtime-state/execution.json. Callers map it to CORE-504
// (02 §5.2 / §5.3: read-through has no readable object).
var ErrNoPublishedProgress = errors.New("git: no published progress at HEAD")

// HeadProgress is the read-through answer for one WI's bare-repo HEAD.
type HeadProgress struct {
	Content   []byte    // runtime-state/execution.json at HEAD, verbatim
	Seq       int64     // the HEAD commit's Polyforge-Seq trailer
	UpdatedAt time.Time // the HEAD commit's committer date (the publish time)
}

// ReadHeadProgress reads the WI's bare-repo HEAD the §5.2 way: the
// runtime-state/execution.json blob at HEAD plus the HEAD commit's seq
// trailer and committer date. Every call reads through directly; no
// state is cached or materialized between calls.
func (m *Manager) ReadHeadProgress(wiID string) (*HeadProgress, error) {
	dir := m.RepoDir(wiID)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return nil, ErrNoPublishedProgress
	}
	// Unborn HEAD (repo created at claim time, nothing pushed yet).
	if _, err := git(dir, nil, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		return nil, ErrNoPublishedProgress
	}
	// The progress object must exist in the HEAD tree.
	listing, err := git(dir, nil, "ls-tree", "HEAD", capsule.ExecutionJSON)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(listing) == "" {
		return nil, ErrNoPublishedProgress
	}
	content, err := git(dir, nil, "show", "HEAD:"+capsule.ExecutionJSON)
	if err != nil {
		return nil, err
	}
	hp := &HeadProgress{Content: []byte(content)}

	// head_seq: the HEAD commit's seq trailer (the push chain's monotonic
	// sequence, enforced by the pre-receive hook). A repo pushed outside
	// the proxy may carry commits without the trailer; the commit count is
	// the honest fallback there (it equals the trailer value for every
	// regular push, which derives the seq the same way).
	seqOut, err := git(dir, nil, "log", "-1",
		"--format=%(trailers:key="+capsule.SeqTrailer+",valueonly)", "HEAD")
	if err == nil {
		if n, perr := strconv.ParseInt(strings.TrimSpace(seqOut), 10, 64); perr == nil {
			hp.Seq = n
		}
	}
	if hp.Seq == 0 {
		count, cerr := git(dir, nil, "rev-list", "--count", "HEAD")
		if cerr == nil {
			if n, perr := strconv.ParseInt(strings.TrimSpace(count), 10, 64); perr == nil {
				hp.Seq = n
			}
		}
	}

	// head_updated_at: the HEAD commit's committer date — the closest git
	// artifact to "when this revision reached the repo".
	dateOut, err := git(dir, nil, "log", "-1", "--format=%cI", "HEAD")
	if err == nil {
		if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(dateOut)); perr == nil {
			hp.UpdatedAt = t
		}
	}
	return hp, nil
}
