# 06 — Memory / Search：Markdown 权威、FTS 索引、启动期对账

> 现行文档（2026-10-16 repo review wave 4 重写为只描述代码现状）。原文描述的 HTML 派生链、`backupBarrier`
> 与一致性快照、`PATCH /memories`、分享链接、wrap 摘要条目、`MEM-501` 等**从未建成或已删除**，原文见 git 历史
> （本文件 2026-10-16 之前的版本）。代码权威：`internal/aihub/memory`（`store.go`、`authority.go`、`reconcile.go`）、
> `internal/aihub/server/server.go` 的 memory 路由、`internal/polyforge/cli/verbs_aihub.go`、`internal/polyforge/runtime/recall.go`。
> 一页式摘要见 `reference.md#§9`。

## 1. 范围 <!-- §1 -->

Memory 是 Core 域数据（项目知识沉淀）：一条 memory = 一个权威 Markdown 文件 + 一行元数据 + 一行 FTS 索引。
写入只有创建与 redact（软删除）两种；**没有更新路径**（改内容 = 新建一条，必要时 redact 旧的）。检索只有
FTS5 trigram 文本检索，没有向量/语义层。

## 2. 不变量 <!-- §2 -->

- INV-7a 写序：① 新内容写同目录临时文件 `memory.md.tmp` 并 fsync → ② 一个事务提交元数据行 + FTS 行
  （FTS 先删后插）→ ③ rename 覆盖权威 `memory.md`（`Store.Create`）。redact：① 目录 rename 进 `_redacted/` →
  ② 同一事务提交 `status='redacted'` + 删除 FTS 行。
- INV-7c 文件为准：元数据行与权威文件不一致时，以文件纠正行（`reconcile.go`）。
- INV-7f 服务前校验：读取在该条目的锁下（`memLock`，与写路径同一把）校验行的 `content_sha` 与现读的权威
  文件一致，不一致不以该状态服务并就地对账——持锁保证在飞的 ②→③ 窗口不会被误判为崩溃残留。

## 3. 文件布局与 frontmatter <!-- §3 -->

```
<aihub-data-dir>/memory/<project_id>/<mem_id>/memory.md            # 权威内容：frontmatter + 正文
<aihub-data-dir>/memory/<project_id>/_redacted/<mem_id>/memory.md  # redact 归位
```

frontmatter 与 `memories` 表列一一镜像，按固定顺序序列化（`authority.go` `frontmatter`）：`id`、`type`、
`owner_user_id`、`title?`、`tags?`、`visibility`、`work_item_id?`、`project_id`、`idempotency_key?`、`related?`、
`created_at`、`updated_at`。`id` 由服务端分配（`mem_` 前缀，与目录名一致）。行的 `title` 是文件的投影
（`memory/title.go` `rowTitle`）：frontmatter 有非空白 `title` 时原样使用，否则从正文派生——第一个非空行，
去掉行首 markdown 标记（`#`、`>`、`-`/`*`/`+`、`[ ]`、`1.`/`1)`），跳过代码围栏行与分隔线，按字符边界截到
80 字符（截断时末尾为 `…`）。派生标题只写入行，不回写 frontmatter。`visibility` ∈
`private|project|team|public|admin`。正文上限 1 MiB（`MEM-413`）。

## 4. 对账（启动期，后台） <!-- §4 -->

aihub 启动后在后台运行 `Store.RebuildIndex`（`cmd/aihub/main.go`；先开始服务，完成时记一行日志）：清扫
`memory.md.tmp` 残留；文件有、行缺 → 从 frontmatter 回填；行有、文件无 → 删行（创建在 ② 与 ③ 之间崩溃）；
已移入 `_redacted/` 但未提交的 redact → 向前补完。按新鲜度戳增量执行（polyforge#129）：每行记录写入时
权威文件的 mtime+size，戳一致即跳过该条目（不读、不哈希、不写索引）；戳不一致或缺失才按内容 sha 校验。
对账开始时先做标题回填（`backfillTitles`）：`title` 为空/空白的 active 行，文件内容 sha 与行一致时按
`rowTitle` 补上标题，分批小事务写入；已补的行不再被选中，不会重复读写。仍会在每次启动被重新选中并读文件的
只有补不上的行：权威文件缺失、内容 sha 与行不一致（交给随后的对账按文件修正，修正时同样派生标题），或正文没有
可用的行。没有在线重建端点。

