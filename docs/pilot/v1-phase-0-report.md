# v1 Pilot Phase-0 报告底稿 <!-- v1 phase-0 report skeleton -->

- **性质**：已填读数（filled）——本报告原为 #22 建立的读数空底稿（skeleton），现由 polyforge#51（wi_cCDknRmmW72g）在观测窗闭合后按 §2a 操作口径机械填入：M1–M9 当前读数与 `PILOT-V1`…`PILOT-V8` 判定读数全部转写自离线只读计算器 `tools/gate-metrics.go` 的输出快照（读取时刻 2026-09-30T18:53Z，数据根 `/root/polyforge-pilot-v1`，调用命令与逐字快照落点见 §4）。填数后本报告**仍不构成任何 Pilot 通过/失败的认证**：M8 耗时阈值未配置、M3 心跳面未接线、M6 无样本、M5 条件(a) 离线不可评估（未覆盖面登记见 §5），§9.4 达标面的整体裁定不属本 chore——与 v1 偏差登记表（`docs/pilot/v1-phase-0-deviation-register.md`）「不在观测数据之外认证任何 Pilot 通过」的立场一致。观测窗闭合依据（§9.1「累计 ≥ 40 张 wrapped WI」，本 WI 为第 40 张）见 §4 的机械计数。
- **权威来源（只读，本文不修改它们）**：判定公式与指标口径的唯一权威是 `docs/design/polyforge-vnext/07-deployment-pilot.md`——§9.1（Pilot 范围与观测窗口）、§9.2（M1–M9 口径，INV-D5 窗口/分母）、§9.2a（M4/M5 机械证据链）、§9.3（八条否决门槛表与 M9 观测项收尾注）。本文对 §9.3 门槛行**逐字引用**，不改写、不重排、不重编号；§9.2 的窗口/分母速注只是导航摘要，不替代原文。偏差与补偿背景引用 **v1** 偏差登记表 `docs/pilot/v1-phase-0-deviation-register.md`（#11）；注意 `docs/pilot/phase-0-deviation-register.md` 是 v0 登记表，**不是** v1 的登记权威。
- **不改门槛公式**：本文不校准、不改写、不重排任何门槛公式；`07-deployment-pilot.md` §12 第 1 条明定门槛判定公式（§9.3）不因校准改变。
- **v0 教训台账覆盖声明（recall-first，X4）**：按 X4（`docs/design/polyforge-vnext/v0-lessons-ledger.md`）先读台账再动笔。与本 docs-only 底稿相关的条目为「v1 pilot findings」F1–F7（均已修复落 main 或已在 #11 登记表登记，修复证据以 #11 登记表为准）与 X4 本身。其对本底稿的约束只有两条：(a) 引用 F1–F7 所登记的 **#11（v1）** 登记表；(b) 填数前本报告是无认证读数的占位底稿，收官填数前不得被视为或用作 Pilot 通过/失败的证据；填数后（即现状）读数如实含未配置/无样本/不可评估面（§5），同样不得被用作 Pilot 通过认证。

---

## 1. 八条否决门槛表（判定公式逐字引自 `07-deployment-pilot.md#§9.3`）

