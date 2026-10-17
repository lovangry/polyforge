package memory

// writethrough.go: the authority-file write-through. Create also writes
// memory/<project>/<id>/memory.md exactly as the file-authority store did
// (tmp + fsync → row → rename), and redact moves the directory into
// _redacted/, so an older aihub rolled back onto this data dir finds a file
// for every row and its reconcile keeps them. Files are written, never read:
// the database is the source of truth.
// Transitional: remove once every deployment has run this release (the release after it).

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
)

// authorityRel is a row's authority_path: memory/<project>/<id>.
func authorityRel(projectID, memID string) string { return "memory/" + projectID + "/" + memID }

// stageAuthority is write step ①: the document into memory.md.tmp, fsynced.
// It answers the tmp path, renamed over memory.md by placeAuthority once
// the row has committed.
func (s *Store) stageAuthority(rel, doc string) (string, error) {
	dir := filepath.Join(s.dataDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", errf(api.ErrMemInternal, "", "mkdir: %v", err)
	}
	tmp := filepath.Join(dir, "memory.md.tmp")
	if err := writeTmpSync(tmp, []byte(doc)); err != nil {
		return "", errf(api.ErrMemInternal, "", "write authority: %v", err)
	}
	return tmp, nil
}

// placeAuthority is write step ③: rename the staged file over memory.md.
func placeAuthority(tmp string) error {
	dir := filepath.Dir(tmp)
	if err := os.Rename(tmp, filepath.Join(dir, "memory.md")); err != nil {
		return errf(api.ErrMemInternal, "", "rename authority: %v", err)
	}
	if err := syncDir(dir); err != nil {
		return errf(api.ErrMemInternal, "", "fsync authority dir: %v", err)
	}
	return nil
}

// redactAuthority moves an entry's authority directory into
// memory/<project>/_redacted/. A row without a file (written by no
// file-authority store, or already moved) has nothing to move.
func (s *Store) redactAuthority(rel string) error {
	rel = filepath.ToSlash(rel)
	i := strings.LastIndexByte(rel, '/')
	if i < 0 || !filepath.IsLocal(rel) {
		return nil
	}
	src := filepath.Join(s.dataDir, filepath.FromSlash(rel))
	dst := filepath.Join(s.dataDir, filepath.FromSlash(rel[:i]+"/_redacted/"+rel[i+1:]))
	if _, err := os.Lstat(src); os.IsNotExist(err) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return errf(api.ErrMemInternal, "", "redact move: %v", err)
	}
	return nil
}

// writeTmpSync writes body to tmp, fsyncs it and its directory.
func writeTmpSync(tmp string, body []byte) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(tmp))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
