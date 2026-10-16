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
`created_at`、`updated_at`。`id` 由服务端分配（`mem_` 前缀，与目录名一致）。`visibility` ∈
`private|project|team|public|admin`。正文上限 1 MiB（`MEM-413`）。

## 4. 对账（启动期，后台） <!-- §4 -->

aihub 启动后在后台运行 `Store.RebuildIndex`（`cmd/aihub/main.go`；先开始服务，完成时记一行日志）：清扫
`memory.md.tmp` 残留；文件有、行缺 → 从 frontmatter 回填；行有、文件无 → 删行（创建在 ② 与 ③ 之间崩溃）；
已移入 `_redacted/` 但未提交的 redact → 向前补完。按新鲜度戳增量执行（polyforge#129）：每行记录写入时
权威文件的 mtime+size，戳一致即跳过该条目（不读、不哈希、不写索引）；戳不一致或缺失才按内容 sha 校验。
没有在线重建端点。

## 5. API 与错误码 <!-- §5 -->

### 5.1 API <!-- §5.1 -->

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/api/v1/memories` | 项目 writer/maintainer 或 admin，否则 `MEM-403`；key 限定在其他项目 → `AUTH-403`；项目不存在 → `CORE-404` | body `{project, type, title, visibility, content, tags, related, work_item_id, idempotency_key}`；`work_item_id` 接受 id 或 `<project>#<seq>`，须同项目；同一 `idempotency_key`（同 owner、同项目）重放返回原条目，键属于他人或他项目 → `MEM-409 IDEMPOTENCY_KEY_REUSED` |
| GET | `/api/v1/memories/search` | 任意 key；结果按可见性过滤 | `q`（必填）、`match`（`all` 缺省 / `any`，其他值 → `MEM-400`）、`type`（精确值，或 `X.*` 前缀匹配 `X.`）、`tags`（逗号）、`limit`（缺省 50；≤0 或 >200 时取 50）、`project`（先过读权限再作过滤）、`excerpt`（>0 时每条带 `content_excerpt`，上限 8192 字符）；只返回 `status='active'` |
| GET | `/api/v1/memories/{id}` | 读权限；无权与不存在同为 `MEM-404` | 元数据；`?format=content` 返回原始 Markdown 正文 |
| POST | `/api/v1/memories/{id}/redact` | 先过读门（`memRead`）：无读权限 → `MEM-404`；能读但非 owner 且非 admin → `MEM-403` | 软删除；非 active → `MEM-409` |

检索有两种模式，唯一入口 `Store.SearchMode`（`server.go` 按 `match=` 选择；membership / 可见性 / `type` / `tags` 过滤与 `limit` 在两种模式下完全相同，`project=` 在两种模式下都在候选 SQL 内收窄）：

- **`match=all`（缺省，`Store.Search`）**：每个词都必须命中——≥3 字符的词作为 FTS5 trigram 短语 AND 起来，更短的词走子串匹配（中文短词常见）。适合 2–4 个关键词的短查询。
- **`match=any`（`memory/search_any.go` `anyTerms`）**：查询转小写，按词字符（字母、数字、组合附标 `unicode.IsMark`、`_`）的连续段切分，段内在 CJK 与非 CJK 文字切换处再切开。
  - 非 CJK 段是一个词：保留 3–40 字符且不在停用词表（英文虚词 + markdown/URL 噪声）中的词。
  - CJK 段（汉字、平假名、片假名、谚文）是没有词界的分句：切成重叠的 3 字符片段（「重试阶梯」→ 重试阶、试阶梯）；不足 3 字符的段不产生任何项；40 字符上限不适用于 CJK。
  - 去重后取至多 32 项：在词池（最长优先）与 CJK 片段池（各分句轮流取）之间交替，任一池用完由另一池补满，互不挤占。
  - 各项加引号以 OR 连接，按 FTS5 bm25 排序；只检查 bm25 顺序的前 max(100, 10×limit) 个候选，不再翻页（AND 模式翻页到底）。过滤后没有可用项 → 无命中（不报错）。用于长的描述性查询（领取时 recall）。

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
  `daemon/memface.go`）。driver 把它作为「非约束性历史」注入每个 step 的 prompt（`v2-converge.md#§1`）——只在有条目时
  渲染该节，每条摘录至多 400 字符、整节至多 6000 字节（`driver/spawn.go`）。
  daemon 的 UDS 上没有 recall/remember 通道。部署顺序：先升级 aihub 再升级 daemon——旧 aihub 忽略 `match=`、按 AND 检索，
  长查询于是没有命中，快照为空。