| 门槛 | 判定（机械，逐字引自 §9.3） | 触发动作（逐字引自 §9.3） | 判定读数 |
|---|---|---|---|
| `PILOT-V1` | M1 计数 > 0（Pilot 累计运行以来任何一次 HEAD 回退） | 否决：回滚或重设计 Sync 域 | **0 → 未触发**（扫描 44 仓 / 497 提交，seq 回退/HEAD 后移点 0；push_failed 186 条中 hook 拒绝类 185 条记为未遂回退佐证、不计入事故数） |
| `PILOT-V2` | M2 分子（实际移除数）> 0 | 否决：回滚或重设计删除面 | **0 → 未触发**（M2 = 0/0：v1 无删除面 ⇒ 分子=分母=0 为机械真读，结构读数非豁免；未来引入删除面后读数自然受判） |
| `PILOT-V3` | M3 计数 > 1（在 ≥ 40 张 WI 的样本内） | 否决：执行恢复语义重设计 | **0 → 未触发**（样本 51 张 WI ≥ 40；in_progress step 共 1 个、最长 13h28m40s < 24h；心跳数据不可用/未配置，不参与判定——`--m3-heartbeat-max-age` 未提供，不设默认值） |
| `PILOT-V4` | M4 比例 < 100%（分母口径见 §9.2；出现任何一次「未在执行权威文件留下证据却判定通过」） | 否决：验证承载面重设计 | **22/39 = 56.41% → 触发（否决门槛成立）**（已执行 verification step 39、另有 pending 未执行 1 不计分母；无证据分子外 12 个旧 schema（无 commands 字段，不计分子仍计分母）+ 5 个 commands 无可读条目） |
| `PILOT-V5` | M5 计数（§9.2a 机械定义）> 0 | 否决：强校验契约重设计 | **0 → 未触发**（已完成 code/author step 评估 83 个，候选（条件(b)∧(c)）0 个——候选为 0 时三条件不可能同时成立，读数 0 机械成立；条件(a) 工作树 diff 离线不可解析，如实呈现） |
| `PILOT-V6` | M6 的滚动 24 小时 p99（样本口径见 §9.2；仅 **FTS5 trigram 写入→可检索延迟**样本）**> 5000 ms**，即超出 `06-memory-search.md#§4.5` 固定可计算阈值「写入→可检索 p99 ≤ 5s」 | 否决：索引路径重设计 | **无样本 → 不参与判定**（memory_remember 事件 33 条、携带 write_ms 字段 0 条；旧 schema 事件面未含观测字段、迁移/服务端 reconcile 回写不产生事件，为已登记的样本集边界） |
| `PILOT-V7` | M7 计数 > 3 | 否决：对账机制重设计 | **1 → 未触发**（最长连续失败尝试 185 次，wi_VCyBYf7uYeb2） |
| `PILOT-V8` | M8 比例（§9.2 定义的、仅基于已发生边界同步请求的耗时超阈值/失败占比）> 1% | 否决：全扫描发布链路时延或失败率重设计 | **188/647 = 29.06% → 触发（否决门槛成立）**（分母 = published 459 + push_failed 186 + wrap_blocked 2，push_backpressure 3 条不计；失败分量：持续类 188（计入）、瞬时类 0（排除）、未分类 0（不计入）；耗时分量：未配置/不参与判定——`--m8-threshold-ms` 未提供，无默认值，不代填） |

**M9 不设门槛（观测项）**——§9.3 收尾注原文：

> 「`M9` 不设否决门槛（观测项）：rewind 数量上界本身已由 `01-workflow-skills.md#§6.2` 的
> `max_per_step`/`max_total` 收敛，超界直接进入终态（`WF-101`），不需要额外的 Pilot 期比例门槛。」
> ——`07-deployment-pilot.md#§9.3`

全部门槛均为「读 API/指标即判」（§9.3），无主观判断项；判定输入为下表 M1–M9。

## 2. M1–M9 指标定义占位（口径权威 = §9.2 / §9.2a；速注不替代原文）