## 5. API 与错误码 <!-- §5 -->

### 5.1 API <!-- §5.1 -->

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/api/v1/memories` | 项目 writer/maintainer 或 admin，否则 `MEM-403`；key 限定在其他项目 → `AUTH-403`；项目不存在 → `CORE-404` | body `{project, type, title, visibility, content, tags, related, work_item_id, idempotency_key}`；`work_item_id` 接受 id 或 `<project>#<seq>`，须同项目；`title` 缺省或空白时由服务端从正文派生（§3）；同一 `idempotency_key`（同 owner、同项目）重放返回原条目，键属于他人或他项目 → `MEM-409 IDEMPOTENCY_KEY_REUSED` |
| GET | `/api/v1/memories/search` | 任意 key；结果按可见性过滤 | `q`（必填）、`match`（`all` 缺省 / `any`，其他值 → `MEM-400`）、`type`（精确值，或 `X.*` 前缀匹配 `X.`）、`tags`（逗号）、`limit`（缺省 50；≤0 或 >200 时取 50）、`project`（先过读权限再作过滤）、`excerpt`（>0 时每条带 `content_excerpt`，上限 8192 字符）；只返回 `status='active'` |
| GET | `/api/v1/memories/{id}` | 读权限；无权与不存在同为 `MEM-404` | 元数据；`?format=content` 返回原始 Markdown 正文 |
| POST | `/api/v1/memories/{id}/redact` | 先过读门（`memRead`）：无读权限 → `MEM-404`；能读但非 owner 且非 admin → `MEM-403` | 软删除；非 active → `MEM-409` |

检索有两种模式，唯一入口 `Store.SearchMode`（`server.go` 按 `match=` 选择；membership / 可见性 / `type` / `tags` 过滤与 `limit` 在两种模式下完全相同，`project=` 在两种模式下都在候选 SQL 内收窄）：

- **`match=all`（缺省，`Store.Search`）**：每个词都必须命中——≥3 字符的词作为 FTS5 trigram 短语 AND 起来，更短的词走子串匹配（中文短词常见）。适合 2–4 个关键词的短查询。
- **`match=any`（`memory/search_any.go` `anyTerms`）**：查询转小写，按词字符（字母、数字、组合附标 `unicode.IsMark`、`_`）的连续段切分，段内在 CJK 与非 CJK 文字切换处再切开。
  - 非 CJK 段是一个词：保留 3–40 字符且不在停用词表（英文虚词 + markdown/URL 噪声）中的词。
  - CJK 段（汉字、平假名、片假名、谚文）是没有词界的分句：切成重叠的 3 字符片段（「重试阶梯」→ 重试阶、试阶梯）；不足 3 字符的段不产生任何项；40 字符上限不适用于 CJK。
  - 去重后取至多 32 项：在词池（最长优先）与 CJK 片段池（各分句轮流取）之间交替，任一池用完由另一池补满，互不挤占。
  - 各项加引号以 OR 连接，按 bm25 × 新鲜度排序（见下）；只检查该顺序的前 max(100, 10×limit) 个候选，不再翻页（AND 模式翻页到底）。过滤后没有可用项 → 无命中（不报错）。用于长的描述性查询（领取时 recall）。

两种模式的排序相同（`memory/freshness.go`）：`w = exp(-age_days / stability_days)`，`age_days` 按
`memories.updated_at`（权威文档最后修订时间；没有更新端点，API 写入的条目等于 `created_at`）到查询时刻计算；
`score = rank × (1 + 0.5 × w)`（rank 为 FTS5 bm25，越负越好），升序，平分时按 `updated_at` 降序（按解析后的时刻比较——整秒与带小数秒的 RFC3339 并存；解析不了的视为最旧）、再按 id。
stability：`rule.*`、`doc.design`、`doc.decision` 不衰减（w = 1）；`doc.review`、`doc.audit` 及其他 `doc.*`
（时点快照）180 天；`experience.*` 90 天；`fact.*` 与 `methodology.*` 180 天；其他类型 90 天。`updated_at`
解析不了按完全陈旧（w = 0），未来时间按 age 0（w ≤ 1）。新鲜度最多把相关度放大 1.5 倍：相近的命中里新的在前，相关度高出 1.5 倍以上的旧条目始终排在
较弱的新条目之前。只有短词（无 rank）的子串扫描按 w 降序。实现上（`ftsPage`）先只从 FTS 索引读出全部命中的
rowid 与 rank，再沿 rank 顺序分批读取条目元数据，直到下界 (1+0.5)×rank 已劣于当前第 offset+limit 名，正文只读
本页；结果与对全部命中精确排序相同。

