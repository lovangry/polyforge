package test

// fault_bitflip_test.go: fault category (d) — block bit-flip between
// write and receive (05 §10 cross-component leg; the CAS integrity
// story of 05 §4.2/§5.1 row 14/15).
//
// Sub-case 1 (in-flight corruption): one byte of the encoded block is
// flipped between the daemon-side write and the Core's receive. The
// Core's block PUT verifies the digest of the DECOMPRESSED original
// against the presented sha on every PUT (fix #6) and answers the typed
// SYNC-400 DIGEST_MISMATCH, writing nothing; the manifest that
// references the sha is then refused SYNC-400 BLOCK_MISSING, and HEAD
// does not advance. The next REAL daemon publish of the same content
// converges (the fault left no partial state).
//
// Sub-case 2 (at-rest corruption after a good publish): one byte of the
// STORED CAS block flips after the publish succeeded. Every receive
// side detects it: the Core's progress read-through answers the typed
// CORE-504 (the block no longer decodes), and the recovery replay
// (05 §7.4) verifies every byte against the manifest's sha and aborts
// with a typed error — no torn rebuild, no HEAD change.

import (
	"context"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/sync/revision"
)

func TestFaultInjectBlockBitFlip(t *testing.T) {
	core := newCoreEnv(t)
	mach := newDaemonEnv(t, core)
	mach.serve()
	pf := mach.pf()

	wi := core.newWI(t, "fault: block bit-flip between write and receive")
	capc, _ := mach.claimWorkingJob(wi)
	writeCapsuleFile(t, capc, "artifacts/flip.txt", "original content\n")
	if _, err := pf.PublishRequest(wi, "step_completed"); err != nil {
		t.Fatalf("working publish: %v", err)
	}
	headSeq, headSha := core.headRow(wi)
	token := core.signToken(wi, 1, "mac_a") // the daemon's in-memory credential
	client := core.client(core.workerKey)

	// --- sub-case 1: flip one byte of the block IN FLIGHT. ---
	content := []byte("flipped in transit\n")
	sha := revision.DigestHex(content)
	encoded, err := revision.EncodeBlock(content)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte deep inside the compressed body (past the magic).
	corrupt := append([]byte(nil), encoded...)
	corrupt[10] ^= 0x40
	// The corrupted upload is refused by the receive-side digest check,
	// before anything is written.
	err = client.PutBlock(wi, 1, sha, corrupt, token)
	ae := mustBeAPIError(t, err, "SYNC-400", "DIGEST_MISMATCH")
	if !strings.Contains(ae.Detail, "zstd") && !strings.Contains(ae.Detail, "sha256") {
		t.Logf("digest mismatch detail: %s", ae.Detail)
	}
	if _, serr := statFile(core.casPath(wi, sha)); serr == nil {
		t.Fatal("the refused block must not exist in the Core CAS")
	}
	// The manifest referencing the never-stored sha is refused and HEAD
	// does not advance (block integrity is the last gate before the row).
	man := manifestFor(wi, 1, headSeq, sha, content)
	msha, err := revision.ComputeManifestSha(man)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PostManifest(wi, man, msha, token)
	mustBeAPIError(t, err, "SYNC-400", "BLOCK_MISSING")
	if hseq, hsha := core.headRow(wi); hseq != headSeq || hsha != headSha {
		t.Fatalf("HEAD must not advance past a corrupted publish: (%d,%s)", hseq, hsha)
	}
	// Convergence: the next real daemon publish of the good content lands.
	writeCapsuleFile(t, capc, "artifacts/flip.txt", "flipped in transit\n")
	writeExecution(t, capc, wi, 1, "code", 2)
	if _, err := pf.PublishRequest(wi, "step_completed"); err != nil {
		t.Fatalf("converging publish after the corruption: %v", err)
	}
	if hseq, _ := core.headRow(wi); hseq != headSeq+1 {
		t.Fatalf("the converging publish must advance HEAD once: %d", hseq)
	}

	// --- sub-case 2: flip one byte of the STORED HEAD block after the good
	// publish. The receive sides must detect it, with typed errors. The
	// flipped block is HEAD's runtime-state/execution.json block — the
	// exact block both the progress read-through and the recovery replay
	// must consume.
	var execSha string
	for _, op := range opsOfHead(t, core, wi, headSeq+1) {
		if op.Path == "runtime-state/execution.json" {
			execSha = op.Sha256
		}
	}
	if execSha == "" {
		t.Fatal("HEAD manifest must reference execution.json")
	}
	stored := core.casPath(wi, execSha)
	if !flipByte(stored, 10) {
		t.Fatal("could not flip the stored block")
	}
	// Core progress read-through: the corrupted block no longer decodes.
	_, _, err = client.Progress(wi)
	if err == nil {
		t.Fatal("the read-through must refuse to render a corrupted HEAD")
	}
	pe := mustBeAPIError(t, err, "CORE-504", "")
	if !strings.Contains(pe.Detail, "undecodable") && !strings.Contains(pe.Detail, "unparsable") {
		t.Fatalf("read-through corruption detail: %s", pe.Detail)
	}
	// Recovery replay (05 §7.4): every restored byte is verified against
	// the manifest's sha; the corrupted block aborts the rebuild with a
	// typed error — and never materializes corrupt bytes: whatever the
	// rebuild managed to write before the abort carries only verified
	// content.
	recDir, rerr := recoverInto(ctxBg(), t, core, wi, 1, token)
	if rerr == nil {
		t.Fatal("remote site reconstruction must refuse a corrupted block")
	}
	if !strings.Contains(rerr.Error(), "undecodable") && !strings.Contains(rerr.Error(), "sha mismatch") {
		t.Fatalf("recovery corruption detail: %v", rerr)
	}
	if raw, rerr2 := readFileIfExists(recDir, "runtime-state/execution.json"); rerr2 == nil {
		// If the entry was applied before the abort, it must be intact
		// (decoded, well-formed) — a corrupted byte never survives the
		// per-file verification.
		if !strings.Contains(string(raw), "\"work_item_id\"") {
			t.Fatalf("no corrupt byte may materialize in the rebuild: %s", raw)
		}
	}
	// And the corruption never touched the chain: HEAD is unchanged.
	if hseq, hsha := core.headRow(wi); hseq != headSeq+1 || hsha == "" {
		t.Fatalf("HEAD must be untouched by the receive-side refusal: (%d,%s)", hseq, hsha)
	}
}