| 指标 | 口径锚点 | 窗口/分母速注（INV-D5；原文以锚点为准） | 当前读数 |
|---|---|---|---|
| M1 现场一致性 | §9.2（事故计数定义见 `05-sync-revision-recovery.md#§9`） | 计数型指标，非比例，无分母（出现即事故）；窗口 = Pilot 累计运行以来。 | 0（计数）。扫描 44 仓 / 497 提交（空仓 2，缺 seq trailer 提交 0）；push_failed 共 186 条，其中 hook 拒绝类 185 条记为未遂回退佐证（不计入事故数）。裸仓面照录：规范仓 44（合并同仓副本 42 份，HEAD 不一致 0 处，空仓 2 个）；wi_3Ot791uZaXqL 仅见于 aihub-data/git（机器目录无此仓）。 |
| M2 误删 | §9.2 | 分子 = 对非终态 WI 的删除尝试中实际移除文件的次数；分母 = 同一窗口内对非终态 WI 的全部删除尝试次数；窗口 = Pilot 累计运行以来。正常应恒为 0。 | 0/0（分子/分母）。v1 无删除面（13 路由 verb 无 delete）⇒ 分子=分母=0 为机械真读（结构读数，非豁免）；PILOT-V2 以分子>0 判，未来引入删除面后读数自然受判。 |
| M3 卡死步 | §9.2 | 计数型指标：`in_progress` 超过 24 小时且对应机器无心跳事件的 step 数；窗口 = Pilot 累计运行以来。 | 0（in_progress 超 24h 的 step 数）。in_progress step 共 1 个，最长 13h28m40s；心跳面：心跳数据不可用/未配置，不参与判定（--m3-heartbeat-max-age 未提供，不设默认值）；step 状态读自已发布 HEAD 的 execution.json read-through（发布失败期间的机器本地最新状态不可见于离线数据），如实呈现。样本 51 张 WI。 |
| M4 校验真实性 | §9.2（证据链约束见 §9.2a INV-D3/INV-D4） | 分子 = `runtime-state/execution.json` 记录含结构化命令证据（`output.commands` 至少一条 `{argv, exit_code, duration_ms}` 且 `exit_code` 可读）的 verification step 次数——任意非空 `output` 不构成证据；分母 = 同一窗口内全部已执行的 verification step 次数；窗口 = Pilot 累计运行以来；旧版 schema 数据（无 `commands` 字段）不计分子。 | 22/39 = 56.41%。已执行 verification step 39（另有 pending 未执行 1 不计分母）；无证据分子外 12 个旧 schema（无 commands 字段）+ 5 个 commands 无可读条目。 |
| M5 谎报逃逸 | §9.2 + §9.2a（机械信号） | §9.2a 给出的机械信号命中次数：该 step 相对自己起始基线（`steps.<id>.step_start_refs`）的工作树 diff 为空、`artifacts/` 无新增、执行权威文件 `output` 为空三条件**同时**成立，仅对 `kind ∈ {code, author}` 的 step 计算；不依赖人工抽查，不以 `RunCommand` 调用次数为信号。计数型指标，不是比例；窗口 = Pilot 累计运行以来。 | 0（计数）。已完成 code/author step 评估 83 个（未完成跳过 2 个，kind 不可判定跳过 0 个）；候选（条件(b)∧(c)）0 个——命中 ⊆ 候选，候选为 0 时三条件不可能同时成立，读数 0 机械成立。条件(a) 工作树 diff：不可评估——step_start_refs 指向 repos/<name> 内层仓库的 commit，而 /repos/ 不入 capsule 同步（.gitignore），内层仓库对象不存在于 pilot 数据根，条件(a) 离线不可解析，如实呈现（条件(b) 基线取该 step 完成提交的前一个 capsule 发布提交，中途恢复性发布会使基线偏晚，已登记）。 |
| M6 检索实时性 | §9.2（阈值权威 = `06-memory-search.md#§4.5`） | 滚动 24 小时窗口内 Memory 写入→可检索延迟样本（仅 FTS5 trigram 文本索引；向量语义检索不参与本指标）的 p99；合格阈值 = `06-memory-search.md#§4.5` 固定可计算值 p99 ≤ 5s（5000 ms）；样本集 = 窗口内全部写入事件产生的延迟样本；样本数为 0 时该窗口不参与否决判定。 | 无样本 → 不参与判定。memory_remember 事件 33 条，其中携带 write_ms 字段 0 条（样本 0 个）；阈值权威 06-memory-search.md#§4.5「写入→可检索 p99 ≤ 5s（5000 ms；固定值，不可配置）」（仅引用，不新造）。无样本原因：旧 schema 事件面未含观测字段；迁移/服务端 reconcile 回写不产生事件（已登记的样本集边界）。 |
| M7 同步收敛 | §9.2 | 计数型指标：全扫描发布后对同一 WI 连续 3 次触发事件扫描结果仍存在未收敛差异的次数；以「触发次数」而非时间周期为分母口径；窗口 = Pilot 累计运行以来。 | 1（计数）。最长连续失败尝试 185 次（wi_VCyBYf7uYeb2）。 |
| M8 发布时延与结果 | §9.2（B13 口径修订） | **仅对已真实发生的边界同步请求计分**，不使用任何「无活动时间窗口」判定：分子 = 以非幂等失败终止或耗时超过运维告警阈值（复用 `05-sync-revision-recovery.md#§9` 已定义口径）的请求次数；分母 = 同一 Pilot 观测窗口内全部实际发生的边界同步请求次数；分母为 0 时该次计算不参与否决判定。 | 188/647 = 29.06%。分母 = published 459 + push_failed 186 + wrap_blocked 2（push_backpressure 3 条为降级标记，不计入分子与分母）；耗时分量：未配置/不参与判定（--m8-threshold-ms 未提供，无默认值，不代填；携带 commit_ms+push_ms 字段的 published 0 条，超阈值 0 条）；失败分量：持续类 188（计入），瞬时类 0（排除），未分类 0（不计入）。 |
| M9 rewind 收敛 | §9.2 | 数据源 = `runtime-state/execution.json` 的 `rewind_stats`（`{total, by_step}`），经 `02-aihub-core.md#§5.2` read-through 逐字读取；窗口 = 单 WI 生命周期内累计值，不做跨 WI 聚合比例。**观测项，不设否决门槛**（§9.3 收尾注，见上文）。 | 观测项，不设门槛。逐 WI read-through 照录（不跨 WI 聚合）：40 个 WI 的 rewind_stats 逐字读数中，14 个缺 rewind_stats 字段（旧 schema，照录标注）；唯一非零 = wi_Xah1Y0REajOb（polyforge#23）`{"count":1,"to_step":["fix"]}`，其余全为 0。逐 WI 逐字读数见紧随本表的「M9 读数块」。 |

