package freeze

// staging.go: the immutable staging area (05 §2.7 window actions step 2,
// SYNC-INV-7 / fix B09). Every byte a manifest references is INDEPENDENTLY
// COPIED out of the live worktree into a Daemon-managed staging directory
// BEFORE the write freeze is released, named by its content sha256:
// <daemon_local>/staging/<wi_id>/<seq>/<sha256>.
//
// HARDLINKS ARE FORBIDDEN (B09): a hardlink shares the inode with the live
// file, so an in-place write or truncate after freeze release would mutate
// the "pinned" staging bytes. Reflink/CoW (FICLONE / clonefile) is used when
// same-filesystem support exists and falls back to a full copy otherwise —
// both guarantee a distinct inode.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Stager manages <root>/staging.
type Stager struct {
	Root string
}

// NewStager creates a stager rooted at daemonLocal (the staging tree is
// created lazily).
func NewStager(daemonLocal string) *Stager {
	return &Stager{Root: filepath.Join(daemonLocal, "staging")}
}

// SeqDir is the staging directory of one publish flow.
func (s *Stager) SeqDir(wiID string, seq int64) string {
	return filepath.Join(s.Root, wiID, itoa(seq))
}

// StageEntry is one file to pin: its ABSOLUTE live-tree path and the
// content sha (of the ORIGINAL bytes) computed by the scan.
type StageEntry struct {
	AbsPath string
	Sha256  string
}

// Stage independently copies every entry into the staging dir for
// (wi, seq) and returns the number of blocks pinned. The content is
// verified against the scan's sha while copying — a mid-window mutation
// between scan and stage is a freeze-window violation and aborts the flow.
func (s *Stager) Stage(wiID string, seq int64, entries []StageEntry) (int, error) {
	dir := s.SeqDir(wiID, seq)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	for _, e := range entries {
		if e.Sha256 == "" {
			return 0, errors.New("freeze: staging entry without sha256")
		}
		dst := filepath.Join(dir, e.Sha256)
		if _, err := os.Stat(dst); err == nil {
			continue // same content pinned twice within the flow: idempotent
		}
		if err := copyIndependent(e.AbsPath, dst); err != nil {
			return 0, err
		}
		// The staged copy must still digest to the scanned sha: it was
		// copied inside the freeze window, so anything else is a violation.
		sum, err := sha256File(dst)
		if err != nil {
			return 0, err
		}
		if sum != e.Sha256 {
			return 0, errors.New("freeze: staged bytes diverged from the scanned digest (freeze window violated)")
		}
	}
	return len(entries), nil
}

// Block returns the staged block bytes for a sha (read back during upload —
// the upload phase reads ONLY staging, never the live tree).
func (s *Stager) Block(wiID string, seq int64, sha string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.SeqDir(wiID, seq), sha))
}

// Cleanup removes a flow's staging directory. The publisher only calls this
// once the outcome is final AND no pending publish record references the
// directory anymore (05 §2.7 release-time cleanup rule).
func (s *Stager) Cleanup(wiID string, seq int64) error {
	return os.RemoveAll(s.SeqDir(wiID, seq))
}

// copyIndependent copies src to dst with a DISTINCT INODE:
// FICLONE reflink first, plain copy fallback. Never a hardlink.
func copyIndependent(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	sfd := int(sf.Fd())
	df, err := os.OpenFile(dst+".part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	defer os.Remove(dst + ".part")
	dfd := int(df.Fd())
	// Try reflink (CoW): same-filesystem FICLONE gives an O(1) distinct
	// inode (B09: the point is never sharing a MUTABLE inode, which a
	// hardlink would). Unsupported → full copy.
	if err := unix.IoctlSetInt(dfd, unix.FICLONE, sfd); err != nil {
		if _, serr := sf.Seek(0, io.SeekStart); serr != nil {
			df.Close()
			return serr
		}
		if _, cerr := io.Copy(df, sf); cerr != nil {
			df.Close()
			return cerr
		}
	}
	// Reflinked or copied: the destination inode is its own; subsequent
	// in-place writes or truncates on the source cannot touch it.
	if err := df.Sync(); err != nil {
		df.Close()
		return err
	}
	if err := df.Close(); err != nil {
		return err
	}
	return os.Rename(dst+".part", dst)
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
