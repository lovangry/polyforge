# 06 — Memory / Search：Markdown 权威、HTML 派生、索引可重建与实时检索

> **注意 / Note（2026-10-12）**：本文部分内容已被 [v2-converge.md](v2-converge.md) 取代；两者冲突时以 v2-converge 为准。Parts of this document are superseded by v2-converge.md; where they conflict, v2-converge wins.

| 权威项 | 值 |
|---|---|
| 文档定位 | Memory 文件化格式权威（Markdown 权威 / HTML 派生）、文件布局与 frontmatter、Markdown→HTML 派生规则、搜索索引可重建、实时检索、Memory 管理 API 与错误码、社交面保留声明与分享链接、wrap 摘要条目、存量迁移 |
| 上游基线 | `00-architecture.md#§2.3`（INV-7）、§4.1 归属清单；`02-aihub-core.md#§4.9`（Core 侧元数据表）、§4.11（目录布局）；`07-deployment-pilot.md#§6.2`（`backupBarrier` 的权威定义） |
| 术语权威 | 本文负责：Memory 权威格式、frontmatter 约定、派生 HTML、搜索索引、实时检索、Memory API、分享链接（A18）、wrap 摘要条目（A13）、检索过滤顺序（A17） |

## 1. 范围与非目标 <!-- §1 -->

### 1.1 范围 <!-- §1.1 -->

- Memory 的文件化格式权威定义：**Markdown 为权威内容、HTML 为派生产物、SQLite 仅保存 Core
  （元数据与派生检索索引），不做内容权威**（`00` INV-7）。
- 文件布局与 frontmatter / 元数据约定（§4.1/§4.2）。
- Markdown→HTML 派生规则，含**派生有效性的判定方式**（§4.3，修复 B12：不能只看文件是否存在）。
- 搜索索引可由权威内容全量重建（§4.4/§5.3 重建流程）；实时检索：新写入可被后续查询命中，延迟
  有界（§4.5）。
- Memory 写路径如何接入 `07` 定义的备份屏障（§4.6，修复 B08）。
- Memory 写路径的**写序与崩溃窗口**——create/update 的三步写序、redact 的两步式样、读路径
  服务前校验（§5.2/§6，修复 B5：任何崩溃点不得留下对外服务的半提交态）。
- 与 AIHub Memory 管理面的 API 与错误码（§5）。
- **社交面全保留声明（A16 裁定）**：可见性分级（`private|project|team|public|admin`，§4.2/§7）、`related` 关联（§4.2）与 redact 软删除（§2.1/§5.2）三类社交面能力在文件化模型中**全部保留**，语义与现行实践一致——文件化只改变权威存储形态，不裁剪任何社交面；三者均由 frontmatter 字段/目录位置承载，可随 §5.3 重建完整恢复。
- **社交面范围声明（B04 裁定）**：v1 Memory 的社交面**有且只有上述三项**（可见性分级、`related`、redact）——它们正是旧系统 Memory 面的等价物；除此之外不存在任何其他 Memory 社交面。旧系统的 spec/plan 批注与修订回路（memory commit annotation / `pf-revise` 一类）**不属于 Memory 社交面**：其 vNext 去向由 `00-architecture.md#§11.1` 迁移矩阵统一裁定（由 grill-review 的有界 on_fail rewind 取代，`01-workflow-skills.md#§6.2`），不是 Memory 域的迁移义务；不存在任何评论/标注类 Memory 功能的迁移或重建义务，本文不为它们设计条目、字段或端点。
- **分享链接（A18 裁定）**：分享链接由 owner 显式动作生成（`POST share`），不构成第六种可见性分级取值，入口文案为显式例外（§5.1）。
- **wrap 摘要条目（A13 裁定）**：WI wrap 时写入一条 wrap 摘要 Memory 条目，正文不含对 session transcript 归档的任何引用（§2.1）。
- **检索过滤顺序（A17 裁定）**：检索/列表先按 project membership 过滤，再在候选集内应用可见性分级（权威声明在 §7）。
- **存量迁移（A12 裁定）**：现有 aihub memory 一次性导出为 Markdown 权威文件 + `rebuild-index`，管理 CLI 直写文件、不走 API（§10.1 迁移契约，含 B03 的映射规则、导入顺序与迁移后校验）。

### 1.2 非目标 <!-- §1.2 -->

- 不设计 Memory 的语义分类学（experience/rule/fact 的类型域沿用现行实践，本文只锁格式）。
- 不设计跨项目 Memory 共享与可见性矩阵的多项目扩展。
- 不把嵌入模型 / 向量检索纳入 v1 的任何行为、接口或验收面（**B02 裁定：向量层为非 v1 演进项**——一期文本检索（FTS5 trigram）必选且为唯一检索路径，向量层 v1 不实现、不部署、不验收，演进方向记入 §10.3）。
- 不设计 Memory 与 Capsule 执行态的耦合（Memory 是 Core 域数据，见 `00-architecture.md#§4.1`）。
- 不重新定义备份屏障（`backupBarrier`）机制本身——该机制的锁语义、管理端点、锁获取/释放时序
  的权威定义在 `07-deployment-pilot.md#§6.2`；本文只定义 Memory 侧写路径如何接入它（§4.6）。

## 2. 术语与不变量 <!-- §2 -->

### 2.1 定义 <!-- §2.1 -->

- **权威内容（authority）**：`memory.md`——唯一可信来源；一切展示、检索、重建以它为准。
- **派生产物（derived）**：`memory.html`（渲染视图）与 FTS 检索索引（`02-aihub-core.md#§4.9` `memory_fts`）。
  派生物损坏/丢失一律**可由权威内容再生**，不构成数据事故。v1 不存在任何向量/语义索引层（B02 裁定，§10.3）。
- **实时检索**：写入提交后的 Memory 在有界延迟内（§4.5）可被文本查询命中，无需人工重建。
- **redact（软删除）**：权威 Markdown 移入 `memory/<project>/_redacted/` 并在 Core 标记
  `status=redacted`；不物理销毁，审计可溯。
- **双摘要（新增，修复 B12/B16：`source_markdown_sha` 与 `html_sha` 是两个独立字段，不共用一个列）**：
  - **`source_markdown_sha`**：记录"这份 `memory.html` 是从哪一次 Markdown 内容摘要派生出来的"。与
    `memories.content_sha`（`02-aihub-core.md#§4.9`，记录"当前 Markdown 的内容摘要"）是两个独立字段，一致时表示
    HTML **所基于的权威源**是最新的，不一致时表示权威源改变了但 HTML 还没跟进（§4.3 B12 分支）。
  - **`html_sha`**：记录 `memory.html` **自身内容**的摘要（对 HTML 字节直接计算，不是对源 Markdown）。
    每次成功派生后回写；若 HTML 文件被经权威写路径之外的方式改动（手工编辑、磁盘损坏部分写入等），
    现读派生时重新计算的 `html_sha` 与记录值不一致，即使 **源 Markdown 完全未变**，也触发重新派生
    （§4.3 B16 分支）。
  
  两个摘要独立存储、独立校验，**任一不匹配均触发重新派生**（不存在"只检查其中一个就认定最新"的简化路径）。
