package memory

// writethrough.go: the authority-file write-through, so an older aihub
// (v2.11 and before, whose reconcile trusts the files) rolled back onto
// this data dir keeps every row. Create places memory/<project>/<id>/
// memory.md (tmp + fsync + rename) BEFORE committing the row: a crash in
// between leaves a file without a row, which this release ignores (no row
// is no memory; the client never got its 201) and an older aihub adopts.
// A create whose commit fails removes its file. Redact commits the row
// first and then moves the directory into _redacted/: a crash in between
// leaves a redacted row with its file in place, which this release serves
// as redacted and an older aihub keeps redacted (redacted rows are
// terminal there). Files are written, never read: the database is the
// source of truth.
// Transitional, with the memories columns fs_mtime_ns / fs_size (core
// DDL): remove once every deployment has run this release (the release
// after it).

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
)

// authorityRel is a row's authority_path: memory/<project>/<id>.
func authorityRel(projectID, memID string) string { return "memory/" + projectID + "/" + memID }

// writeAuthority places the document as rel/memory.md: tmp + fsync,
// rename, directory fsync.
func (s *Store) writeAuthority(rel, doc string) error {
	dir := filepath.Join(s.dataDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return errf(api.ErrMemInternal, "", "mkdir: %v", err)
	}
	tmp := filepath.Join(dir, "memory.md.tmp")
	if err := writeTmpSync(tmp, []byte(doc)); err != nil {
		return errf(api.ErrMemInternal, "", "write authority: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "memory.md")); err != nil {
		return errf(api.ErrMemInternal, "", "rename authority: %v", err)
	}
	if err := syncDir(dir); err != nil {
		return errf(api.ErrMemInternal, "", "fsync authority dir: %v", err)
	}
	return nil
}

// dropAuthority removes the directory of a create that did not commit
// (a fresh id: nothing else lives there).
func (s *Store) dropAuthority(rel string) {
	_ = os.RemoveAll(filepath.Join(s.dataDir, filepath.FromSlash(rel)))
}

// redactAuthority moves a redacted entry's authority directory into
// memory/<project>/_redacted/, best effort: the redact has committed, and
// a file left in place is harmless (an older aihub keeps a redacted row
// redacted whatever its file position).
func (s *Store) redactAuthority(rel string) {
	rel = filepath.ToSlash(rel)
	i := strings.LastIndexByte(rel, '/')
	if i < 0 || !filepath.IsLocal(rel) {
		return
	}
	src := filepath.Join(s.dataDir, filepath.FromSlash(rel))
	dst := filepath.Join(s.dataDir, filepath.FromSlash(rel[:i]+"/_redacted/"+rel[i+1:]))
	if _, err := os.Lstat(src); err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(dst), 0o750) == nil {
		_ = os.Rename(src, dst)
	}
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