### 5.2 错误码 <!-- §5.2 -->

`MEM-400` 输入不合法（缺 `q`/`type`/`content`、可见性越界、frontmatter 不合法）；`MEM-403` 无写权限 / 能读但非 owner
的 redact；`MEM-404` 不存在或无权读（含 redact 时无读权限）；`MEM-409` 幂等键复用、redact 非 active 条目；`MEM-413` 正文超 1 MiB；
`MEM-500` 文件系统/事务失败；`MEM-502` 对账遍历/清扫失败；`MEM-503` FTS 查询/扫描失败（常量见 `internal/common/api/errors.go` `ErrMem*`）。另：key 限定在其他项目 → `AUTH-403`；
创建时项目不存在 → `CORE-404`。

## 6. 可见性 <!-- §6 -->

先按 project membership 过滤、再按可见性（`server.go` `memAllowed`）：`admin` 只给 admin；`private` 只给 owner
与 admin；`project`/`team`/`public` 给该项目任意有读权限的成员（三者当前行为相同）与 admin。

## 7. 备份 <!-- §7 -->

`pf backup` → `POST /api/v1/admin/backup`（admin）只对 SQLite 做 `VACUUM INTO <data-dir>/backups/aihub-<ts>.db`
（`core/admin.go`）：**不含** memory 文件与 git 仓库，也不与写路径互斥。memory 文件可由对账从 Markdown 重建
索引，但 Markdown 本身只在完整备份里：生产完整备份由 `.polyforge/deploy.sh` 的在线备份完成（`docs/upgrade.md`）。

## 8. CLI 与领取时快照 <!-- §8 -->

- `pf recall --project P <query> [--any] [--type T|X.*|*] [--tags] [--limit 1..200，缺省 20]`：`--project` 必填且须在本机
  `[workspace]` 映射中；以 `excerpt=120` 调 search，缺省 `match=all`，`--any` 改用 `match=any`。`pf remember --project P --type T [--title] [--visibility project]
  [--tags] [--work-item] <content|-|@file>`。`pf memory show <id> [--max-bytes 262144] [--json]`。
  （`cli/verbs_aihub.go`；fragment：`skills/polyforge/verbs/{recall,remember,memory}.md`。）
- 领取时 daemon 以 WI 的 goal + content 的前 1500 字符为查询，用 `match=any`、限定该 WI 的项目，取前 10 条（`-recall-topk`，上限 50）写入
  capsule `runtime-state/recalled-memories.json`（`{work_item_id, query, top_k, items[{id,type,title,excerpt}], notice,
  created_at}`，摘录 1200 字符）；出错或无命中时写空列表 + notice，从不让领取失败（`runtime/recall.go`、
  `daemon/memface.go`）。driver 把它作为「非约束性历史」注入每个 step 的 prompt（`v2-converge.md#§1`）——每条只渲染一行
  `[type] title`（不含 id 与正文；title 为空时取摘录的第一个非空行），丢弃 id-only 噪声标题
  （`^Verification Report — \S+$`、`^Controller-sink artifact`），至多 10 行，无条目则不加该节（`driver/spawn.go`）。
  部署顺序：先升级 aihub 再升级 daemon——旧 aihub 忽略 `match=`、按 AND 检索，长查询于是没有命中，快照为空。