### M9 读数块（工具输出逐字照录，2026-09-30T18:53Z 快照）

```text
M9 rewind 收敛（观测项，不设否决门槛）— 逐 WI read-through，不跨 WI 聚合:
    wi_0k05ak9v95dS          {"count":0,"to_step":[]}
    wi_340KBOZg8gXm          {"by_step":{},"total":0}
    wi_3WWHkJNT5tiA          {"by_step":{},"total":0}
    wi_3uxqBceRVcFh          （rewind_stats 字段缺失，旧 schema）
    wi_5x0CNtmiRMXJ          （rewind_stats 字段缺失，旧 schema）
    wi_7ORyHe0iLg2H          {"count":0,"to_step":[]}
    wi_8cGon3rDrypq          {"count":0,"to_step":[]}
    wi_DLzwgbXI2YG2          （rewind_stats 字段缺失，旧 schema）
    wi_DNsIyHZwZQB5          （rewind_stats 字段缺失，旧 schema）
    wi_L2ZyiXbEectN          {"count":0,"to_step":[]}
    wi_MuyoyHwOewS7          {"by_step":{},"total":0}
    wi_Pr1snK01qDS8          （rewind_stats 字段缺失，旧 schema）
    wi_PstymWtmKFAt          {"count":0,"to_step":[]}
    wi_RrKTmUqYzift          {"count":0,"to_step":[]}
    wi_TobxjUMungXl          {"by_step":{},"total":0}
    wi_VCyBYf7uYeb2          {"by_step":{},"total":0}
    wi_Xah1Y0REajOb          {"count":1,"to_step":["fix"]}
    wi_XlhUr5fXLjO1          {"count":0,"to_step":[]}
    wi_ZiKzuDaiGpgZ          （rewind_stats 字段缺失，旧 schema）
    wi_av0LZhpUbqRY          {"by_step":{},"total":0}
    wi_cVWghIMEIuiQ          {"count":0,"to_step":[]}
    wi_cVhdv3IInCRq          {"per_step":{},"total":0}
    wi_cnTF84S2eEpd          （rewind_stats 字段缺失，旧 schema）
    wi_dDCz1MjpczvZ          {"by_step":{},"total":0}
    wi_f50ZPPKYiXrI          {"count":0,"to_step":[]}
    wi_fE9931dzUMv2          {"count":0,"to_step":[]}
    wi_g3QsCdv1joZ8          （rewind_stats 字段缺失，旧 schema）
    wi_hPl82OPb7V3q          （rewind_stats 字段缺失，旧 schema）
    wi_hVYd0WqGUrPq          {"by_step":{},"total":0}
    wi_ibYL3MtSq0Lp          （rewind_stats 字段缺失，旧 schema）
    wi_ihbMI4rH43er          （rewind_stats 字段缺失，旧 schema）
    wi_izqGJFHcCaeH          （rewind_stats 字段缺失，旧 schema）
    wi_jwVtx8w5XDXE          {"count":0,"to_step":[]}
    wi_pyBtx4KHYyFV          {"by_step":{},"total":0}
    wi_qRJFlbdZOuEf          {"by_step":{},"total":0}
    wi_qxRRoSbpFblJ          {"by_step":{},"total":0}
    wi_s11m8pp8Pdw9          {"by_step":{},"total":0}
    wi_tEnGWCxYEXzw          （rewind_stats 字段缺失，旧 schema）
    wi_xpI1NzK8C36O          （rewind_stats 字段缺失，旧 schema）
    wi_xzrlpQQWRyLY          {"by_step":{},"total":0}
  缺 rewind_stats 字段的 WI: 14 个
```

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

