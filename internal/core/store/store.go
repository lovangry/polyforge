// Package store opens the Core SQLite database and provides shared low-level
// helpers. Design authority: docs/design/polyforge-vnext/02-aihub-core.md §4
// (preamble: journal_mode=WAL, foreign_keys=ON, busy_timeout=5000).
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/silgrid/polyforge/internal/core/schema"

	_ "modernc.org/sqlite"
)

// DB is the Core database handle. All Core writes go through the single
// active service process (INV-2a); reads may be concurrent under WAL.
type DB struct {
	*sql.DB
}

// Open opens (creating if needed) the Core database at
// <data_dir>/aihub.db and migrates it to the current schema version.
func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "aihub.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// SQLite is a single-writer database; one write connection avoids
	// SQLITE_BUSY between our own goroutines (busy_timeout still covers
	// external readers).
	sqlDB.SetMaxOpenConns(4)
	if err := schema.Migrate(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{sqlDB}, nil
}

// Now returns the canonical RFC3339 UTC timestamp used for all Core columns
// (02 §4 preamble: 全部时间戳为 RFC3339 字符串（UTC）).
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// NewID returns "prefix" + 12 random lowercase alphanumeric characters
// (02 §4.2: id = "wi_" + 12 位随机; same shape for mem_/usr_/…).
func NewID(prefix string) (string, error) {
	var b [12]byte
	// random alphanumeric alphabet (62 chars) → 12 chars.
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: rand: %w", err)
	}
	out := make([]byte, 12)
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return prefix + string(out), nil
}

// NewSecret returns n cryptographically random bytes, hex encoded.
func NewSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}
