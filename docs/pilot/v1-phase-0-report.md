# v1 Pilot Phase-0 报告底稿 <!-- v1 phase-0 report skeleton -->

- **性质**：底稿（skeleton）——只建结构，不含任何实测读数。M1–M9 的当前读数与 `PILOT-V1`…`PILOT-V8` 的判定读数全部为「待填」，待 40 票观测窗收官、数据齐备后按权威口径机械读取并填入。本底稿**不构成任何 Pilot 通过/失败的证据**，与 v1 偏差登记表（`docs/pilot/v1-phase-0-deviation-register.md`）「不在观测数据之外认证任何 Pilot 通过」的立场一致。
- **权威来源（只读，本文不修改它们）**：判定公式与指标口径的唯一权威是 `docs/design/polyforge-vnext/07-deployment-pilot.md`——§9.1（Pilot 范围与观测窗口）、§9.2（M1–M9 口径，INV-D5 窗口/分母）、§9.2a（M4/M5 机械证据链）、§9.3（八条否决门槛表与 M9 观测项收尾注）。本文对 §9.3 门槛行**逐字引用**，不改写、不重排、不重编号；§9.2 的窗口/分母速注只是导航摘要，不替代原文。偏差与补偿背景引用 **v1** 偏差登记表 `docs/pilot/v1-phase-0-deviation-register.md`（#11）；注意 `docs/pilot/phase-0-deviation-register.md` 是 v0 登记表，**不是** v1 的登记权威。
- **不改门槛公式**：本文不校准、不改写、不重排任何门槛公式；`07-deployment-pilot.md` §12 第 1 条明定门槛判定公式（§9.3）不因校准改变。
- **v0 教训台账覆盖声明（recall-first，X4）**：按 X4（`docs/design/polyforge-vnext/v0-lessons-ledger.md`）先读台账再动笔。与本 docs-only 底稿相关的条目为「v1 pilot findings」F1–F7（均已修复落 main 或已在 #11 登记表登记，修复证据以 #11 登记表为准）与 X4 本身。其对本底稿的约束只有两条：(a) 引用 F1–F7 所登记的 **#11（v1）** 登记表；(b) 本报告是无认证读数的占位底稿，收官填数前不得被视为或用作 Pilot 通过/失败的证据。

---

## 1. 八条否决门槛表（判定公式逐字引自 `07-deployment-pilot.md#§9.3`）

| 门槛 | 判定（机械，逐字引自 §9.3） | 触发动作（逐字引自 §9.3） | 判定读数 |
|---|---|---|---|
| `PILOT-V1` | M1 计数 > 0（Pilot 累计运行以来任何一次 HEAD 回退） | 否决：回滚或重设计 Sync 域 | 待填 |
| `PILOT-V2` | M2 分子（实际移除数）> 0 | 否决：回滚或重设计删除面 | 待填 |
| `PILOT-V3` | M3 计数 > 1（在 ≥ 40 张 WI 的样本内） | 否决：执行恢复语义重设计 | 待填 |
| `PILOT-V4` | M4 比例 < 100%（分母口径见 §9.2；出现任何一次「未在执行权威文件留下证据却判定通过」） | 否决：验证承载面重设计 | 待填 |
| `PILOT-V5` | M5 计数（§9.2a 机械定义）> 0 | 否决：强校验契约重设计 | 待填 |
| `PILOT-V6` | M6 的滚动 24 小时 p99（样本口径见 §9.2；仅 **FTS5 trigram 写入→可检索延迟**样本）**> 5000 ms**，即超出 `06-memory-search.md#§4.5` 固定可计算阈值「写入→可检索 p99 ≤ 5s」 | 否决：索引路径重设计 | 待填 |
| `PILOT-V7` | M7 计数 > 3 | 否决：对账机制重设计 | 待填 |
| `PILOT-V8` | M8 比例（§9.2 定义的、仅基于已发生边界同步请求的耗时超阈值/失败占比）> 1% | 否决：全扫描发布链路时延或失败率重设计 | 待填 |

**M9 不设门槛（观测项）**——§9.3 收尾注原文：