// manifestFor builds the minimal manifest the daemon-side publisher would
// produce for one put op at seq (parent = current HEAD).
func manifestFor(wi string, seq, parent int64, sha string, content []byte) revision.Manifest {
	mode := int64(0o644)
	m := revision.Manifest{
		WorkItemID: wi, Seq: seq, Parent: &parent, Generation: 1,
		Kind:      revision.KindIncremental,
		LiveFiles: 2, DataBytes: int64(len(content)),
		ExecutionStateIncluded: true,
		BuiltAt:                "2026-01-01T00:00:00Z",
	}
	m.Ops = []revision.Op{{Op: revision.OpPut, Path: "artifacts/flip.txt", Sha256: sha,
		Bytes: int64(len(content)), Mode: &mode}}
	return m
}

func ctxBg() context.Context { return context.Background() }

// recoverInto runs the recovery replay into a fresh capsule (05 §7.4)
// and returns the capsule dir plus the abort error, if any.
func recoverInto(ctx context.Context, t *testing.T, core *coreEnv, wi string, gen int64, token string) (string, error) {
	t.Helper()
	ws := t.TempDir()
	boot := bootstrapFromCore(ctx, t, core, core.workerKey, wi)
	capc, _, err := capsuleCreate(ws, wi, gen, "mac_b", boot.BundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.CheckoutInventory(boot); err != nil {
		t.Fatal(err)
	}
	_, rerr := recoverReplay(ctx, capc, core.client(core.workerKey))
	return capc.Dir, rerr
}
