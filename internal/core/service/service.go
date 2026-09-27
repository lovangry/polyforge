// Package service implements the single-active service guarantees and the
// service_secret epoch machinery. Design authority:
// docs/design/polyforge-vnext/02-aihub-core.md §2.2 (单 active Service 三层),
// §4.10 (core_meta/service_secret_epochs), §6.6 (凭证轮换与重新授权).
package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/silgrid/polyforge/internal/core/store"
)

// ErrLockHeld is CORE-508: another process holds <data_dir>/service.lock.
var ErrLockHeld = errors.New("CORE-508 LOCK_HELD")

// DefaultGraceSeconds is the default grace window for a regular (mode=grace)
// secret rotation (02 §6.6 step 3; configurable).
const DefaultGraceSeconds = 3600

// Lock takes the exclusive non-blocking lock on <data_dir>/service.lock and
// returns the release function. Failure to acquire ⇒ ErrLockHeld (process
// must exit, 02 §2.2 layer 2).
func Lock(dataDir string) (release func(), err error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "service.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// Flock-style advisory lock via O_EXCL sentinel is not durable across
	// crashes; use an OS-level exclusive lock on the open file.
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %v", ErrLockHeld, err)
	}
	return func() {
		unlockFile(f)
		f.Close()
	}, nil
}

// Service is the running aihub process's service-level state: its identity,
// heartbeat and the secret-epoch machinery backing capability tokens.
type Service struct {
	db      *store.DB
	dataDir string
	id      string
}

// Register bootstraps the service row: reads/creates service_id, writes the
// initial heartbeat, and ensures at least one active secret epoch exists
// (auto-creating epoch 1 with a fresh random secret if the data dir is empty,
// rotated_by=NULL = 系统自动, 02 §4.10).
func Register(db *store.DB, dataDir string) (*Service, error) {
	svc := &Service{db: db, dataDir: dataDir}
	id, err := store.NewSecret(16)
	if err != nil {
		return nil, err
	}
	svc.id = "svc_" + id

	var current string
	err = db.QueryRow(`SELECT value FROM core_meta WHERE key='service_id'`).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// fresh install: register self
	case err != nil:
		return nil, err
	default:
		svc.id = current // keep stable service identity across restarts
	}

	if err := svc.ensureEpoch1(); err != nil {
		return nil, err
	}
	if err := svc.Heartbeat(); err != nil {
		return nil, err
	}
	return svc, nil
}