> 「`M9` 不设否决门槛（观测项）：rewind 数量上界本身已由 `01-workflow-skills.md#§6.2` 的
> `max_per_step`/`max_total` 收敛，超界直接进入终态（`WF-101`），不需要额外的 Pilot 期比例门槛。」
> ——`07-deployment-pilot.md#§9.3`

全部门槛均为「读 API/指标即判」（§9.3），无主观判断项；判定输入为下表 M1–M9。

## 2. M1–M9 指标定义占位（口径权威 = §9.2 / §9.2a；速注不替代原文）

| 指标 | 口径锚点 | 窗口/分母速注（INV-D5；原文以锚点为准） | 当前读数 |
|---|---|---|---|
| M1 现场一致性 | §9.2（事故计数定义见 `05-sync-revision-recovery.md#§9`） | 计数型指标，非比例，无分母（出现即事故）；窗口 = Pilot 累计运行以来。 | 待填 |
| M2 误删 | §9.2 | 分子 = 对非终态 WI 的删除尝试中实际移除文件的次数；分母 = 同一窗口内对非终态 WI 的全部删除尝试次数；窗口 = Pilot 累计运行以来。正常应恒为 0。 | 待填 |
| M3 卡死步 | §9.2 | 计数型指标：`in_progress` 超过 24 小时且对应机器无心跳事件的 step 数；窗口 = Pilot 累计运行以来。 | 待填 |
| M4 校验真实性 | §9.2（证据链约束见 §9.2a INV-D3/INV-D4） | 分子 = `runtime-state/execution.json` 记录含结构化命令证据（`output.commands` 至少一条 `{argv, exit_code, duration_ms}` 且 `exit_code` 可读）的 verification step 次数——任意非空 `output` 不构成证据；分母 = 同一窗口内全部已执行的 verification step 次数；窗口 = Pilot 累计运行以来；旧版 schema 数据（无 `commands` 字段）不计分子。 | 待填 |
| M5 谎报逃逸 | §9.2 + §9.2a（机械信号） | §9.2a 给出的机械信号命中次数：该 step 相对自己起始基线（`steps.<id>.step_start_refs`）的工作树 diff 为空、`artifacts/` 无新增、执行权威文件 `output` 为空三条件**同时**成立，仅对 `kind ∈ {code, author}` 的 step 计算；不依赖人工抽查，不以 `RunCommand` 调用次数为信号。计数型指标，不是比例；窗口 = Pilot 累计运行以来。 | 待填 |
| M6 检索实时性 | §9.2（阈值权威 = `06-memory-search.md#§4.5`） | 滚动 24 小时窗口内 Memory 写入→可检索延迟样本（仅 FTS5 trigram 文本索引；向量语义检索不参与本指标）的 p99；合格阈值 = `06-memory-search.md#§4.5` 固定可计算值 p99 ≤ 5s（5000 ms）；样本集 = 窗口内全部写入事件产生的延迟样本；样本数为 0 时该窗口不参与否决判定。 | 待填 |
| M7 同步收敛 | §9.2 | 计数型指标：全扫描发布后对同一 WI 连续 3 次触发事件扫描结果仍存在未收敛差异的次数；以「触发次数」而非时间周期为分母口径；窗口 = Pilot 累计运行以来。 | 待填 |
| M8 发布时延与结果 | §9.2（B13 口径修订） | **仅对已真实发生的边界同步请求计分**，不使用任何「无活动时间窗口」判定：分子 = 以非幂等失败终止或耗时超过运维告警阈值（复用 `05-sync-revision-recovery.md#§9` 已定义口径）的请求次数；分母 = 同一 Pilot 观测窗口内全部实际发生的边界同步请求次数；分母为 0 时该次计算不参与否决判定。 | 待填 |
| M9 rewind 收敛 | §9.2 | 数据源 = `runtime-state/execution.json` 的 `rewind_stats`（`{total, by_step}`），经 `02-aihub-core.md#§5.2` read-through 逐字读取；窗口 = 单 WI 生命周期内累计值，不做跨 WI 聚合比例。**观测项，不设否决门槛**（§9.3 收尾注，见上文）。 | 待填 |

## 2a. M1–M9 在 v1 实栈上的机械计算式（收官填数操作口径）

