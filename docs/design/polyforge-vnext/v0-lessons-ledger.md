# v0 Lessons Ledger — every real-machine failure v0 ever hit
# (source: pilot history, HANDOFF, ticket records; the authoritative
# registry the v1 implementation should have been audited against)

## Protocol/harness class
P1  Real pi rejects invented RPC envelopes (v0 first pilot ticket:
    "Unknown command: undefined"). v1 spawn envelope must be the v0
    live-tested prompt-command/record-stream shape.
P2  Probe argv must be faithful: --no-tools was NOT a real pi flag (v0
    probe found it; removed). Never invent harness flags.
P3  PI_SESSION_DIR does NOT control pi's transcript location. Real
    transcript: ~/.pi/agent/sessions/<dashify(cwd)>--/<ts>-<uuid>.jsonl.
    pi CAN resume from any .jsonl path via --session.
P4  cc protocol: UUID sessions, stream-json bidirectional stdin,
    --resume <uuid>, zero-turn crash fallback (same UUID fresh).
P5  WI Agent inherits the operator's GLOBAL pi config unless isolated:
    skills/context-files/prompt-templates (3 flags), env (PF_*/
    POLYFORGE_* strip + secret value strip), and ambient MCP/extension
    registrations leak other systems' toolfaces (the operator's
    mcp-adapter.json). --no-extensions must STAY absent (subagent tool
    is extension-provided) — residual exposure closed by instruction.
P6  Model selection is per-step bundle business (model_candidates with
    ordinal advancement on INFRA failure only — business FAIL never
    advances). The agent must not silently run on the operator's
    global default model/settings (settings pollution: e.g. max_tokens
    values the operator set for another provider break the inherited
    provider with 400s).

## Provider/infra class
I1  Provider 400/503/connection-hangs are normal weather. Detection:
    message.stopReason=="error" + errorMessage records.
I2  Candidate advancement must be bounded and the ordinal persisted
    in execution state (crash-safe).
