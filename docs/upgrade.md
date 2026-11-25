# Upgrade guide

How a deployment moves from one Polyforge release to the next: the aihub
server, its database and the work machines. A release that needs more than
installing it adds one section at the end of this file, and a released
section is never rewritten: the file is append-only, its sha256 pinned by
`test/w254_docs_set_acceptance_test.go`.

The server goes first: deploy the new aihub (`pf aihub`), then upgrade every
work machine with the installer (`tools/install.sh`; see
[install](install.md)).

## v0.2.0, the first public release: workflow contract 1

v0.2.0 starts this file. It is the first release of the unified product:
one `pf` binary (the server is `pf aihub`), the workflow lanes, the skills
and Pi Web, published together as one GitHub Release. Its workflow contract
is **contract 1**: every built-in lane declares `contract: 1`, and the
daemon implements contract 1 alone and refuses any other value (`WFC-101`);
[the workflow contract reference](reference/workflow-contract.md) is its
rule. Changes made before this release are in the git history, not here.

Moving from the earlier public v0.1.x tags is a drain and a reinstall, in
this order:

- **Drain the work machines.** On every work machine still on v0.1.x, stop
  claiming (`pf ops auto off`) and let every work item in flight finish, or
  cancel it (`pf cancel <wi>`). A work item claimed under v0.1.x cannot
  continue under v0.2.0: its capsule holds a v0.1.x `workflow.md` lane,
  which v0.2.0 refuses (`WFC-101`). One left over parks blocked with kind
  `contract`, naming this section; cancel it and file it again.
- **The server.** `pf aihub` replaces the separate `aihub` binary of v0.1.x
  (the image `ghcr.io/silgrid/polyforge-aihub:<tag>` runs it). The server never
  migrates its database at start, and a database written by v0.1.x lacks
  one schema change v0.2.0 needs: the messaging read cursor is split into a
  delivery cursor and a read cursor. With the server stopped and `aihub.db`
  backed up, run these statements as one transaction (SQLite 3.35 or later,
  for `DROP COLUMN`), then start v0.2.0 on the database:

  ```sql
  ALTER TABLE conversation_members ADD COLUMN delivered_seq INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE conversation_members ADD COLUMN read_seq INTEGER NOT NULL DEFAULT 0;
  UPDATE conversation_members SET delivered_seq = last_read_seq, read_seq = last_read_seq;
  ALTER TABLE conversation_members DROP COLUMN last_read_seq;
  ```

  These are `deploy/sql/v2.20.7/upgrade.sql`; the same directory's
  `probe.sql` answers 0 when a database already carries the split, and its
  `rollback.sql` reverses it (the backup is the simpler way back). Apply
  them by hand on this move: the deploy's SQL step selects only the
  directories of the release tags between the running version and the
  target, and no public tag carries this one.
- **The work machines.** Re-run the installer on each machine, the same
  command as a first install; add `--with-pi-web` to install Pi Web from
  the same release (Pi Web ships only with the GitHub Release). v0.2.0
  keeps a machine's state per Cell, one per AIHub profile under
  `<data-dir>/cells/<profile>/`, and its daemon refuses the v0.1.x data dir
  until it is migrated once, with the daemon stopped: `pf migrate
  cell-layout` (the plan; it writes nothing), `pf migrate cell-layout
  --apply`, then `pf daemon ensure`.
- **The workspace yaml.** A `.polyforge.yaml` that still carries the root
  `summarize:` or `memory_policy:` section is refused: `pf config migrate`
  moves both into cli.toml and deletes them from the yaml, leaving a
  `<file>.bak-<UTC timestamp>` copy of every file it changes. A per-step
  key must name `default`, `task` or a step of an installed lane; one that
  names a step of a v0.1.x lane is stale and governs nothing. `pf doctor`'s
  `workspace yaml` row names each stale key: move its models to a step that
  exists, or delete it.