## 4. 读数填充记录与证据索引（polyforge#51 收官填数）

- **填充记录（可复现）**：本报告的读数由 polyforge#51（wi_cCDknRmmW72g，chore/docs-only）于 2026-09-30 收官填入：读数 = 离线只读计算器 `tools/gate-metrics.go` 对数据根 `/root/polyforge-pilot-v1` 的输出快照，工具自报读取时刻 **2026-09-30T18:53Z**（人读表首行 2026-09-30T18:53:09Z / JSON `reading_time` 2026-09-30T18:53:12Z，同一快照的两面）；调用命令（于 `repos/polyforge` 仓根前台执行，均 exit 0）：`go run ./tools/gate-metrics.go /root/polyforge-pilot-v1`（人读表）与 `go run ./tools/gate-metrics.go --json /root/polyforge-pilot-v1`（JSON）。**未携带任何可选旗标**：`--m8-threshold-ms` 与 `--m3-heartbeat-max-age` 均未提供——未配置即为读数的一部分（M8 耗时分量、M3 心跳面均如实记「未配置/不参与判定」，工具无默认值、不代填）。工具输出逐字快照存于本 WI capsule `artifacts/gate-metrics-readings.txt` / `artifacts/gate-metrics-readings.json`；本文全部读数格均转写自该快照，未手工重算。读数来源面的原始指引（Core API / execution.json / 事件体新字段等）见原底稿下述清单，与 §2a 逐条对应。
- **观测窗闭合依据（§9.1「累计 ≥ 40 张 wrapped WI」，机械计数）**：main 仓 v1 期（merge 日期 ≥ 2026-09-30，显式 `+00:00` 锚点）`wi_*` 分支式 merge 共 **39 条**——PR #60–#93 = 登记簿快照的 34 张；PR #94–#98 = 快照后 wrap 的 5 张：#50（wi_340KBOZg8gXm，登记簿自身）、#47（wi_7ORyHe0iLg2H，台账 F11–F14 追加）、#49（wi_pyBtx4KHYyFV，gate-metrics 工具）、#48（wi_VCyBYf7uYeb2，README Quickstart）、#46（wi_DNsIyHZwZQB5，F12 停机 push 竞态修复）（slug 均以 capsule `runtime-state/work-item.json` 为权威）。39 张逐一机械核验：capsule 在位 + merged PR + `execution.json` 步面全 done，39/39 通过；排除面照录登记簿 §1（#9 v0 已 cancel、#10 v0 未执行、#14 paused 无 merged PR）。**本 WI（polyforge#51）wrap 后即为第 40 张，§9.1 观测窗随本 WI 闭合**（先于 30 天日历条件满足）。佐证：aihub.db `work_items_total` = 51（gate-metrics JSON 照录）——该数是 `work_items` 表总行数（含排队/取消/paused/v0 面），不是 wrapped 计数；wrapped 计数以上述机械判定式为准（39 → 40）。
- **证据索引**：(a) `docs/pilot/v1-wrapped-register.md`——34 张 wrapped 票的逐票证据登记簿（每票四拍：目标/交付物/关键裁定/发现 + 机械证据引用），取数快照 2026-09-30T13:46Z（排除表复核 14:10Z）；其 §1 判定式、§2 证据表（含「无裸仓的 4 张」标注）与 §4 M1–M9 字段级取数落点是本报告读数面的票据底册；#46–#50 及本 WI 在其快照之后完成窗口（机械计数见上），登记簿自身声明的补录政策属后续更新，不在本 WI 追改。(b) `docs/pilot/v1-phase-0-deviation-register.md`（#11）——读数解读的偏差/补偿背景：装机面（单机双 daemon、`boot.sh` 起停）、协议面（8420/8421 同机双栈）、规模面（1 人/1 仓/窗口重计）与 F1–F7 真机发现逐条登记；其既定口径——缺陷存续期的失败样本（F2 发布 404、F3 恢复失败、F5 卡死）按既有口径如实计入分子分母、不预先豁免——本报告读数照此执行（M7 的 185 次连续失败段 / M8 持续类失败分量的主体即 F12 停机竞态窗口的实测样本，root-cause 与修复见 v1 票 #46 及台账 F16）。
- **读数来源面（原底稿清单，收官填数实际取数路径与 §2a 一致）**：Core API 状态与事件面（M1/M2/M3/M7）、`runtime-state/execution.json`（M4 的结构化命令证据、M9 的 `rewind_stats`，经 read-through）、§9.2a 机械信号（M5）、events.log `memory_remember` 事件体的 `write_ms`（M6）、events.log 发布尝试事件 `published` 携带的 `commit_ms`/`push_ms`（M8）。本 WI 的实际取数由 `tools/gate-metrics.go` 离线完成（#49 落盘，读只读面：裸仓 + events.log + aihub.db `mode=ro` + execution.json read-through）。
- **不认证声明（沿底稿立场）**：填数后本报告的读数格是机械转写，不是 Pilot 通过认证——M8 耗时分量未配置、M3 心跳面未接线、M6 无样本、M5 条件(a) 离线不可评估（未覆盖面登记见 §5）；`PILOT-V1`…`PILOT-V8` 之外的整体 §9.4 达标面（三模式行使、graceful shutdown 触发等）属 owner 判断，不属本 chore。