本节把每条指标在 **v1 实栈**上的机械计算式写死：数据来源 ∈ {events.log（daemon 数据目录 `<DataDir>/events.log`，JSONL）、aihub（Core API 状态面）、裸仓、`runtime-state/execution.json`（read-through）}，收官填数者从哪读、怎么算，以本节为准；口径锚点仍以 `07-deployment-pilot.md#§9.2`/`§9.2a` 原文为权威，本节不替代、不修改任何门槛公式（§12 第 1 条：校准不改判定式）。其中 M6/M8 的两个耗时样本字段（`write_ms`、`commit_ms`/`push_ms`）是本底稿配套的事件体观测增量（daemon/observe 代码已携带，见各条「数据源 = 事件体新字段」标注）：**观测字段增量不构成行为/判定/线协议变更**，与 #11 登记表「不在观测数据之外认证任何 Pilot 通过」立场一致——收官前本节不产生任何实测读数。

- **M1 现场一致性**（数据源 = 裸仓 + events.log）：沿窗口内每张 WI 裸仓 first-parent 链逐提交读 `Polyforge-Seq` trailer，计数任何 seq 回退 / HEAD 后移点。v1 pre-receive hook（FF-only、禁 ref 删除、seq 严格单调）使实际回退结构上恒为 0；未遂回退以 hook 拒绝的形式落在 `push_failed.error`，记为佐证，不计入事故数。计数型，无分母；窗口 = Pilot 累计运行以来。
- **M2 误删**（数据源 = v1 栈删除面本身）：v1 无删除面（13 个路由 verb 无 delete；无 `DELETE capsule-data`/tombstone；runtime `RemoveAll` 限于本 WI staging/swap 与异代隔离）⇒ 分子 = 分母 = 0——这是删除面缺席下的**机械真读**，不是豁免；公式如实写明，PILOT-V2 以分子 > 0 判，不受影响（未来若引入删除面，读数自然受判）。
- **M3 卡死步**（数据源 = aihub）：计数「step `in_progress` 超 24h 且所属 machine `last_heartbeat_at` 超存活阈值」的 step 数。计数型，无分母；窗口 = Pilot 累计运行以来。
- **M4 校验真实性**（数据源 = `runtime-state/execution.json`，read-through）：分子 = verification step 中 `output.commands` 至少含一条可读 `{argv, exit_code, duration_ms}` 者的次数；分母 = 全部已执行 verification step 次数；旧版 schema（无 `commands` 字段）不计分子；窗口 = Pilot 累计运行以来。
- **M5 谎报逃逸**（数据源 = execution.json `step_start_refs` + 工作树 diff + `artifacts/` + `output`）：§9.2a 三条件同时成立（对起始基线的工作树 diff 为空、`artifacts/` 无新增、`output` 为空）的 `kind ∈ {code, author}` step 计数。计数型，无分母；窗口 = Pilot 累计运行以来。
- **M6 检索实时性**（数据源 = events.log，**事件体新字段 `write_ms`**）：滚动 24h 窗口内全部 `memory_remember.write_ms` 样本的 p99；样本数为 0 的窗口不参与否决判定；阈值逐字引用 `06-memory-search.md#§4.5` 的 p99 ≤ 5000 ms，不新造数值。**样本集边界如实登记**：迁移（CLI 直写）与服务端 reconcile 自愈回写不产生事件、不入样本集；覆盖该路径需新事件 kind，未获授权，不在本 fix 内。窗口内样本集 = 全部 `memory_remember` 事件。
- **M7 同步收敛**（数据源 = events.log）：一次全扫描发布失败后，同一 WI 连续 ≥3 次发布尝试（每次尝试恰产生一条 `published` 或 `push_failed`）仍存在「本地提交未达已发布 HEAD」未收敛差异，计 1 次；任一 `published` 即收敛、终结该段。**聚合口径（公式注明）**：`push_backpressure` 为降级段标记，不计入；seal 路径 ≤3 次内部重试聚合为一条 `wrap_blocked`，不产生逐次扫描样本。计数型，无比例分母；窗口 = Pilot 累计运行以来。
- **M8 发布时延与结果**（数据源 = events.log，**事件体新字段 `commit_ms`/`push_ms`**）：请求单位 = 每次实际发生的发布尝试（v1 对 §9.2「边界同步请求」的机械实现；wrap_blocked 聚合的内部重试按 1 计并注明）。分母 = count(`published`) + count(`push_failed`) + count(`wrap_blocked`)，分母为 0 不参与否决判定。分子 = ① 耗时超阈值分量：`published` 中 `commit_ms + push_ms > T` 的条数；② 失败分量：以持续类（非幂等）错误终止的尝试数——error 串属 pre-receive 拒绝（非 FF / ref 删除 / gitlink / blob 超限 / seq 违规 / attempt-id 不匹配）、裸仓 404、鉴权/凭证类；排除瞬时网络/超时类（#11 登记表 F2 的 404 样本按既有口径计入分子与分母，不预先豁免）。`push_backpressure` 不计入分子与分母。**阈值 T 状态**：T = `05-sync-revision-recovery.md#§9` 运维告警口径的运维配置值；v1 装机至今未配置——**T 待 Pilot 运维配置（属 §12 校准议题，校准不改判定式），本底稿不发明数值**。
- **M9 rewind 收敛**（数据源 = execution.json `rewind_stats`，read-through）：单 WI 生命周期累计 `{total, by_step}` 逐字读取，不跨 WI 聚合；观测项，无门槛。

