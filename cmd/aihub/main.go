// Command aihub is the v1 server binary: single-active, SQLite-backed,
// one HTTP port for the business face, the admin face and the git proxy.
//
// Architecture: docs/design/polyforge-vnext/v1-architecture-spec.md.
// Modules: internal/aihub/{core,memory,git,server}.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/aihub/git"
	"github.com/silgrid/polyforge/internal/aihub/memory"
	"github.com/silgrid/polyforge/internal/aihub/server"
	"github.com/silgrid/polyforge/internal/version"
)

// lockDataDir takes the exclusive single-active lock. Uses an open file
// descriptor held for the process lifetime (flock), matching the v0
// service.lock discipline; a second aihub fails immediately.
func lockDataDir(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dataDir+"/service.lock", os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, core.Fail("CORE-508", "LOCK_HELD", "another aihub process holds %s/service.lock", dataDir)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func main() {
	var (
		dataDir     = flag.String("data-dir", "/var/lib/polyforge/aihub", "AIHub data directory")
		listen      = flag.String("listen", ":8420", "HTTP listen address")
		janitorFreq = flag.Duration("git-janitor-period", time.Hour, "bare-repo gc/repack period")
		// Machines liveness threshold for GET /api/v1/machines: 5× the
		// daemon heartbeat default (60s). Tune to ≥5× the actual
		// -heartbeat-every the fleet runs with.
		staleAfter = flag.Duration("machines-stale-after", 5*time.Minute, "machines liveness threshold for the stale projection (default 5m = 5× heartbeat default)")
		// --version / -version: print the build-injected version string
		// (internal/version.Version, the git tag at build; "dev" un-injected)
		// and exit 0 — before any data-dir lock or server start.
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	// Single-active guard: exclusive lock on the data dir (CORE-508).
	release, err := lockDataDir(*dataDir)
	if err != nil {
		log.Fatalf("aihub: %v", err)
	}
	defer release()

	db, err := core.Open(*dataDir)
	if err != nil {
		log.Fatalf("aihub: open core: %v", err)
	}
	defer db.Close()

	// Fresh-install bootstrap: an empty users table gets one admin and a
	// one-time api key on stderr (plaintext never persisted).
	if key := bootstrapAdmin(db); key != "" {
		log.Printf("aihub: fresh install: seeded bootstrap admin usr_root — one-time api key (shown once, store it now): %s", key)
	}

	// Memory startup reconcile (authority files are the truth source).
	mems := memory.NewStore(db, *dataDir)
	if n, err := mems.RebuildIndex(); err != nil {
		log.Printf("aihub: startup memory reconcile: %v", err)
	} else if n > 0 {
		log.Printf("aihub: startup memory reconcile: %d entries reindexed", n)
	}

	gitMgr, err := git.NewManager(*dataDir, core.NewOwnerManager(db))
	if err != nil {
		log.Fatalf("aihub: git manager: %v", err)
	}

	jan := git.NewJanitor(gitMgr, *janitorFreq)
	jan.Start()
	defer jan.Stop()

	srv := server.New(db, *dataDir, gitMgr)
	srv.StaleAfter = *staleAfter
	httpSrv := &http.Server{Addr: *listen, Handler: srv}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("aihub: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("aihub: listening on %s (data-dir %s)", *listen, *dataDir)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("aihub: %v", err)
	}
}

// bootstrapAdmin seeds the first admin identity on a fresh install.
// Returns the plaintext api key when it seeded one, "" otherwise.
func bootstrapAdmin(db *core.DB) string {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil || n > 0 {
		return ""
	}
	const uid = "usr_root"
	keyID, err := core.NewID("key_")
	if err != nil {
		log.Fatalf("aihub: bootstrap: %v", err)
	}
	plain, err := core.NewSecret(24)
	if err != nil {
		log.Fatalf("aihub: bootstrap: %v", err)
	}
	sum := sha256.Sum256([]byte(plain))
	tx, err := db.Begin()
	if err != nil {
		log.Fatalf("aihub: bootstrap: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO users(id, display_name, email, kind, role, created_at)
		VALUES (?1,'bootstrap admin','root@localhost','human','admin',?2)`, uid, core.Now()); err != nil {
		log.Fatalf("aihub: bootstrap: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO api_keys(id, user_id, project_id, name, key_hash, created_at)
		VALUES (?1,?2,NULL,'bootstrap',?3,?4)`, keyID, uid, hex.EncodeToString(sum[:]), core.Now()); err != nil {
		log.Fatalf("aihub: bootstrap: %v", err)
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("aihub: bootstrap: %v", err)
	}
	return plain
}