I3  wall_minutes parsing: float vs integer mismatch made budget gates
    misfire (v0 #46).
I4  Wait, hang and stream windows need injectable bounds or tests
    take hours (v0 shipped parameterized windows).

## Sync/ship class
S1  Ship once pushed the BASE sha instead of the sealed HEAD (v0 #46
    era). Ship must verify remote branch == sealed head.
S2  Sync roots excluded repos/ and release trees from the capsule
    revision chain (v1: .gitignore equivalents).
S3  Stale-generation quarantine: pushes with superseded credentials
    must be fenced server-side (attempt-scoped credentials; takeover
    revokes).
S4  Publish/flush semantics: wrap/shutdown flushes are blocking with
    typed timeout (60s); shutdown with dead agents must not wedge
    (v0 #32: 20min wedge -> 5min bounded shutdown budget).

## Daemon/lifecycle class
D1  Dead agent + detached bash descendants escape the process group —
    six-layer cleanup with (pid,starttime) guards; PF_WI_ID env marker.
D2  Agent death mid-answer-relay: answers must not be lost — idempotent
    3-state ledger, failure never consumes the answer id; new attempt
    must see the pending question (re-injection).
D3  Restart must reconcile with aihub before reviving (wrapped -> don't
    revive; running -> kill+recover; blocked -> leave).
D4  Claim idempotency: re-sending the same key returns the same attempt
    (network retries); takeover requeue idempotency.
D5  Stale claim detection: a crash between claim and spawn must not
    orphan a generation forever (v0 known gap #Start-crash).
D6  needs_input had NO proactive owner notification in v0 (watcher
    scripts filled the gap). v1 answer: check at turn start / events
    file — the events file exists; owner-facing surface?
D7  Shutdown sequence: stop_notice -> flush(shutdown) -> seal; never
    let observe re-derive an agent after seal.

## Data/capsule class
C1' (v0 lesson, superseded by v1 C1 ruling) transcripts are valuable:
    in v1 they must actually REACH the capsule git (sync mechanism was
    the missing piece — see P3).
C2  Work-item context delivery: v0 delivered goal/content via daemon
    proxy_call (read_workflow_ref). When a contact surface is deleted,
    enumerate the business it carried before deleting it.
C3' execution.json is the only progress authority; Version must bump
    on every change the daemon should observe.
C4  Digest integrity: bundle digest pinned at claim must be verifiable
    (v0 verified server-side; v1 wires it at materialize).

## Process class
X1  Smoke commands given to agents need timeout + explicit stdin close
    (#31 40min wedge).
X2  pkill/kill argv self-match traps: always script files, never
    inline patterns.
X3  One-shot agents must not start background tasks; tests run in
    foreground.
X4  The memory ledger exists precisely to prevent re-hits — every
    implementation/review lane MUST recall it first (v1 lanes did not;
    that is why F1/F3/F4 happened).

## v1 pilot findings (F1–F7) — the lessons the ledger itself predicted

X4 said every implementation/review lane MUST recall this ledger first; the
v1 pilot lanes did not, and every one of these findings is a re-hit of an
entry above. Recorded here so no future lane re-derives them.

F1  Goal never reached the agent. v1 deleted v0's daemon proxy_call
    (read_workflow_ref) without enumerating the business it carried (C2):
    with no agent→server read path at all, the WI agent could not even know
    its own goal. Fixed: the claim answer's task definition is materialized
    in full as runtime-state/work-item.json (the ONE authoritative statement
    of the ask inside the capsule) and recovery repairs a missing copy.
    Fixed in main (v1-fix-pilot1, merged as 1059da5 / pilot commit 39dfd1c).

F2  Slug-built git paths. A submitted "<project>#<seq>" spelling leaked into
    the push remote URL (a 404 — "#" is the URL fragment separator) and the
    capsule directory name. Fixed: the claim answer's canonical work-item id
    is the authority; every path (capsule dir, external git-dir, push
    remote, session id, spawn journal) is keyed by it, the submitted
    spelling is registered as an alias only, and Restart migrates
    pre-canonical residue in place. Fixed in main (v1-fix-pilot1, 1059da5).

F3  Transcripts never reached the capsule. The v0-ported PI_SESSION_DIR env
    is dead for transcript placement (P3): a fresh derivation writes to the
    harness's own session store, nothing landed in capsule git (C1'). Fixed
    daemon-side: locate the live transcript after spawn, copy it into the
    capsule at every commit boundary (before the commit, so it rides the
    chain), and resume from the capsule copy with the live file as fallback.
    Fixed in main (v1-fix-pilot1, 1059da5).

F4  The candidate chain was a decoration and settings pollution was open
    (P6 + I2). The bundle's model_candidates were never consumed: the spawn
    pinned no model (the agent silently ran on the operator's global
    default), the ordinal was neither advanced nor persisted, business
    FAIL/rewind semantics were not wired (INV-1g), and the observe face's
    StepView.Model rendered permanently empty. Fixed: the minimal
    frontmatter read surface (defaults + per-step model_candidates) is back
    in common/workflow; materialize extracts the defaults chain onto the
    job; every derivation spawn is pinned to the candidate at the persisted
    ordinal (argv — measured: capsule-local project settings are
    trust-gated OFF in rpc mode, see spawn.go buildCmd); ONLY a
    provider-class recovery advances the ordinal (bounded, wraps —
    exhaustion is not terminal) and the advance is crash-safe in
    state.json; the executor records the effective model into
    steps.<id>.model and the observe face renders it.
    Fixed in this lane (v1-lessons-fix, base 1059da5).

F5  Fatal provider error 处置与 idle 看门狗（I1 re-hit）：fatal 级 provider
    错误此前按普通中断处理（不自恢复、不能及时判死），长静默步无看门狗。
    Fixed: fatal provider error 即刻恢复 + idle watchdog（10 分钟窗口 +
    probe）。已修，main d84115b（分支 v1-f5-watchdog，branch commit
    2a0ab4a）。

F6  Goal 携带的 recall 指令不可执行（X4 re-hit）：agent 无 agent→server
    memory 读路径，无法先读台账再动手。Fixed: materialize 时静态记忆快照 +
    UDS 窄通道 recall/remember + CLI memory 动词（`pf recall` /
    `pf remember`）。已修，main c740fb0（分支 v1-f6f7，commit e0db578）。

F7  步定义物化错位与完成标记缺失（03-execution-capsule §4.5 re-hit）：
    物化把步定义复制到 capsule `agents/` 而非 Pi 发现目录 `.pi/agents/`，
    且未写完成标记。Fixed: `.pi/agents` 物化目标目录 + 完成标记
    `sync/materialized.json`（RelMaterializeMarker）+ trust gate
    （snapshot 仅在 marker 完整且匹配 pinned bundle 身份时可信）。已修，
    main c740fb0（分支 v1-f6f7，commit e0db578）。
