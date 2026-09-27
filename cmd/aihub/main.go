// Command aihub is the AIHub vNext service: single-active, SQLite-backed,
// one HTTP port serving the 25 business endpoints plus the management face.
// Design authority: docs/design/polyforge-vnext/02-aihub-core.md.
package main

import (
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
			if err := svc.SweepRetired(time.Duration(service.DefaultGraceSeconds) * time.Second); err != nil {
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
