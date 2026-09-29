package core

// Core tests: CRUD, claim idempotency, takeover revocation, terminal
// transitions, ready-queue ordering, dependencies and admin faces.
// Every assertion is red when the behavior it names is broken.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// testEnv is a fresh core environment.
type testEnv struct {
	DB      *DB
	Admin   *AdminManager
	WIs     *WIManager
	Owners  *OwnerManager
	UserID  string // admin user id
	UserIDB string // second (writer) user id
	Project string // project id
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	env := &testEnv{
		DB:     db,
		Admin:  NewAdminManager(db),
		WIs:    NewWIManager(db),
		Owners: NewOwnerManager(db),
	}
	u, err := env.Admin.CreateUser("root", "root@example.com", "admin", "human")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	env.UserID = u.ID
	u2, err := env.Admin.CreateUser("second", "second@example.com", "writer", "human")
	if err != nil {
		t.Fatalf("create user 2: %v", err)
	}
	env.UserIDB = u2.ID
	p, err := env.Admin.CreateProject(CreateProjectRequest{Name: "alpha"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	env.Project = p.Project.ID
	return env
}

// mustCreateWI creates a queued REGISTRATION ticket in project alpha
// (no workflow bundle: never claimable, #17).
func mustCreateWI(t *testing.T, env *testEnv, goal string, attrs map[string]any, priority string) string {
	t.Helper()
	if priority == "" {
		priority = api.PriorityNormal
	}
	wi, err := env.WIs.Create(CreateRequest{
		Project:  "alpha",
		Goal:     goal,
		Attrs:    attrs,
		Priority: priority,
		Reporter: env.UserID,
	})
	if err != nil {
		t.Fatalf("create wi: %v", err)
	}
	return wi.ID
}

// mustCreateExecWI creates a queued EXECUTION ticket carrying a workflow
// bundle reference (claimable). A client-pinned digest is included so the
// pin is explicit in tests; the default stand-in path is covered by
// TestWorkItemCRUD.
func mustCreateExecWI(t *testing.T, env *testEnv, goal string, attrs map[string]any, priority string) string {
	t.Helper()
	if priority == "" {
		priority = api.PriorityNormal
	}
	wi, err := env.WIs.Create(CreateRequest{
		Project:      "alpha",
		Goal:         goal,
		Attrs:        attrs,
		Priority:     priority,
		Reporter:     env.UserID,
		BundleName:   strPtrTest("demo-bundle"),
		BundleDigest: strPtrTest("digest-of-demo-bundle"),
	})
	if err != nil {
		t.Fatalf("create exec wi: %v", err)
	}
	return wi.ID
}

func strPtrTest(s string) *string { return &s }

func TestWorkItemCRUD(t *testing.T) {
	env := newEnv(t)
	wi, err := env.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "do the thing", Reporter: env.UserID,
		Labels: []string{"x"}, Priority: api.PriorityHigh,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if wi.Status != api.StatusQueued {
		t.Fatalf("new WI status = %q, want queued", wi.Status)
	}
	if wi.Slug == "" || wi.Slug == "alpha#0" {
		t.Fatalf("slug not assigned: %q", wi.Slug)
	}
	// Get by id and by slug.
	got, err := env.WIs.Get(wi.ID, GetOptions{})
	if err != nil || got.ID != wi.ID {
		t.Fatalf("get by id: %v %v", got, err)
	}
	got, err = env.WIs.Get(wi.Slug, GetOptions{})
	if err != nil || got.ID != wi.ID {
		t.Fatalf("get by slug: %v %v", got, err)
	}
	// Patch.
	newGoal := "do it better"
	updated, err := env.WIs.Patch(wi.ID, map[string]json.RawMessage{
		"goal":   json.RawMessage(`"` + newGoal + `"`),
		"labels": json.RawMessage(`["a","b"]`),
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if updated.Goal != newGoal || len(updated.Labels) != 2 {
		t.Fatalf("patch did not apply: %+v", updated)
	}
	// List filter.
	items, err := env.WIs.List(ListFilters{Project: "alpha", Status: api.StatusQueued})
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v %d", err, len(items))
	}
	// Execution-state attrs are rejected at any depth.
	if _, err := env.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "bad", Reporter: env.UserID,
		Attrs: map[string]any{"custom": map[string]any{"current_step": "x"}},
	}); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "EXECUTION_STATE_REJECTED" {
		t.Fatalf("execution-state attrs not rejected: %v", err)
	}
	// Execution ticket: bundle_name pins the workflow reference; a missing
	// client digest gets the deterministic stand-in (full §4.5 checkout
	// hashing is future work), and the triple is PATCH-immutable.
	exec, err := env.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "exec ticket", Reporter: env.UserID,
		BundleName: strPtrTest("demo-bundle"),
	})
	if err != nil {
		t.Fatalf("exec create: %v", err)
	}
	if exec.BundleName == nil || *exec.BundleName != "demo-bundle" || exec.BundleDigest == nil || *exec.BundleDigest == "" {
		t.Fatalf("bundle reference not persisted: %+v", exec)
	}
	if _, err := env.WIs.Patch(exec.ID, map[string]json.RawMessage{"bundle_name": json.RawMessage(`"other"`)}); !IsCode(err, api.ErrCoreBadInput) {
		t.Fatalf("bundle patch not rejected: %v", err)
	}
	// Empty goal, unknown project.
	if _, err := env.WIs.Create(CreateRequest{Project: "alpha", Goal: "  ", Reporter: env.UserID}); !IsCode(err, api.ErrCoreBadInput) {
		t.Fatalf("empty goal not rejected: %v", err)
	}
	if _, err := env.WIs.Create(CreateRequest{Project: "nope", Goal: "g", Reporter: env.UserID}); !IsCode(err, api.ErrCoreNotFound) {
		t.Fatalf("unknown project not 404: %v", err)
	}
}

