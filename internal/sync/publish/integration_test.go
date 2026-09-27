package publish_test

// Integration tests against the REAL internal/server over a temp data dir:
// the full publish round-trip through the actual Core accept chain (block
// PUT → manifest POST → HEAD advance → progress read-through), takeover
// fencing, idempotent replay, wrong-parent 409, materialization exclusions,
// symlink-escape refusal, the final-flush evidence chain and end-generation
// validation. These implement, in-process, the 05 §10 categories that are
// testable without a network: 幂等重放 / fencing / 排除清单 / 路径逃逸 /
// 先数据后状态 / flush 证据.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/capsule"
	corecapsule "github.com/silgrid/polyforge/internal/core/capsule"
	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/server"
	"github.com/silgrid/polyforge/internal/sync/aihub"
	"github.com/silgrid/polyforge/internal/sync/freeze"
	"github.com/silgrid/polyforge/internal/sync/publish"
	recoverpkg "github.com/silgrid/polyforge/internal/sync/recover"
	"github.com/silgrid/polyforge/internal/sync/revision"
)

// coreEnv boots the REAL server against a temp data dir (read-only use of
// internal/server: we only import and drive it, never modify it).
type coreEnv struct {
	t       *testing.T
	ts      *httptest.Server
	db      *store.DB
	dataDir string
	admin   string
	writer  string
	client  *aihub.Client
}

func newCoreEnv(t *testing.T) *coreEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc, err := service.Register(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.SeedBase(db); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.MakeDemoBundle(dir); err != nil {
		t.Fatal(err)
	}
	srv := server.New(db, dir, filepath.Join(dir, "workflows"), svc)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	e := &coreEnv{t: t, ts: ts, db: db, dataDir: dir}
	e.admin = e.mintKey("usr_admin", nil)
	e.writer = e.mintKey("usr_worker", nil)
	e.client = aihub.New(ts.URL, e.writer)
	return e
}

func (e *coreEnv) mintKey(user string, project *string) string {
	e.t.Helper()
	plain := fmt.Sprintf("pfk_it_%s_%d", user, os.Getpid())
	sum := sha256.Sum256([]byte(plain))
	var proj any
	if project != nil {
		proj = *project
	}
	if _, err := e.db.Exec(`INSERT INTO api_keys(id, user_id, project_id, key_hash) VALUES (?1,?2,?3,?4)`,
		"key_it_"+user, user, proj, hex.EncodeToString(sum[:])); err != nil {
		e.t.Fatal(err)
	}
	return plain
}

func (e *coreEnv) newWI(t *testing.T) string {
	t.Helper()
	body := `{"project":"p1","goal":"integration","bundle_name":"demo","repo_inventory":[],"repo_inventory_base":[]}`
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/work-items", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.writer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.ID == "" {
		t.Fatal("no wi id")
	}
	return out.ID
}

// pubEnv assembles a real-remote publisher over the core env.
type pubEnv struct {
	*coreEnv
	wi      string
	capsule *capsule.Capsule
	pub     *publish.Publisher
	store   *publish.Store
	stager  *freeze.Stager
	gen     int64
	token   string
}

func newPubEnv(t *testing.T) *pubEnv {
	e := newCoreEnv(t)
	wi := e.newWI(t)
	claim, err := e.client.Claim(wi, "it-key-1", "mac_a")
	if err != nil {
		t.Fatal(err)
	}
	capc, _, err := capsule.Create(t.TempDir(), wi, claim.Generation, "mac_a", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := capc.MarkReady(); err != nil {
		t.Fatal(err)
	}
	daemonLocal := t.TempDir()
	st := publish.NewStore(daemonLocal)
	stager := freeze.NewStager(daemonLocal)
	mutex := freeze.NewManager()
	pub := publish.New(&realRemote{c: e.client}, mutex, stager, st)
	return &pubEnv{
		coreEnv: e, wi: wi, capsule: capc, pub: pub, store: st, stager: stager,
		gen: claim.Generation, token: claim.GenerationToken,
	}
}

// realRemote adapts the aihub HTTP client to publish.Remote against the
// live server.
type realRemote struct{ c *aihub.Client }

func (r *realRemote) PutBlock(ctx context.Context, wi string, seq int64, sha string, block []byte, token string) error {
	return r.c.PutBlock(wi, seq, sha, block, token)
}
func (r *realRemote) PostManifest(ctx context.Context, wi string, m revision.Manifest, sha, token string) (int64, error) {
	return r.c.PostManifest(wi, m, sha, token)
}
func (r *realRemote) GetHead(ctx context.Context, wi string) (*revision.HeadInfo, bool, error) {
	return r.c.GetHead(wi)
}

func (e *pubEnv) writeCapsule(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.capsule.Dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(content), 0o644)
}

