# 01 — Workflow 与 Skills：frontmatter schema、执行语义与预算

> **注意 / Note（2026-10-12）**：本文部分内容已被 [v2-converge.md](v2-converge.md) 取代；两者冲突时以 v2-converge 为准。Parts of this document are superseded by v2-converge.md; where they conflict, v2-converge wins.

| 权威项 | 值 |
|---|---|
| 文档定位 | 三类 skill（路由/执行/step skill）权威布局、workflow.md frontmatter schema、bundle digest、model_candidates、grill、e2e-author、budget、on_fail rewind、执行语义 |
| 上游基线 | `00-architecture.md`（全部术语与不变量的唯一权威基线；本文只交叉引用，不重复定义） |
| 术语权威 | 本文负责：workflow bundle、workflow.md frontmatter、step skill（step agent 定义文件）、路由 skill、bundle digest、budget、on_fail rewind、grill、e2e-author、执行语义（rewind vs resume） |

## 1. 范围与非目标 <!-- §1 -->

### 1.1 范围 <!-- §1.1 -->

- 【polyforge#106 修订】skill 的权威布局与内容边界（§2.1/§3）：路由 skill（`skills/polyforge/`）与 step skill（`workflows/<bundle>/agents/*.md`，即 step agent 定义文件）；原 agent 侧执行 skill 已随 WI-Agent 引擎删除，workflow 的执行引擎是 Daemon 内的 Go driver。
- `workflow.md` 的两段构成（§4.1/§4.4）：YAML frontmatter 是本 WI 执行图的**唯一结构 schema**（steps 顺序/id/kind/interactive/gate/on_fail/budget/model_candidates，及 workflow 级声明），达到「任一 frontmatter 可据此机械校验」的程度；Markdown 正文是流程级说明，由 WI Agent（wi agent）第 0 步加载。
- bundle 引用规则（§4.5：引用即 `bundle_name`，digest 链已删）与创建 WI 时 AIHub 服务端的 frontmatter 校验（§4.6；Core 侧落库只有 `bundle_name`，权威见 `02-aihub-core.md#§4.4`/`#§4.5`）。
- model_candidates 的声明与切换语义（§4.1/§6.3）；grill（interactive，位于 spec 之前 + spec 产物的总结性确认问答）与 e2e-author（§4.2/§4.3）；参考流程 grill → spec → grill-review → plan → e2e-author → e2e-baseline → code → e2e-run → review → verification → release（review 可处于流程中的任意位置，§4.2）。
- 预算（budget）声明的层次与超预算行为（§6.6）。
- 执行语义（§6）：顺序成功路径、业务 gate FAIL 的有界 on_fail rewind、基础设施失败在当前 step 原地 resume（按候选顺序有界切换模型候选），以及驱动主循环对「全部完成」与「失败终止」两类终态的显式区分。

### 1.2 非目标 <!-- §1.2 -->

- 不设计 step 的进程隔离与命令执行入口（权威见 `03-execution-capsule.md#§5`）；本文 frontmatter schema 不声明、也不引用任何 step 级隔离类字段——v1 面向 trusted internal repo，采用宿主机原生（host-native）执行，不引入 per-step isolation（权威裁定见 `00-architecture.md#§1.2`/INV-12、`03-execution-capsule.md#§1.2`），step skill 对宿主机工具链与网络的可达性与其余 harness-native subagent 完全一致，不做逐 step 的 OS 级能力收窄（harness 层 `tools` 声明除外，见 §4.4）。
- 不定义 WI 的 bundle 引用 Core 存储 DDL、创建事务与 API 面（权威见 `02-aihub-core.md#§4.4`/`#§4.5`/`#§5.1`）；本文只定义被校验、被固化的对象（frontmatter schema 与 bundle 引用规则）。
- 不定义 Capsule 内的物化协议、step agent 定义文件解析与防冲突（权威见 `03-execution-capsule.md#§4.5`）。
- 不设计 step 结果如何同步到 AIHub、Revision 发布协议（权威见 `05-sync-revision-recovery.md#§4`、`#§5`）。
- 不复用、不搬入 workflow-v2 的 invocation / episode / retry / supersede 状态机语义与表结构；该历史仅允许以回归引用的形式出现在 `07-deployment-pilot.md#§10`，本文不得出现任何相关状态机痕迹。
- 不设计多 active AIHub 或分布式调度（权威见 `00-architecture.md#§1.2`）。
- 不给出量化的历史测试指标、日期或 rollout 排期（这些属于 `07-deployment-pilot.md`；本文测试小节只列类别）。

## 2. 术语与不变量 <!-- §2 -->

### 2.1 定义（本文为权威） <!-- §2.1 -->

**三类 skill（skill 层定稿，本节为唯一权威布局）**：

