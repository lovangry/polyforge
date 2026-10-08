package test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

// v1_s04: wrap + ship, driver/envelope edition. The driver completes the
// workflow and the daemon's terminal wiring wraps it; the sealed
// capsule's inventory repo (repos/<name>, a real git worktree the fix
// step's envelope action created with its own origin remote) is pushed
// by the ship bridge to that origin as the fixed-name branch
// polyforge/<wi>. Verified with real git against the origin bare repo:
// the remote branch SHA equals the sealed worktree head, and a second
// ship is an idempotent reuse (pushed=false, reused=true) — never a
// force-push.
func TestV1S04WrapShip(t *testing.T) {
	inst := newV1Install(t)
	// The inventory repo's origin: a real bare repo the test owns.
	originDir := shortTmp("/tmp", "pfv1origin")
	origin := filepath.Join(originDir, "demo.git")
	gitRun(t, "", "init", "--bare", "-b", "main", origin)

	wi := inst.newWI(t, "s04: wrap then ship the sealed capsule",
		[]map[string]any{
			{"do": "envelope", "id": "grill", "outcome": "completed", "handoff": true},
			// The fix step's envelope action lays the inventory repo (a
			// real worktree with its own origin) before reporting done.
			{"do": "envelope", "id": "fix", "outcome": "completed", "handoff": true,
				"name": "demo", "origin": origin},
			{"do": "envelope", "id": "verification", "outcome": "completed"},
			{"do": "envelope", "id": "release", "outcome": "completed"},
		}, nil)
	d := newV1Daemon(t, inst, optUseDriver())
	d.start()
	d.pf(t, "submit", wi)
	inst.waitWIStatus(t, wi, "wrapped", 90*time.Second)

	// The driver-era completion face: every step done, envelopes archived.
	waitWorkflowStatus(t, d, wi, "completed", 30*time.Second)
	env := archivedEnvelope(t, d.capsuleDir(wi), "fix", 2)
	if env["outcome"] != "completed" {
		t.Fatalf("fix envelope wrong: %v", env["outcome"])
	}

	// The sealed worktree carries the inventory repo and its head.
	repoDir := filepath.Join(d.capsuleDir(wi), "repos", "demo")
	sealedHead := strings.TrimSpace(gitRun(t, repoDir, "rev-parse", "HEAD"))

	// Ship through the real CLI (machine output for the assertions).
	out := d.pf(t, "--json", "ship", wi)
	var ship cli.ShipResponse
	if err := json.Unmarshal([]byte(out), &ship); err != nil {
		t.Fatalf("ship answer undecodable: %v (%q)", err, out)
	}
	if ship.Branch != "polyforge/"+wi {
		t.Fatalf("ship branch wrong: %q", ship.Branch)
	}
	if len(ship.Repos) != 1 || ship.Repos[0].Name != "demo" {
		t.Fatalf("ship inventory wrong: %+v", ship.Repos)
	}
	oc := ship.Repos[0]
	if !oc.Pushed || oc.Head != sealedHead {
		t.Fatalf("ship outcome wrong: %+v (sealed head %s)", oc, sealedHead)
	}

	// Real-git verification: the origin now holds the branch at the
	// sealed commit.
	remoteRef := strings.TrimSpace(gitRun(t, origin, "rev-parse", "refs/heads/polyforge/"+wi))
	if remoteRef != sealedHead {
		t.Fatalf("origin branch %s != sealed head %s", remoteRef, sealedHead)
	}

	// A second ship is an idempotent reuse, never a second push.
	out2 := d.pf(t, "--json", "ship", wi)
	var ship2 cli.ShipResponse
	if err := json.Unmarshal([]byte(out2), &ship2); err != nil {
		t.Fatalf("second ship undecodable: %v (%q)", err, out2)
	}
	if len(ship2.Repos) != 1 || ship2.Repos[0].Pushed || !ship2.Repos[0].Reused {
		t.Fatalf("second ship not an idempotent reuse: %+v", ship2.Repos)
	}
}