- **wrap 摘要条目（A13 裁定）**：WI wrap 时写入的**恰好一条** Memory 条目——经 §5.2 的标准创建路径（type 沿用现行实践），承载该次 wrap 的收尾摘要。**写入契约（保证恰好一条且不因崩溃丢失/重复）**：写入者是 **Daemon**（wrap 收尾方），时机在 `end-generation(reason=wrap)` 的 flush 证据落定之后、作为 wrap 收尾序列的最后一步；创建请求携带确定性客户端幂等键 `wrap-summary-<wi_id>-<generation>`——`memories` 表设 `idempotency_key TEXT UNIQUE`（NULL=非幂等创建；显式键按唯一约束去重，`02-aihub-core.md#§4.9`），摘要 POST 成功但响应丢失后重试命中唯一约束、不产生第二条；wrap 已落定而摘要缺失（两者之间崩溃）时由**启动对账**（§5.3 新增步骤：扫描 `work_items` 中 `status='wrapped'` 的 WI，凡不存在 `idempotency_key = wrap-summary-<wi>-<终代 generation>` 的 Memory 行，即机械生成摘要并按同一键补建——执行者是 AIHub 自身，不需要任何 Daemon 存活；**生成源的空链 fallback**：该 WI 存在已发布 Revision 时从其 `execution.json` 生成；该 WI 从未发布过任何 Revision（空链收尾，如登记票直接取消前的一次空跑 wrap）时从 Core 自身的 WI 记录生成（goal/status/closed_at/终代 generation——这些是 Core 权威数据，不依赖 Revision）；两种来源都标注于摘要正文的来源行）；摘要失败不回滚 wrap 终态。其正文**不得包含对 session transcript 归档（`logs/session/<generation>.jsonl`，`03-execution-capsule.md#§4.4`）的任何引用或链接**：transcript 归档只是审计材料（`05-sync-revision-recovery.md#§7.4`），不进入 Memory 检索面，wrap 摘要的可用性因此不依赖归档的存在。每个 WI 的每次 wrap 写一条，不逐 step、不逐 attempt 追加。

### 2.2 `id` 分配权威（解决 frontmatter 与 POST 的冲突） <!-- §2.2 -->

**裁定：服务端分配，客户端不得在 `POST /api/v1/memories` 的请求体中带 `id` 字段**（带则
`MEM-400 CLIENT_ID_NOT_ALLOWED`）。服务端分配 `mem_id` 后，写 `memory.md` 时把该 id 回填进
frontmatter（目录名与 frontmatter `id` 始终一致是写入时的不变式，非输入约束）。frontmatter 中
`id` 字段仍标为**必填**——它描述的是"写入完成后的文件必须自标识"（目录重命名/备份时可自
校验），不是"创建请求必须带 id"。两者正交不冲突：**写入时机**不同（POST 请求体 vs 磁盘文件）。
非创建路径（如 §5.3 重建或手动导入）读到的现有 `memory.md` 必须带正确 `id`（与目录名一致），
不一致则 `MEM-400 FRONTMATTER_ID_MISMATCH`。

### 2.3 本文不变量（`00` INV-7 展开） <!-- §2.3 -->

- INV-7a（修复 B5 后的写序）：create/update 的权威文件替换恒为三步——**① 新内容写入同目录
  临时文件 `memory.md.tmp` 并 fsync → ② 同一 Core 事务提交元数据 + FTS（含 `content_sha` =
  ① 所写内容的摘要）→ ③ rename 临时文件原子覆盖权威 `memory.md` → 派生 HTML**。①②③ 必须
  在**单次 `backupBarrier` 读锁**（§4.6）内完成，不得拆到多次锁窗口，也不得调换②③顺序
  （②先于③正是 B5 修复的核心：消除「文件已写、元数据未提交」的对外可见半提交态）。redact 为
  同构的两步式样：**① rename 移动目录到 `_redacted/` → ② 同一事务提交 `status='redacted'` +
  FTS 删除**（§5.2）。写入提交（②）即 FTS 文本可检索（§4.5 的计时起点）。
- INV-7b：Markdown 与 HTML 内容分歧时，Markdown 为权威；任何消费方（含 UI 渲染）不得反向把
  HTML 覆盖回 Markdown。
- INV-7c：`memories.content_sha`（`02-aihub-core.md#§4.9`）必须等于权威 Markdown 的 sha256；漂移由对账任务
  校正（以文件为准）。
- INV-7d（新增，修复 B12/B16）：HTML 派生是否需要重新生成，判定依据是下面**两个独立条件任一成立则
  重新派生**，而不是"当前是否存在一个 `memory.html` 文件"：
  1. `source_markdown_sha` 与**当前 Markdown 实际内容摘要**不一致（权威源变了）；
  2. 重新计算的 `html_sha`（对现有 `memory.html` 字节内容）与记录的 `html_sha` 不一致（HTML 自身被改了）。
  
  文件存在但任一摘要陈旧，与文件根本不存在，是同一类"派生过期"事件，走同一条重新派生路径（§4.3）：
  重新渲染 HTML，并同时回写新的 `source_markdown_sha`（等于本次权威内容摘要）与新的 `html_sha`
  （等于新渲染 HTML 字节的摘要）两个字段。
- INV-7e（新增，修复 B08；B15 裁定把 create 纳入同一模式）：新建 `memory.md`（create）、替换
  `memory.md`（PATCH）与移动目录（redact）这三类权威文件写
  操作，在动笔前必须先尝试获取 `07-deployment-pilot.md#§6.2` `backupBarrier` 的读锁，覆盖写序（§5.2）的**全部**
  步骤——从①开始写入，经② SQLite 元数据提交，到③ rename 完成与 HTML 派生——不能只覆盖 SQL
  事务本身，也不能只覆盖到②（③前的文件替换与派生同样在锁内，§4.6）。
- INV-7f（新增，修复 B5）：**任何读路径在服务前校验**——详情、检索/列表在把一条 Memory 对外
  服务前，必须校验 `memories.content_sha` 与**现读现算**的权威文件摘要一致（active 路径
  `memory.md` 缺失即视为不一致，不查任何可能与文件不同步的缓存列）。不一致时不以该状态对外
  服务，并触发对账；对账必须在该 `mem_id` 的写互斥锁（§6）下**复检**，防止把在飞写的②→③窗口
  误判为崩溃残留。细则见 §6。

## 3. 模块与代码目录 <!-- §3 -->

```
internal/memory/
├── authority/   # 权威 Markdown 读写、frontmatter 解析（§4.2）
├── derive/     # Markdown→HTML 渲染、source_markdown_sha 记录与陈旧判定（§4.3）
├── index/      # FTS 索引与重建（§4.4/§5.3）
└── api/        # Memory 管理端点（§5.1）
```

## 4. 数据结构：文件格式与索引 <!-- §4 -->

### 4.1 文件布局（memory 子树权威；v1 目录清单与 `02-aihub-core.md#§4.11` 的 `memory/` 子树一致——`02` 表中 `vec-<model-tag>.bin` 一行为非 v1 演进占位记录，v1 不实现该文件，见 §10.3） <!-- §4.1 -->

```
<data_dir>/memory/<project_id>/<mem_id>/
├── memory.md        # 权威内容（frontmatter + 正文）
└── memory.html      # 派生（§4.3），Core 元数据旁附两个独立摘要：source_markdown_sha 与 html_sha
<data_dir>/memory/<project_id>/_redacted/<mem_id>/…   # redact 归位
```

写序 ①（§5.2）在 `<mem_id>/` 内使用瞬态临时文件 `memory.md.tmp`——提交前存在、③ rename 后
消失、崩溃残留由 §5.3 第 1 步清扫；它是瞬态物，不构成目录布局的持久条目，故不进入上述与
`02-aihub-core.md#§4.11` 逐字一致的清单。

两个摘要都落在 `02-aihub-core.md#§4.9` `memories` 表的同名列：`source_markdown_sha`（本次派生所依据的源 Markdown
内容摘要）与 `html_sha`（现有 `memory.html` 自身字节的摘要），两列独立写入、独立校验，不共用一个
列、也不互相代替（`02-aihub-core.md#§4.9` 的 DDL 注释已明确区分两者）。两者的判定语义见上文 INV-7d 与下文 §4.3。

### 4.2 frontmatter / 元数据约定（权威） <!-- §4.2 -->

