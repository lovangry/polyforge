// Package testutil holds test-only file helpers that carry no domain
// imports (no SQLite/store), so any test package — including ones that
// deliberately keep their import graph light — can use them.
package testutil

import (
	"os"
	"path/filepath"
)

// WriteFileTree writes every rel→content pair under root (mkdir -p).
func WriteFileTree(root string, files map[string]string) error {
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
