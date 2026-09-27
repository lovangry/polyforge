// Command aihub is the AIHub vNext service: single-active, SQLite-backed,
// one HTTP port serving the 25 business endpoints plus the management face.
// Design authority: docs/design/polyforge-vnext/02-aihub-core.md.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/server"
)

func main() {
	var (
		dataDir      = flag.String("data-dir", "/var/lib/polyforge/aihub", "AIHub data directory")
		listen       = flag.String("listen", ":8420", "HTTP listen address (single port, A5)")
		workflowsDir = flag.String("workflows-dir", "workflows", "local checkout workflows/ directory (deployment config, 02 §4.5)")
	)
	flag.Parse()

	// 02 §2.2 layer 2: exclusive lock; a second process exits CORE-508.
	release, err := service.Lock(*dataDir)
	if err != nil {
		log.Fatalf("aihub: %v", err)
	}
	defer release()

	db, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("aihub: open core: %v", err)
	}
	defer db.Close()

	// Fresh-install bootstrap (real-usage gap found in the polyforge#17
	// dogfood round): every management verb (users/api-keys/projects
	// create, 02 §5.1 M1–M3) requires an admin api key, and the design's
	// machine-registration flow (02 §4.10: "first registration call by the
	// deployment operator with any valid human/admin api key") assumes one
	// already exists — but nothing ever created the FIRST one, so a default
	// install was unusable from the pf CLI. On the first boot of an empty
	// deployment (no users at all) seed one bootstrap admin and hand its
	// api key to the operator exactly once, on stderr (the same
	// plaintext-once convention as machine registration, 02 §4.10).
	if key, berr := bootstrapAdmin(db); berr != nil {
		log.Fatalf("aihub: bootstrap admin: %v", berr)
	} else if key != "" {
		log.Printf("aihub: fresh install: seeded bootstrap admin usr_root — one-time api key (shown once, store it now): %s", key)
	}

	svc, err := service.Register(db, *dataDir)
	if err != nil {
		log.Fatalf("aihub: register service: %v", err)
	}

	srv := server.New(db, *dataDir, *workflowsDir, svc)

	// Background maintenance: service heartbeat, grace-window retirement and
	// tombstone hard-deletes (02 §2.2/§6.6/05 §2.10).
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			if err := svc.Heartbeat(); err != nil {
				log.Printf("aihub: heartbeat: %v", err)
			}
		}
	}()
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			if err := svc.SweepRetired(svc.StoredGraceSeconds()); err != nil {
				log.Printf("aihub: sweep secrets: %v", err)
			}
		}
	}()
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			if err := srv.Caps.SweepTombstones(); err != nil {
				log.Printf("aihub: sweep tombstones: %v", err)
			}
		}
	}()

	log.Printf("aihub: listening on %s (data-dir %s)", *listen, *dataDir)
	if err := http.ListenAndServe(*listen, srv); err != nil {
		log.Fatalf("aihub: %v", err)
	}
}

// bootstrapAdmin seeds the first admin identity on a fresh install. It
// returns the plaintext api key when it seeded one, "" when the deployment
// already has any user (the normal case — including every test harness,
// which seeds its own identities after store.Open). Only a truly empty
// users table triggers the seed, so an existing deployment is never
// touched.
func bootstrapAdmin(db *store.DB) (string, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "", nil
	}
	// Deterministic id: the log line (and every operator runbook) names
	// THE bootstrap admin; a random id would make the message a lie (found
	// by the S2 scenario's own assertion).
	const uid = "usr_root"
	keyID, err := store.NewID("key_")
	if err != nil {
		return "", err
	}
	plain, err := store.NewSecret(24)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(plain))
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO users(id, display_name, kind, role) VALUES (?1,'bootstrap admin','human','admin')`, uid); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO api_keys(id, user_id, project_id, key_hash) VALUES (?1,?2,NULL,?3)`,
		keyID, uid, hex.EncodeToString(sum[:])); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return plain, nil
}