// Heartbeat refreshes service_id + service_heartbeat_at in core_meta
// (02 §2.2 layer 3).
func (s *Service) Heartbeat() error {
	if _, err := s.db.Exec(`INSERT INTO core_meta(key, value) VALUES ('service_id', ?1)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, s.id); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO core_meta(key, value) VALUES ('service_heartbeat_at', ?1)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, store.Now())
	return err
}

// CheckActive verifies the caller-visible service identity still matches the
// registered row (guards the stale-process double-write window, CORE-509).
func (s *Service) CheckActive() error {
	var current string
	if err := s.db.QueryRow(`SELECT value FROM core_meta WHERE key='service_id'`).Scan(&current); err != nil {
		return err
	}
	if current != s.id {
		return errors.New("CORE-509 STALE_SERVICE")
	}
	return nil
}

func (s *Service) ensureEpoch1() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM service_secret_epochs`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return s.rotateLocked("grace", 0, "", false)
}

// secretPath returns the on-disk secret file for an epoch (0600, 02 §4.11).
// The secret bytes never live in SQLite (02 §4.10).
func secretPath(dataDir string, epoch int) string {
	return filepath.Join(dataDir, fmt.Sprintf("service.secret.%d", epoch))
}

func (s *Service) loadSecret(epoch int) ([]byte, error) {
	return os.ReadFile(secretPath(s.dataDir, epoch))
}

// CurrentEpoch reads core_meta.current_secret_epoch.
func (s *Service) CurrentEpoch() (int, error) {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM core_meta WHERE key='current_secret_epoch'`).Scan(&v); err != nil {
		return 0, err
	}
	e, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("service: bad current_secret_epoch %q: %w", v, err)
	}
	return e, nil
}

// RotateSecret implements M5 (02 §6.6). mode ∈ {grace, rollback};
// graceSeconds only meaningful (and forced 0) for rollback, where every
// non-retired older epoch is retired immediately and its file removed in the
// same call — no revival window for any pre-rollback credential.
func (s *Service) RotateSecret(mode string, graceSeconds int, rotatedBy string) error {
	switch mode {
	case "grace":
		return s.rotateLocked("grace", DefaultGraceSeconds, rotatedBy, true)
	case "rollback":
		return s.rotateLocked("rollback", 0, rotatedBy, true)
	default:
		return errors.New("CORE-400 unknown rotate mode")
	}
}

func (s *Service) rotateLocked(mode string, graceSeconds int, rotatedBy string, retireOld bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var maxEpoch sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(epoch) FROM service_secret_epochs`).Scan(&maxEpoch); err != nil {
		return err
	}
	next := int(maxEpoch.Int64) + 1

	secret, err := store.NewSecret(32)
	if err != nil {
		return err
	}
	p := secretPath(s.dataDir, next)
	if err := os.WriteFile(p, []byte(secret), 0o600); err != nil {
		return err
	}

	var by any
	if rotatedBy != "" {
		by = rotatedBy
	}
	if _, err := tx.Exec(`INSERT INTO service_secret_epochs(epoch, mode, created_at, retired_at, rotated_by)
		VALUES (?1, ?2, ?3, NULL, ?4)`, next, mode, store.Now(), by); err != nil {
		os.Remove(p)
		return err
	}
	if _, err := tx.Exec(`INSERT INTO core_meta(key, value) VALUES ('current_secret_epoch', ?1)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(next)); err != nil {
		return err
	}

	if retireOld && mode == "rollback" {
		// Retire ALL older non-retired epochs now, delete their files.
		rows, err := tx.Query(`SELECT epoch FROM service_secret_epochs WHERE epoch < ?1 AND retired_at IS NULL`, next)
		if err != nil {
			return err
		}
		var olds []int
		for rows.Next() {
			var e int
			if err := rows.Scan(&e); err != nil {
				rows.Close()
				return err
			}
			olds = append(olds, e)
		}
		rows.Close()
		for _, e := range olds {
			if _, err := tx.Exec(`UPDATE service_secret_epochs SET retired_at=?1 WHERE epoch=?2`, store.Now(), e); err != nil {
				return err
			}
			os.Remove(secretPath(s.dataDir, e))
		}
	} else if retireOld {
		// mode=grace: older epochs stay valid until SweepRetired retires them
		// after the grace window.
		_ = graceSeconds
	}
	return tx.Commit()
}

// SweepRetired retires grace-mode epochs whose grace window has elapsed and
// removes their secret files (02 §6.6 step 4). Call periodically.
func (s *Service) SweepRetired(grace time.Duration) error {
	if grace < 0 {
		grace = 0 // force-sweep for tests / operators
	}
	cutoff := time.Now().UTC().Add(-grace).Format(time.RFC3339Nano)
	rows, err := s.db.Query(`SELECT epoch FROM service_secret_epochs
		WHERE mode='grace' AND retired_at IS NULL AND created_at < ?1 AND epoch < ?2`,
		cutoff, mustCurrentEpochOrZero(s))
	if err != nil {
		return err
	}
	var olds []int
	for rows.Next() {
		var e int
		if err := rows.Scan(&e); err != nil {
			rows.Close()
			return err
		}
		olds = append(olds, e)
	}
	rows.Close()
	for _, e := range olds {
		if _, err := s.db.Exec(`UPDATE service_secret_epochs SET retired_at=?1 WHERE epoch=?2 AND retired_at IS NULL`, store.Now(), e); err != nil {
			return err
		}
		os.Remove(secretPath(s.dataDir, e))
	}
	return nil
}

func mustCurrentEpochOrZero(s *Service) int {
	e, err := s.CurrentEpoch()
	if err != nil {
		return 0
	}
	return e
}

// RollbackBoundary returns R = the latest mode=rollback epoch (0 if none).
// Credentials bound to epoch ≤ R are dead permanently (02 §6.6 回滚边界判定).
func (s *Service) RollbackBoundary() (int, error) {
	var r sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(epoch) FROM service_secret_epochs WHERE mode='rollback'`).Scan(&r)
	if err != nil {
		return 0, err
	}
	return int(r.Int64), nil
}