func TestClaimIdempotency(t *testing.T) {
	env := newEnv(t)
	id := mustCreateExecWI(t, env, "goal one", nil, "")

	res1, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentClaim, IdempotencyKey: "key-1"}, env.UserID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if res1.Claim.Generation != 1 {
		t.Fatalf("generation = %d, want 1", res1.Claim.Generation)
	}
	if res1.Claim.AttemptID == "" || res1.Credential.Secret == "" {
		t.Fatalf("attempt/credential not minted")
	}
	// #5: the claim response carries the WI's pinned bundle — the runtime
	// materializes the workflow from resp.Claim, not resp.WorkItem.
	if res1.Claim.BundleName != "demo-bundle" || res1.Claim.BundleDigest != "digest-of-demo-bundle" {
		t.Fatalf("claim bundle fields wrong: %+v", res1.Claim)
	}
	if res1.Credential.Username != id {
		t.Fatalf("credential username = %q, want %q", res1.Credential.Username, id)
	}
	// Same key + caller + WI: the same attempt, no new generation.
	res2, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentClaim, IdempotencyKey: "key-1"}, env.UserID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res2.Claim.AttemptID != res1.Claim.AttemptID || res2.Credential.Secret != res1.Credential.Secret {
		t.Fatalf("idempotent replay returned a different attempt")
	}
	// Same key, different caller: rejected.
	if _, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentClaim, IdempotencyKey: "key-1"}, env.UserIDB); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("key reuse not rejected: %v", err)
	}
	// Second claim without a key: already claimed.
	if _, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentClaim}, env.UserIDB); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "ALREADY_CLAIMED" {
		t.Fatalf("double claim not rejected: %v", err)
	}
	// Credential checks.
	if attempt, err := env.Owners.CheckPushCredential(id, id, res1.Credential.Secret); err != nil || attempt != res1.Claim.AttemptID {
		t.Fatalf("valid credential rejected: %v", err)
	}
	if _, err := env.Owners.CheckPushCredential(id, id, "wrong-secret"); !IsCode(err, api.ErrGitUnauthorized) {
		t.Fatalf("wrong secret accepted: %v", err)
	}
	if _, err := env.Owners.CheckPushCredential(id, "otheruser", res1.Credential.Secret); !IsCode(err, api.ErrGitUnauthorized) {
		t.Fatalf("wrong username accepted: %v", err)
	}
}