## 5. Phase-1 输入：观测窗未覆盖面登记（每条挂证据，不作推测）

本节把 phase-0 观测窗**实际未覆盖的面**登记为 phase-1 的输入——每条都指向可查证据（偏差登记表 / 登记簿 / 读数快照 / 台账），不挂证据的不写。分两类：运行形态面（窗口内根本未发生的场景）与数据面缺口（使对应门槛不可判或判定不完整的观测缺口）。

### 5.1 运行形态面（窗口内未发生）

- **跨机**：整个窗口为单机双 daemon 形态（偏差登记表 §1/§2：无 systemd、`boot.sh` 起停、aihub 与双 daemon 同容器；v0-pilot vNext 栈 8420 与 v1 栈 8421 同机并存）——跨机拓扑下的调度、同步与恢复行为**未观测**。phase-1 需至少一次双机（或真 systemd 监督）部署才能覆盖。
- **长跑**：窗口按 §9.1 的 40 张票计数闭合，未走到 30 天日历条件；连续多日活栈长跑稳定性**未观测**——现有长跑证据全部是测试场景面（#42 的 fd/goroutine/events.log 泄漏探测，登记簿 #42 段：test-face，非活栈证据）。graceful shutdown 虽在演练/测试面触发过（drill-shutdown.sh、s17/s19 场景、#36/#37 裸仓链的 `shutdown flush` 痕迹），但多日连续运行下的资源/日志/事件面演化无样本。
- **迁移终局**：legacy→vNext 迁移的终局状态**未行使**——delta 迁移工具面已在 #27/#43 演练（seed-v1.sh delta 模式 + 对账报告 JSON 化），但从未观测 cutover-terminal 状态（旧栈下线、vNext 独撑）；当前仍是 v0 栈与 v1 栈同机并行（偏差登记表 §2）。迁移终局的判定标准本身也待 owner 定义。