- **The lanes.** The built-in lanes are `develop` (the default development
  lane), `micro`, `integrate` and `release`; v0.1.x's `autopilot`,
  `bug-fix`, `chore` and `feature-full` are gone. A queued work item bound
  to one of them is refused at the claim and stays queued (`WFC-123`;
  `pf doctor`'s `workflow lanes` row lists it): re-bind it while it is
  queued and never claimed (`pf file edit <wi> --bundle develop`), or
  cancel it. A lane you wrote yourself becomes a directory under
  `<workspace>/.polyforge/workflows/<lane>/` holding `workflow.yaml` (the
  former `workflow.md` frontmatter, with `contract: 1`), `README.md` (its
  prose) and `steps/<id>.md`; a `workflow.md` is refused with that rewrite.
  Check each lane with `pf workflows validate <lane-dir>`;
  [bundles](bundles.md) teaches the format.
- **Finish.** `pf ops auto on` on each work machine, then `pf doctor`.

## v0.2.1, the project deactivation status

v0.2.1 adds the project's global **deactivation** state: a maintainer or
admin deactivates a project (`pf projects deactivate <id-or-name>`), the
server then refuses every write that does work on it — creating or
modifying work items, comments, submits and claims (the auto-claim
included), resume execution, memory writes — with the typed
`PROJECT_DEACTIVATED` error, while reads keep working under the existing
permissions. Nothing is deleted: only the hard delete
(`pf projects delete`, unchanged) removes data. `pf projects restore`
brings the project back. The CLI, Pi Web and the AIHub console hide
deactivated projects from their lists by default and offer a restore
entry in the same place. Deactivation is refused (CORE-409, the blocking
work items listed) while the project still has queued, running, paused
or blocked work items.

A database written by v0.2.0 or earlier lacks the `projects.status`
column; the new server refuses to serve projects until the migration
below has run. Both v0.2.0 and v0.2.1 are public tags, so an unattended
`deploy.sh` from v0.2.0 applies it inside the switch window
(`deploy/sql/v0.2.1/`); there is nothing to run by hand on that move:

```sql
ALTER TABLE projects ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
  CHECK (status IN ('active','deactivated'));
```

The migration is purely additive: every existing project answers
`active` (the only state a pre-v0.2.1 database can hold), and no row is
rewritten or lost. The same directory's `probe.sql` answers 0 once the
column exists (the run-once guard), and its `rollback.sql` drops the
column again — deactivation state is operational, not durable data; a
retry upgrade re-adds it with every project `active`.

## v0.2.1, the pre-submit analysis check

v0.2.1 also adds the pre-submit analysis check (the `pf submit` one, the
`verbs/submit.md` skill): an execution ticket on a lane that requires
acceptance — `develop` and `micro` — carries a `## 分析` (or `## Analysis`)
section with content before it can be submitted, the daemon's
auto-claim skips a ticket without one the same way it skips a ticket
without acceptance, and `pf queue` marks it 需要交互：未分析.

The upgrade impact: a develop or micro work item filed before v0.2.1 has
no `## 分析` section. After the upgrade `pf submit` refuses such a ticket
(exit 1, nothing claimed; `--allow-no-analysis` bypasses it only on the
owner's explicit word), and the auto-claim does not take it. Before turning the auto-claim back on
(`pf ops auto on`), run `pf queue` and settle every queued execution
ticket: write the analysis report into its `## 分析` section, or the
owner's dated 「免分析（<date>, <who>）」 line, or cancel the ticket. A
ticket already in flight is untouched — the check runs at submit and
claim, never mid-run.

This section also corrects the one above: the server runs no startup
check for the `projects.status` migration. Until the migration has run,
the requests that touch the missing column fail with SQLite's own errors
(no typed refusal); serve traffic only after the migration, which an
unattended `deploy.sh` applies inside its switch window.

## v0.2.1, the scope engine in Python

The release-shipped verification-scope engine,
`workflows/_shared/scripts/verify-scope`, is rewritten in Python: the same
path, the same command line and the same report, with one implementation of
the `carries:` header parse shared with the step scripts' `pfstep` library —
the shell implementation is gone, with no forwarding stub. The upgrade
impact: a host that runs a repository's bar on the host (a tree without
`.polyforge/Dockerfile.verify`) now needs `python3` for every engine run,
not only for `--json` — the documented `pf doctor` prerequisite already
names python3, and a bar in a container is unaffected (the verification
image provides it).

Two rules that lived in the workflow's own text become declarations of each
repository's `.polyforge/verify-scope.conf`: the light-tier prefix an
exceeded `e2e_start_threshold` puts before `e2e_runner=` is the declared
`e2e_start_prefix=` (pi-web declares its current
`E2E_SERVER_MODE=start`), and `facts.py build --replay` classifies by the
repository-declared `not_runnable_rule=<toolchain>|<rule>|<pattern>` lines
(pi-web declares its browser preflight's text). An engine older than
this section reading a conf that declares the keys diagnoses them as
unknown and ignores them, and an older facts.py simply skips keys it does
not read, so a conf may declare them before the release ships; after the
upgrade the older hardcodes are gone, so a repository that wants the
behavior declares the key.

## v0.2.1, the integrate full gate runs every command

The integrate lane's full-gate step gates a batch with `--keep-going`:
the gate runs every command of the full bar, never stopping at the first
failing one, so one red batch records every failing command — none is
left `not_run` — and the triage names each command's own failed units at
once. A red that used to wait a whole round behind the first one (the
commands after it never ran) now shows in the same batch; the cost is the
longer red run, the commands after the first red running too. The
script's default is unchanged — every other caller of `gate.py` (the
release's, an operator's run) still stops at the first failing command
unless it passes `--keep-going` itself — and the develop and micro
lanes' verification is untouched: develop's `verify.py` runs its whole
plan, as before, and micro's evidence (`run.py`) still stops at its
first failing line.

## v0.2.1, units already red before the change

`facts.py build --replay` understands a unit whose reproduction test
already exists and is red before the change (an integration fix ticket:
the test came with the batch, the fix is the change): the unit declares
`"pre_existing_red": true` in `artifacts/build/units.json` and reports no
`test_commit`; the engine replays its recorded run on the tree of the
change's base (the merge-base with the fetched default branch) — red
there is the unit's red evidence, which the result records, and a run
that exited 0 there fails the unit (the test was green before the
change, so it was not already red; review classes that `test_weakened`).
Everything else is unchanged: an ordinary unit keeps its test commit and
the four requirements of the test-first shape, and verification still
runs every recorded run green on the final tree.

## v0.2.1, baseline-complete empty unit declarations

A build may record an unchanged baseline with an empty `artifacts/build/units.json` only when sibling `change-analysis.json` declares `baseline_complete`: a non-empty justification of at most 500 characters and non-empty per-AC criteria with integer `ac` values and existing `impl` and `tests` paths. The declaration is mutually exclusive with non-empty units and `pre_existing_red`, and never substitutes for an acceptance criterion that is not implemented. For this self-referential change, the owner rulings of 2026-11-21T08:30:41Z and 08:43:02Z authorize repository-script self-validation; ordinary tickets still require the normal red-then-green unit evidence.

## v0.2.1, no-forge repositories are merged locally

The merge step now creates and pushes a no-ff merge commit for repositories without a forge, recording `how: merged` and `merge_commit` in the landing record.

## v0.2.1, local landing follows the capsule baseline

For a local non-bare origin, the merge step now advances the capsule's `origin/HEAD` baseline in the origin-side worktree rather than pushing a hosting service. If the owner has another branch checked out, a temporary detached worktree advances only the baseline ref. Dirty changes are preserved through a stash retry when necessary; a conflicting stash stays available by its recorded object ID, while landing still succeeds with `stash_conflict: true`. Bare or remote origins use push. Landing records `worktree_synced`, and repositories with nothing to publish record an empty published branch and land as `how: nothing` with an empty merge commit.

## Capsule build-cache warmup

Repositories can opt into best-effort warmup with `.polyforge/capsule-cache.conf`.
See the configuration reference for `node_modules`, `build_paths`, and offline
`install` fallback behavior. The feature is optional and failures do not block
materialization.

## v0.2.1, the merge step lands each PR as one squash commit

`gh pr merge --squash --match-head-commit <head>` replaces the no-ff merge commit for forge repositories (a no-forge repository gets `git merge --squash` plus one explicit commit in its baseline worktree): each PR lands as exactly one new commit on the default branch's first-parent line, its message the PR title, which publish.json now carries as `title` (a no-forge entry without one is a usage error, exit 2). The landing record writes `how: squash` for the step's own landing. A PR the step finds already MERGED, squashed on the default branch with no branch side, is taken as `how: merged_elsewhere` (`accepted: true`, `by_this_run` false) when the commit's tree is byte-identical to the verified head's tree — the content the lane verified, re-committed; any drift keeps the refusal, as does a MERGED answer that names no merge commit on the read-back.

## v0.2.1, a no-forge head already on the remote baseline

For a repository without a forge whose verified head is already contained by
`origin/<base>` (landed by hand, or by an earlier run), the merge step no
longer re-merges an empty no-op: it judges the containment against the fetched
remote-tracking branch (`git branch -r --contains`, never the capsule's local
branch) and records the landing as it stands — `how: merged` with no
`by_this_run` and the verified head itself as `merge_commit`. Before, that
corner claimed `by_this_run: true` with the baseline tip as the merge commit.

## v0.2.1, capsule clones run the target repo's commit-msg hooks

A repository that ships `.husky` (husky's convention, the usual commitlint setup) now has its hooks wired into the capsule clone at materialize time: `core.hooksPath` points at the clone's own `.husky` directory, so every commit the step agent makes in the clone runs the target repo's own commit-msg validation. A message the repo's commitlint rejects fails the commit loudly — and a hook whose runtime is missing (husky needs `node_modules`; the preheat of polyforge#26 provides it) fails the same way, never silently passes. The daemon's hardened task-repo reads keep their `core.hooksPath=/dev/null` pin: only the agent's working clone is wired.

## v0.2.1, the commit message language follows the repository

A capsule's messages in a task repository now follow the repository's own language, read from the repository's own facts: the last 50 commit bodies on its default branch — Chinese when they carry more Chinese characters than Latin letters, an exact tie keeping English, so an English-primary repository is byte-for-byte unchanged (this repository included). The repo's conventional prefix (its type-enum word, `test:`, `feat:`, …) is kept verbatim, never translated, and the repo's own commit-msg validation stays the judge of type-enum and header length. The same reading governs the sync merge message (`合并 main 到 <branch> (sync)` in a Chinese-primary repository, the English bytes unchanged otherwise), the publish PR title's one line and the landing squash's message (the PR title verbatim, both languages), and the capsule's own test/impl commit bodies — the build and publish skills teach the rule. A Chinese-primary repository's message line that carries no Chinese character at all is refused by the shared helpers (`workflows/_shared/scripts/pfstep/commitlang.py`, `commitmsg.py`). The machine messages (sync, landing) keep running through the hardened no-hooks git: the language follows the repo, the hooks path never does.

## v0.2.1, the skills teach the commit-message language

The build and publish skills now teach the commit-language rule where the capsule's messages are made: a task repository's own language — read from its own facts by `workflows/_shared/scripts/pfstep/commitlang.py` — governs the capsule's test and impl commit bodies and the publish PR title's one line (the conventional prefix kept verbatim, the header length the repository's own commitlint's business), and a refusal from the repository's commit-msg hook is fixed in the message, never routed around (`--no-verify` and a `core.hooksPath` of one's own joined build.md's Never list). The rule is pinned by a whole-repo contract test, `test/commit_language_contract_test.go`, registered in the contract-scan fast check's `-run` alternation.

## v0.2.1, a project repository pinned to a development branch

A task repository can be registered to a branch: `pf projects patch <id-or-name> --repos name=url#branch` (the fragment on the URL, the last `#` splitting; a trailing `#` that names no branch is refused at registration) pins the repo, and the pin rides the whole lane — the capsule clone stands on it (`git clone --branch`), sync merges the pin in (a repo pinned to `feat-cloud` never takes in `main`'s moves), verification measures the baseline against it (`merge-base HEAD origin/<pin>`), publish opens the PR against it (a no-forge repo's local landing targets it) and the merge step's squash lands on it, with the origin's advertised default branch left untouched. The claim carries the pin in capsule.json's `repo_branches`, and a pin naming a branch the origin does not carry fails the claim itself (DAEMON-507: the pin is a fact, not a hint) — the capsule restore re-attaches to the pin by name after a crash. A repo registered without a fragment keeps today's behavior exactly: the clone checks out the origin's default and every step's baseline is the default branch, and such a capsule's capsule.json stays byte-identical to before (the fact is omitted, never written as an empty object).

## v0.2.1, the capsule's message language follows the work item's goal

The lane's messages in a task repository now follow the work item's own goal, never the repository's history: the publish PR title's one line is written in Simplified Chinese by default (a goal written in English keeps the line English; identifiers, paths, type prefixes and proper nouns keep their own spelling), and the same reading takes the sync merge message and the capsule's own test and impl commit bodies — a repository whose commits are all English is no reason to write this one in English, closing the drift loop the old reading fed (English landings pushing the ratio further English). The goal is read from the capsule's runtime-state/work-item.json by `workflows/_shared/scripts/pfstep/commitmsg.py`'s `goal_language` (Chinese when the goal carries a Chinese character, English when it does not); `workflows/_shared/scripts/pfstep/commitlang.py` and its repository-history reading (the last 50 default-branch bodies' Chinese-to-Latin ratio) are gone. The repo's conventional prefix (its type-enum word, `test:`, `feat:`, …) is kept verbatim, the repo's own commit-msg validation stays the judge of type-enum and header length, and the machine messages (sync, landing) keep running through the hardened no-hooks git: the language follows the goal, the hooks path never does.
