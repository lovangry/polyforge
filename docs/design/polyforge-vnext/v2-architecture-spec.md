# v2 Architecture Spec — Design Record

> Status: RULED (human-approved). This is the authoritative summary of the v2 architecture
> redesign. The full specification lives in the work item (polyforge#61 content field).
> Implementation is decomposed into 9 follow-up tickets (§13 of the spec).

## Scope

Replaces the custom publish engine with git, reorganizes the pf binary into
message-driven Runtime + Observe modules, and simplifies the control protocol
from 5 methods to 4. Validated through three review rounds (two model reviews
of the git proposal, one review of the message-driven model).

## Key Decisions

1. **Revision chain = git history**: capsule is a git worktree (`.gitignore`
   excludes `repos/` and `artifacts/release-*/`); aihub hosts bare repos per
   WI served via git-http-backend proxy through existing HTTP auth.

2. **git-dir outside capsule**: `--git-dir=/var/lib/polyforge/git/wi_xxx.git`
   prevents subagent hook injection (RCE via `.git/hooks/`).

3. **Control protocol = 4 methods**: `flush(reason)`, `answer`, `stop_notice`,
   `probe`. `quiesce`/`resume` deleted (step boundaries are natural pause
   points). `step_notify`/`publish_request` deleted (daemon watches
   `execution.json`).

4. **Agent is zero-blocking during normal execution**: writes execution.json,
   continues. Observe detects changes and triggers async git commit+push.

5. **Recovery engine** (renamed from "dual-track"): cascading resume→fresh→bounded
   fail. Discriminator: transcript available (not same/cross-machine).

6. **Message-driven Runtime/Observe**: 5 messages with attempt-id fencing.
   Runtime = lifecycle (start/stop/recover). Observe = per-WI monitoring goroutine.

7. **Transcripts NOT in git** (secret scanner would block wraps): side-channel
   upload to aihub `sessions/wi_xxx/`.

8. **Process cleanup**: 6 layers, cgroup v2 as layer 0 (atomic kill of all
   descendants). Layers 1-5 as fallback for non-cgroup environments.

9. **Module reorg**: `internal/aihub/` + `internal/polyforge/` + `internal/common/`.
   Dependency direction enforced by Go import paths.

10. **Code reduction**: ~17000 lines → ~5900 lines (est. 65% reduction).

## Implementation Tickets (§13 of full spec)

1. aihub git proxy + pre-receive hooks + reconciler
2. Runtime module (Start/Stop/Recover/Restart + process cleanup)
3. Observe module (monitor goroutine + events + answer relay)
4. Control protocol rework (flush(reason) + probe + answer + stop_notice)
5. pf-execute skill revision
6. Recovery engine (resume/fresh/session.json extension)
7. Module reorganization (9 packages → 3 directories)
8. Regression rewrite + new scenario e2e
9. Migration plan (SQLite revision chain → git revision chain)
