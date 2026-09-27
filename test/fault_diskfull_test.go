package test

// fault_diskfull_test.go: fault category (a) — disk-full on the publish
// stage (02 §10 disk-full category, cross-component daemon leg).
//
// Injection: RLIMIT_FSIZE — a bounded writer that errors at N bytes
// (writes past N fail with EFBIG; Go ignores SIGXFSZ, so the process
// survives exactly like a daemon hitting a full disk). N is armed above
// every file the flow legitimately writes at that moment (Core DB, WAL,
// daemon state, capsule) but far below the new artifact's block, so the
// FIRST write the publish flow attempts past N is the staging pin of the
// new big block (05 §2.7 window action step 2).
//
// Expected: the publish fails with the typed daemon envelope; NO partial
// HEAD (Core's head row unchanged; no block reached the Core CAS); no
// undecided pending publish record (it is written only after staging);
// and the next publish after the fault converges — proving the failure
// left no half-state behind.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/silgrid/polyforge/internal/daemon"
	"github.com/silgrid/polyforge/internal/sync/publish"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

func TestFaultInjectDiskFullOnPublishStage(t *testing.T) {
	core := newCoreEnv(t)
	mach := newDaemonEnv(t, core)
	mach.serve()
	pf := mach.pf()

	wi := core.newWI(t, "fault: disk-full on the publish stage")
	capc, _ := mach.claimWorkingJob(wi)
	writeCapsuleFile(t, capc, "artifacts/first.md", "small file\n")
	if _, err := pf.PublishRequest(wi, "step_completed"); err != nil {
		t.Fatalf("bootstrap publish: %v", err)
	}
	headSeq, headSha := core.headRow(wi)

	// Arm-point snapshot: the largest file the components have written so
	// far (Core DB + WAL, daemon state, the capsule's current content) —
	// taken BEFORE the big artifact exists. Any legitimate in-window write
	// stays below N+margin; only the big staging pin can cross it.
	limit := largestFileUnder(t, core.dataDir, mach.home) + 64<<10

	// The WI Agent produces one big artifact (32 MiB): the next publish
	// must stage + upload it.
	const bigSize = 32 << 20
	writeCapsuleFile(t, capc, "artifacts/big.bin", strings.Repeat("B", bigSize))
	// Re-stamp execution.json so the manifest carries it too (SCAN-INV-6:
	// data files first, execution.json last).
	writeExecution(t, capc, wi, 1, "code", 2)
	if limit >= bigSize {
		t.Fatalf("injection limit %d not below the big artifact %d", limit, bigSize)
	}
	armBoundedWriter(t, limit)

	// The publish under the fault: typed error envelope, nothing partial.
	code, raw := udsBody(t, mach.socket, "POST", "/jobs/"+wi+"/publish_request",
		map[string]any{"reason": "step_completed"}, nil, nil)
	if code != 500 {
		t.Fatalf("disk-full publish must fail, got %d %s", code, raw)
	}
	var env errEnvelope
	json.Unmarshal(raw, &env)
	if env.Error.Code != "DAEMON-500" || !strings.Contains(env.Error.Detail, "file too large") {
		t.Fatalf("typed error envelope: %+v (raw %s)", env.Error, raw)
	}

	// No partial HEAD: the Core head row and its CAS are untouched.
	if hseq, hsha := core.headRow(wi); hseq != headSeq || hsha != headSha {
		t.Fatalf("HEAD must not advance on a failed publish: (%d,%s) != (%d,%s)",
			hseq, hsha, headSeq, headSha)
	}
	// No undecided pending record: staging fails BEFORE SavePending
	// (SYNC-INV-8's record exists before the first PUT, and no PUT ran).
	if rec, err := (publish.NewStore(mach.cfg.DataDir)).LoadPending(wi); err != nil || rec != nil {
		t.Fatalf("no pending record may survive the failed staging: %+v %v", rec, err)
	}
	// The ring carries the honest sync_failed event for live observers.
	obs := observeRaw(t, mach, wi)
	if !strings.Contains(obs, daemon.EvSyncFailed) {
		t.Fatalf("the ring must record sync_failed: %s", obs)
	}

	// Disarm; the next publish converges on the same content.
	disarmBoundedWriter(t)
	raw, err := pf.PublishRequest(wi, "step_completed")
	if err != nil {
		t.Fatalf("retry publish after the fault: %v", err)
	}
	var pub daemon.PublishOutcome
	json.Unmarshal(raw, &pub)
	if !pub.Published || pub.HeadSeq != headSeq+1 {
		t.Fatalf("converging publish: %+v (raw %s)", pub, raw)
	}
	// The bytes that reached the Core are the FULL original content —
	// the partial staging file from the fault never leaked.
	if hseq, hsha := core.headRow(wi); hseq != pub.HeadSeq || hsha != pub.ManifestSha {
		t.Fatalf("core head after convergence: (%d,%s) vs (%d,%s)", hseq, hsha, pub.HeadSeq, pub.ManifestSha)
	}
	for _, op := range opsOfHead(t, core, wi, pub.HeadSeq) {
		if op.Op != "put" || op.Path != "artifacts/big.bin" {
			continue
		}
		block, err := core.client(core.workerKey).GetBlock(wi, op.Sha256)
		if err != nil {
			t.Fatal(err)
		}
		content, err := revision.DecodeBlock(block)
		if err != nil {
			t.Fatal(err)
		}
		if len(content) != bigSize || strings.Trim(string(content), "B") != "" {
			t.Fatalf("the converged block must be the full original bytes: %d bytes", len(content))
		}
	}
}

// observeRaw fetches the layer-1 observe stream (events + snapshot) as raw
// JSON, for substring assertions on the ring.
func observeRaw(t *testing.T, m *daemonEnv, wi string) string {
	t.Helper()
	_, raw := udsBody(t, m.socket, "GET", "/jobs/"+wi+"/observe?after=0", nil, nil, nil)
	return string(raw)
}

// largestFileUnder walks the Core data dir and the machine home and
// returns the largest existing file size (the injection limit must sit
// above everything the flow may legitimately rewrite).
func largestFileUnder(t *testing.T, dirs ...string) int64 {
	t.Helper()
	var max int64 = 4096
	for _, dir := range dirs {
		filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() && fi.Size() > max {
				max = fi.Size()
			}
			return nil
		})
	}
	return max
}

// armBoundedWriter lowers RLIMIT_FSIZE: every write extending a regular
// file past n bytes fails with EFBIG (a bounded writer at n bytes).
func armBoundedWriter(t *testing.T, n int64) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	newLimit := syscall.Rlimit{Cur: uint64(n), Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &newLimit); err != nil {
		t.Skipf("cannot arm RLIMIT_FSIZE on this system: %v", err)
	}
	t.Cleanup(func() { disarmBoundedWriter(t) })
	savedLimit = old
}

var savedLimit syscall.Rlimit

// disarmBoundedWriter restores the pre-test RLIMIT_FSIZE.
func disarmBoundedWriter(t *testing.T) {
	t.Helper()
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &savedLimit); err != nil {
		t.Logf("restore RLIMIT_FSIZE: %v", err)
	}
}