```markdown
---
id: mem_xxxxxxxxxxxx            # 必填，"mem_"+12 位，与目录名一致
type: experience.debug          # 必填，类型域沿用现行实践
owner_user_id: usr_…            # 必填（无此字段则 rebuild_index 无法从 Markdown 重建 owner_user_id 列，
                                 # 也无法实现 §8 的 private 可见性判定）
title: …                        # 可选，≤120 字符
tags: [a, b]                    # 可选
visibility: project             # 必填 one of private|project|team|public|admin
work_item_id: wi_…              # 可选
project_id:  prj_…             # 必填（rebuild 据此重建 project 归属；导入时由迁移工具按映射写入）
idempotency_key: …            # 可选（wrap 摘要等幂等创建时写入；rebuild 有文件无行分支据 frontmatter
                                 # 恢复该列，保证 wrap-summary 键可从权威内容重建、补建不会重复）
related: [mem_…]                # 可选
created_at: 2026-09-26T00:00:00Z
updated_at: 2026-09-26T00:00:00Z
---
<正文：CommonMark + GFM（表格/任务列表/strikethrough）>
```

frontmatter 字段与 `02-aihub-core.md#§4.9` `memories` 表列一一镜像（表列由 API 写入时从 frontmatter 同步）；
frontmatter 缺必填字段 → `MEM-400 FRONTMATTER_INVALID`。

### 4.3 Markdown→HTML 派生规则与陈旧判定（权威，含 B12/B16 修复） <!-- §4.3 -->

- 渲染器：**goldmark**（CommonMark 0.31 + GFM 扩展），确定性输出（同输入必同输出，无时间戳/
  随机 id 注入）。
- 扩展白名单：GFM tables、task list、strikethrough、autolink；**禁用**原始 HTML 与任意脚本
  注入（渲染输出经转义，`unsafe=false`）。
- 派生成功后，同时落盘三样东西：`memory.html` 本身、本次派生所依据的 Markdown 内容摘要
  `source_markdown_sha`、以及新生成的 `memory.html` 自身内容的摘要 `html_sha`（均写入 §4.1
  描述的 `02-aihub-core.md#§4.9` 列，与 `memory.html` 的写入在同一次派生操作内完成，不分次提交）。

**陈旧判定（B12/B16：修复"HTML 崩溃恢复返回旧派生内容"与"HTML 自身被篡改无法检测"）**。原方案
只在"Markdown 与已记录 `content_sha` 不同"这一个分支里触发重新派生，遗漏了两个真实存在的窗口：
(a) Markdown 写入事务已提交（`content_sha` 已更新为新值），但派生 HTML 的动作还没来得及执行就
崩溃——此时 `memory.html` 文件依旧存在（是上一版本的内容）；(b) HTML 文件自身被经权威写路径之外
的方式修改或部分写坏，而源 Markdown 完全未变。修复后的判定改为三个独立、都必须满足才算"有效"
的条件：

1. `memory.html` 文件存在；
2. 其旁附的 `source_markdown_sha` 等于**当前 Markdown 实际内容的摘要**（现读现算，不是读
   `memories.content_sha` 这个可能同样滞后的缓存列——用当前磁盘上 `memory.md` 的字节直接计算
   sha256 作比对基准，确保判定不依赖任何可能与文件本身不同步的中间缓存）；
3. **（新增，修复 B16）** 对当前 `memory.html` 文件字节现读现算 sha256，与记录的 `html_sha` 一致
   （检测 HTML 自身是否被篡改，与第 2 条独立，即使第 2 条通过也不跳过本条）。

`GET .../{id}?format=html` 的处理顺序：先按上述三条判定当前 HTML 是否有效；**任一条不满足**
（文件不存在，或 `source_markdown_sha` 与当前 Markdown 摘要不一致，或 `html_sha` 与当前 HTML
字节摘要不一致）都视为过期，触发重新派生一次并回写 `memory.html`、新的 `source_markdown_sha`
与新的 `html_sha`，再返回新生成的内容。这是一个独立于"Markdown 与已记录 `content_sha` 是否
一致"（INV-7c 的漂移对账，§5.3/§6）的判定分支，三者分别覆盖不同的崩溃/篡改窗口：
INV-7c/INV-7f 覆盖「元数据与权威文件摘要不一致」的窗口（B5 写序下的典型成因是②已提交、③未
执行即崩溃——元数据新、文件旧，由对账以文件为权威回滚元数据，§6）；`source_markdown_sha`
比对覆盖"Core 元数据（含 `content_sha`）
已经更新、但 HTML 还没来得及重新生成"的窗口（即③已执行、派生未执行的崩溃点）；`html_sha` 比对覆盖"HTML 本身在权威写路径之外被
改动"的窗口。任一判定命中都触发重新派生。

- 派生失败（畸形 Markdown）→ 权威内容照常保留，`MEM-501 DERIVE_FAILED` 告警，检索不受影响
  （索引从 Markdown 文本抽取）；`source_markdown_sha`/`html_sha` 在失败时均不更新，下一次读取
  仍会判定为过期并重试派生。

### 4.4 搜索索引（可重建，权威） <!-- §4.4 -->

- **文本索引（v1 唯一索引层）**：`memory_fts`（FTS5，`02-aihub-core.md#§4.9`）——**tokenizer 固定为 trigram**（FTS5 `tokenize='trigram'`）：按 3 字符滑窗切分，使任意子串检索对中文等无空格分词边界的语言同样成立（中文子串检索直接由滑窗保证，不依赖分词器/词典等语言学前置）。正文抽取规则：frontmatter 剥离后，Markdown
  结构字符（`#*_>`）剔除、代码块保留原文。
- **可重建性**：`POST /api/v1/memories/rebuild-index`（§5.1）从权威 Markdown 全量重建 FTS 索引
  （以及 §4.3 的 HTML 派生与 `source_markdown_sha`/`html_sha` 双摘要）；重建**只依赖** `<data_dir>/memory/` 下的
  Markdown 文件，不需要任何不可再生数据。v1 不存在任何语义/向量索引层（B02 裁定，§10.3）。

### 4.5 实时检索（权威语义：固定可计算阈值，与 M6/PILOT-V6 同一数值；v1 检索路径唯一 = FTS 文本检索） <!-- §4.5 -->

- 写路径（INV-7a）使 FTS 更新与元数据提交**同事务**——写入返回成功即文本可检索，检索路径上
  不存在任何异步层（B02 裁定：向量层退出 v1，v1 不存在语义检索路径，§10.3）。
- 「写入→可检索延迟」的合格阈值是**固定可计算值（唯一权威，
  `07-deployment-pilot.md` 的 M6/PILOT-V6 判定直接指向该值，不在任何一侧另定数值）**：

  **写入→可检索 p99 ≤ 5s（5000 ms；固定值，不可配置，不随负载浮动）**

  - **样本口径**：每个 Memory 写入事件产生一个延迟样本——**计时起点**为该 Memory 的
    `INSERT`/`UPDATE memories` 事务提交成功的时刻，**计时终点**为 `q=` 文本检索（FTS5
    trigram）实际可查到该 `mem_id`/`content_sha` 对应条目的时刻。
  - **判定**：对统计窗口内的全部样本计算 p99（滚动窗口与样本集定义在 `07-deployment-pilot.md#§9.2` M6，
    满足其 INV-D5 的窗口/分母要求，本文不重复定义）；`p99 ≤ 5000 ms` 即合格。单个样本超过
    5000 ms 本身不构成违约（阈值约束的是分位数，不是逐样本上限），但该样本计入 p99 计算。
- 检索 API：`q=` 文本检索（FTS，v1 唯一检索路径；**不存在 `semantic` 参数**，不存在语义检索
  端点变体）。检索路径的过滤顺序恒为先按 project
  membership 过滤、再在候选集内应用可见性分级（A17 裁定，权威在 §7）。

### 4.6 Memory 写路径接入备份屏障（权威，修复 B08） <!-- §4.6 -->