func (e *pubEnv) publishReq() publish.Request {
	return publish.Request{
		WorkItemID: e.wi, CapsuleDir: e.capsule.Dir, Generation: e.gen, Token: e.token,
		Conditions: freeze.Conditions{ProcessGroupDead: func() bool { return true }},
	}
}

func (e *pubEnv) flushReq() publish.FlushRequest {
	return publish.FlushRequest{
		WorkItemID: e.wi, CapsuleDir: e.capsule.Dir, Generation: e.gen, Token: e.token,
		Conditions: freeze.Conditions{ProcessGroupDead: func() bool { return true }},
	}
}

// TestRealPublishRoundTrip: block PUT → manifest POST → HEAD advance →
// progress read-through; then an incremental revision; then idempotent
// replay; then wrong-parent 409.
func TestRealPublishRoundTrip(t *testing.T) {
	e := newPubEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1,"current_step":"code"}`)
	e.writeCapsule("artifacts/spec.md", "# spec")

	res, err := e.pub.Publish(context.Background(), e.publishReq())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Published || res.HeadSeq != 0 {
		t.Fatalf("bootstrap: %+v", res)
	}
	// HEAD advanced on the real Core.
	h, found, err := e.client.GetHead(e.wi)
	if err != nil || !found || h.Seq != 0 {
		t.Fatalf("head: %+v %v", h, err)
	}
	// Progress read-through returns the published execution.json.
	_, prog, err := e.client.Progress(e.wi)
	if err != nil {
		t.Fatal(err)
	}
	if prog["current_step"] != "code" {
		t.Fatalf("read-through: %v", prog)
	}

	// Incremental: change + new file + a delete.
	e.writeCapsule("runtime-state/execution.json", `{"version":2,"current_step":"verify"}`)
	e.writeCapsule("artifacts/extra.md", "extra")
	os.Remove(filepath.Join(e.capsule.Dir, "artifacts", "spec.md"))
	res, err = e.pub.Publish(context.Background(), e.publishReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.HeadSeq != 1 {
		t.Fatalf("incremental: %+v", res)
	}

	// Idempotent replay (05 §2.8 scenario one): resubmit the EXACT seq-1
	// manifest the publisher used (read back from the Core's audit copy)
	// even though its parent (0) no longer equals HEAD (1): the
	// idempotency row must hit and succeed WITHOUT a parent check.
	coreM := e.readCoreManifest(t, 1)
	replay := convertCoreManifest(coreM)
	replaySha, err := revision.ComputeManifestSha(replay)
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := e.client.PostManifest(e.wi, replay, replaySha, e.token); perr != nil {
		t.Fatalf("idempotent replay must succeed: %v", perr)
	}
	h2, _, _ := e.client.GetHead(e.wi)
	if h2.Seq != 1 {
		t.Fatalf("replay must not create a second revision: head %d", h2.Seq)
	}

	// Wrong-parent 409: a fresh manifest claiming seq 2 with parent 0.
	state, _ := e.store.LoadState(e.wi)
	scan1 := &revision.ScanResult{Files: state.LiveFiles}
	m2, err := revision.BuildManifest(&revision.ScanResult{Files: map[string]revision.FileInfo{}}, scan1, revision.BuildOptions{
		WorkItemID: e.wi, Seq: 2, Parent: ptr(0), Generation: e.gen, Kind: revision.KindIncremental,
	})
	if err != nil {
		t.Fatal(err)
	}
	sha2, _ := revision.ComputeManifestSha(*m2)
	if _, perr := e.client.PostManifest(e.wi, *m2, sha2, e.token); perr == nil {
		t.Fatal("wrong parent must be SYNC-409")
	} else if !aihub.IsCode(perr, "SYNC-409") {
		t.Fatalf("want SYNC-409, got %v", perr)
	}
}

// convertCoreManifest re-types the Core struct into the daemon wire type
// (identical JSON shape by contract).
func convertCoreManifest(m corecapsule.Manifest) revision.Manifest {
	return revision.Manifest{
		WorkItemID: m.WorkItemID, Seq: m.Seq, Parent: m.Parent, Generation: m.Generation,
		Kind: m.Kind, Ops: convertOps(m.Ops), LiveFiles: m.LiveFiles, DataBytes: m.DataBytes,
		ExecutionStateIncluded: m.ExecutionStateIncluded, BuiltAt: m.BuiltAt,
	}
}

func convertOps(ops []corecapsule.Op) []revision.Op {
	out := make([]revision.Op, 0, len(ops))
	for _, op := range ops {
		out = append(out, revision.Op{
			Op: op.Op, Path: op.Path, Sha256: op.Sha256, Bytes: op.Bytes,
			Mode: op.Mode, Target: op.Target,
		})
	}
	return out
}

// readCoreManifest reads the Core's on-disk manifest audit copy — the same
// bytes the (missing) manifest-content endpoint would return; used here as
// the in-process transport for the recovery gap.
func (e *pubEnv) readCoreManifest(t *testing.T, seq int64) corecapsule.Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.dataDir, "capsules", e.wi, "rev", fmt.Sprint(seq), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m corecapsule.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func ptr(n int64) *int64 { return &n }

// TestRealTakeoverFencing: after admin force-takeover, the OLD generation
// token is refused at the FIRST block PUT (SYNC-403) — no orphan blocks.
func TestRealTakeoverFencing(t *testing.T) {
	e := newPubEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	if _, err := e.pub.Publish(context.Background(), e.publishReq()); err != nil {
		t.Fatal(err)
	}
	// Admin force-takeover: ends the old generation, returns NEW creds.
	adminClient := aihub.New(e.ts.URL, e.admin)
	newClaim, err := adminClient.ForceTakeover(e.wi, "mac_b")
	if err != nil {
		t.Fatal(err)
	}
	// Old token: refused at block upload (05 §2.9 block-stage fencing).
	block, _ := revision.EncodeBlock([]byte("orphan attempt"))
	err = e.client.PutBlock(e.wi, 1, revision.DigestHex([]byte("orphan attempt")), block, e.token)
	if err == nil || !aihub.IsCode(err, "SYNC-403") {
		t.Fatalf("old owner must be fenced at block PUT: %v", err)
	}
	// New token publishes fine.
	e2 := &pubEnv{
		coreEnv: e.coreEnv, wi: e.wi, capsule: e.capsule, pub: e.pub, store: e.store,
		stager: e.stager, gen: newClaim.Generation, token: newClaim.GenerationToken,
	}
	e2.writeCapsule("runtime-state/execution.json", `{"version":2}`)
	res, err := e2.pub.Publish(context.Background(), e2.publishReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.HeadSeq != 1 {
		t.Fatalf("new owner must advance HEAD: %+v", res)
	}
}

// TestRealExclusionsAndSymlinkEscape: materialization targets never enter a
// manifest (SYNC-INV-10); an escaping symlink op is refused by the Core
// accept chain (SYNC-400 PATH_ESCAPE).
func TestRealExclusionsAndSymlinkEscape(t *testing.T) {
	e := newPubEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	e.writeCapsule(".pi/agents/code.md", "agent definition")
	e.writeCapsule("workflow.md", "snapshot")
	e.writeCapsule("harness/pi/session-data/t.jsonl", "opaque session bytes")
	res, err := e.pub.Publish(context.Background(), e.publishReq())
	if err != nil {
		t.Fatal(err)
	}
	m := e.readCoreManifest(t, res.HeadSeq)
	for _, op := range m.Ops {
		for _, banned := range []string{".pi/", "workflow.md", "session-data/"} {
			if strings.HasPrefix(op.Path, banned) || strings.Contains(op.Path, banned) {
				t.Fatalf("excluded path %s leaked into the manifest", op.Path)
			}
		}
	}
	// Escaping symlink: refused at the accept chain.
	mode := int64(420)
	bad := revision.Manifest{
		WorkItemID: e.wi, Seq: res.HeadSeq + 1, Parent: &res.HeadSeq, Generation: e.gen,
		Kind: revision.KindIncremental,
		Ops: []revision.Op{
			{Op: revision.OpSymlink, Path: "repos/r/evil", Target: "../../etc"},
			{Op: revision.OpPut, Path: "runtime-state/execution.json", Sha256: revision.DigestHex([]byte(`{"version":2}`)), Bytes: 12, Mode: &mode},
		},
		ExecutionStateIncluded: true,
	}
	sha, _ := revision.ComputeManifestSha(bad)
	// Block for the exec put must exist for the chain to reach path checks.
	blk, _ := revision.EncodeBlock([]byte(`{"version":2}`))
	if err := e.client.PutBlock(e.wi, bad.Seq, bad.Ops[1].Sha256, blk, e.token); err != nil {
		t.Fatal(err)
	}
	if _, perr := e.client.PostManifest(e.wi, bad, sha, e.token); perr == nil {
		t.Fatal("escaping symlink must be refused")
	}
}

// TestRealFlushEvidenceChain: changed-branch flush publishes and the
// end-generation call with the Daemon-generated evidence is ACCEPTED by the
// real Core validator (02 §6.4); the WI lands in the mapped status.
func TestRealFlushEvidenceChain(t *testing.T) {
	e := newPubEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	if _, err := e.pub.Publish(context.Background(), e.publishReq()); err != nil {
		t.Fatal(err)
	}
	e.writeCapsule("artifacts/late.md", "late change")
	fe, err := e.pub.FinalFlush(context.Background(), e.flushReq())
	if err != nil {
		t.Fatal(err)
	}
	if fe.PublishedSeq != 1 || !fe.FrozenCheckpoint.MatchesHead {
		t.Fatalf("changed flush: %+v", fe)
	}
	if err := e.client.EndGeneration(e.wi, "wrap", "", e.token, fe); err != nil {
		t.Fatalf("real Core must accept the Daemon-generated evidence: %v", err)
	}
	// The WI is wrapped on the Core.
	var status string
	if err := e.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, e.wi).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "wrapped" {
		t.Fatalf("status = %s", status)
	}
}

// TestRealEmptyChainFlush: first claim, no writes, immediate pause: the
// empty-chain sentinel evidence (published_seq=0 + EMPTY_SYNC_BYTES_DIGEST)
// is the ONLY accepted shape without a HEAD.
func TestRealEmptyChainFlush(t *testing.T) {
	e := newPubEnv(t) // claimed, capsule empty, nothing published
	fe, err := e.pub.FinalFlush(context.Background(), e.flushReq())
	if err != nil {
		t.Fatal(err)
	}
	if fe.PublishedSeq != 0 || fe.ManifestSha != "" {
		t.Fatalf("empty chain: %+v", fe)
	}
	if err := e.client.EndGeneration(e.wi, "pause", "", e.token, fe); err != nil {
		t.Fatalf("real Core must accept the empty-chain evidence: %v", err)
	}
	var status string
	if err := e.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, e.wi).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "paused" {
		t.Fatalf("status = %s", status)
	}
}

// TestRealNoChangeFlushDoesNotPublish: unchanged surface → evidence anchored
// at HEAD, no new revision (05 §6.1 branch 2).
func TestRealNoChangeFlushDoesNotPublish(t *testing.T) {
	e := newPubEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1}`)
	if _, err := e.pub.Publish(context.Background(), e.publishReq()); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.client.GetHead(e.wi)
	fe, err := e.pub.FinalFlush(context.Background(), e.flushReq())
	if err != nil {
		t.Fatal(err)
	}
	if fe.PublishedSeq != head.Seq || fe.ManifestSha != head.ManifestSha {
		t.Fatalf("no-change evidence must anchor at HEAD: %+v", fe)
	}
	after, _, _ := e.client.GetHead(e.wi)
	if after.Seq != head.Seq {
		t.Fatal("no-change flush must not publish")
	}
	if err := e.client.EndGeneration(e.wi, "pause", "", e.token, fe); err != nil {
		t.Fatal(err)
	}
}

