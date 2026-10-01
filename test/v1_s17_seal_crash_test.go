package test

// v1_s17: a SIGKILL landing inside the SEALING window, finished by the
// startup reconciliation. The agent-driven flush(wrap) has ALREADY
// pushed the sealed capsule state (its git push goes direct to aihub,
// never through the proxy), the runtime is inside stopOp with the job
// persisted sealing (sealed_push=true), and SessionEnd has NOT yet
// reached aihub when the daemon process is kill -9ed. The window is
// milliseconds wide in real time, so the test makes it deterministic:
// a test-side hold-proxy fronts the daemon's aihub API face, forwards
// everything untouched, and intercepts the POST */session-end —
// registering it and blocking the handler until the test says so. The
// kill happens only once the proxy holds the request in hand.
//
// The restarted daemon (same socket/dataDir/workspace, direct aihub)
// must finish the interrupted close through the reconciliation's
// sealed_push=true skip-push branch: SessionEnd lands (aihub
// wrapped), the capsule goes read-only, the local bookkeeping loses
// the sealing record, and no agent is ever re-derived — the run never
// stalls in a half-sealed state.

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The hold-proxy: the s17 determinism mechanism.

// holdProxy is a reverse proxy in front of aihub that forwards every
// request untouched but intercepts the runtime's SessionEnd call
// (POST /api/v1/work-items/{id}/session-end): the hit is registered
// and the handler BLOCKS until the test drops the proxy — the request is
// never forwarded, so aihub cannot hear a verdict from the doomed
// daemon. The daemon's git pushes bypass the proxy entirely (they are
// pinned direct via optServerURL's --git-remote-base), so only the API
// client's traffic passes through.
type holdProxy struct {
	proxy       *httputil.ReverseProxy
	held        chan struct{} // closed on the first held session-end
	heldOnce    sync.Once
	release     chan struct{} // closed to unblock held handlers
	releaseOnce sync.Once
	srv         *http.Server
	ln          net.Listener
}