**背景（问题重述）**：`07-deployment-pilot.md#§6.2` 定义的 `backupBarrier` 是一个进程级读写锁，用于保证
`pf-deploy backup` 拍下的 SQLite 快照与 `<data_dir>` 下的文件快照处于同一个"无在飞写入"时间
点。原设计只要求 Core 的 SQL 写事务在事务开始前取该锁的读锁、事务结束后释放——但 Memory 的两类
权威文件写操作（替换 `memory.md`、redact 时移动目录）在**事务之外**也有实际的磁盘写入动作，
如果这些动作不受该锁保护，备份仍然可能在"文件已经改了但 SQL 事务还没提交"或反过来的窗口里
拍下不一致的快照对。

**权威规则**：Memory 的写路径必须把 `backupBarrier` 读锁的持有窗口，从"进入 SQL 事务前"提前
到"开始写权威文件之前"，并保持到写序（§5.2）全部完成为止（② SQLite 元数据提交、③ rename
覆盖与 HTML 派生均在锁内）——即覆盖整个写操作的端到
端时间窗口，而不只是其中的 SQL 事务片段。具体到本文**三**类权威文件写操作（create/update/redact 全覆盖，不存在任何一类跳过这把锁）：

- **`POST /api/v1/memories`（§5.2 创建；B08 遗漏修复，B15 裁定复核保持）**：在执行 §5.2 写序的
  **①（写临时文件）**这一步**之前**，先尝试获取 `backupBarrier` 读锁；①→②→③ 以及 HTML
  首次派生全部完成后，才释放该读锁。原方案只要求 update/redact 取锁，遗漏了 create：create
  的写序同样横跨文件系统与 SQLite 两类状态（§5.2），跳锁同样会让备份拍到半提交态，必须与
  update/redact 同样受锁保护。
- **`PATCH /api/v1/memories/{id}`（§5.2 更新）**：在执行 §5.2 写序的**①（写临时文件）**（含
  此前“复制旧正文到 `history/`”这一步）**之前**，先尝试获取 `backupBarrier` 读锁；①→②→③
  以及 HTML 重新派生全部完成后，才释放该读锁。
- **`POST /api/v1/memories/{id}/redact`（§5.2 redact）**：在执行其写序的**①（rename 移动
  目录到 `_redacted/`）**这一步**之前**，先尝试获取 `backupBarrier` 读锁；①→②（同一
  Core 事务提交 `status='redacted'` + FTS 删除）全部完成后，才释放该读锁。

**事务层不重复获取同一锁（新增，防死锁）**：上述三个入口均在本层（Memory API handler）获取该
读锁**恰好一次**（与 `07-deployment-pilot.md#§6.2` 第 7 条对同一把锁的权威定义一致：同一屏障一次只取一把读锁），并在
整个请求处理期间传递已持有的状态；内部调用的 Core 事务层（`02-aihub-core.md#§7` 单写者事务）**MUST NOT** 自己
再次请求同一个 `backupBarrier`（非可重入读写锁，重复 `RLock` 会与正在等待写锁的备份相互等待，
死锁机理的权威描述见 `07-deployment-pilot.md#§6.2`）：事务层接受一个已持有锁的上下文参数（或简
单地不自己接触 `backupBarrier`，完全交给 Memory API 层处理）。

获取读锁与 `07-deployment-pilot.md#§6.2` 描述的"写锁互斥"语义完全复用该文档定义的 `sync.RWMutex` 行为，本文不
重新定义该互斥体本身：当 `pf-deploy backup` 正持有写锁排空在飞写入时，上述三类 Memory 写路径
的读锁获取会阻塞，直到备份完成释放写锁；反之当任一 Memory 写路径持有读锁未释放时，备份的写锁
获取会等待其完成，保证不会拍到"文件已改、Core 未提交"或"Core 已提交、文件还没改完"的中间态
（B5 写序下，备份窗口内 ①②③ 要么都没发生、要么全部完成，中间态只存在于读锁持有的窗口内部，
对备份不可见）。
写入 `memory.md` 之外的、不改变权威文件内容的只读检索/详情端点不需要获取该锁。

## 5. API 与错误码（本层） <!-- §5 -->

### 5.1 Memory 管理 API（挂载于 AIHub；端点方法/路径/调用方权威一律在 `02-aihub-core.md#§5.1` 行 22–25，本节只展开写序、协议细节与权限语义） <!-- §5.1 -->

> **端点权威声明**：本节不重复定义端点清单或调用方授权——凡与 `02` canonical 行 22–25 冲突处以 `02` 为准。本节展开的是：三步写序（§5.2）、rebuild 流程（§5.3）、可见性矩阵与检索过滤顺序（§7）、匿名分享的消费语义（`GET /api/v1/share/<token>`：单条、只读、无 api_key/membership、token 不可枚举，权威定义在 `02-aihub-core.md#§5.1` 行 24）。

| 端点 | 方法 | 语义 | 错误码 |
|---|---|---|---|
| `/api/v1/memories` | POST | 创建（body: frontmatter 字段不含 `id` + 正文；服务端分配 `mem_id`，§2.2） | MEM-400 / MEM-409 |
| `/api/v1/memories/{id}` | GET | 详情（服务前按 INV-7f 校验 `content_sha` 与文件摘要，不一致不以该状态对外服务并触发对账；返回 Markdown 正文 + 元数据；`format=html` 返回派生，按 §4.3 的陈旧判定决定是否重新派生） | MEM-404 |
| `/api/v1/memories/{id}` | PATCH | 更新（生成新版本：旧正文归档 `history/<updated_at>.md`，`content_sha` 更新，FTS 先删后插；写路径先获取 §4.6 的备份屏障读锁） | MEM-404 / MEM-400 |
| `/api/v1/memories/{id}/redact` | POST | 软删除（§2.1；写路径先获取 §4.6 的备份屏障读锁） | MEM-404 / MEM-409 |
| `/api/v1/memories/{id}/share` | POST | 生成分享链接（A18 裁定，见下；仅 owner 可调用） | MEM-404 / MEM-403 |
| `/api/v1/memories/search?q=&type=&tags=&limit=` | GET | 实时文本检索（§4.5；v1 唯一检索路径，无 `semantic` 参数） | MEM-400 |
| `/api/v1/memories/rebuild-index` | POST | 全量重建（§4.4） | MEM-502 |

**分享链接（A18 裁定，权威）**：分享链接**只能由该条 Memory 的 owner 经显式动作生成**——`POST /api/v1/memories/{id}/share`，非 owner 调用返回 `MEM-403`。它是一条由 owner 显式授予的、超出该条目 `visibility` 分级的独立访问通道，**不构成第六种可见性分级取值**：`visibility` 的取值域仍只有 `private|project|team|public|admin` 五种（§4.2），分享动作不写改 frontmatter 的 `visibility` 字段。**入口文案为显式例外**：各入口（UI/CLI）呈现分享动作时，文案必须把它表述为「owner 显式授予的、对其可见性分级的一次例外」，不得表述为又一种 visibility 取值或与分级并列的常规选项——链接的生成动作即例外的全部授权依据，无默认开启、无隐式入口。

### 5.2 核心写流程（编号步骤，非可执行代码） <!-- §5.2 -->

**写序总则（权威，修复 B5：崩溃不得留下对外服务的半提交态）**：create 与 update 的权威
文件替换遵循同一**三步写序**，三步在**单次 `backupBarrier` 读锁**（§4.6）内完成、不得调换
顺序：

- **① 写临时文件**：新内容写入同目录临时文件 `memory.md.tmp` 并 **fsync**（父目录随后
  fsync）。此刻权威 `memory.md` 尚未变动，对外可见状态仍是旧值（create：尚不存在）。
- **② SQLite 事务提交**：同一 Core 事务内提交元数据与检索索引——`memories` 行（含
  `content_sha` = ① 所写内容的摘要）与 `memory_fts`（同事务先删后插），事务提交即元数据定稿。