// TestRegistrationTicketNotClaimable pins #17: bundle_name IS NULL is a
// pure registration ticket — claim and resume both answer CORE-409
// NOT_CLAIMABLE, and it never appears in the queue's executable segments.
func TestRegistrationTicketNotClaimable(t *testing.T) {
	env := newEnv(t)
	id := mustCreateWI(t, env, "registry only", nil, "")
	if _, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentClaim}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "NOT_CLAIMABLE" {
		t.Fatalf("claim of registration ticket allowed: %v", err)
	}
	// resume path: a paused/blocked registration ticket (only reachable
	// through legacy data) is equally refused.
	if _, err := env.DB.Exec(`UPDATE work_items SET status='paused' WHERE id=?1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentResume}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "NOT_CLAIMABLE" {
		t.Fatalf("resume of registration ticket allowed: %v", err)
	}
	// The dependency-blocked diagnosis still wins for an edge case that
	// has both problems (order: blockers, then bundle).
	blocker := mustCreateExecWI(t, env, "blocker exec", nil, "")
	blockedReg := mustCreateWI(t, env, "blocked registry", nil, "")
	if err := env.WIs.AddDependency(blocker, blockedReg, "blocks"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Owners.Claim(blockedReg, ClaimRequest{Intent: IntentClaim}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "DEPENDENCY_BLOCKED" {
		t.Fatalf("dependency diagnosis lost: %v", err)
	}
	// The queue's executable segments never list registration tickets.
	exec := mustCreateExecWI(t, env, "queue exec", map[string]any{"requires_human_session": false}, "")
	mustCreateWI(t, env, "queue registry", map[string]any{"requires_human_session": false}, "")
	q, err := env.WIs.ReadyQueue("alpha")
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if len(q.Items) != 1 || q.Items[0].ID != exec {
		t.Fatalf("executable segment = %+v, want only %s", q.Items, exec)
	}
}

// TestClaimIdempotencyReplayTTL pins #22: the cached response carries the
// attempt's plaintext credential; past the TTL a replay is refused and the
// caller must re-claim with a fresh key.
func TestClaimIdempotencyReplayTTL(t *testing.T) {
	env := newEnv(t)
	id := mustCreateExecWI(t, env, "ttl goal", nil, "")
	res1, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentClaim, IdempotencyKey: "ttl-1"}, env.UserID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	old := idempotencyTTL
	idempotencyTTL = time.Nanosecond
	defer func() { idempotencyTTL = old }()
	// Age the cache row past the window and replay: refused.
	if _, err := env.DB.Exec(`UPDATE claim_idempotency SET created_at=?1 WHERE idempotency_key='ttl-1'`,
		time.Now().UTC().Add(-2*idempotencyTTL).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentClaim, IdempotencyKey: "ttl-1"}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "IDEMPOTENCY_KEY_EXPIRED" {
		t.Fatalf("expired replay not refused: %v", err)
	}
	// A FRESH key claims a new attempt (generation 2).
	idempotencyTTL = 24 * time.Hour
	res2, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentTakeover, IdempotencyKey: "ttl-2"}, env.UserID)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if res2.Claim.Generation != 2 || res2.Claim.AttemptID == res1.Claim.AttemptID {
		t.Fatalf("re-claim did not mint a new attempt: %+v", res2.Claim)
	}
}

func TestTakeoverRevokesOldCredential(t *testing.T) {
	env := newEnv(t)
	id := mustCreateExecWI(t, env, "goal two", nil, "")
	res1, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentClaim, MachineID: "mac-1"}, env.UserID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	res2, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentTakeover, MachineID: "mac-2"}, env.UserIDB)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if res2.Claim.Generation != 2 {
		t.Fatalf("takeover generation = %d, want 2", res2.Claim.Generation)
	}
	if res2.Claim.AttemptID == res1.Claim.AttemptID {
		t.Fatalf("takeover reused the attempt id")
	}
	// THE fencing main layer: the old credential dies at the git face.
	if _, err := env.Owners.CheckPushCredential(id, id, res1.Credential.Secret); !IsCode(err, api.ErrGitUnauthorized) {
		t.Fatalf("OLD credential still valid after takeover: %v", err)
	}
	if _, err := env.Owners.CheckPushCredential(id, id, res2.Credential.Secret); err != nil {
		t.Fatalf("new credential rejected: %v", err)
	}
	// Takeover of a non-running WI is refused.
	id2 := mustCreateWI(t, env, "goal three", nil, "")
	if _, err := env.Owners.Claim(id2, ClaimRequest{Intent: IntentTakeover}, env.UserIDB); !IsCode(err, api.ErrCoreConflict) {
		t.Fatalf("takeover of queued WI allowed: %v", err)
	}
	// The superseded attempt cannot close the WI.
	if err := env.Owners.EndGeneration(id, res1.Claim.AttemptID, 1, "wrapped", ""); !IsCode(err, api.ErrCoreForbidden) {
		t.Fatalf("superseded attempt could close the WI: %v", err)
	}
}

func TestTerminalTransitions(t *testing.T) {
	env := newEnv(t)
	id := mustCreateExecWI(t, env, "goal four", nil, "")
	res, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentClaim}, env.UserID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := env.Owners.EndGeneration(id, res.Claim.AttemptID, 1, "wrapped", "done"); err != nil {
		t.Fatalf("wrap: %v", err)
	}
	wi, err := env.WIs.Get(id, GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if wi.Status != api.StatusWrapped || wi.ClosedAt == nil {
		t.Fatalf("wrap did not terminalize: %+v", wi)
	}
	if attempt, _ := env.Owners.ActiveAttempt(id); attempt != "" {
		t.Fatalf("active attempt survived wrap")
	}
	// Terminal is read-only.
	if _, err := env.WIs.Patch(id, map[string]json.RawMessage{"goal": json.RawMessage(`"x"`)}); !IsCode(err, api.ErrCoreGone) {
		t.Fatalf("terminal patch allowed: %v", err)
	}
	if _, err := env.Owners.Claim(id, ClaimRequest{Intent: IntentResume}, env.UserID); !IsCode(err, api.ErrCoreGone) {
		t.Fatalf("claim on terminal allowed: %v", err)
	}

	// pause → resume → block → cancel cycle on a second WI.
	id2 := mustCreateExecWI(t, env, "goal five", nil, "")
	res2, err := env.Owners.Claim(id2, ClaimRequest{Intent: IntentClaim}, env.UserID)
	if err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if err := env.Owners.EndGeneration(id2, res2.Claim.AttemptID, 1, "paused", ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	wi2, _ := env.WIs.Get(id2, GetOptions{})
	if wi2.Status != api.StatusPaused {
		t.Fatalf("pause status = %q", wi2.Status)
	}
	res3, err := env.Owners.Claim(id2, ClaimRequest{Intent: IntentResume}, env.UserID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res3.Claim.Generation != 2 {
		t.Fatalf("resume generation = %d", res3.Claim.Generation)
	}
	if err := env.Owners.EndGeneration(id2, res3.Claim.AttemptID, 2, api.StatusBlocked, "gate failed"); err != nil {
		t.Fatalf("block: %v", err)
	}
	wi2, _ = env.WIs.Get(id2, GetOptions{})
	if wi2.Status != api.StatusBlocked || wi2.WaitingOn == nil || *wi2.WaitingOn != "owner" {
		t.Fatalf("block mapping wrong: %+v", wi2)
	}
	canceled, err := env.WIs.Cancel(id2, env.UserID)
	if err != nil || canceled.Status != api.StatusCanceled {
		t.Fatalf("cancel: %v", err)
	}
	// Running WI cannot be cancelled directly.
	id3 := mustCreateExecWI(t, env, "goal six", nil, "")
	if _, err := env.Owners.Claim(id3, ClaimRequest{Intent: IntentClaim}, env.UserID); err != nil {
		t.Fatalf("claim3: %v", err)
	}
	if _, err := env.WIs.Cancel(id3, env.UserID); !IsCode(err, api.ErrCoreConflict) {
		t.Fatalf("cancel of running allowed: %v", err)
	}
}

func TestReadyQueueOrdering(t *testing.T) {
	env := newEnv(t)
	// Created in an order that does NOT match priority order; each gets a
	// requires_human_session classification. These are EXECUTION tickets:
	// registration tickets never appear in the executable segments.
	mustCreateExecWI(t, env, "low", map[string]any{"requires_human_session": false}, api.PriorityLow)
	urgID := mustCreateExecWI(t, env, "urgent", map[string]any{"requires_human_session": false}, api.PriorityUrgent)
	normID := mustCreateExecWI(t, env, "normal", map[string]any{"requires_human_session": false}, api.PriorityNormal)
	mustCreateExecWI(t, env, "needs human", map[string]any{"requires_human_session": true}, "")
	mustCreateExecWI(t, env, "unclassified", nil, "")
	// A running and a paused WI for the visibility segments.
	rid := mustCreateExecWI(t, env, "running one", map[string]any{"requires_human_session": false}, "")
	if _, err := env.Owners.Claim(rid, ClaimRequest{Intent: IntentClaim}, env.UserID); err != nil {
		t.Fatalf("claim running: %v", err)
	}
	pid := mustCreateExecWI(t, env, "paused one", map[string]any{"requires_human_session": false}, "")
	if res, err := env.Owners.Claim(pid, ClaimRequest{Intent: IntentClaim}, env.UserID); err != nil {
		t.Fatalf("claim paused: %v", err)
	} else if err := env.Owners.EndGeneration(pid, res.Claim.AttemptID, 1, "paused", ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// A waiting_on-tagged queued WI is not executable.
	wid := mustCreateWI(t, env, "waiting", map[string]any{"requires_human_session": false}, "")
	if _, err := env.WIs.Patch(wid, map[string]json.RawMessage{"waiting_on": json.RawMessage(`"owner"`)}); err != nil {
		t.Fatalf("patch waiting_on: %v", err)
	}

	q, err := env.WIs.ReadyQueue("alpha")
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	// items: exactly the three classified-false executables, ordered
	// urgent → normal → low by priority (created_at is a tiebreak).
	if len(q.Items) != 3 {
		t.Fatalf("items = %d, want 3: %+v", len(q.Items), q.Items)
	}
	if q.Items[0].ID != urgID || q.Items[1].ID != normID {
		t.Fatalf("priority ordering wrong: %s, %s", q.Items[0].ID, q.Items[1].ID)
	}
	if len(q.NeedsHumanSession) != 1 || q.NeedsHumanSession[0].Goal != "needs human" {
		t.Fatalf("needs_human_session segment wrong: %+v", q.NeedsHumanSession)
	}
	if len(q.Unknown) != 1 || q.Unknown[0].Goal != "unclassified" {
		t.Fatalf("unclassified segment wrong: %+v", q.Unknown)
	}
	if len(q.Running) != 1 || q.Running[0].ID != rid {
		t.Fatalf("running segment wrong: %+v", q.Running)
	}
	if len(q.Paused) != 1 || q.Paused[0].ID != pid {
		t.Fatalf("paused segment wrong: %+v", q.Paused)
	}
	// The waiting_on-tagged WI is in NO executable segment.
	for _, seg := range [][]api.WorkItem{q.Items, q.NeedsHumanSession, q.Unknown} {
		for _, wi := range seg {
			if wi.ID == wid {
				t.Fatalf("waiting WI appeared in an executable segment")
			}
		}
	}
	// Unknown project is a clean 404.
	if _, err := env.WIs.ReadyQueue("nope"); !IsCode(err, api.ErrCoreNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

func TestDependencyBlocksClaimAndQueue(t *testing.T) {
	env := newEnv(t)
	blocker := mustCreateWI(t, env, "blocker", nil, "")
	blocked := mustCreateWI(t, env, "blocked", nil, "")
	if err := env.WIs.AddDependency(blocker, blocked, "blocks"); err != nil {
		t.Fatalf("add dep: %v", err)
	}
	q, err := env.WIs.ReadyQueue("alpha")
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	for _, wi := range q.Items {
		if wi.ID == blocked {
			t.Fatalf("dependency-blocked WI appeared in items")
		}
	}
	if _, err := env.Owners.Claim(blocked, ClaimRequest{Intent: IntentClaim}, env.UserID); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "DEPENDENCY_BLOCKED" {
		t.Fatalf("claim of dependency-blocked WI allowed: %v", err)
	}
	// Cycle detection.
	if err := env.WIs.AddDependency(blocked, blocker, "blocks"); !IsCode(err, api.ErrCoreConflict) || ErrorReason(err) != "DEPENDENCY_CYCLE" {
		t.Fatalf("cycle not detected: %v", err)
	}
	third := mustCreateWI(t, env, "third", nil, "")
	if err := env.WIs.AddDependency(third, blocker, "blocks"); err != nil {
		t.Fatalf("legit edge rejected: %v", err)
	}
	if err := env.WIs.RemoveDependency(third, blocker, "blocks"); err != nil {
		t.Fatalf("remove dep: %v", err)
	}
}

func TestAdminFaces(t *testing.T) {
	env := newEnv(t)
	if _, err := env.Admin.CreateProject(CreateProjectRequest{Name: "beta"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := env.Admin.CreateProject(CreateProjectRequest{Name: "beta"}); !IsCode(err, api.ErrCoreConflict) {
		t.Fatalf("duplicate project allowed: %v", err)
	}
	// Machine heartbeat: first registers + issues key; second refreshes.
	h1, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "m1"})
	if err != nil || !h1.Registered || h1.Key == nil {
		t.Fatalf("heartbeat register: %+v %v", h1, err)
	}
	h2, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "m1"})
	if err != nil || h2.Registered || h2.Key != nil {
		t.Fatalf("heartbeat repeat: %+v %v", h2, err)
	}
	users, err := env.Admin.ListUsers()
	if err != nil || len(users) != 3 { // admin + writer + machine
		t.Fatalf("users: %v %d", err, len(users))
	}
	// Rotate identifier: the plaintext changes and is returned once.
	plain1, err := env.Admin.RotateIdentifier("alpha")
	if err != nil || plain1 == "" {
		t.Fatalf("rotate: %v", err)
	}
	plain2, err := env.Admin.RotateIdentifier("alpha")
	if err != nil || plain2 == "" || plain2 == plain1 {
		t.Fatalf("rotate 2: %v", err)
	}
	if _, err := env.Admin.RotateIdentifier("nope"); !IsCode(err, api.ErrCoreNotFound) {
		t.Fatalf("rotate unknown: %v", err)
	}
}