## 3. 观测窗与计数口径

- **观测窗口**（`07-deployment-pilot.md#§9.1`）：30 天，或累计 ≥ 40 张 wrapped WI，以先满足者为准（两者均为 Pilot 设计参数，校准议题见 §12 第 1 条——校准不改变 §9.3 判定公式）。
- **v1 计数起点**：2026-09-30 v1 装机，**重新计数**——v0 pilot 已 wrap 的 7 张不计入 v1 窗口（那是旧系统代码跑出的结果，不构成 v1 栈的观测样本）。口径依据见 v1 偏差登记表第 3 条（规模面）：`docs/pilot/v1-phase-0-deviation-register.md`（#11）。
- **就绪队列口径**（§9.2 定稿裁定）：vNext 就绪队列是扁平过滤列表，任何涉及队列条目的计数一律以扁平两数组 `{executable, blocked}`（`02-aihub-core.md#§5.1b`）的返回条目数为口径，不按段分别计数、不设段间配额。
- **偏差与补偿背景**：装机面（单机双 daemon、`boot.sh` 起停）、协议面（8420/8421 同机双栈）、规模面（1 人 / 1 仓 / 窗口重计）与真机发现面 F1–F7 逐条登记于 #11 登记表（`docs/pilot/v1-phase-0-deviation-register.md`）。其结论：**§9.3 八条门槛的判定公式与输入定义（M1–M9）不受任何一条偏差影响**；但**公式不变不等于读数不受影响**——缺陷存续期间实际发生的失败样本（如 F2 的发布 404、F3 的恢复/续跑失败、F5 的卡死）按既有口径如实计入分子与分母，不预先豁免，最终以观测窗数据按 §9.3 机械判定。
- 本底稿与 #11 登记表同姿态：观测数据齐备之前，既不宣布通过、也不宣布失败。

## 4. 当前读数占位

- 上文 M1–M9 的「当前读数」与门槛表 `PILOT-V1`…`PILOT-V8` 的「判定读数」**全部为「待填」**，待 pilot 收官（观测窗口按 §9.1 先满足者为准）后填入；本文不含任何实测数值，收官填数前任何一格不得被解释为通过或失败。
- 收官填数时的读数来源（机械，§9.2 / #11 登记表总结；逐条操作口径见 §2a）：Core API 状态与事件面（M1/M2/M3/M7）、`runtime-state/execution.json`（M4 的结构化命令证据、M9 的 `rewind_stats`）、§9.2a 机械信号（M5）、events.log `memory_remember` 事件体的新字段 `write_ms`（M6——**数据源 = 事件体新字段**，公式见 §2a）、events.log 的发布尝试事件，`published` 事件体携带新字段 `commit_ms`/`push_ms`（M8——**数据源 = 事件体新字段**，公式见 §2a）。
- 填数属另一个工作项；本底稿只建结构，不改门槛公式、不改指标口径、不改任何权威文档。