- **③ rename 覆盖**：把 ① 的临时文件 rename 覆盖权威 `memory.md`（同文件系统 rename 原子
  生效），随后 fsync 目录。至此权威内容与元数据一致，才执行 HTML 派生（派生崩溃/失败只留
  陈旧派生物，由 §4.3 读时判定自愈，不影响权威与元数据）。

**崩溃窗口（B5）**：

- **② 前崩溃**（含 ① 中途崩溃）：SQLite 事务未提交、自动回滚；权威 `memory.md` 从未被动过；
  临时文件成为孤儿残留，由启动/对账任务清扫丢弃（§5.3 第 1 步）。**无任何对外可见变化**。
- **② 后、③ 前崩溃**：元数据已新、权威文件仍旧（create：`memory.md` 仍不存在）。此时
  `memories.content_sha` 与现读现算的文件摘要**必然不一致**，读路径服务前校验（INV-7f）
  命中：该条 Memory 不以该状态对外服务；启动/对账任务**以文件为权威把元数据回滚至与（旧）
  文件一致的值**——update 即整条 PATCH 回滚（`content_sha`/`title`/`tags`/`related`/
  `visibility`/`updated_at`/FTS 全部随旧文件重算），create 即删除孤儿 `memories` 行与 FTS
  记录。**整个操作回滚，不留「元数据新、文件旧」的半提交态。**
- 任何时刻，读路径（详情/检索/列表）在服务前校验 `memories.content_sha` 与现读现算的文件
  摘要一致（INV-7f，细则 §6）：不一致触发对账且不以该状态对外服务——上述②③之间的崩溃窗口
  因此**不可能被对外服务**，无论对账任务是否已经跑过。

**创建（`POST /api/v1/memories`）**：

1. 校验请求体不含 `id` 字段（否则 `MEM-400 CLIENT_ID_NOT_ALLOWED`）。
2. 服务端分配新 `mem_id`，把它回填进 frontmatter。
3. **先获取 `backupBarrier` 读锁**（§4.6，修复 B08 遗漏，B15 裁定复核保持），持有至第 8 步，
   覆盖写序 ①②③ 全部步骤。
4. **①** 新内容写入 `<mem_id>/memory.md.tmp` 并 fsync。
5. **②** 同一 Core 事务提交：`INSERT memories` 行（`content_sha` = ① 内容摘要，
   `owner_user_id` 取自本次请求的认证身份，不采信客户端传入的任何 `owner_user_id` 字段）+
   `INSERT memory_fts`（使写入返回成功即可检索，INV-7a）。
6. **③** rename 临时文件覆盖权威 `memory.md`（原子，随后 fsync 目录）。
7. 派生 `memory.html` 并记录本次派生的 `source_markdown_sha`/`html_sha`（§4.3）。
8. **释放 `backupBarrier` 读锁**。
9. 返回 `{id: mem_id}`。

**更新（`PATCH /api/v1/memories/{id}`）**：

1. **先获取 `backupBarrier` 读锁**（§4.6），持有至第 7 步，覆盖写序 ①②③ 全部步骤。
2. 复制当前 `memory.md` 到 `history/<now>.md`（权威路径此刻未变动；崩溃残留的孤儿 history
   文件不参与任何判定，可由对账清扫）。
3. **①** 新内容写入 `<mem_id>/memory.md.tmp` 并 fsync。
4. **②** 同一 Core 事务提交：`UPDATE memories` 行（`content_sha`、`title`、`tags`、`related`、
   `visibility`、`updated_at` 全部随新 frontmatter 更新——遗漏 `visibility` 会让
   `public→private` 的一次 PATCH 在本步遗漏后 Core 行仍是旧可见性，直到下次重建才纠正，形成
   越权窗口，因此本步骤必须覆盖它）+ `DELETE`/`INSERT memory_fts`（先删旧文本再插新文本，同
   事务，不留旧行）。
5. **③** rename 临时文件覆盖权威 `memory.md`（原子，随后 fsync 目录；任何时刻权威路径上
   要么是完整的旧文件、要么是完整的新文件，不存在半文件）。
6. 重新派生 `memory.html` 并同时回写 `source_markdown_sha` 与 `html_sha`（§4.3：每次成功派生两个摘要一并更新；旧派生不保留）。
7. **释放 `backupBarrier` 读锁**。
8. 返回 `{id: mem_id, updated_at}`。

**Redact（`POST /api/v1/memories/{id}/redact`）**：与 create/update 同构的两步式样，同样在单次
`backupBarrier` 读锁（§4.6）内完成：

1. **先获取 `backupBarrier` 读锁**（§4.6），持有至第 4 步。
2. **①（文件步）** 把 `<data_dir>/memory/<project>/<mem_id>` 整体移动（同文件系统 rename，
   原子）到 `<data_dir>/memory/<project>/_redacted/<mem_id>`。
3. **②（元数据步）** 同一 Core 事务提交：更新 `memories` 行 `status='redacted'` + 从
   `memory_fts` 删除该 `mem_id` 的记录（移出检索）。
4. **释放 `backupBarrier` 读锁**。

**redact 的崩溃窗口（同理定义，文件位置即权威状态）**：

- **① 前崩溃**：文件未动、元数据未动，无任何可见变化。
- **① 后、② 前崩溃**：文件已在 `_redacted/`，Core 行仍 active、FTS 仍在。读路径服务前校验
  （INV-7f）以 active 路径为准——文件缺失即不一致，不以该状态对外服务（详情按 `MEM-404`
  处理并触发对账，检索命中被剔除）；对账任务以文件位置为权威，把 Core 行更正为 `redacted`
  并删除 FTS（§5.3 第 5/6 步），即**前向完成**该 redact。与 create/update 的「回滚」方向相反，
  但遵循同一条「文件为权威」原则：文件的位置状态就是权威状态；若反方向把 Core 回滚为 active，
  反而制造「文件已 redact、元数据 active」的更长半提交态。
- **② 后崩溃**：操作已完整提交，无窗口。

先移动文件、后提交 Core 的顺序理由：①②之间崩溃时，§5.3 `rebuild_index` 的 `_redacted/`
扫描分支能纠正 Core 行（文件已确凿在 `_redacted/`，以文件位置为准）；反过来若先提交 Core 后
移动文件，崩溃窗口内文件仍在原路径但 Core 已标记 `redacted`，常规目录扫描会把它当"有文件有
行"重新按 active 内容对账，反而把 `redacted` 状态错误覆盖回 `active`——这正是本条顺序要避免
的方向错误。

### 5.3 重建流程（索引丢失恢复） <!-- §5.3 -->

`POST /api/v1/memories/rebuild-index` 按以下编号步骤全量重建（覆盖两个目录：正常 `memory.md`
与 `_redacted/` 下已软删的内容）；启动时的对账与定期调度任务执行同一流程：

1. **清扫临时文件（B5）**：遍历并删除 `<data_dir>/memory/**/memory.md.tmp` 残留——它们只
   可能是写序②前崩溃的遗留物（§5.2），权威路径从未被其影响，删除即完成该次回滚的收尾；同时
   清扫崩溃残留的孤儿 `history/*.md` 快照（不参与任何判定）。
