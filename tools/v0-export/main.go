// Command v0-export exports a v0 pilot aihub deployment (READ-ONLY) into
// the v1 seeding inputs. See package v0export for the output contract.
//
// Usage:
//
//	v0-export --db /path/to/aihub.db --data-dir /path/to/aihub-data --out ./export
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/silgrid/polyforge/tools/v0export"
)

func main() {
	var (
		dbPath   = flag.String("db", "", "v0 aihub.db (opened read-only; required)")
		dataDir  = flag.String("data-dir", "", "v0 aihub data dir (authority memory/ tree, capsules/; required)")
		out      = flag.String("out", "", "output directory (created; required)")
		noCaps   = flag.Bool("skip-capsules", false, "skip the capsule snapshot replay")
		onlyWI   = flag.String("only-wi", "", "export ONLY this work item's capsule snapshot (work-items and memories stay full exports)")
	)
	flag.Parse()
	rep, err := v0export.Run(v0export.Options{
		DBPath:      *dbPath,
		DataDir:     *dataDir,
		Out:         *out,
		SkipCapsules: *noCaps,
		OnlyWI:      *onlyWI,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "v0-export: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("v0-export: %d work item(s), %d memor(ies) (%d redacted), %d capsule snapshot(s) → %s\n",
		rep.WorkItems, rep.Memories, rep.RedactedMemories, len(rep.CapsuleSnapshots), *out)
	for _, o := range rep.OrphanFiles {
		fmt.Printf("  orphan authority file (exported from the file): %s\n", o)
	}
	for _, m := range rep.ShaMismatches {
		fmt.Printf("  content_sha mismatch: %s\n", m)
	}
	for _, m := range rep.ManifestShaMismatch {
		fmt.Printf("  manifest_sha mismatch: %s\n", m)
	}
	if len(rep.OrphanFiles)+len(rep.ShaMismatches)+len(rep.ManifestShaMismatch) > 0 {
		os.Exit(3) // exported, but the reconciliation findings need eyes
	}
}