// TestRealRecoveryRoundTrip: publish from capsule A, reconstruct into a
// fresh capsule B via the recovery replay over the REAL HTTP face — the
// ops-bearing listing (row 19 include=ops, amendment polyforge#14) serves
// manifest content inline, blocks come via row 17 — assert full replacement
// fidelity. This is the §7.4 end-to-end against real stored bytes with
// HTTP as the only transport.
func TestRealRecoveryRoundTrip(t *testing.T) {
	e := newPubEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":1,"current_step":"code"}`)
	e.writeCapsule("artifacts/a.md", "alpha content")
	if _, err := e.pub.Publish(context.Background(), e.publishReq()); err != nil {
		t.Fatal(err)
	}
	e.writeCapsule("artifacts/b.md", "beta content")
	if _, err := e.pub.Publish(context.Background(), e.publishReq()); err != nil {
		t.Fatal(err)
	}
	// Simulate the takeover machine: fresh capsule dir, recovery from HEAD.
	freshDir := t.TempDir()
	capB, _, err := capsule.Create(freshDir, e.wi, e.gen+1, "mac_b", "")
	if err != nil {
		t.Fatal(err)
	}
	rr := &httpRecoveryRemote{c: e.client, stripOps: true}
	res, err := recoverpkg.Recover(context.Background(), capB, rr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Head == nil || res.Head.Seq != 1 {
		t.Fatalf("recovery head: %+v", res.Head)
	}
	raw, err := os.ReadFile(filepath.Join(capB.Dir, "artifacts", "a.md"))
	if err != nil || string(raw) != "alpha content" {
		t.Fatalf("a.md after recovery: %q %v", raw, err)
	}
	raw, err = os.ReadFile(filepath.Join(capB.Dir, "artifacts", "b.md"))
	if err != nil || string(raw) != "beta content" {
		t.Fatalf("b.md after recovery: %q %v", raw, err)
	}
	// execution.json restored byte-exact from the zstd CAS block.
	raw, err = os.ReadFile(filepath.Join(capB.Dir, "runtime-state", "execution.json"))
	if err != nil || !strings.Contains(string(raw), `"current_step":"code"`) {
		t.Fatalf("execution.json after recovery: %q %v", raw, err)
	}
}

// TestRealRecoveryHTTPOnlyReplay: full recovery replay over HTTP-only
// transport using the ops-bearing listing inline (no per-seq fallback):
// bootstrap → incremental (with a delete) → a hand-posted checkpoint, then
// a fresh-capsule reconstruction whose every byte crossed the wire.
func TestRealRecoveryHTTPOnlyReplay(t *testing.T) {
	e := newPubEnv(t)
	e.writeCapsule("runtime-state/execution.json", `{"version":9}`)
	e.writeCapsule("artifacts/a.md", "alpha content")
	if _, err := e.pub.Publish(context.Background(), e.publishReq()); err != nil {
		t.Fatal(err)
	}
	// Incremental: add b.md, drop a.md.
	e.writeCapsule("artifacts/b.md", "beta content")
	os.Remove(filepath.Join(e.capsule.Dir, "artifacts", "a.md"))
	if _, err := e.pub.Publish(context.Background(), e.publishReq()); err != nil {
		t.Fatal(err)
	}
	// Hand-post a checkpoint at seq 2 (full put list of the live set:
	// b.md + execution.json; a.md stays dead).
	shaB := revision.DigestHex([]byte("beta content"))
	shaX := revision.DigestHex([]byte(`{"version":9}`))
	mode := int64(420)
	p1 := int64(1)
	cp := revision.Manifest{
		WorkItemID: e.wi, Seq: 2, Parent: &p1, Generation: e.gen,
		Kind: revision.KindCheckpoint,
		Ops: []revision.Op{
			{Op: revision.OpPut, Path: "artifacts/b.md", Sha256: shaB, Bytes: 12, Mode: &mode},
			{Op: revision.OpPut, Path: "runtime-state/execution.json", Sha256: shaX, Bytes: 12, Mode: &mode},
		},
		LiveFiles: 2, DataBytes: 24, ExecutionStateIncluded: true,
	}
	cpSha, err := revision.ComputeManifestSha(cp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.PostManifest(e.wi, cp, cpSha, e.token); err != nil {
		t.Fatal(err)
	}

	// Fresh takeover machine: every recovery read crosses HTTP only, and
	// the listing carries ops inline (stripOps=false) so the replay never
	// falls back to GetManifest.
	capB, _, err := capsule.Create(t.TempDir(), e.wi, e.gen+1, "mac_c", "")
	if err != nil {
		t.Fatal(err)
	}
	// Local stale file: full replacement must clear it.
	os.MkdirAll(filepath.Join(capB.Dir, "artifacts"), 0o755)
	os.WriteFile(filepath.Join(capB.Dir, "artifacts", "stale.md"), []byte("stale"), 0o644)

	rr := &httpRecoveryRemote{c: e.client}
	res, err := recoverpkg.Recover(context.Background(), capB, rr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Head == nil || res.Head.Seq != 2 {
		t.Fatalf("recovery head: %+v", res.Head)
	}
	// Checkpoint window: a.md (deleted before the checkpoint) and the stale
	// local file must NOT exist; b.md + execution.json must be byte-exact.
	for _, gone := range []string{"artifacts/a.md", "artifacts/stale.md"} {
		if _, serr := os.Stat(filepath.Join(capB.Dir, filepath.FromSlash(gone))); !os.IsNotExist(serr) {
			t.Fatalf("%s must not exist after HTTP-only replay", gone)
		}
	}
	raw, err := os.ReadFile(filepath.Join(capB.Dir, "artifacts", "b.md"))
	if err != nil || string(raw) != "beta content" {
		t.Fatalf("b.md after recovery: %q %v", raw, err)
	}
	raw, err = os.ReadFile(filepath.Join(capB.Dir, "runtime-state", "execution.json"))
	if err != nil || string(raw) != `{"version":9}` {
		t.Fatalf("execution.json after recovery: %q %v", raw, err)
	}
	if res.Restored != 2 {
		t.Fatalf("restored = %d, want 2", res.Restored)
	}
}

// httpRecoveryRemote is the recovery transport over the REAL HTTP face:
// every method goes through the aihub client (rows 16/17/19). stripOps
// drops the inline ops of the listing rows to force the per-seq
// GetManifest fallback (also over HTTP) — used to prove both paths.
type httpRecoveryRemote struct {
	c        *aihub.Client
	stripOps bool
}

func (r *httpRecoveryRemote) GetHead(ctx context.Context, wi string) (*revision.HeadInfo, bool, error) {
	return r.c.GetHead(wi)
}
func (r *httpRecoveryRemote) ListRevisions(ctx context.Context, wi string, since int64) ([]revision.RevisionMeta, error) {
	metas, err := r.c.ListRevisions(wi, since)
	if err != nil {
		return nil, err
	}
	if r.stripOps {
		for i := range metas {
			metas[i].Kind = ""
			metas[i].Ops = nil
		}
	}
	return metas, nil
}
func (r *httpRecoveryRemote) GetManifest(ctx context.Context, wi string, seq int64) (revision.Manifest, error) {
	return r.c.GetManifest(wi, seq)
}
func (r *httpRecoveryRemote) GetBlock(ctx context.Context, wi, sha string) ([]byte, error) {
	return r.c.GetBlock(wi, sha)
}

var _ = sql.ErrNoRows