2. 遍历 `<data_dir>/memory/<project>/*/memory.md`：解析 frontmatter，计算当前内容 sha。
3. **元数据补建/更新（条件执行，按文件与行的关系分支）**：
   - **有文件无行**（`memories` 表无该 `mem_id`）：以文件为准插入元数据行——**完整 frontmatter→DDL
     映射**（保证插入的行满足 `02-aihub-core.md#§4.9` 全部约束）：`id`=目录名、`type`、`visibility`、
     `project_id`、`owner_user_id`、`title`、`tags`、`related`、`idempotency_key`、`work_item_id`、
     `created_at`/`updated_at` **均从 frontmatter 读出**（`type`/`visibility` 是 NOT NULL 列，frontmatter
     缺失即 `MEM-400 FRONTMATTER_INVALID`，不会插入非法行——与 §4.2 的必填校验同源）；
     `content_sha`=当前现算、`content_len`=文件长度；`status='active'`（`_redacted/` 下的文件走第 5 步、
     补建为 `status='redacted'`）；`authority_path`=文件相对路径；`source_markdown_sha`/`html_sha` 置 NULL
     （派生窗口随后统一处理）。
   - **有行但内容不匹配**（`row.content_sha != 当前文件 sha`）：以现文件为准更新
     `content_sha`/`title`/`tags`/`related`/`visibility`/`updated_at`，并强制重新派生 HTML
     （连同回写 `source_markdown_sha` 与 `html_sha`，不假设旧 `source_markdown_sha` 与文件仍然一致）。
     B5 写序下该分支的典型成因即②已提交、③未执行的崩溃窗口（元数据新、文件旧）——本分支
     以文件为准重算元数据，对该窗口而言效果即把该次操作整体回滚（§6）。
   - **有行且内容匹配**（`row.content_sha == 当前文件 sha`）：元数据已是最新，本子步骤不做
     任何写入（不重写 `memories` 行，不强制重新派生 HTML）。
4. **FTS 重建（对每个有效 active Markdown 无条件执行，不以第 3 步命中哪个分支为前提）**：
   不论第 3 步落在“有文件无行”“有行但内容不匹配”还是“有行且内容匹配”哪一支——包括元数据
   完全匹配、第 3 步整段跳过的情形——都对该 `mem_id` 执行 `memory_fts` 先删后插，重建其检索
   记录。这一步专门覆盖“Markdown 权威内容与 `memories` 元数据两者都完好一致，唯独
   `memory_fts` 索引本身丢失或损坏”的窗口：该窗口不落在第 3 步任何一个条件分支里，若 FTS
   重建被第 3 步的判定结果 gate 住，这类纯索引丢失将永远不会被本流程修复（对应 §9 的专项
   测试：仅 FTS 丢失、Markdown+元数据完全匹配）。
5. 遍历 `<data_dir>/memory/<project>/_redacted/*/memory.md`：**无行时先补建**（迁移导入的
   redacted 存量走本分支——按 frontmatter/文件路径重建 `memories` 行，`status='redacted'`、
   `content_sha` 取文件摘要，**不写入 `memory_fts`**）；若对应行存在且
   `status != 'redacted'`，更正为 `redacted` 并从 `memory_fts` 删除该记录——文件已在
   `_redacted/` 即视为已软删，纠正滞后的 Core 行（redact ①后②前崩溃的窗口在此前向完成，
   §5.2）。迁移验收的 redacted 行数核对因此可全量成立。
6. **行驱动扫描（B5）**：遍历 `memories` 中 `status != 'redacted'` 但 active 路径缺
   `memory.md` 的行——若该 `mem_id` 的目录已在 `_redacted/` 下，由第 5 步同一逻辑更正为
   `redacted` 并删 FTS；若两处皆无（create ②后③前崩溃的孤儿行：元数据已 INSERT、文件从未
   放置），**删除该行与 FTS 记录**——整条 create 回滚，不留「有行无文件」的半提交态。
7. 返回重建统计（重建条数、耗时）。

### 5.4 错误码表（前缀 MEM） <!-- §5.4 -->

| 码 | 语义 |
|---|---|
| MEM-400 | 请求/frontmatter 非法（含 CLIENT_ID_NOT_ALLOWED、FRONTMATTER_ID_MISMATCH、FRONTMATTER_INVALID，§2.2） |
| MEM-404 | Memory 不存在 |
| MEM-403 | 分享动作越权（`POST share` 的调用方非 owner，§5.1 A18 裁定） |
| MEM-409 | id 重复 / 终态（redacted）不可写 |
| MEM-413 | 正文超限（≤1 MiB） |
| MEM-501 | DERIVE_FAILED（派生失败，权威内容不受影响） |
| MEM-502 | 重建失败（携带已完成批次，可续跑） |

## 6. 并发与崩溃恢复 <!-- §6 -->

- **写并发**：同 `mem_id` 的并发请求必须在**进入文件写作之前**获取一个进程内
  per-`mem_id` 互斥锁（内存锁表，键为 `mem_id`，单 AIHub 进程内有效，与 `00` INV-2 单 active
  Service 一致），持有直到整个操作（写序 ①②③ + HTML 派生）全部完成才释放——不是只锁
  Core 事务那一段。文件写入仍用 tmp + rename 原子替换，崩溃不留半文件。这个
  per-`mem_id` 互斥锁与 §4.6 的 `backupBarrier` 读锁是两个不同层次的锁：前者防止同一条 Memory
  的并发写互相踩踏，后者防止任一 Memory 写与备份的一致性快照互相踩踏；两者独立获取，互不替代。
  **读路径触发的单条对账修复（见下）同样必须先取这把互斥锁并在锁内复检**——写者若正处在
  ②→③之间，等到复检时③已完成、摘要一致，不做任何回滚，防止把在飞写误判为崩溃窗口。
- **写序与崩溃窗口（B5 修复；写序的权威定义在 §5.2 写序总则）**：create/update 的三步写序
  （①临时文件+fsync → ②事务提交 → ③rename 覆盖）与 redact 的两步式样（①rename 移动 →
  ②事务提交）保证任何崩溃点都不留下对外服务的半提交态：
  1. **② 前崩溃**：事务回滚、权威文件未动、临时文件被清扫（§5.3 第 1 步）——无任何可见变化。
  2. **② 后 ③ 前崩溃**（create/update）：元数据新、文件旧——读路径服务前校验（INV-7f）不以
     该状态对外服务；对账以文件为权威**回滚元数据至旧值**（update：随旧文件重算全部元数据
     与 FTS；create：删孤儿行与 FTS，§5.3 第 3/6 步），整个操作回滚。
  3. **① 后 ② 前崩溃**（redact）：文件已移、元数据未提交——读路径以 active 路径为准判
     不一致、不服务；对账以文件位置为权威**前向完成** redact（§5.2 redact 崩溃窗口、
     §5.3 第 5 步）。
  4. **③ 后、派生前崩溃**：权威与元数据已一致，仅 HTML 陈旧——由 §4.3 读时判定自愈（B12），
     不属半提交态。
- **对账式样（均在 §5.3 `rebuild_index` 内实现，可定期调度、启动时执行或手动触发）**：
  1. **有文件无行**：以文件为准补建元数据与索引（方向固定：文件 → Core，INV-7c）。B5 写序
     下崩溃不再产生该式样（权威文件只会在②提交之后才被③放置/替换），保留该分支以覆盖经
     权威写路径之外的手工放置/导入。
  2. **有行但内容不匹配**（`sha256(当前文件内容) != memories.content_sha`）：B5 写序下的
     典型成因即②已提交、③未执行的崩溃窗口（元数据新、文件旧）——以现文件为准更新元数据、
     重建 FTS、重新派生 HTML（连同回写 `source_markdown_sha` 与 `html_sha`，不信任旧
     `source_markdown_sha` 匹配即无需重派的假设，INV-7d）；对该窗口而言效果即把该次操作
     **整体回滚**。本分支同样覆盖经权威写路径之外的手工改动（方向不变：以文件为准）。
  3. **有行无文件**（active 路径缺 `memory.md`）：目录在 `_redacted/` → 前向完成 redact；
     两处皆无 → 删除孤儿行与 FTS（§5.3 第 6 步行驱动扫描）。
  4. **临时文件残留**：直接丢弃（§5.3 第 1 步）。索引损坏/丢失走同一重建，检索能力恢复且不
     要求任何不可再生数据。