func newHoldProxy(t *testing.T, target string) *holdProxy {
	t.Helper()
	tu, err := url.Parse(target)
	if err != nil {
		t.Fatalf("hold proxy target %q: %v", target, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold proxy listen: %v", err)
	}
	p := &holdProxy{
		proxy:   httputil.NewSingleHostReverseProxy(tu),
		held:    make(chan struct{}),
		release: make(chan struct{}),
		ln:      ln,
	}
	p.srv = &http.Server{Handler: p}
	go func() { _ = p.srv.Serve(ln) }()
	t.Cleanup(p.close)
	return p
}

func (p *holdProxy) url() string { return "http://" + p.ln.Addr().String() }

// isHeld reports whether the proxy has a session-end in hand.
func (p *holdProxy) isHeld() bool {
	select {
	case <-p.held:
		return true
	default:
		return false
	}
}

// drop unblocks the held session-end handler: the request was never
// forwarded, and the released handler answers 503 so no caller can
// mistake the release for a verdict.
func (p *holdProxy) drop() { p.releaseOnce.Do(func() { close(p.release) }) }

func (p *holdProxy) close() {
	p.drop()
	_ = p.ln.Close()
	_ = p.srv.Close()
}

func (p *holdProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost &&
		strings.Contains(r.URL.Path, "/api/v1/work-items/") &&
		strings.HasSuffix(r.URL.Path, "/session-end") {
		p.heldOnce.Do(func() { close(p.held) })
		select {
		case <-p.release:
			http.Error(w, "session-end held by the s17 test proxy (never forwarded)",
				http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
		return
	}
	p.proxy.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Helpers.

// daemonStateRecord reads <dataDir>/runtime-state.json and returns the
// daemon's persisted record of wi (nil when absent — the file may not
// exist yet, or the record may already be gone).
func daemonStateRecord(t *testing.T, dataDir, wi string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "runtime-state.json"))
	if err != nil {
		return nil
	}
	var st struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if json.Unmarshal(raw, &st) != nil {
		return nil
	}
	for _, j := range st.Jobs {
		if j["work_item_id"] == wi {
			return j
		}
	}
	return nil
}

// assertCapsuleSealedReadOnly walks the preserved scene and pins the
// sealReadOnly contract: regular files 0444, directories 0555, symlinks
// untouched. The capsule's .git is a regular "gitdir: ..." pointer file
// and is sealed like any other file.
func assertCapsuleSealedReadOnly(t *testing.T, capDir string) {
	t.Helper()
	for _, pin := range []string{
		"capsule.json",
		".git", // the regular pointer file, not a directory
		"runtime-state",
		"runtime-state/spawn-journal.jsonl",
		"logs/fakepi.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(capDir, pin)); err != nil {
			t.Fatalf("sealed capsule misses %s: %v", pin, err)
		}
	}
	walked := 0
	err := filepath.Walk(capDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // deliberately untouched by sealReadOnly
		}
		walked++
		if info.IsDir() {
			if info.Mode().Perm() != 0o555 {
				t.Errorf("sealed dir %s: mode %#o, want 555", p, info.Mode().Perm())
			}
			return nil
		}
		if info.Mode().IsRegular() && info.Mode().Perm() != 0o444 {
			t.Errorf("sealed file %s: mode %#o, want 444", p, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk sealed capsule: %v", err)
	}
	if walked == 0 {
		t.Fatal("the sealed-capsule walk covered nothing")
	}
}

// ---------------------------------------------------------------------------
// The scenario.

func TestV1S17SealCrashResume(t *testing.T) {
	inst := newV1Install(t)
	wi := inst.newWI(t, "s17: daemon killed mid-seal, restart finishes the close",
		[]map[string]any{
			{"do": "step", "id": "code"},
			{"do": "wrap"}, // flush(wrap) then park: the agent-driven close
		}, nil)

	// The hold-proxy fronts the daemon's aihub API face; the daemon's
	// git pushes are pinned direct by optServerURL.
	proxy := newHoldProxy(t, inst.url)
	d := newV1Daemon(t, inst, optServerURL(proxy.url()))
	d.start()
	t.Cleanup(func() { killOrphans(wi) })
	d.pf(t, "submit", wi)
	waitUntil(t, 30*time.Second, "agent never spawned", func() bool {
		return d.agentPIDOf(t, wi) > 0
	})
	capDir := d.capsuleDir(wi)
	waitUntil(t, 30*time.Second, "first step never landed", func() bool {
		for _, rec := range fakeJournal(t, capDir) {
			if rec["type"] == "step" && rec["phase"] == "done" {
				return true
			}
		}
		return false
	})

	// Deterministic SEALING window: the agent's flush(wrap) was acked —
	// its seal push landed on aihub's git chain DIRECT (bypassing the
	// proxy, which never sees git traffic) — and the runtime is now
	// blocked inside stopOp at SessionEnd, which the proxy holds in
	// hand. The kill below cannot land anywhere else.
	waitUntil(t, 60*time.Second, "daemon never reached SessionEnd (the proxy saw no session-end POST)", func() bool {
		return proxy.isHeld()
	})
	waitUntil(t, 30*time.Second, "the wrap flush was never acked", func() bool {
		for _, rec := range fakeJournal(t, capDir) {
			if rec["type"] == "flush" && rec["reason"] == "wrap" && rec["ok"] == true {
				return true
			}
		}
		return false
	})

	// Belt and braces: the local record is persisted sealing with the
	// flush's seal knowledge (sealed_push=true) — the exact restart input.
	waitUntil(t, 15*time.Second, "runtime-state.json never showed the sealing record", func() bool {
		rec := daemonStateRecord(t, d.dataDir, wi)
		return rec != nil && rec["state"] == "sealing" &&
			rec["stop_reason"] == "wrapped" && rec["sealed_push"] == true
	})

	// The kill lands strictly before SessionEnd: aihub (asked direct)
	// still says running, and stopOp's killAgentTree — which runs before
	// SessionEnd — has already collected the agent tree.
	if st := inst.wiStatus(t, wi); st != "running" {
		t.Fatalf("aihub status while the session-end is held: %q, want running", st)
	}
	waitUntil(t, 15*time.Second, "the agent tree was not collected before SessionEnd", func() bool {
		return len(orphanPIDs(wi)) == 0
	})

	// SIGKILL the daemon mid-seal, then release the held request (it was
	// never forwarded — aihub never heard a verdict).
	d.crash()
	proxy.drop()

	// Restart the SAME daemon (same socket, data dir, workspace) — now
	// direct to aihub, no proxy in front.
	d2 := &v1Daemon{
		t: t, inst: inst, sock: d.sock, dataDir: d.dataDir, workspace: d.workspace,
		machineID: d.machineID, logPath: filepath.Join(t.TempDir(), "daemon2.log"),
	}
	t.Cleanup(func() { d2.stop() })
	d2.start()

	// The reconciliation finished the interrupted close: SessionEnd
	// landed and aihub reached the terminal verdict.
	inst.waitWIStatus(t, wi, "wrapped", 60*time.Second)

	// The local bookkeeping no longer carries the half-sealed record
	// (dropJob + persist run after the read-only seal), and the
	// restarted daemon's face has no job for the work item.
	waitUntil(t, 30*time.Second, "the sealing record never left runtime-state.json", func() bool {
		return daemonStateRecord(t, d2.dataDir, wi) == nil
	})
	for _, j := range d2.status(t).Jobs {
		if j.WorkItemID == wi {
			t.Fatalf("restarted daemon still tracks %s: %+v", wi, j)
		}
	}

	// The preserved scene is read-only: files 0444, dirs 0555.
	assertCapsuleSealedReadOnly(t, capDir)

	// No re-derivation ever happened: the spawn journal holds exactly
	// the one original derivation, and no orphan survives the cycle.
	spawns := spawnJournal(t, capDir)
	if len(spawns) != 1 {
		t.Fatalf("spawn journal has %d entries, want 1 (no re-derivation): %+v", len(spawns), spawns)
	}
	if spawns[0]["resume"] != false {
		t.Fatalf("the only spawn entry must be the original derivation: %+v", spawns[0])
	}
	waitUntil(t, 30*time.Second, "orphan agents survived the seal-crash cycle", func() bool {
		return len(orphanPIDs(wi)) == 0
	})
}