### 5.2 数据面缺口（门槛不可判/判定不完整的直接原因）

以下缺口均来自本次填数的工具快照与登记事实，phase-1 必须闭合才能使对应门槛可判：

- **M8 耗时阈值 T 从未配置**（读数快照 M8 注：`--m8-threshold-ms` 未提供，无默认值不代填；报告 §2a 早已登记为 §12 校准议题）：PILOT-V8 当前判定**不完整**——失败分量已触发（29.06% > 1%），耗时分量不参与；配置 T 后 PILOT-V8 的完整面才存在。
- **M3 心跳面未接线**（读数快照 M3 注：machines 表 0 行 + `--m3-heartbeat-max-age` 未配置，心跳数据不可用/不参与判定）：M3 当前只覆盖 step 时长面（in_progress > 24h），机器失联面（`last_heartbeat_at` 超存活阈值）无数据。
- **M6 样本集边界**（读数快照 M6 注：33 条 `memory_remember` 事件 0 条携带 `write_ms`）：旧 schema 事件面未含观测字段，且迁移（CLI 直写）/服务端 reconcile 自愈回写不产生事件——PILOT-V6 本窗口**不参与判定**。phase-1 需新事件 kind 或样本面补齐（未获授权前不动，见 #11 登记表与报告 §2a 的既定边界）。
- **M5 条件(a) 离线不可解析**（读数快照 M5 注：`step_start_refs` 指向 `repos/<name>` 内层仓库的 commit，而 `/repos/` 不入 capsule 同步，内层仓库对象不在 pilot 数据根）：M5=0 亩「命中 ⊆ 候选（(b)∧(c)）为 0」保障，不是三条件全量核验。phase-1 若要全量核验，需 capsule 同步面覆盖内层仓对象或提供等效离线证据。
- **4 张无裸仓 WI**（登记簿头部要点：#11/#22/#23/#44）：裸仓归档面 v1 中途才建立/回补，该 4 票在 M1 裸仓面无链可读（M1 读数靠其余仓 + hook 拒绝佐证仍为 0）。phase-1 需裸仓归档面全程在位。
- **M4 旧 schema 分子排除**（读数快照 M4 注：12 个无 `commands` 字段 + 5 个无可读条目不计分子）：PILOT-V4 触发的主体是**证据面缺口**（旧 schema 票无结构化命令证据）而非谎报证实——phase-1 需新票全部携带结构化命令证据，才能把 M4 读数归因到真实验证承载面。

以上登记不改变任何门槛公式（§12 第 1 条：校准不改判定式），也不预设 phase-1 的优先级排序——排序属 owner 决策。