- **读路径服务前校验（INV-7f 的执行细则）**：详情 GET、检索/列表在把一条 Memory 对外服务前，
  校验 `memories.content_sha` 与现读现算的文件摘要（active 路径 `memory.md` 缺失即不一致）。
  不一致的处理：先获取该 `mem_id` 的写互斥锁并**复检**——复检一致说明命中的是在飞写的②→③
  窗口，正常服务；复检仍不一致说明是崩溃/非权威写路径残留，执行单条对账（按上文式样，与
  §5.3 同一逻辑）后再服务（文件完全缺失且不在 `_redacted/` 时，对账删行后返回 `MEM-404`）。
  检索路径对命中条目做同一校验，不一致条目从本次结果集剔除并投递对账任务。
- **派生 HTML 过期的独立恢复分支（B12）**：即使 `memories.content_sha` 与文件一致（上述对账
  式样都未命中，说明 Markdown 侧元数据是最新的），HTML 侧仍可能因为"③已执行、派生动作还
  没执行完就崩溃"而过期；这类窗口由 §4.3 定义的读时判定（同时比对 `source_markdown_sha` 与当前
  Markdown 实际摘要、`html_sha` 与现有 HTML 字节摘要，INV-7d 的两条独立判定）在下一次 `GET .../{id}?format=html` 时发现并自动重新派生，不需要等待
  下一次 `rebuild_index`，也不依赖任何后台巡检。

## 7. 安全边界 <!-- §7 -->

- **检索过滤顺序（A17 裁定，权威）**：任何检索/列表路径的过滤恒为两层、顺序固定——**第一层按 project 访问资格过滤**：候选集 = 调用方有 membership 的 project ∪ 全局 admin 可见的全部 project（`02-aihub-core.md#§4.10` 统一规则：对象访问 = membership OR 全局 admin——admin 是角色级例外；带 project-scope key 的调用方至多见到 scope 内 project，scope 是收窄，admin 凭全局 key 见全部）；无访问资格的 project 条目无论 `visibility` 取何值都不进入候选；**第二层才在候选集内应用可见性分级**（`private|project|team|public|admin` 的权限矩阵见下，§5.1 owner 显式生成的分享链接是分级之外唯一的例外通道，A18）。不存在「先按可见性聚合、再裁剪项目」的反向路径；文本检索（FTS，v1 唯一检索路径）遵循该两层顺序。
- 可见性执行在 API 层（frontmatter `visibility` → 查询过滤器）；`public` 层要求正文不含
  secrets（创建时按关键词粗检，`MEM-400` 拒绝明文 token 模式）。
- **一期权限矩阵**：`private` = 仅 `owner_user_id`（创建者，等于调用方 api_key 对应
  `users.id`）可读；`team` = 同 `02-aihub-core.md#§4.10` `projects` 下的全部 `users` 可读（一期无独立 team 实体，
  `team` 等价于 `project`，两者合并）；`admin` = 仅 `users.role='admin'` 可读；`project` =
  同 project 内全部可读（与 `team` 等价）。权限判定在搜索/详情 API 执行，字段随 `memories` 行
  一同重建（§5.3），不丢失。
- 渲染禁原始 HTML（§4.3），派生 HTML 无脚本注入面；目录权限 0750（`07-deployment-pilot.md#§8`）。
- redact 不物理销毁：审计可溯；硬删除仅管理员 + 与 Capsule 数据同一删除保护理念（延迟确认），
  一期只实现到 redact 层。
- Memory 写路径与备份的互斥关系（§4.6）本身是一条安全/一致性边界：任何绕过该读锁直接写
  `memory.md` 或移动目录的实现路径都视为违反本文档约束，必须走 §5.2 描述的流程。

## 8. 监控 <!-- §8 -->

- 写入→可检索延迟（按 §4.5 的固定阈值「写入→可检索 p99 ≤ 5s」判定，窗口内 p99 超标即告警）；
  索引重建时长与批次失败率；`MEM-501`/`MEM-502`
  计数；Markdown↔Core 漂移对账发现数（应为 0，非 0 即 bug）；读路径服务前校验（INV-7f）
  命中数（按「在飞写复检通过」与「触发对账回滚/前向完成」分别计数——前者属正常瞬态，后者
  非 0 即存在崩溃残留或非权威写路径改动）；HTML 派生陈旧命中次数（每次读时
  判定发现 `source_markdown_sha` 或 `html_sha` 任一不一致都计数，两个判定分支分别统计，用于观察
  崩溃窗口与 HTML 被篡改窗口的实际触发频率）；`backupBarrier` 读锁等待时长（用于判断备份是否长时间阻塞 Memory 写入，或反之）。

## 9. 测试与故障注入 <!-- §9 -->

- 权威性：手改 HTML 制造分歧 → 断言检索/重建以 Markdown 为准且 HTML 被重派生覆盖。
- 实时性：批量写入后按 §4.5 样本口径逐条计时 → 断言统计窗口内样本的 p99 ≤ 5s（FTS 写入返回
  成功即可被 `q=` 检索命中；v1 无向量层，全部样本均为 FTS 样本）。
- 重建：删除全部索引（FTS 清空）→ 重建后检索能力恢复且逐条 sha 校验一致。
- **仅 FTS 丢失专项**（§5.3 无条件重建分支）：构造 Markdown 权威内容与 `memories`
  元数据完全匹配（`row.content_sha == 当前文件 sha`，无需补建或更新任何元数据字段）的
  前提下，单独删除该 `mem_id` 在 `memory_fts` 中的记录（模拟索引自身损坏/丢失，不动
  Markdown 与 `memories` 行）→ 调用 `rebuild-index` 后断言该 `mem_id` 重新可被文本检索命中，
  且本轮重建未触发任何 `memories` 行写入、未触发重新派生 HTML（断言 FTS 重建不依赖于
  元数据是否需要补建/更新这一判定结果）。
- **B12 专项**：模拟"③ rename 已执行、HTML 派生动作未执行"崩溃窗口（在写序③之后、派生
  步骤之前杀进程）→ 断言下一次 `GET .../{id}?format=html` 通过
  `source_markdown_sha` 陈旧判定发现过期并重新派生，而不是返回旧内容；断言此路径不依赖
  `memories.content_sha` 与文件是否一致这一条件单独触发。
- **B08 专项**：并发发起一次 `pf-deploy backup`（持有 `backupBarrier` 写锁）与一次 Memory
  PATCH/redact → 断言 Memory 写路径的读锁获取被正确阻塞至备份完成，或反之，两者不产生交叉
  写入导致的不一致快照。
- **B5 专项（写序崩溃注入，三个可注入崩溃点）**：在写路径埋设可注入的崩溃点钩子，对 §5.2
  写序逐一注入：
  1. **① 后、② 前崩溃**（临时文件已 fsync、事务未提交；①前注入——进程在写任何字节前
     死亡——归入同一断言，只是连临时文件都不存在）：断言事务已回滚
     （`memories`/`memory_fts` 无该次写入痕迹）、权威 `memory.md` 未变（update 为旧内容，
     create 则不存在）、残留 `memory.md.tmp` 被启动/对账清扫后**无任何对外可见变化**，
     读路径照常服务旧状态。
  2. **② 后、③ 前崩溃**（元数据已提交、rename 未执行）：断言读路径服务前校验命中——详情
     不以该状态对外服务、检索命中被剔除；启动对账后元数据被以文件为权威回滚至旧值
     （update：整条 PATCH 回滚，含 FTS 与 `updated_at`；create：孤儿行与 FTS 记录被删除，
     §5.3 第 6 步），回滚完成后读路径恢复服务旧内容。
  3. **正常路径（不注入崩溃）**：①→②→③ 与 HTML 派生全部完成后，断言权威文件/元数据/
     FTS/双摘要一致且读路径校验通过、正常服务新内容（对照组，验证两个崩溃注入点本身没有
     改变正常路径的可观察结果）。
  同一注入钩子亦适用于 redact 的两步式样：①后②前注入 → 断言读路径不服务该条并触发对账，
  对账前向完成 redact（Core 行 `redacted`、FTS 删除）。