// tokenCandidate describes one epoch against which a presented token can be
// checked.
type tokenCandidate struct {
	epoch   int
	secret  []byte
	retired bool
}

// TokenResult is the outcome of a capability-token check.
type TokenResult struct {
	OK    bool
	Epoch int
	// Reason is the CORE-403 reason when !OK: stale_generation |
	// secret_rotated_expired | secret_rolled_back.
	Reason string
}

// SignToken computes token = HMAC-SHA256(secret[epoch], wi|g|machine), hex
// encoded, with the CURRENT epoch (02 §5.1).
func (s *Service) SignToken(wiID string, generation int64, machineID string) (token string, epoch int, err error) {
	e, err := s.CurrentEpoch()
	if err != nil {
		return "", 0, err
	}
	secret, err := s.loadSecret(e)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(signHMAC(secret, wiID, generation, machineID)), e, nil
}

func signHMAC(secret []byte, wiID string, generation int64, machineID string) []byte {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s|%d|%s", wiID, generation, machineID)
	return mac.Sum(nil)
}

// CheckToken verifies a presented token for the given (wi, generation,
// machine) triple against the current epoch and every epoch still inside its
// grace window, in the order mandated by 02 §5.1/§6.6:
//
//   - match on current epoch            ⇒ OK
//   - match on a grace-window epoch > R  ⇒ OK (epoch reported for re-sign hint)
//   - match on an epoch ≤ R (rollback)   ⇒ secret_rolled_back
//   - match on a retired epoch > R       ⇒ secret_rotated_expired
//   - no match at all                    ⇒ stale_generation (fenced)
//
// Comparison is constant-time per candidate.
func (s *Service) CheckToken(tokenHex string, wiID string, generation int64, machineID string) TokenResult {
	if len(tokenHex) != 64 {
		return TokenResult{Reason: "stale_generation"}
	}
	presented, err := hex.DecodeString(tokenHex)
	if err != nil {
		return TokenResult{Reason: "stale_generation"}
	}
	current, err := s.CurrentEpoch()
	if err != nil {
		return TokenResult{Reason: "stale_generation"}
	}
	R, err := s.RollbackBoundary()
	if err != nil {
		return TokenResult{Reason: "stale_generation"}
	}

	cands, err := s.candidates(current)
	if err != nil {
		return TokenResult{Reason: "stale_generation"}
	}
	// Current epoch first, then history from newest to oldest.
	for _, c := range cands {
		want := signHMAC(c.secret, wiID, generation, machineID)
		if subtle.ConstantTimeCompare(presented, want) == 1 {
			if c.epoch < R {
				return TokenResult{Reason: "secret_rolled_back"}
			}
			if c.retired {
				return TokenResult{Reason: "secret_rotated_expired"}
			}
			return TokenResult{OK: true, Epoch: c.epoch}
		}
	}
	return TokenResult{Reason: "stale_generation"}
}

func (s *Service) candidates(current int) ([]tokenCandidate, error) {
	rows, err := s.db.Query(`SELECT epoch, retired_at FROM service_secret_epochs ORDER BY epoch DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type epochRow struct {
		epoch   int
		retired bool
	}
	var hist []epochRow
	for rows.Next() {
		var e int
		var retired sql.NullString
		if err := rows.Scan(&e, &retired); err != nil {
			return nil, err
		}
		hist = append(hist, epochRow{e, retired.Valid})
	}
	rows.Close()

	secret, err := s.loadSecret(current)
	if err != nil {
		return nil, err
	}
	out := []tokenCandidate{{epoch: current, secret: secret}}
	for _, h := range hist {
		if h.epoch == current {
			continue
		}
		sec, err := s.loadSecret(h.epoch)
		if err != nil {
			if os.IsNotExist(err) {
				continue // swept secret ⇒ cannot verify ⇒ treated as no match
			}
			return nil, err
		}
		out = append(out, tokenCandidate{epoch: h.epoch, secret: sec, retired: h.retired})
	}
	return out, nil
}