- **路由 skill**：`skills/polyforge/`，构成 = `SKILL.md`（路由清单与装配入口）+ `verbs/` 下按用户动词拆分的 13 个 fragment（`ship` (removed by polyforge#166: delivery is the workflows' deliver step) 为基线修订 polyforge#37 所加，分流表 `00-architecture.md#§5.1`）。它是用户 harness 的**唯一安装面**：只读用户请求、判定意图、把请求转发给本机 Daemon（`00-architecture.md#§3.1`），不解析 workflow、不触碰执行现场、不保存状态、**永不执行 workflow**——用户在 Interactive 模式下只提交意图、观察、回答（`00-architecture.md#§3.4a`）。
- 【polyforge#106 修订】**执行 skill 已删除**：workflow 的驱动主循环（§6.5）不在任何 agent 进程内运行——它是 Daemon 内的 Go driver，Daemon 无条件绑定，版本与 Daemon 天然锁定（部署边界见 `07-deployment-pilot.md#§2.3`；引擎唯一裁定见 `00-architecture.md#§3.6` 修订注记）。
- **step skill（step agent 定义文件）**：`workflows/<bundle>/agents/*.md`，一个 step 的执行指令包，元数据写在其 frontmatter（`name`/`description`/`tools`/`output`，§4.4），由 WI Agent 第 0 步物化进 Capsule 供 harness 派生为执行该 step 的 step agent（step agent 即 WI Agent 派出执行 step 的 subagent；`03-execution-capsule.md#§4.5`）。

**workflow 与执行图**：

- **workflow bundle**：一个可被选择的执行方案包 = `workflows/<bundle-name>/` 目录，构成为 `workflow.md`（流程声明：frontmatter + 正文）+ `agents/*.md`（N ≥ 1 个 step skill）。整体有唯一的 bundle digest（§4.5）。**不存在任何 registry/installer/Core catalog**：bundle 随 aihub 仓交付，创建 WI 时由 AIHub 从本机 checkout 读取（§4.6），没有注册接口，也没有「已注册 bundle 列表」。
- **workflow.md**：一个 bundle 的流程声明文件，两段构成——YAML frontmatter（唯一结构 schema，§4.1，AIHub 创建 WI 时校验）+ Markdown 正文（流程级说明，供 WI Agent 与人工阅读，WI Agent 第 0 步加载，`03-execution-capsule.md#§4.5`）。正文不是结构 schema：执行图的全部结构信息只来自 frontmatter。
- **执行图（steps 数组）**：本 WI 绑定版本的 `workflow.md` frontmatter 的 `steps` 数组，数组顺序即执行序（V3）；WI Agent 第 0 步把它连同定义文件物化为 Capsule 内只读快照（`03-execution-capsule.md#§4.5`），执行期间不变（`00-architecture.md#§2.3` INV-8）。**Core 不存储任何解析结果**——WI 记录对 workflow 只保留 `bundle_name` 一个 scalar（`02-aihub-core.md#§4.4`）。
- **budget**：对请求量、token 量与墙钟时间的上界声明；workflow 级与 step 级皆可声明，两层同时生效（§6.6）。
- **on_fail rewind**：业务 gate FAIL 时向更早 step 的有界回退；上界见 §6.2。
- **model_candidates**：一个 step 的有序模型候选列表，声明在 workflow.md frontmatter（defaults 可整体缺省，step 可覆盖，V9/§6.3）；它是模型选择的唯一载体——step skill frontmatter 不写 `model`（§4.4，INV-1g）。
- **grill**：interactive 追问机制，由参考流程中两个显式 step 实现（§4.2/§4.3）：`grill`（spec 之前，把模糊需求问清）与 `grill-review`（spec 之后，对 spec 产物做一次总结性确认问答）。
- **e2e-author**：在 code 运行之前对照 spec 编写端到端验收场景的 step（§4.2）；场景由两个执行者消费——`e2e-baseline`（code 之前对照实现前的工作树铺 red bar）与 `e2e-run`（code 之后复跑同一套场景的硬 gate）。
- **gate FAIL / 基础设施失败**：前者是 review/verification 等业务闸对产出质量的否定判定；后者是进程崩溃、断网、机器重启、上游限流、step agent 输出不符合 `output` schema（§4.4）等**未产出任何业务判定**的失败。二者的区分是本文核心不变量之一（`00-architecture.md#§2.3` INV-9，本文 §6）。
- **interactive step**：需要人在会话内驱动才能完成的 step（frontmatter 标注 `interactive: true`），grill 与 grill-review 均属此类。
- **`next_step_by_order`**：驱动主循环用来选出「下一个待执行 step」的判定过程（§6.5）；其唯一职责是在执行图中定位最早处于非终止态的 step，**不判断也不影响** workflow 自身是成功完成还是已失败终止——那是调用方（§6.5 驱动主循环）在其返回空结果后必须另行判定的责任。

### 2.2 本文新增不变量 <!-- §2.2 -->

- INV-1a（派生自 `00-architecture.md#§2.3` INV-8）：本 WI 的 bundle **引用**（`bundle_name`）**首次认领冻结**（R2 窗口：queued 且从未认领时可经 `PATCH` 变更，`02-aihub-core.md#§4.5`）；已物化进 Capsule 的快照在该 generation 生命周期内只读不变（WI Agent 只读该快照，绝不现场重读 checkout 原文）；**跨 generation/跨机的重新物化不做 digest 校验（校验链已删，polyforge#149 B9），本机 checkout 漂移属配置事故（告警继续，v1 信任内网）**——Core 不存任何解析结果，也不承诺 checkout 侧的版本锁定（`02-aihub-core.md#§4.4`）。bundle 原文此后再变更不影响本 WI。
- INV-1b（派生自 INV-9）：rewind 只由业务 gate FAIL 触发；一切基础设施失败永远原地 resume，不回退。
- INV-1c：每个 workflow 实例的累计 rewind 次数受上界约束且计入预算（§6.2/§6.6）。
- INV-1d：rewind 目标必然使 `[target, gate)` 区间内全部 step 被重置为 `pending`（§6.2）；旧产物归档为历史尝试而非当前输入，不存在第二种失效机制。
- INV-1e：`next_step_by_order` 返回空结果有且只有两种原因——「执行图全部 step 已到达终止态（done 或 skipped）」与「workflow 已进入失败终态（`failed_gate` 或 `failed_rewind_exhausted`）」；驱动主循环必须先判定是哪一种，才能决定是记 `completed` 并触发 wrap，还是保持失败终态、不触发 wrap（§6.5）。
- INV-1f：WI 的 workflow 引用（`bundle_name`，`02-aihub-core.md#§4.4`）随创建事务写入；**首次认领冻结**（R2 窗口：queued 且从未认领时可经 `PATCH` 三向变更，`02-aihub-core.md#§2.6` INV-2e/§4.5），认领后不存在任何重新固定/替换窗口；变更已认领 WI 的 workflow 的唯一路径是取消当前 WI 并新建 WI（§4.6）。
- INV-1g（model_candidates 裁决）：模型候选的权威位置只在 workflow.md frontmatter；候选指针**只由基础设施失败的原地重派按序推进**，切换次数以候选列表长度为上界（§6.3）；业务 gate FAIL **绝不切换候选**——它保持当前候选走 §6.2 的 rewind 语义，不存在「gate FAIL 后换模型重试」的路径。
- INV-1h（输出契约）：step skill frontmatter 不写 `model`；step agent 的最终输出必须是符合其 `output` JSON Schema 的 JSON，不符合即按该 step 的基础设施失败原地重派（§4.4/§6.3），不产出业务判定、不触发 gate。

## 3. 模块与代码目录 <!-- §3 -->

参考 `00-architecture.md#§3.3` 给出的目标仓库布局；**skill 层的权威布局由本文锁定**（`00-architecture.md#§3.3` 只保留仓库总览，不另立第二布局）：

```
aihub/
├── skills/
│   ├── polyforge/          # 路由 skill：SKILL.md + verbs/（13 个用户动词 fragment）
└── workflows/
    └── <bundle-name>/
        ├── workflow.md     # frontmatter（唯一结构 schema）+ 正文（流程级说明）
        └── agents/          # step skill（step agent 定义文件），每 step 一个 <step-id>.md
```

本文对应 `internal/workflow/` 目录，职责划分：

- `internal/workflow/schema/`：workflow.md frontmatter 的 schema 定义与校验器（§4.1；AIHub 创建事务调用）与 step skill frontmatter 的校验库（§4.4；供 bundle 测试与执行侧告警复用——AIHub 创建事务不调用后者，`02-aihub-core.md#§4.5`）。
- `internal/workflow/digest/`：bundle digest 计算（§4.5；AIHub 创建事务调用）。
- `internal/workflow/drive/`：顺序推进、rewind、resume（含候选切换）、incomplete 的执行语义（§6）。
- `internal/workflow/budget/`：预算声明的解析与作用域判定（消耗记账数据落在 Capsule `execution.json`，本包只负责判定逻辑，§6.6）。

## 4. 数据结构：workflow.md frontmatter schema 与 step skill <!-- §4 -->

### 4.1 workflow.md frontmatter 完整 schema（唯一结构 schema） <!-- §4.1 -->

`workflow.md` 以 YAML frontmatter 开头（`---` 定界），frontmatter 是本 WI 执行图的**唯一结构 schema**；其后的 Markdown 正文是流程级说明，不参与结构校验（加载与物化见 §4.4 与 `03-execution-capsule.md#§4.5`）。

frontmatter 顶层字段：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 必填 | 正则 `^[a-z][a-z0-9-]{1,63}$`，必须等于 bundle 目录名（V2） |
| `description` | string | 可选 | ≤500 字符 |
| `budget` | Budget | 可选（workflow 级） | 结构见 §6.6；缺省 = 无 workflow 级上界 |
| `rewind.max_total` | int | 可选 | 缺省 4，必须 >0；整个 workflow 累计 rewind 上界 |
| `rewind.max_per_step` | int | 可选 | 缺省 2，必须 >0；同一 step 被回退重入的上界 |
| `defaults.model_candidates` | []string | 可选 | 步骤未声明时使用的有序候选（V9） |
| `steps` | []Step | 必填 | 1 ≤ len ≤ 32，元素 `id` 唯一；**数组顺序即唯一执行序**（V3） |

Step 元素字段：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `id` | string | 必填 | 正则 `^[a-z][a-z0-9-]{0,63}$`，steps 内唯一；同时是 step skill 文件名绑定：`agents/<id>.md`（V7） |
| `kind` | string | 必填 | one of `qa`\|`spec`\|`author`\|`code`\|`gate`\|`review`（V8）；分类分桶（§9）与结构约束（V10）的判定依据 |
| `interactive` | bool | 可选，缺省 false | true 表示需要人在会话内驱动（grill/grill-review 等）；无位置限制（规则 V4） |
| `gate` | bool | 可选，缺省 false | true 表示本 step 产出业务判定（review/verification/grill-review 皆可）；`interactive` 与 `gate` 可同时为 true |
| `on_fail` | OnFail | 可选，仅 `gate: true` 的 step 允许携带 | `OnFail = {target: 更早的 step id（必填）}`——声明即 rewind 边；未声明 `on_fail` 即 §6.2 第 1 步的 `failed_gate` 终止。gate FAIL 判定全文一律随 `pending_rewind_input` 送达 target（polyforge#147 C5：`action` 与 `include_verdict` 键已删除，判定转发无条件） |
| `context_policy` | string | 可选，缺省 `inherit` | `inherit`\|`fresh`：见 §4.4a。`inherit` 的 step 续上上游交接（派发输入带 handoff 文件正文），`fresh` 从零开始；`gate: true` 的 step 必须是 `fresh`（闸只凭自己的输入判，不继承有倾向性的上下文） |
| `handoff` | bool | 可选，缺省 false | true 表示本 step 必须写交接文件 `artifacts/handoff-<step-id>.md`：见 §4.4a |
| `budget` | Budget | 可选 | step 级，覆盖/叠加 workflow 级，结构同 §6.6 |
| `model_candidates` | []string | 可选 | 覆盖 `defaults`（整体替换，非合并）；有序，切换语义见 §6.3（V9） |

frontmatter 不声明 step 间输入 wiring、执行目录（`cwd`）或任何排序/位置键：上游产物落 Capsule `artifacts/` 与 `execution.json` 的 `steps.<id>.output`（`03-execution-capsule.md#§4.2`），下游 step 的 agent 指令按产物路径约定读取（§4.2）；step 执行的 `cwd` 由 WI Agent 按 `03-execution-capsule.md#§5.1` 的 `RunCommand.cwd` 取值域（`repos/<name>` 或 `artifacts`）选择。结构 schema 只承载结构、调度与预算，不重复表达执行细节。

v1 schema 不声明、也不识别任何 step 级 isolation 类键名；此类键名与其余任何未知顶层/字段键一样，一律按 V1 拒绝（无独立错误分支，不单独列举任何具体键名）；v1 不引入 per-step isolation 的权威裁定见 `00-architecture.md#§1.2`/INV-12、`03-execution-capsule.md#§1.2`。

**校验规则**（规则集标识 `WF-201`，每条规则有唯一 `rule_id`；创建时任一规则失败 → 创建整体失败，HTTP 面按 `02-aihub-core.md#§4.5`/`#§5.3` 返回 `CORE-400` 并在 `detail` 携带命中的 `rule_id`）：

| 规则 | 内容 |
|---|---|
| V1 | frontmatter 必须是纯数据 YAML 映射；YAML 中任何脚本性/标签性节点（例如 `!!binary`、自定义 tag）整体拒绝；未知顶层键与未知 step 键一律拒绝，不忽略 |
| V2 | `name` 与全部 `steps[].id` 符合命名正则；`steps` 非空（1 ≤ len ≤ 32）且 `id` 唯一；`name` 等于 bundle 目录名 |
| V3 | `steps` 数组顺序即唯一执行序；schema 不存在 `position`/排序类声明，不存在第二个顺序来源 |
| V4 | `interactive: true` 的 step 无位置限制；无人驱动（批处理）模式下调度到任意位置的 interactive step 均不派发，其 WI 一律转 blocked（`waiting_on=owner`；判定与错误码见本文 §5.2 `WF-105`，Core 侧状态机权威见 `02-aihub-core.md#§2.4`），不依赖本层位置假设——两段式 grill（spec 前 `grill` + spec 后 `grill-review`）由此可表达 |
| V5 | `on_fail` 只出现在 `gate: true` 的 step；`on_fail.target` 必须是 `steps` 中存在且数组位置严格早于本 step 的 step id |
| V6 | `budget`/`rewind` 各数值必须 >0；step 级字段语义不得与 workflow 级冲突 |
| V7 | 每个 step 的 `agents/<id>.md` 必须存在于 bundle 内，且 `agents/` 下只允许 `.md` 文件；AIHub 创建事务对它们只做存在性检查与按字节摘要（digest 输入，§4.5），**不解析、不校验、不落库其内容**（`02-aihub-core.md#§4.5`）；agents 文件 frontmatter 的内容校验（§4.4）由 `internal/workflow/schema` 的库实现供测试与执行侧告警使用 |
| V8 | `kind` 取值集合为 `qa`\|`spec`\|`author`\|`code`\|`gate`\|`review`，集合外拒绝；`kind: review` 的 step 可处于数组内任意不违反其余规则的位置（含最后）——review 的位置自由度由 `kind` 表达，无需独立 position 字段 |
| V9 | `model_candidates`（defaults 与 step 级）为非空字符串有序列表，元素格式与解释权在 harness 适配器契约（`00-architecture.md#§3.6a`；编码未决项见 §12 第 4 项）；step 级声明整体替换 `defaults`（非合并）；有界切换与禁止切换语义见 §6.2/§6.3（INV-1g） |
| V10 | 若存在任何 `kind: qa` 且 `gate: false` 的 step（spec 前追问，如 `grill`），则必须存在至少一个 `kind: spec` 的 step，且该 qa step 在数组中的位置严格早于全部 `kind: spec` step；判定直接基于数组位置扫描。违反 → `WF-201`（`rule_id=V10`）。`kind: qa` 且 `gate: true` 的 step（spec 后确认，如 `grill-review`）不受本规则约束，可处于任意位置 |

schema 只允许上表数据类型；上表规则编号 V1–V10 是当前唯一有效集合，不存在编号跳号或已废弃编号。

### 4.2 参考流程（review 可处于任意位置） <!-- §4.2 -->

vNext 自带参考 bundle `feature-full`（`workflows/feature-full/`），frontmatter `steps` 数组顺序为：

1. `grill`（`kind: qa`，`interactive: true`，非 gate）
2. `spec`（`kind: spec`；消费 `grill` 的问答记录产物）
3. `grill-review`（`kind: qa`，`interactive: true` 且 `gate: true`；消费 `spec` 的产物；`on_fail: {target: spec}`）
4. `plan`（`kind: author`；消费 `spec` 的产物）
5. `e2e-author`（`kind: author`；消费 `spec` 的产物，在 code 之前编写端到端验收场景）
6. `e2e-baseline`（`kind: code`，非 gate；消费 `e2e-author` 的场景产物，对照实现前的工作树执行场景并把红 bar 交给 `code`）
7. `code`（`kind: code`；消费 `plan` 的产物与 `e2e-baseline` 铺好的验收 bar）
8. `e2e-run`（`kind: code`，`gate: true`；复跑 `e2e-author` 编写的同一套场景；`on_fail: {target: code}`）
9. `review`（`kind: review`，`gate: true`；`on_fail: {target: code}`）
10. `verification`（`kind: gate`，`gate: true`；最后一道机器闸，在 `review` 之后复跑完整绿灯 bar；`on_fail: {target: code}`）

（2026-10-13 起 feature-full / bug-fix / quick-fix 不再含 `release` 槽位，见下方「发版独立成 bundle」。）

**发版独立成 bundle（2026-10-13 裁定，取代下面三段 release 槽位 / dispatcher / deploy-face 的现行行为）**：feature-full、bug-fix、quick-fix 的 `release` step 已删除（chore 本来就没有）；发版与部署**只**经 `workflows/release/` 这个项目无关的 bundle 发生，作为单独的 WI 提交。三步：`release`（`kind: code`，`gate: true`：capsule 内 fetch 并重置到 origin 默认分支头、记录发版前远端 tag 列表、探测 `.polyforge/release.sh` 支持 `--verify`、跑 `.polyforge/release.sh [bump]`，bump 逐字取自 WI 目标；owner 规则不变——零 owner → **FAIL**（不再 defer：没有 procedure 的发版 WI 是错误）、恰一 owner → 执行、≥2 → FAIL `PF-RELEASE-AMBIGUOUS_OWNER`）→ `verify`（`kind: gate`，`gate: true`，fresh 上下文，不信任上一步：发版 commit 与 origin 头、新 tag 存在且指向发版 commit、发版前每个 tag 仍指向同一对象（不可变）、发版说明非空，最后 `release.sh --verify <version>`，全部命令取证）→ `deploy`（`kind: code`，`gate: true`：只部署 verify 通过并写入 `artifacts/verified-version.json` 的版本，绝不用 "latest"；owner 为携带 `.polyforge/deploy.sh` 的 repo，**零 owner → `defer`**，≥2 → FAIL，执行 `.polyforge/deploy.sh <v> --confirm <v>`；procedure 以新版本在线或已回滚结束，回滚 = step FAIL）。三步均声明 `requires_command_evidence: true`（V4 命令取证门禁保留在 driver：completed 结果不带合格 `command_evidence` 即被机械拒收），均不声明 frontmatter `inputs:`（polyforge#147 C5 已删该键）：上游产物（`artifacts/release-record.json`、`artifacts/verified-version.json`）按路径约定读取；三步均不声明 `on_fail`：失败即终止（`failed_gate`），不得 rewind 回 release——已发布的 tag / release / image 不可变，回退重发会撞版本或静默烧版本，重发由人另提新 WI。verdict 词汇（driver 的 release verdict binding 机械强制，作用于最小信封的 `output` 顶层字段，polyforge#147 C10 对该绑定的删除被本裁定取代）：`release` 为闭集 `[pass, fail]`（pass 欠非空 `version` 与至少一条 `artifacts_published`），`deploy` 为 `[pass, defer, fail]`（pass 欠非空 `version`），defer 只在 `deploy` 合法。原「deploy 永远只由人触发」的合同同时被取代：deploy procedure 现在由 release bundle 的 `deploy` step 触发，防线改为 `--confirm <version>`（必须等于位置参数版本）、服务器侧锁与失败自动回滚，均在 `.polyforge/deploy.sh` 内。

**release step（发版钩子，定稿裁定）**〔已被 2026-10-13「发版独立成 bundle」取代，保留为历史〕：release 是 bundle 内的一个普通 step 槽位——平台零发版专用代码，全部复用既有 step 机器（inputs/output schema/worktree/预算/rewind/边界同步）。不新增 step kind：沿用 `code`（缺省，产生版本号 commit/tag 的 diff）或 `gate`（纯验证型发版），按项目自选；失败语义建议 `gate: true`——发版失败属业务判定，走既有有界 `on_fail` rewind。`agents/release.md` 是随参考 bundle 交付的模板骨架（版本号来源/构建/打 tag/上传目标/渠道晋升，全为 TODO 占位），各项目在各自 bundle 中自填——发版逻辑 per-project，多项目天然隔离。平台自身的发版（`aihub` 服务二进制与工作机二进制二产物）是同一钩子的一个实例，不另立发版模型；`pf upgrade` 只是消费已发布制品的部署管道（`07-deployment-pilot.md#§6`）。

**release dispatcher（polyforge#123 定稿裁定）**〔已被 2026-10-13「发版独立成 bundle」取代，保留为历史〕：随参考 bundle 交付的三个 `steps/release.md`（feature-full / bug-fix / quick-fix）是**项目无关的 dispatcher，不是 procedure**——模板正文不含任何项目私有发版步骤。归属判定只有一个机械规则：扫描 capsule 的 `repos/`，某 repo 携带 `.polyforge/release.sh` 或 `.polyforge/release.md` 即为 release-procedure owner。**零 owner → `verdict: defer`**（本 capsule 显式不发版，不发明 procedure，流程继续 wrap）；**恰一 owner → 在本 step 内执行其 procedure**（过渡语义，单一可拆除接缝；train 子票拆除）；**两个及以上 owner → step FAIL**，`fail_reason` 以 `PF-RELEASE-AMBIGUOUS_OWNER` 开头并点名每一个候选 repo——绝不静默择一。verdict 词汇表为闭集 `[pass, defer, fail]`：defer 只在 release step 的 schema 里合法，任何其它 step 的输出不得出现 defer；pass 必须携带非空 `version` 与至少一条 `artifacts_published`，fail 必须携带非空 `fail_reason`，defer 既不欠 version 也不欠 artifacts_published。

**发布凭据两分（polyforge#123 合同声明）**：release 生命周期涉及两个**彼此独立**的凭据——(1) **git tag-push credential**：把发版 tag 推到项目 git remote 的凭据；(2) **shelf/publish write credential**：向发布架（发布制品目录/通道）写入制品的凭据。两者不得混用、不得假设同源；各自如何存储、如何授予（哪台机器、哪个身份、哪个最小权限面）是刻意的后续设计——留待 P2 子票的机制设计，本层只裁定"两凭据分离"这一合同本身。`.polyforge/release.sh|.md` 是各项目 repo 内的 procedure，其自身使用的凭据形态即这两类，模板与 dispatcher 对其内容不做任何假设。

**deploy face 合同（polyforge#123）**〔已被 2026-10-13「发版独立成 bundle」取代，保留为历史〕：`.polyforge/deploy.sh|.md` 是各项目 repo 内的**部署 procedure**——aihub 服务二进制升级 + 每台工作机 `pf upgrade` 的既定序列（含回滚）。它**永远只由人触发**：任何自动化路径（workflow step、daemon lane、release procedure、CLI verb）都不得引用或触发 deploy procedure——共享 release dispatcher 刻意不提及该文件，CLI 不设 deploy verb（P3+ 排除项：deploy 永远人门控）。

流程级 `rewind` 声明：`max_total: 4`，`max_per_step: 2`。

要点：

- step 间数据流不入结构 schema（§4.1）：上游产物落 Capsule `artifacts/` 与 `execution.json` 的 `steps.<id>.output`（`03-execution-capsule.md#§4.2`），下游 step 的 agent 指令（§4.4 正文）按产物路径约定读取。grill 的问答记录（`artifacts/grill-qa.md`，§4.3）即按此约定被 `spec` 与 `grill-review` 消费。
- `grill` 在 `spec` 之前、`grill-review` 在 `spec` 之后：两者都是普通 step，位置由数组顺序表达；`grill` 先于全部 `kind: spec` step 由 V10 结构性保证，`grill-review`（`gate: true` 的 qa step）的位置由 bundle 作者在数组中排定。
- `grill-review` 是两段式 grill 的既定实现（不是待设计的回调）：其 PASS/FAIL 判定复用 §6.2 既有 rewind 机制；FAIL 且目标为 `spec` 时，FAIL 判定全文随目标 step 的输入一并送回（无条件），不引入任何新状态或恢复机制。
- `review` 的位置自由度由 `kind: review` 表达（V8：可插在任意合法位置）；WI 创建固化后，执行序即 frontmatter `steps` 数组顺序，运行时不再漂移。

### 4.3 grill 的两段式行为 <!-- §4.3 -->

grill 的两段职责分别由两个独立 step 承载，不是单 step 内的隐藏相位：

1. **`grill`**（spec 之前）：对模糊需求逐项发问，`interactive: true`，非 gate；问答记录写入 Capsule `artifacts/grill-qa.md`，产物路径记入 `steps.grill.output` 供 `spec` 消费。
2. **`grill-review`**（spec 之后）：对 spec 产物做一次总结性确认问答，`interactive: true` 且 `gate: true`；PASS 表示用户确认无异议，直接推进 `plan`；FAIL 表示用户要求修改，触发 `on_fail: {target: spec}`（复用 §6.2 机制），问答追加进同一份 `artifacts/grill-qa.md`（按 step id 分节）。

`grill-review` 的判定语义是「人是否确认」而非质量评分，但驱动引擎不区分判定来源，一律按 gate FAIL 语义处理，因此不需要为其新增状态机分支，恢复路径与其余 gate 完全一致。

**交互渠道**：grill / grill-review 的问答由引擎的同一份驱动逻辑（`00-architecture.md#§3.6` 修订注记）驱动，走一条 needs_input/answer 的中继路径而非另立通道——引擎遇到 interactive step 时，把待问问题**持久化写入 `runtime-state/execution.json`**（`03-execution-capsule.md#§4.2` 的 `steps.<id>.pending_question`；execution.json 的权威 schema 在 `03-execution-capsule.md#§4.2`，本文只引用），该写入**作为 step interrupted 边界同步触发**一次发布请求（`00-architecture.md#§4.3` 的「step 中断」类触发时机），使待问问题经发布链路呈现给 owner 正在使用的 observer UI；owner 的回答经 Daemon 的 answer API（`answer(wi_id, payload)`，`04-local-runtime.md#§5.1`，以 `answer_id` 幂等，中继报文格式见 `00-architecture.md#§3.4a`）中继回进行中的同一个 step 的执行上下文，引擎收到后原地续跑同一 step，不新增状态机分支。每问每答即时追加写入 Capsule 内 `artifacts/grill-qa.md`。无人驱动（批处理）模式下两者均按本文 §5.2 `WF-105` 的规则处理（不派发，WI 转 blocked，`waiting_on=owner`，`02-aihub-core.md#§2.4`：`waiting_on` 非空即 blocked，非 pause），不依赖 schema 层位置假设；Daemon 侧只按 `00-architecture.md#§3.4` 的 Interactive/Batch/Drain 判据决定当前是否处于无人驱动上下文，具体拦截与状态转换语义仍在本层。

### 4.4 step skill：step agent 定义文件 <!-- §4.4 -->

```
workflows/<bundle-name>/
├── workflow.md            # 必需：frontmatter（结构）+ 正文（流程级说明）
└── agents/
    └── <step-id>.md       # 每 step 一个；文件名 = step id（V7）
```

每个 `agents/<step-id>.md` 是一个 step skill = 一份 **step agent 定义文件**：frontmatter（元数据）+ Markdown 正文（执行指令）。**Capsule 侧权威**（物化协议、只读快照、防冲突三层、harness 解析）在 `03-execution-capsule.md#§4.5`，本文只定义其内容 schema，两文字段集一致、互不另立。

frontmatter 字段：

| 字段 | 类别 | 说明 |
|---|---|---|
| `name` | 硬必需 | harness 派生时引用的 step agent 名；必须等于文件名（即 step id）；缺失或不符 → `WF-201`（`rule_id=V7`） |
| `description` | 硬必需 | 一句话职责说明，供 WI Agent 选择与组装派发 prompt |
| `tools` | 硬必需 | 该 step 派生 step agent 的**工具面声明**，由 harness 适配器在派发时落实；OS 层文件系统读写权限不受本字段影响，仍由 `03-execution-capsule.md#§5.2` 的 v1 Native runner profile 统一裁定，本字段不做 OS 级权限放大或收窄 |
| `output` | 硬必需 | JSON Schema；执行该 step 的 step agent 的最终输出必须是符合本 schema 的 JSON |
| `model` | 禁止出现 | 模型选择是流程作者的决定，唯一载体是 workflow.md frontmatter 的 `model_candidates`（§4.1，INV-1g）；出现 `model` 键 → `WF-201`（`rule_id=V7`） |

不存在 `kind`/`phase`/`title`/`budget_hint` 或任何其他 frontmatter 字段：step 的 `kind`、调度与预算声明全部在 workflow.md frontmatter（§4.1），不在 step skill 内重复。

**校验时机与生效**：AIHub 创建事务不解析 agents frontmatter——只做存在性检查与按字节摘要（V7/§4.5，`02-aihub-core.md#§4.5`）；`internal/workflow/schema` 提供本表校验库，供 bundle 测试与执行侧告警使用，不阻塞创建；派发时由 harness 适配器按 `03-execution-capsule.md#§4.5` 的物化产物解析生效。

**输出契约（INV-1h）**：step agent 的最终输出必须是符合 `output` schema 的 JSON，**不符合即按该 step 的基础设施失败处理**（§6.3，原地重派并推进候选指针），不产出业务判定、不触发 gate（与 `03-execution-capsule.md#§4.5` 一致）。「不符合」的判定来源有两个（polyforge#27 修订）：一是执行侧自查（WI Agent 读取 schema 的既有路径）；二是**发布时机械校验**——每次全扫描发布（HEAD 推进）时，Daemon 侧校验器从已固化进 staging 的快照字节中逐 step 比对每个已完成 step 的输出与其 pinned `output` JSON Schema。校验器是 draft-07 的机械完整实现，schema-authoring 规则与本仓 bundle 测试（`internal/workflow/schema`）强制的同一契约：参考 bundle 的 output schema 只使用标准 draft-07 关键字，真校验器接入即同一契约生效，不要求改写任何既有 schema。该检查只读将随本次发布写入 Revision 的快照字节，**绝不触碰 Capsule 内 `runtime-state/` 活文件**（INV-6：runtime-state 是 WI Agent 的单写者私有域，Daemon 的校验只在发布面读已发布快照；Capsule 侧契约的权威在 `03-execution-capsule.md#§5.1a`）。失配分类不变：不合 schema = 该 step 的基础设施失败（§6.3），不产出业务判定、不新增终态。

正文为自然语言执行指令，由该 step 的 WI Agent 在派生 step agent 时组装进其 prompt（WI Agent 驱动逻辑的一部分，`00-architecture.md#§3.6`），不属于 harness 适配器或 Daemon 的实现细节。model/候选声明位置一律在 workflow.md frontmatter 的 step/defaults 字段（§4.1）——保证模型选择是流程作者的决定，而非 step skill 作者的决定。

### 4.4a step 间上下文传递：`context_policy` 与 `handoff`（polyforge#147） <!-- §4.4a -->

step 之间只有两种上下文关系，两者都由 workflow.md frontmatter 的 step 键声明，**语义全在路径约定上，没有任何摘要/清单校验**：

- **`context_policy: inherit`（缺省）**：本 step 是上游工作的延续。派发时 driver 取**最近一个已 done 且声明了 `handoff: true` 的更早 step**，按约定路径 `artifacts/handoff-<step-id>.md` 读出其交接文件正文，原样拼进本 step 的派发 prompt（「Handoff from step …」小节）；没有这样的上游、或文件不在，就只是少了那一段，不是失败。
- **`context_policy: fresh`**：本 step 从零开始，不注入任何上游交接正文；它要读的上游产物按**产物路径约定**自取（`artifacts/<name>.md`，§4.2 要点一）。`gate: true` 的 step 一律 `fresh`（解析器强制）。
- **`handoff: true`**：本 step **必须**在收尾前把交接内容写成文件 `artifacts/handoff-<step-id>.md`（路径由 step id 机械决定，step 模板的 Handoff duty 一节即此约定的文案）。下游 inherit step 按这同一条路径读取。

**无摘要校验（C2/C3 裁定）**：结果信封是最小契约——`outcome` + `output`（`needs_input` 另带 `question_id` + `question`）——信封里**没有** `handoff_ref`、没有 sha256、没有 bytes、没有 artifacts 清单。driver 在最小契约之外只保留两道机械门禁。其一是 V4 命令取证门禁（owner 裁定保留在 driver）：欠取证的 step（`requires_command_evidence: true`，或 step id 为 `verification`）的 completed 结果另带 `command_evidence: [{argv, exit_code, duration_ms}]`，至少一条、每条 argv 非空且 `duration_ms > 0`，否则信封被拒；被接受的取证由 driver 写入该 step 的 `output.commands`。其二是已提交测试文件门禁（polyforge#156，作用于任意 step，同一拒收车道）：completed 结果的 `output.test_files` 非空时，每条路径必须是 repo 相对路径、至少存在于一个 task repo，且在其存在的每个 task repo 中都已提交在 HEAD（driver 只跑 `git ls-tree`，禁用 hooks/fsmonitor，不扫工作区），否则信封被拒并点名路径与 repo——resume 只按 carrier ref（HEAD）恢复 task repo，未提交的测试文件会丢失。交接文件的存在与内容由路径约定承载：写在约定路径上就算交接完成，driver 不校验摘要、不比对清单、不因为缺少摘要字段判信封不合格；交接文件缺失时 inherit step 的派发只是少了那段正文，不构成信封级失败。产物同理：step 产出一律写进 `artifacts/` 下由各 step 指令点名的路径，下游按路径读。

### 4.5 bundle 引用（digest 链已删除，polyforge#149 B9） <!-- §4.5 -->

**bundle 引用只有 `bundle_name` 一个 scalar**。audit subtraction C（polyforge#149 B9）删除了整条
digest 链：不再对 bundle 计算任何聚合摘要，不再派生 `sha256("bundle-name:"+名)` stand-in，也不
接受请求显式携带的 digest pin。原先由 digest 承担的「checkout 与 pin 一致」校验随链删除——
执行侧从本机 checkout 按 `bundle_name` 物化 bundle，不做内容校验（`03-execution-capsule.md#§4.5`）；
本机 checkout 缺失或漂移属**配置事故**（告警继续，v1 信任内网），不存在任何 Core 侧阻断或
版本回取路径。

历史注记：本节曾规定 RFC 8785 canonical JSON 的唯一聚合摘要计算规则与黄金向量（对 bundle 内
`workflow.md` 与 `agents/` 全部文件逐文件 SHA-256 后再整体 SHA-256）。该规则连同其落库列、
claim 响应字段与执行侧校验分支一起删除；workflow 包的 `Digest`/`FileEntry` 保留为通用库函数
（不再有生产调用方）。

**预算作用域**：每个维度（requests/tokens/wall_minutes）独立判定两层上界：step 级上界限制单个
step 单次执行尝试的消耗；workflow 级上界限制整个 WI 生命周期内全部 step 执行尝试的累计消耗
（含 rewind 产生的重跑）。任一层达到上界均触发 §6.6 的超预算行为，谁先触发即报谁的错误。

### 4.6 创建时的校验与固化（服务端内部步骤；无独立端点、无注册面） <!-- §4.6 -->

**主体裁定（消除 aihub#793 指出的启动循环）**：workflow 的校验与 digest 固化发生在 **WI 创建事务内、由 AIHub 服务端完成**——创建必先于 claim，时序约束天然满足；不存在独立的解析/固定端点，不存在任何「先注册 bundle 再使用」的前置步骤（`02-aihub-core.md#§4.5`：原 workflow-pin 端点与 bundle 注册面整体删除）。提交方（路由 Skill/`pf` CLI 代表用户发起创建）只提交 `bundle_name`；解析、校验与固化全部在服务端完成。**WI Agent 从不参与创建期校验，只按 digest 物化并执行**（`03-execution-capsule.md#§4.5`），这消除了「claim 后才能启动 WI Agent、WI Agent 又负责首次固定」的循环依赖。

1. **提交**：提交方（路由 Skill `file` 动词 / `pf` CLI 代表用户）经 `pf` CLI **直连** AIHub 发起 `POST /api/v1/work-items`（`02-aihub-core.md#§5.1`；`file` 属数据面动词，按 `00-architecture.md#§5.1` 分流表直连 AIHub、不经 Daemon），body 必带 `bundle_name`；不存在「已注册 bundle 列表」接口——提交方按 bundle 名直接提交，可用集合就是 AIHub 本机 checkout 的 `workflows/` 目录内容（部署配置给定，不落库、不经 API）。
2. **frontmatter 校验**：AIHub 服务端读本机 aihub checkout 内 `workflows/<bundle>/workflow.md` 的 frontmatter，按 §4.1 的 V 规则全集校验；对 `agents/<id>.md` 只做存在性检查（V7）。任一规则失败 → 创建整体失败（`CORE-400`，`detail.rule_id` 命中 V 编号，`02-aihub-core.md#§4.5`）。校验过程只做纯数据校验，不执行 bundle 内任何内容，校验中间结果不落库。
3. **引用落库**：校验通过后在同一创建事务内把 `bundle_name` 写入 `work_items`，与 repo inventory 冻结（`02-aihub-core.md#§4.6`）一并提交。落库的只有 name（INV-1f；`02-aihub-core.md#§2.6` INV-2e；digest 列已删，polyforge#149 B9）。
4. **首次认领冻结**：`bundle_name` 在 R2 窗口（queued 且从未认领）内存在唯一的 UPDATE 路径——`PATCH bundle_name` 三向变更（与创建共用同一落库路径，`02-aihub-core.md#§4.5`）；首次认领后不存在任何 UPDATE 路径，也不存在「认领后整体替换」窗口；若确需变更已认领 WI 的 workflow，唯一路径是取消（terminal cancel）当前 WI（`02-aihub-core.md#§6.5`）并新建一个 WI 重新走创建流程；不存在让同一个已认领 WI 回到「未固化」状态的路径（INV-1f）。
5. **漂移告警**：Core 不存 bundle 内容，也不再计算 digest（polyforge#149 B9）；同一 `bundle_name` 的 checkout 漂移只能靠执行侧告警（`WF-203` 场景：物化时缺文件/缺 bundle 报警继续）。
6. **执行侧如何取得内容**：不从 Core 取——Core 无任何文件取回端点（`02-aihub-core.md#§4.4`）。WI Agent 第 0 步从执行机本机 aihub checkout 物化 `workflow.md` 与 `agents/*.md` 进 Capsule，不做 digest 校验（校验链已删，`03-execution-capsule.md#§4.5`）；Daemon 的 claim 预检只确认 WI 的 `bundle_name` 存在（`04-local-runtime.md#§6.1`），不代行校验、不拉取任何步骤集。
7. **与 WI Agent 凭证边界的关系（呼应 §5.1 与 `00-architecture.md#§5`/`#§8`）**：上述 1–6 步全部由提交方与 AIHub 服务端完成；WI Agent 自始至终**不持有任何 Core API key 或 Revision 发布 token**，只能通过 `04-local-runtime.md#§5.3` 的 Daemon 本地认证代理调用允许的 WI 生命周期/读操作（Revision 发布权仅属 Daemon）。

## 5. API 与错误码（本层） <!-- §5 -->

### 5.1 本层调用面 <!-- §5.1 -->

| 操作 | 调用面 |
|---|---|
| 创建 WI（含 frontmatter 校验与 digest 固化） | 提交方（路由 Skill/`pf` CLI）经 `pf` CLI **直连** AIHub（`file` 为数据面动词，分流表 `00-architecture.md#§5.1`；不经 Daemon）→ `POST /api/v1/work-items`（`02-aihub-core.md#§5.1`）；校验与固化是创建事务内部步骤（§4.6），**无独立端点、无注册接口、无 bundle 列表接口** |
| 校验 frontmatter / 计算 digest | 随创建在 AIHub 服务端事务内一并完成（§4.6 第 2/3 步）；提交方只发起创建请求，不做本地重实现 |
| step agent 执行 | 权威在 `03-execution-capsule.md#§5` 与 `03-execution-capsule.md#§6` |
| 执行结果同步发布 | 权威在 `05-sync-revision-recovery.md#§4` 与 `05-sync-revision-recovery.md#§5` |
| 交互问答中继（grill/grill-review） | `04-local-runtime.md#§5.1` 的 answer API（§4.3） |

### 5.2 错误码表（前缀 WF） <!-- §5.2 -->

| 码 | 语义 |
|---|---|
| WF-200 | `bundle_name` 在 AIHub 本机 checkout 的 `workflows/` 内不存在（§4.6 第 2 步）；创建整体失败 |
| WF-201 | frontmatter/结构校验失败（`rule_id` ∈ V1–V10，含位置信息）；创建时 HTTP 面为 `CORE-400` + `detail.rule_id`（§4.6 第 2 步、`02-aihub-core.md#§4.5`）；agents 文件 frontmatter 违反 §4.4 schema 时同样按本码（`rule_id=V7`）由校验库报告，供测试与执行侧告警 |
| WF-202 | 创建事务内本机 checkout 并发变更，导致校验读取与 digest 输入不一致（§4.5 第 3 步重算不可复现）；创建整体失败，无中间状态落库，重试创建即可 |
| WF-203 | 执行机本机 checkout 的 bundle 文件漂移（例如 checkout 被误改、停留在错误版本或缺失文件）：物化不做 digest 校验（校验链已删，polyforge#149 B9），缺 bundle/缺文件按 `03-execution-capsule.md#§4.5` **告警继续**；确需修复时更新本机 checkout 后由新 generation 重物化，Core 不提供任何文件取回端点（`02-aihub-core.md#§4.4`） |
| WF-101 | rewind 上界耗尽，终态 `failed_rewind_exhausted` |
| WF-102 | gate FAIL 且无 rewind（`on_fail` 缺失），终态 `failed_gate` |
| WF-103 | 超预算（`OVER_BUDGET`），workflow 转 blocked（§6.6） |
| WF-105 | interactive step 在无人驱动（批处理）模式下被调度：不派发，其 WI 转 blocked（`waiting_on=owner`，`02-aihub-core.md#§2.4`） |

`WF-104` 号位保留不再使用：grill 必须先于 spec 的约束已由 V10（`kind: qa` 且 `gate: false` 的 step 严格早于全部 `kind: spec` step）结构性保证，不需要单独的位置规则或错误码。

## 6. 核心流程（编号步骤） <!-- §6 -->

### 6.1 成功路径：顺序推进 <!-- §6.1 -->

1. 按执行图（frontmatter `steps` 数组顺序，V3 已保证其即执行序）逐个执行。
2. 每个 step 执行完成后，其结果写入 Capsule 的 `runtime-state/execution.json`（`03-execution-capsule.md#§4`），随后按 `00-architecture.md#§4.3` 的四类触发时机之一提交一次同步发布。
3. `kind: review` 的 step 已在创建时定位于数组内的固定位置（V8），运行时不再跳转或漂移。

### 6.2 业务 gate FAIL → 有界 on_fail rewind <!-- §6.2 -->

当一个 `gate: true` 的 step 产出 `FAIL` 判定时，按下列步骤处理：

1. 若该 step 未声明 `on_fail`：workflow 立即终止，终态为 `failed_gate`（`WF-102`），FAIL 判定全文随下一次同步发布一并写入 `execution.json`（权威 schema 见 `03-execution-capsule.md#§4.2`，本文只引用）。流程到此结束。
2. 若声明了 `on_fail: {target: X}`：先检查两类上界。若本 workflow 实例的累计 rewind 次数已达到 `rewind.max_total`（缺省 4），或本次回退目标 step 被重入的次数已达到 `rewind.max_per_step`（缺省 2）：workflow 终止，终态为 `failed_rewind_exhausted`（`WF-101`）。这两类上界检查中的任一命中都必须立即终止流程，不得在命中后继续执行下面的回退步骤。
3. 若两类上界均未命中：累计 rewind 计数（`consumed_rewinds_total`）与该目标的重入计数（`rewinds_reached[target]`）各加一；两者的唯一持久化落盘形态是 `execution.json` 的 `rewind_stats`（`03-execution-capsule.md#§4.2`），本文只定义计数语义。
4. 对区间 `[target, gate)`（包含 `target` 本身，不包含触发 FAIL 的 gate step 自身）内的每个 step：把它当前的执行结果归档为一条历史尝试记录（不覆盖删除，供审计；归档 attempts 的权威 schema 在 `03-execution-capsule.md#§4.2` 的 `steps.<id>.attempts`，本文只引用），随后把它在 `execution.json` 中的状态重置为 `pending`、输出清空。清空输出的目的是防止任何下游 step 在重跑前消费到陈旧产物。
5. 把当前执行位置设回 `target`。
6. 把本次 FAIL 判定全文（无条件）作为附加输入传给 `target` step，保证触发回退的意见必定到达被回退的 step，不依赖任何旁路传递。该判定全文在触发 rewind 时即作为**待消费 rewind 输入**持久化在 `execution.json` 中（权威 schema 见 `03-execution-capsule.md#§4.2` 的 `steps.<id>.pending_rewind_input`，本文只引用），`target` 重跑时从该记录消费，不依赖 WI Agent 进程内存。
7. 本次 rewind 消耗计入预算（§6.6）。
8. 流程从 `target` 重新按 §6.1 的顺序推进逻辑继续执行。

**候选指针在 rewind 中不推进（INV-1g）**：被重置为 `pending` 的 step 重跑时从其生效候选列表第 1 项开始（候选指针随 `pending` 重置一并清零）；触发 FAIL 的 gate step 自身的候选指针保持不变。gate FAIL 的任何处理路径中都不存在「切换候选后重试同一 gate」的动作——候选切换的唯一触发原因是基础设施失败的重派（§6.3）。

**上界裁定**：`max_total` 缺省 4、`max_per_step` 缺省 2，均可在 workflow.md frontmatter 中配置（规则 V6）。达到任一上界即终止并给出可观测的失败终态，不存在无界回退。也不存在「运行时自动升级为需要人工授权后继续」的第三条路径——人工介入只能是 WI 层面的新决策（按 §4.6 第 4 步取消并新建 WI），不是 workflow 内部可以自行进入的状态。

区间 `[target, gate)` 内被重置为 `pending` 的 step 会在 `next_step_by_order`（§6.5）的下一轮判定中被自然选中并重跑，这是它们被重新执行的唯一机制，不存在第二种触发方式（INV-1d）。

### 6.3 基础设施失败 → 当前 step 原地 resume（按候选顺序有界切换），追加 attempt 历史 <!-- §6.3 -->

1. 判定标准：step agent 进程死亡或被杀、上游接口连续 429/5xx、宿主机重启、网络中断、本机磁盘或工具链不可用、step agent 最终输出不符合该 step 的 `output` JSON Schema（§4.4，INV-1h）——一切「未产出任何业务判定」的失败均归为基础设施失败。其中「输出不合 schema」有两个机械来源：WI Agent 派发侧自查，与每次全扫描发布时 Daemon 对已固化快照字节的 draft-07 校验（`03-execution-capsule.md#§5.1a`，polyforge#27）；二者失配分类相同，均走本节语义，不存在第二条处理路径。
2. 处理（两个动作同一次写入原子完成，互不矛盾）：
   a. 该 step 在 `execution.json.steps[step_id].status` 中保持 `in_progress`，不重置为 `pending`（这是对**业务结果车道**而言：不归档为失败结果，不触发 gate FAIL 语义，也不清空上一次已成功 step 的输出）。
   b. **同时向 `execution.json.steps[step_id].attempts[]` 追加一条历史记录**，字段为 `{attempt_seq, started_at, interrupted_at, cause: "infra_failure", detail, consumed_budget}`（该数组结构与 §6.2 rewind 归档历史属于同一 `attempts[]` 列表，只是 `cause` 不同：`gate_fail_rewind` vs `infra_failure`；归档 attempts 的权威 schema 在 `03-execution-capsule.md#§4.2`，本文只引用）。这满足 spec 「基础设施失败原地恢复」场景的「attempt 历史追加记录」要求，与上面 a 项「不归档为失败尝试」不冲突：**归档为失败尝试**指的是把 step 的业务结果记为 FAIL/重置为 `pending`（本节不做）；**追加 attempt 历史**指的是在不改变 step 状态机的前提下，单纯追加一条可审计的中断记录（本节做）。
3. 待 Daemon 检测到恢复条件满足（`04-local-runtime.md#§7`），在原地重新派发同一个 step，不跳过、不回退到更早的 step；重新派发前先写入下一条 `attempts[]` 记录的 `attempt_seq`（递增）。
4. **重派时的模型候选切换（有界，INV-1g 的 infra 侧）**：该 step 的生效候选列表 = step 级 `model_candidates`，缺省取 `defaults.model_candidates`（V9；两处皆缺省时不存在可切换候选，重派沿用 harness 适配器缺省模型）。首次派发用列表第 1 项；每次基础设施失败重派前进一档；**切换次数以候选列表长度为上界**——耗尽后保持末位候选继续原地重派，重派本身是否继续由恢复条件与预算（§6.6）决定，候选耗尽不是独立的终止条件。候选指针不是新的持久化字段：当前候选序号 = 1 +（该 step `attempts[]` 中位于最后一条 `gate_fail_rewind` 归档之后的 `infra_failure` 条目数；无任何 `gate_fail_rewind` 归档时从头计数），对列表长度取 min——该复算只依赖 `execution.json` 的既有归档结构（`03-execution-capsule.md#§4.2`），因此崩溃恢复后新 WI Agent 复算出的指针与崩溃前一致，不依赖进程内存。
5. 已完成（`done`）的 step 绝不因基础设施失败而重跑——**唯一例外**：发布时机械校验证伪其 `done` 判定的 step（输出不合 `output` schema，`03-execution-capsule.md#§5.1a`，polyforge#27）。此时「已完成」不成立：按本节基础设施失败语义原地重派，WI Agent 在追加第 2b 条 `infra_failure` 记录的同一次原子写入内把该 step `status` 由 `done` 改回 `in_progress`（不重置为 `pending`；`03-execution-capsule.md#§4.2`），候选指针推进同第 4 步有界规则，不新增任何终态。被中断进程此前已消耗的预算额度计入累计消耗（写入对应 attempt 记录的 `consumed_budget`），不因中断而回滚。
6. 与 gate FAIL 的边界（`00-architecture.md#§2.3` INV-9）：FAIL 的前提是 step 确实产出了业务判定；任何没有产出判定的中断（含输出不符合 `output` schema），一律按基础设施失败处理并 resume，不允许把「无判定的死亡」误记为业务否决。

### 6.3b 归档 attempts 的两分法（cause 语义） <!-- §6.3b -->

`execution.json` 的 `steps.<id>.attempts[]`（归档 attempts；文件格式与字段集的权威 schema 在 `03-execution-capsule.md#§4.2`，本文只引用）只存在两类元素，以 `cause` 二分，两类互斥：

1. **`gate_fail_rewind`（rewind 归档）**：由 §6.2 第 4 步的归档动作追加——step 因 rewind 被重置或产出被归档时，把当时的执行结果归档为一条历史尝试记录；语义是业务车道的失效归档（INV-1d），随后该 step 被重置为 `pending` 重跑。
2. **`infra_failure`（基础设施失败中断）**：由 §6.3 第 2b 步追加——记录一次未产出业务判定的中断；语义是可审计的中断追加，不改变 step 状态机（step 保持 `in_progress` 原地 resume），同时是 §6.3 第 4 步候选指针复算的输入。

两类的判定来源互斥（INV-1b/INV-9）：只有产出业务 FAIL 判定的回退才产生 `gate_fail_rewind` 归档；一切未产出判定的中断一律是 `infra_failure`。同一 step 的两类记录共存于同一个 `attempts[]` 数组，时间序，不互相覆盖。

### 6.4 incomplete 结果 <!-- §6.4 -->

1. step 可以诚实地产出 `incomplete` 判定（区别于 FAIL：不是质量否决，而是未能完成）。
2. 收到 `incomplete` 时，WI 转为 Core 的 `blocked` 状态、`waiting_on=owner`（`02-aihub-core.md#§2.4`/`#§4.2`）。
3. 当前执行位置保持不变：既不前进也不回退，`execution.json` 中记录的当前 step 不变。
4. 处置权交给人：可以原地重派当前 step 续跑，也可以终止该 WI。这是一个有明确出路的合法状态，不与任何自动恢复机制耦合。

### 6.5 驱动主循环（polyforge#106 起：Daemon 内的 Go driver，`00-architecture.md#§3.6` 修订注记） <!-- §6.5 -->

【polyforge#106 修订】本节原文把驱动主循环放在 WI Agent 进程内的执行 skill 里；该引擎删除后，主循环本体是 Daemon 内的 Go driver（§2.1 修订；引擎唯一裁定见 `00-architecture.md#§3.6`）：workflow 唯一的驱动者是这份运行在 Daemon 内、interactive 与 batch 两条路径共用同一实现的逻辑，它按 step 图逐 step 派生短驻 harness 进程（envelope 一次一步），不派生长驻 agent。其职责是反复调用 `next_step_by_order` 选出下一个待执行 step 并派发，直到没有下一个 step 为止；「没有下一个 step」之后必须做的第一件事，是判定这是「全部完成」还是「已进入失败终态」（INV-1e），这两者外观相同（都是 `next_step_by_order` 返回空），但后续动作完全不同：

1. 只要 `next_step_by_order` 返回一个 step，就派发该 step 的执行（一个短驻 harness 进程的 envelope，step skill 见 `03-execution-capsule.md#§5`），并等待其结果。
2. 结果为 `completed`：把该 step 标记为 `done`，回到第 1 步继续选下一个 step。
3. 结果为 `gate FAIL`：按 §6.2 处理（处理中不切换任何候选，INV-1g）。若处理后 workflow 已进入 `failed_gate` 或 `failed_rewind_exhausted` 终态，驱动主循环必须立即退出，不得继续调用 `next_step_by_order`；若处理后是走了 rewind 分支且流程仍在推进，则回到第 1 步继续。
4. 结果为基础设施失败：按 §6.3 处理，等待恢复条件后原地重派同一 step（含按序候选切换；不调用 `next_step_by_order` 前进）。
5. 结果为 `incomplete`：按 §6.4 处理，驱动主循环立即退出（不再调用 `next_step_by_order`）。
6. 当 `next_step_by_order` 第一次返回空结果时，驱动主循环必须显式检查当前 workflow 状态：
   - 若状态已经是 `failed_gate` 或 `failed_rewind_exhausted`（在第 3 步的处理中已经写入）：直接退出，不得把这两种终态误判为完成，不得触发 wrap。
   - 否则（意味着执行图中全部 step 都已到达 `done`）：这才是真正跑完全部 step 的成功路径，标记 workflow 状态为 `completed`，触发一次同步发布，并进入该 WI 的 wrap 流程（Core 侧 wrap 端点权威见 `02-aihub-core.md#§5.1`）。

**失败终态之后**：`failed_gate`/`failed_rewind_exhausted` 是 Capsule 侧（`execution.json`）的终态，**不直接映射为** Core 侧 WI 状态机的终态（`wrapped`/`failed`/`cancelled`，`02-aihub-core.md#§2.5`）——workflow 失败不等于 WI 失败，这是刻意的两层分离（`00-architecture.md#§3.2` 归属判据）：WI 在 Core 侧转为 `blocked`（`waiting_on=owner`，与 §6.4 的 incomplete 走同一 Core 端点），由人决定是取消该 WI，还是新建一个 WI 重新走创建流程（§4.6）；这里不要求也不允许先重新执行才能取消（该终止契约的完整授权与状态映射规则以 `02-aihub-core.md#§2.5`/`#§5.1` 为权威）。

### 6.6 预算（budget） <!-- §6.6 -->

- 结构：`{requests: int, tokens: int, wall_minutes: int}`，三个字段各自可选，取值须 >0；workflow 级声明 + step 级覆盖/叠加（规则 V6）；三个维度互相独立判定。
- 层次：**step 级预算**是单个 step 每次执行尝试的消耗上限；**WI 累计预算**（即 workflow 级声明）是跨该 WI 全部 step 执行尝试之和的消耗上限（含 rewind 产生的重跑消耗）。两层同时生效，谁先触发即报谁的错误——不存在「只要 WI 级不超就忽略 step 级」或反之的例外。
- 记账：per-step `budget_consumed` 消耗记录与 wall-clock 基线（wall 维度计时的起算基准）写在 Capsule `execution.json`（执行态，`00-architecture.md#§2.3` INV-1；权威 schema 在 `03-execution-capsule.md#§4.2`，本文只引用）；每次 rewind 额外计一次 requests 单位并预估目标 step 的 token 消耗。WI 级基线 scalar 由创建事务从 frontmatter 抄录固化（`02-aihub-core.md#§4.4`），运行期生效额度的计算以该基线为起点（`02-aihub-core.md#§6.7`）。
- **超预算行为**：任一维度、任一层级达到上界 → 当前 step 被停止 → WI 转为 Core 的 `blocked` 状态（`waiting_on=owner`，`02-aihub-core.md#§2.4`）并伴随 `WF-103 OVER_BUDGET`。超预算不是 FAIL，不触发 rewind，也不自动 resume。
- **超预算之后的处理路径**：完全由 `02-aihub-core.md` 的授权与 CAS 规则定义，本文不重复也不新发明任何机制——具体为「Core 侧的增加预算 API」（谁可以调用、CAS 如何避免并发覆盖、新预算值如何作为独立可追加记录 `workflow_budget_increments` 合并进不可变的预算基线），权威见 `02-aihub-core.md#§6.7`。人工增加预算后，WI 按 `02-aihub-core.md#§2.5` 的 `unblock` 语义重新激活（新一次 claim），WI Agent 原地重派被停止的 step；若人工判断不值得继续，则直接取消该 WI（终止契约同 §6.5）。

## 7. 并发与崩溃恢复 <!-- §7 -->

- 竞态面：owner generation 唯一活跃 + fencing（`00-architecture.md#§2.3` INV-3/INV-5，落地细节见 `03-execution-capsule.md#§2.4`）保证同一时刻至多一个 generation 的执行结果能写入 Core、推进 Revision HEAD——本层的 rewind/resume 状态机正是依赖这一「至多一份结果生效」的仲裁结果，不建立自己的并发控制。但 fencing 只在**发布权**层面排他，不保证**本地执行**立即停止：接管（尤其跨机）之后，旧 generation 的 WI Agent 进程可能仍短暂存活并继续驱动同一份只读物化快照（`03-execution-capsule.md#§4.5`），本文不得声称「双 WI Agent 本地执行」已被 fencing 排除——被排除的只是旧结果生效的可能性：旧侧的任何输出既无法通过 fencing 写入 Core/Revision，也不会被新 generation 从干净目录+远端 HEAD 重建的 Capsule（`03-execution-capsule.md#§6.1`）读取或采信，因此对本层已记录的 workflow 状态没有可观测影响。
- rewind 与 resume 互斥：同一 step 在任意时刻只可能处于二者触发的处理路径之一，判定以 `execution.json` 中该 step 最后一条结果为准（单写者序列，`00-architecture.md#§2.3` INV-6；该单写者约束在 host-native 同 UID 形态下是防误写措施而非安全边界，不构成机制级隔离，见 `00-architecture.md#§8`）。
- 崩溃恢复：引擎在某 step 执行中崩溃（进程退出或 driver 中断）→ Daemon 基于当前 `execution.json` 重新驱动（重启 driver，及 deploy 时的 fail-closed 检查——in-flight 且无 driver journal 戳记的 legacy 胶囊拒绝恢复，polyforge#106）→ driver 调用 `next_step_by_order` 找到状态为 `in_progress` 的 step → 按 §6.3 原地 resume（候选指针按 §6.3 第 4 步从 `attempts[]` 复算，与崩溃前一致）；已经是 `done` 的 step 不会被重新选中，也不会重跑。

## 8. 安全边界 <!-- §8 -->

- bundle 来源：AIHub 与执行机各自读本机 aihub checkout 的 `workflows/` 目录（部署配置给定）；不存在注册面、不存在远端 bundle 上传路径。frontmatter schema 拒绝未知键与一切非数据类型节点，杜绝「schema 内嵌脚本」的可能性。
- workflow.md frontmatter 只含数据声明；step skill 的 `tools` 声明是 harness 层工具面，OS 层文件系统读写权限完全由 `03-execution-capsule.md#§5` 的 v1 Native runner profile 统一裁定，不存在按 step skill 做 OS 级权限放大或收窄的机制。该路径级约束在 host-native 同 UID 形态下是防误写措施（权限位 + 路径不暴露），不是安全边界，不构成机制级隔离（`00-architecture.md#§2.3` INV-6、`00-architecture.md#§8`）。
- grill / grill-review 的问答记录（`artifacts/grill-qa.md`）是 Capsule 产物，随同步发布流转；本层不把问答内容发往除既定同步面之外的任何通道。

## 9. 监控 <!-- §9 -->

- 按 step `kind` 分桶的 step 时长分布。
- gate FAIL 发生率；rewind 次数分布（相对 `max_total` 的占用比例）。
- 基础设施失败触发的 resume 次数（持续偏高提示环境不稳定，而非 workflow 设计问题）；候选切换次数分布（相对各 step 生效候选列表长度的占用比例）。
- `OVER_BUDGET`（`WF-103`）触发次数，按维度（requests/tokens/wall_minutes）与层级（step/workflow）分列。
- 全部指标数据来源是 Capsule `execution.json`，随同步发布流转；AIHub 侧只做 read-through 聚合展示，不落任何执行态明细（`00-architecture.md#§2.3` INV-1）。

## 10. 测试与故障注入 <!-- §10 -->

测试类别（不给具体历史数据或日期，量化指标与回归清单权威在 `07-deployment-pilot.md`）：

- schema 校验：V1–V10 每条规则各至少一条正例与一条反例，断言返回的 `rule_id` 与预期规则一致（创建失败面为 `CORE-400` + `detail.rule_id`，§4.6 第 2 步）。
- 物化（digest 链已删，polyforge#149 B9）：断言按 `bundle_name` 物化的快照与 checkout 当前内容一致；另断言创建后修改本机 checkout 内 bundle 的任一文件，不影响已创建 WI 的 `bundle_name` 与已物化 Capsule 内的快照（跨代重新物化不做内容校验，见 INV-1a），且该 WI 的执行图不受影响（INV-1a/INV-1f）。
- rewind 有界性：构造一个必然 FAIL 的 gate，断言达到 `max_total` 与 `max_per_step` 各自的上界时都能正确终止为 `failed_rewind_exhausted`，且不会产生超出上界次数的额外派发。
- 终态区分：断言「全部 step 完成」与「进入 `failed_gate`/`failed_rewind_exhausted`」两种路径下 `next_step_by_order` 均返回空，但驱动主循环分别走向 wrap 与保持失败终态两条不同分支（INV-1e）。
- 候选切换有界性：断言基础设施失败重派按序前进一档、以候选列表长度为上界、耗尽后保持末位；断言 gate FAIL 触发的 rewind 不推进任何候选指针、被重置 step 重跑从第 1 项开始（INV-1g）；断言 WI Agent 崩溃恢复后按 §6.3 第 4 步复算的候选指针与崩溃前一致。
- 输出契约：构造 step agent 最终输出不符合该 step `output` JSON Schema 的场景，断言按 §6.3 基础设施失败处理（原地重派并推进候选指针），不产生业务判定、不触发 gate（INV-1h）。
- 可注入故障类别：step agent 进程被强制终止（预期按 §6.3 原地 resume，已完成 step 不重跑）；上游接口连续错误（预期 resume，不产生 FAIL 判定）；同步链路中断（预期由 `05-sync-revision-recovery.md` 侧补发，本层不感知）；宿主机断电重启（预期从 `execution.json` 恢复到中断前的执行点，含候选指针复算）；交互会话中途退出（预期 grill/grill-review 保持 `in_progress`，重新进入会话后继续问答）。

## 11. 迁移与 rollout <!-- §11 -->

本层无存量数据迁移。参考 bundle `feature-full` 随 aihub 仓交付（`workflows/feature-full/`），不存在注册步骤——创建 WI 时由 AIHub 从本机 checkout 读取（§4.6）。引入顺序（不含具体日期）：frontmatter 校验器 → digest 计算与固化机制 → 驱动语义 → bundle 内容，最后与 Daemon/WI Agent 联调；总体 rollout 排期与否决门槛权威在 `07-deployment-pilot.md`。

## 12. 未决问题 <!-- §12 -->

1. budget 的 token 计量口径（模型 usage 回报 vs 本地估算）尚未最终定案；已声明的缺省行为是优先使用模型侧 usage 回报，缺报时退回本地估算，误差容忍范围待与 `07-deployment-pilot.md` 的验收口径对齐。
2. 多个 gate step 分别指向不同 rewind target 时的复合回退路径，其可读性与人体工学尚待优化；上界机制已保证其必然收敛，不构成正确性问题。
3. bundle 是否需要引入独立于 digest 之外的可读语义版本号（digest 本身已充分保证固定性与可复算性）：开放，不影响 `00-architecture.md#§2.3` INV-8。
4. `model_candidates` 条目字符串的具体编码（模型标识、effort 等）由 harness 适配器契约（`00-architecture.md#§3.6a`）收敛；v1 只锁定有序、step 覆盖 defaults、infra 有界切换与 gate 禁止切换这四点语义（§4.1 V9/§6.3）。