- step agent 不持有 aihub 凭据路径，记忆访问全部由 daemon 代理（owner 裁决 option C）。step 进程带 `PF_WI_ID` /
  `PF_DAEMON_ID` 标记（与 step-origin 守卫、reaper 用的是同一组；其余 `PF_*` / `POLYFORGE_*` 被剥掉），`pf` 据此进入
  step 模式（`cli/stepctx.go`）：`pf recall` / `pf memory show` 走 daemon UDS 的只读面
  `GET /step/memories/search`、`GET /step/memories/{id}`（`daemon/stepmem.go`）——daemon 以自己的机器凭据转发，
  work item 取自调用方的 UDS 对端凭据（`runtime.StepProcessOrigin`），项目取自该 job、不取自请求（请求里别的项目 →
  DAEMON-403，别项目的记忆 → MEM-404）；GET 是读，过 step-origin 守卫，非 step 调用方被拒（人直接读 aihub）。
  `pf remember` 不联网：写一个文件到 capsule `artifacts/memory-outbox/<n>.md`（frontmatter type/title/tags + 正文，
  `capsule/memoryoutbox.go`）。其余直连 aihub 的动词（file、queue、cancel、管理面…）在 step 内一律 DAEMON-403，
  该次调用的 aihub client 拒绝一切请求，`cli.toml` 的 `api_key` 不被使用。`cli.toml` 仍在共享 HOME 下可读——这是防误用，
  不防恶意。
- outbox 上传（`daemon/memoutbox.go`）：每个 step 结果（`step_done` / `gate_verdict`，后台）、workflow 终态（有界等待，
  保证封存前上传）和每次操作者 close（wrap / fail / pause，teardown 前）都 flush 一次。校验：type `experience.<kind>`、
  单行 title、正文 ≤2KB、tag 为普通名字、无疑似凭据内容（私钥头、常见 token 前缀、`key=…` 形式、daemon 自己的 key）；
  每次 flush 至多一条非 wrap 记忆，每个 WI 至多一条 `experience.wrap`。去重：同 WI 已上传/待传的同 title，及 aihub 里同
  WI、同 type、同 title 的记忆。上传时 `work_item_id` = job 的 WI、`project` = job 的项目。结果逐行写入
  `artifacts/memory-log.md`；文件移到 `pending/`（已接受待传）、`done/`（已传或重复）、`rejected/`（拒绝，log 写原因）。
  aihub 故障（传输错误、5xx、408、429）保留在 `pending/`，下次 flush 重试；4xx 为终局拒绝。上传失败从不让 step 失败。
  WI 在仍有待传文件时被封存：留在封存 capsule 的 `pending/`，封存后不再 flush（尽力而为）。
- 每个 step 的 prompt 带共享的记忆职责节（`workflows/_shared/memory.md`，变量
  `{{project}}` `{{work_item}}` `{{record}}` `{{skip}}` 经 `workflow.RenderStepVars` 渲染）：开工前用 2–3 组不同关键词
  （含中英文）`pf recall --any`，读相关条目正文；收工前按记忆策略判断，至多 `pf remember` 一条 `experience.<kind>`
  （正文 ≤2KB，不写密钥；写前 recall 查重可选，去重在 daemon 侧），不在 `record` 里但有价值的照写并打 `policy-proposal`
  标签、正文加「建议新增条目：…」。workflow 的最后一步另带 `workflows/_shared/memory-wrap.md`：每个 WI 恰好一条
  `experience.wrap`（title `<wi slug>: <做了什么>`，正文：做了什么、关键决定、遗留，并列出本 WI 的 `policy-proposal` 记忆）。
- 记忆策略：`.polyforge.yaml` 的可选段 `memory_policy:`（`default` / `<project>`: `{record: [...], skip: [...]}`；
  `default` 未声明的列表取内置默认，项目的列表追加到 `default` 之后、去重）。领取时 daemon 把该项目的生效策略写入
  capsule `runtime-state/memory-policy.json`（`runtime/memorypolicy.go`），driver 渲染进记忆职责节；缺段＝内置默认
  （`workspace.DefaultMemoryPolicySection`），从不拒绝领取；段格式错误同整个文件无效一样拒绝领取（DAEMON-414）。
  `pf init` 在缺段时把默认段以文本追加到文件末尾（不重写原有字节）；`pf doctor` 只读：无文件 pass（内置默认），
  文件缺段 warn 并提示 `pf init`，有段 pass。