- **可注入故障**：写序三注入点（B5 专项，见上）；FTS 表损坏（预期
  `MEM-502` → 重建恢复）；1 MiB 边界正文
  （`MEM-413`）；并发同 id 更新（预期串行化、无半文件）。

## 10. 迁移与 rollout <!-- §10 -->

### 10.1 Memory 一次性迁移契约（A12 + B03 裁定） <!-- §10.1 -->

**迁移范围二分（B03 裁定）**：旧系统（现行 aihub，workflow-v2）的存量按类别二分，两类处置相反：

- **workflow-v2 执行状态：不迁移**。旧系统的 invocation / episode / step 状态机、run attempt 等执行态一律留在旧库（`00-architecture.md#§1.2`：workflow-v2 执行状态不迁移，vNext 按新系统对待；`01-workflow-skills.md#§1.2`：vNext 不复用其状态机语义），vNext 从零开始，不存在任何执行状态迁移义务，也不存在两系统间的执行状态同步通道。
- **Memory：必须迁移**。Memory 是旧系统唯一必须迁移的存量数据（`07-deployment-pilot.md#§11`：Memory 存量是唯一迁移项）——它承载团队知识（经验/规则/事实），不迁移即丢失；其迁移是一次性、单向动作，不构成两系统间的同步通道。

**导出形态（A12 裁定）**：现有 aihub memory（现行数据库存储）一次性导出为**权威 Markdown + 元数据**——每条旧 memory 落一个 `memory.md`（frontmatter 按 §4.2 镜像旧记录全部字段：`id`/`type`/`owner_user_id`/`title`/`tags`/`visibility`/`work_item_id`/`related`/`created_at`/`updated_at` + 正文），由**管理 CLI 直写文件**、**不走 Memory API**：迁移是服务静默窗口内的一次性管理动作，API 写序（§5.2 的 `backupBarrier` 读锁与 per-`mem_id` 互斥）为单条在线写入设计，逐条经 API 搬运既无必要也不适用；CLI 直写不产生 `memory.md.tmp`/`history/` 等 API 写序产物（§5.3 第 1 步无可清扫对象）。旧系统的 redacted（软删）条目落 `_redacted/`，保持软删状态不复活。

**id 映射规则（B03 裁定）**：

- **project_id**：旧项目名 → 新 `projects` 行。迁移前先按旧项目清单在新系统逐个创建 `projects` 行（多项目是功能面，`00-architecture.md#§12` 第 1 项），迁移 CLI 产出「旧项目名 → 新 `project_id`」映射表并留档；memory 文件按新 `project_id` 落位。
- **user_id**：旧 user → 新 `users` / machine user 行。人类用户按旧 email 在新 `users` 建行；机器/agent 账号按旧 actor 建 machine user；frontmatter 的 `owner_user_id` 写映射后的新 id。
- **WI id（`work_item_id`）**：不迁移旧 WI（执行状态不迁移，见上），故**不做 id 映射**——frontmatter 中的旧 `work_item_id` 值**置空**，并在迁移报告中记入置空清单（条目 `mem_id` + 原值），供人工后续按需补挂新 WI 引用。`related` 中的 `mem_…` 引用在同批迁移内天然保持有效（`mem_id` 不变）；指向不在迁移范围内的 `mem_id` 时置空该项并记入同一清单。
- **不可解析关联的分层处置（与 `02-aihub-core.md#§4.9` 的 NOT NULL/FK 约束一致）**：`project_id`/`owner_user_id` 是**必需身份，不置空**——旧 project 名无法映射时，迁移工具先在目标库按旧名建占位 `projects` 行（visible=false）再挂 FK；旧 user 无法映射时，先建占位 machine user（display_name=旧名+`(imported)`）再挂 owner；两者记入「占位身份清单」供人工合并。**可空关联**（`work_item_id`、`related` 条目）才适用「置空 + 记入不可解析清单」——不猜值、不静默丢弃、不因单条解析失败中断整批迁移。

**导入顺序（B03 裁定）**：① `projects` / `users`（含 machine user）先建——全部 id 映射就位；② memory 文件落位——CLI 按 frontmatter 直写 `<data_dir>/memory/<new_project_id>/<mem_id>/memory.md`；③ 调用 `POST /api/v1/memories/rebuild-index`（§5.3，与启动对账同一流程）从 Markdown 权威文件全量重建元数据与 FTS——存量条目落入 §5.3 第 3 步「有文件无行」补建分支（该分支保留的「经权威写路径之外的手工放置/导入」覆盖面正是为迁移预留）。此后权威恒为文件，存量数据库内容不再被任何读路径引用。

**迁移后校验（B03 裁定）**：① **条数对账**——重建后 `memories` 行数 = 导出条目数（active 与 redacted 分列比对）；② **`content_sha` 抽样**——按固定比例随机抽取条目，现读现算 `memory.md` 的 sha256 与 `memories.content_sha` 比对，任一抽样不一致即迁移缺陷（抽样只为控成本，一致性本身不分级）；③ **检索冒烟**——对每条抽样条目以其正文 3 字符子串做 `q=` 检索，断言可命中；④ **置空清点**——迁移报告中的置空清单条数与实际置空条目数一致。校验失败 → 修复后整批重跑：迁移是幂等的一次性动作，重跑以文件为准重建（§5.3 同一流程），不产生重复行。
### 10.2 引入顺序 <!-- §10.2 -->

- 引入顺序：authority 读写（含 §5.2 写序）→ 启动/对账任务（含 `memory.md.tmp`
  清扫、孤儿行回滚、`_redacted/` 状态纠正，§5.3）→ FTS 索引与实时检索 → HTML 派生（含
  `source_markdown_sha`/`html_sha` 双摘要记录）。**对账任务必须与 authority 读写同期上线**：读路径服务前校验
  （INV-7f）在对账未就位时会永久拒服务，两者不得拆开交付。Memory 写路径接入
  `backupBarrier`（§4.6）与 `07` 的备份能力上线顺序一致：备份屏障机制先于 Memory 写路径的
  接入代码上线，避免出现"备份已存在但 Memory 写入尚未接入锁"的中间窗口。

### 10.3 演进方向（非 v1 项）：向量语义检索层（B02 裁定） <!-- §10.3 -->

- **向量层不在 v1 的任何行为、接口或验收面内**：v1 不实现、不部署、不验收——不存在向量索引文件（§4.1 目录布局不含 `vec-*` 文件）、不存在 `semantic` 检索参数、不存在向量层健康检查与降级路径，M6/`PILOT-V6`（`07-deployment-pilot.md#§12` 第 6 项）不为其设任何验收口径。
- **演进时的嵌入对象**：届时只 embed **memory 条目**——wrap 摘要条目（§2.1 A13）与人工创建/更新的记忆；不 embed 其他任何数据（不 embed WI 正文、执行态、session transcript 或 HTML 派生物）。
- **文件格式占位（仅演进方向记录）**：`vec-<model-tag>.bin`（按 `mem_id` → 向量的定长记录文件；模型标签入文件名，模型升级 = 新文件并行写入 + 原子切换，旧文件可删）**仅作为演进方向记录**——v1 不为其实现任何读写路径，v1 目录布局（§4.1）不保留该文件。
- 模型选型与维度等开放问题见 §11 第 1 项；是否为其补设延迟门槛亦属 v1 之后的演进决策。

## 11. 未决问题 <!-- §11 -->

1. **向量层（非 v1 演进项，§10.3）的模型选型与维度**：开放，属 v1 之后的演进决策；v1 不部署向量层、不为任何向量文件格式作实现承诺（`vec-<model-tag>.bin` 仅为 §10.3 的演进方向占位记录）。
2. **Memory 版本历史（`history/`）的保留深度**：一期全量保留，裁剪策略开放。
3. **跨项目 Memory 引用（`related` 跨 project）**：一期 `related` 限同项目；跨项目开放且不得
   绕过可见性。
