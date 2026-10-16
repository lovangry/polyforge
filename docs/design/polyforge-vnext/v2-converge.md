# v2 收敛设计：减法、监督重构、watch 与身份模型

> 状态：**r3，owner 裁决已全部纳入，待审阅**（2026-10-12）。
> 来源：2026-10-12 owner 与 review 会话的完整讨论；r2 吸收了一次独立深度审查（25 个漏洞 + 历史回归对照，附录 A/B），owner 已同意其 18 条修补默认值。
> 本文取代的范围：04 §6.2 监督车道、v1.5-session-model 中 watch/adoption 相关描述、`skills/polyforge/scripts/watch.sh`。
> 落地进度：本文第 2–8 节的设计均已落地（含监督重写与 `pf watch` 进展字段）；见第 8 节状态列、第 10 节映射与文末「已知缺口」。
> 落地方式：**快速落地**——直接切换到新设计，不走"新旧并行观察 → 再删旧"的分阶段迁移（owner 2026-10-12）。

---

## 0. 定位与规则（已定）

- polyforge 是**团队级多 agent 系统**，将开源；保持克制，不大量加功能。
- 核心三根支柱：**任务**（aihub WI）、**记忆**（项目演进与知识沉淀）、**workflow**（团队统一的任务处理规范）——目标是团队的 agent 在同一套体系下像一个 agent 一样运转。
- 新功能冻结，先做减法；大改现有逻辑先问 owner；**心智模型越简单越好**（以"概念数减少"衡量方案）。
- **按步骤选模型**是核心能力，保留；**按步骤选 harness**（pi / Claude Code / Codex）要做，排在减法之后。
- **agent 不可信**：执行 WI 的 agent 不得驱动 daemon、不得读取机器凭证、不得写入任何会被人或交互 agent 信任的文件（#135）。

## 1. 业务领域划分

| 类型 | 领域 | 主要内容 |
|---|---|---|
| 主线 | 安装与升级 | install.sh（安装与升级均重跑它）、发版、aihub 部署 |
| 主线 | 团队与权限 | 用户、key、项目、成员角色、agent 信任边界 |
| 主线 | 任务管理与生命周期 | WI 状态机、依赖、就绪队列、领取（session 锁）、blocked 出口 |
| 主线 | 记忆 | 存储与检索、recall/remember、领取时快照 |
| 主线 | workflow 定义 | bundle、step、gate/rewind、按步骤选模型（将来：harness） |
| 主线 | 执行 | daemon、capsule、driver、并发、工作区映射 |
| 主线 | 交付 | ship → git 分支 / PR (removed by polyforge#166: delivery is the workflows' deliver step. The driver checks the deliver step's recorded commands and the delivered history after the fact and escalates violations to a human; prevention rests on the step contract and the machine's local pre-push hook.) |
| 横切 | 人工协同 | 提问/回答、暂停/继续、进展可见（`pf watch`） |
| 横切 | 可靠性 | 监督（判死）、恢复、session 隔离、接管 |
| 横切 | 运维 | doctor、backup、ops status、事件日志 |

## 2. 执行与监督

### 2.1 分工（已定）

| 组件 | 职责 | 不做什么 |
|---|---|---|
| **driver** | 执行 WI：按步骤派发 step 进程、收结果文件、推进 `execution.json`、按死因处理、维护失败计数 | 不判断"活着但不推进" |
| **supervisor**（原 observe 组件，改名以免与命令混淆） | 观察执行动态、**判断 step 是否已死**、判断 **driver 是否卡在步骤之间**、M4 僵尸会话重验、记录事件、推送进度到 aihub、提供查询接口 | 不推进执行 |
| **恢复引擎**（runtime） | driver 自身异常退出后的重拉；保留有界失败语义（连续多次恢复失败 → blocked；唯一的失败账本是“连续恢复失败”计数器，步骤结果被接受即清零） | 不判 step 死活 |
| **`pf watch`**（CLI） | 面向人的进展与决策通知 | 不做任何判断 |

**例外（已定）**：step 进程**退出却没交结果**，由 driver 直接处理（它在等结果时就能看到），再告知 supervisor。

### 2.2 driver ↔ supervisor 契约

二者同在 `pf daemon` 进程内。

**通道规则**
- 实时信号走进程内通道：**每个 WI 一条无界队列，发送方永不阻塞**（supervisor 的 select 循环与 runtime 的串行处理都不得被投递卡住）；**事实与判定不得丢弃**。
- driver 消费判定时**不得在 opMu 下做网络 / git 等长操作**（避免一个 WI 的推送卡住所有 WI）。
- harness 的海量输出**只保留最新状态**（合并，不排队）。
- 持久化：`execution.json` 是**唯一权威**（进度、待答问题、失败计数、放宽后的阈值、终态）；事件日志是**补充记录**——driver 事件也写入（替代只打 stderr 的 `stdLogger`），每条事件带**跨重启单调递增 id**（不再每次重启从 1 重编），文件按大小轮转；磁盘满时事件追加失败只降级为日志告警，权威仍在 `execution.json`。
- supervisor 不再轮询 `execution.json` 作为主路径；推送进度到 aihub 改由事件触发，**保留退避与同错熔断**（F18/F22 教训：曾 185 次推送风暴）。
- 转录同步（F3）以最近一次 `step_started.transcript_path` 为源，commit 前同步。（现状：job 级收尾前的活转录同步**保留**——C11 裁定，polyforge#150；实现为 `runtime/pitranscript.go` 的 `syncLiveTranscript`（commit 24cde0d 恢复）：job 在 pause / bounded-fail / seal 等收尾路径上结束时，当前派发的 step 未走到提交边界，其活转录若不在收尾提交前同步就会丢失——收尾前同步保证终态前转录落盘。）

**driver → supervisor（事实）**

| 事件 | 字段 |
|---|---|
| `step_started` | step, spawn_n, model, harness, pid, transcript_path, started_at, 本步阈值（含已放宽值，见 2.4） |
| `step_waiting_human` / `step_answered` | step, spawn_n, question_id |
| `step_ended` | step, spawn_n, outcome（正常交结果 / 退出未交结果）, exit_code, 最后一条错误信号 |

**supervisor → driver（判定）**

| 事件 | 字段 |
|---|---|
| `step_dead` | step, spawn_n, cause（见 2.3）, evidence |
| `driver_stalled` | 当前所处步骤间隙、持续时长 |

**围栏与幂等（r2 补）**
- driver 只接受 `spawn_n == 当前派发` 的判定，过期判定丢弃。
- **每次派发至多一次判定、至多一次失败记录**：driver 收到判定后该派发进入 terminating 状态，之后这次派发的进程退出、后续判定一律忽略；supervisor 对某次派发发出判定后静默，直到 `step_ended`。
- supervisor 的状态**按每次派发一份**：`step_started` 新建，`step_ended` 丢弃（根治 #124 turn_duration 跨步不清零误杀）；**步骤级累计量**（总时长 T 的已用时间）由 driver 持久化在 `execution.json`，随 `step_started` 下发。

### 2.3 判死规则（supervisor）与死因处理（driver）

supervisor 对每次派发维护：派发时刻、最后一次"真实输出"时间、当前是否在执行工具（哪个、何时开始）、是否在等模型回复（何时开始）、本次派发的"工具时间 / 等模型时间"。

**"真实输出"**：只有工作类事件算活着；UI 刷新类不算（F15 教训）。

**判死规则（多条同时命中时按编号优先）**

| # | 规则 | 条件 | 死因 cause |
|---|---|---|---|
| ⓪ | 启动后无信号 | 派发后 X 内没有任何工作类信号（harness 卡在启动、stdout 被孙进程持有等） | `harness_silent` |
| ① | 本轮结束后停住 | harness 发出"本轮结束"，宽限 1 分钟仍无结果文件：原因出错 → `model_error`（换候选；但该轮错误文本先过与退出路径相同的 D7 分类，见下表与 polyforge#162）；正常结束 → `ended_without_result`（同模型重试） | `model_error` / `ended_without_result` |
| ② | 等模型太久 | 在等模型回复，超过 X 无任何输出 | `model_hung` |
| ③ | 工具太久 | 某工具超过 Y 未结束 | `tool_hung` |
| ④ | 总时长超 | 本步**累计**运行超过 T | `step_timeout_long_task`（主要耗在工具上）或 `step_timeout_slow_model`（主要耗在等模型 / 单次回复异常慢） |
| — | 步骤之间卡住 | driver 不在任何 step、也不在等人，超过 N 分钟（如推送 git 子进程卡死仍持锁） | `driver_stalled` |

等人回答期间没有 step 进程，**不计时**。

**"退出未交结果"的细分（driver 自己发现，r2 补）**

| 细分 | 判据 | 处理 |
|---|---|---|
| 模型类 | 错误文本命中额度·鉴权特征（401/403/鉴权失败/api key、quota / insufficient / billing / credit；中文：余额不足 / 欠费 / 额度·配额用尽 / 认证失败 / 模型·接口无权限 等）或模型名不可解析，或退出前最后信号为"本轮出错"且文本不属限流类/上游瞬时类。规则 ① 的判决（进程**存活**的出错轮次，pi rpc 出错后不退出）同样把该轮错误文本送入 `classifyWaitFailure`（polyforge#162），不命中限流 / 上游瞬时时仍记 `model_error` 换模型 | 同 `model_error`：换模型（F4 教训：模型 pin 失败必须推进候选）。额度·鉴权文本最先判：同时带 429 或连接中断特征（"429 … exceeded your current quota"、"insufficient balance … connection reset"）仍立即换模型 |
| 限流类（polyforge#147 D7） | 错误文本命中瞬时限流特征（429 / rate limit / too many requests / overloaded；中文：并发数过高 / 请求过于频繁 / 请求过多 / 限流 / 繁忙 等），且不命中额度·鉴权特征 | `rate_limited`：原模型退避重试，不推进候选指针、不计失败计数 |
| 上游瞬时类（wi_seWTgHeKtaa8） | harness 报告的上游返回损坏或上游流中断：工具调用参数 / JSON 不完整·截断·畸形、空回复（0 token）、上游连接 reset / refused / closed / EOF；不命中额度·鉴权与限流特征 | `upstream_transient`：原模型按 D7 退避重试，**最多 2 次**，第 3 次即归档为模型类（`exit_model_class`）并换模型；不计失败计数。裸的 "invalid …"（工具名、请求参数）、上下文超长、driver 自身的本地 I/O 错误（如 "read transcript: unexpected EOF"）都**不**属此类 |

**文本分类顺序**（`driver/supervision.go` `classifyWaitFailure`）：机器类 → 正常结束一轮后干净退出（`ended_without_result`）→ 额度·鉴权（换模型）→ 限流（同模型）→ 上游瞬时（同模型，界 2）→ 模型名不可解析（换模型）→ 本轮出错（模型类）→ 其余。非进程退出（driver 自身失败）不做上游瞬时匹配。中文 provider 文本同样覆盖（polyforge#155）：额度·鉴权类含 余额不足 / 欠费 / 额度·配额 用尽·不足·超出 / 账户…停用 / 鉴权失败 / 认证失败 / 模型·接口·API·服务 无权限 等（裸"权限不足"不算，本地 权限不够 / 权限被拒绝 / 拒绝访问 归机器类），限流类含 并发数过高·超限 / 并发超额 / 请求过于频繁 / 请求频率 / 请求过多 / 限流 / 速率限制 / 繁忙 / 负载过高；均为多字短语（CJK 下 `(?i)`、`\b` 无效，不用单字），两者同时出现仍额度·鉴权优先（换模型）。
| 机器类 | driver 自身 I/O 错误（ENOSPC、EACCES 等）或翻译器上报的宿主错误 | 机器问题 |
| 其余 | — | 同模型重试 |

**driver 按死因处理（已定）**

| 死因 | 处理 |
|---|---|
| 退出未交结果（其余） | 同模型重试 |
| `harness_silent` | 同模型重试 |
| `ended_without_result` | 同模型重试；重试的 prompt 明确告知"上一轮已结束但没有写结果信封"。进程在正常结束一轮后以退出码 0 自行退出且无结果，也归此类（被杀 / 崩溃仍是退出其余） |
| `model_error`、`model_hung`、退出（模型类） | 换下一个候选模型。规则 ① 的 `model_error`（存活进程的出错轮次）先把轮次错误文本过与退出路径相同的 D7 分类（polyforge#162）：只有限流类 / 上游瞬时类文本把失败记录的死因改为 `rate_limited` / `upstream_transient`（原模型重试、退避计入 T、上游瞬时界 2）；机器类特征及其余文本一律保持 `model_error`（换模型）。`step_dead` / supervisor 判决的 cause 仍是 `model_error`，失败记录（`step_failed`、attempt）才带 `rate_limited` / `upstream_transient`——有意如此，由 `test/v2_live_error_turn_d7_test.go` 固定 |
| `step_timeout_slow_model` | **放宽 T（翻倍，写入 `execution.json`）后换下一个候选模型**（polyforge#147 D6）：新候选在**放宽后的完整额度**下运行，累计耗时连续结转但严格低于翻倍后的界——绝不在启动瞬间即被判超时 |
| `rate_limited`（429 / rate limit / too many requests / overloaded / 中文限流文本；额度/账单/权限类不在此列，归模型类换模型） | **不换模型**：原模型退避重试（polyforge#147 D7），退避 1s 起逐次翻倍、单次上限 300s；**退避等待计入该步累计 T**；**不另设重试次数上限**，唯一的界是该步累计 T（T 耗尽即走既有超时处理）；**不放宽 T**（只有超时类死因放宽）；不计入失败计数 |
| `upstream_transient`（上游返回损坏 / 上游流中断，见上表） | **不换模型**：原模型按 D7 同一退避重试（1s 起翻倍、单次上限 300s，**退避计入该步累计 T**），不推进候选指针、不计失败计数；**连续 2 次后**第 3 次归档为 `exit_model_class` → 换下一个候选（界只由 attempts 归档里尾部连续的 `upstream_transient` 计数，崩溃恢复后的 driver 读到同样的剩余次数） |
| `tool_hung` | 同模型重试，并**放宽 Y**（同 T 的放宽规则） |
| `step_timeout_long_task` | 同模型重试（**不换模型**）并放宽 T（翻倍并持久化）；再超时 → 同样再翻倍 T 并持久化，然后 blocked（人工"继续"在新放宽的界下运行，不会一启动就超时） |
| 机器问题 | blocked，通知人，不换模型 |
| daemon 重启 | 同模型重新派发，**不计失败** |
| `driver_stalled` | 终止当前 git/推送子进程，driver 重拉（计入 driver 异常退出界） |
| driver 自身异常退出 | 恢复引擎重拉；**连续多次恢复失败（唯一账本：连续恢复失败计数器）→ blocked**——防止"崩溃 → 重启不计失败 → 再崩溃"无界循环（#97 教训） |
| 同一步失败计数达上限（默认 3） | blocked，通知人 |

**候选用尽**：指针停在末尾，后续"换模型"按同模型重试计数；决策事件注明"候选已用尽"。

**重试形态**：第一次重试接着原对话（resume 原 transcript）；第二次起重开对话（fresh），把上次 transcript 作为只读参考输入——避免上下文超限类错误在任何模型上都重复失败（F13 一族）。

**失败计数（r2 补）**
- 存放：`execution.json` 该 step 的字段（attempts 记录带 cause 子类）。
- 计入：上表除"daemon 重启"、"限流类 `rate_limited`"（polyforge#147 D7：退避重试只受 T 约束）与界内的"上游瞬时类 `upstream_transient`"外的所有死因。**信封被拒不计入**——polyforge#147 C4 起它不再走"进程内重试耗尽后重启 driver"的升级车道：进程内重试 3 次仍不合格 → 该步直接 blocked，原因码 `result_invalid`，不重启 driver、不路由 `on_fail`（rewind 只留给评审 FAIL 裁决）。
- 清零：gate rewind 回到该步、换到新候选模型、人工"继续"。
- daemon 重启触发的中断**不归档为失败、不推进候选指针**；一般地，被中断的 attempt 永不推进候选指针。（现状：`FaultClass` 分类层已删除，改为**死因规则**——只有模型类死因 `model_error`、`model_hung`、`step_timeout_slow_model`、`exit_model_class` 推进候选指针；指针在 gate_fail rewind 时重置；见 `driver/candidates.go`、`driver/supervision.go`。事件体与 attempts 记录带 `failure_cause`，不再有 `fault_class`。）
- （现状：候选列表五级优先，高到低：`pf submit --models a,b,c` 的手工列表（仅由本 daemon 保存、按 WI、作用于该 WI 的每一步；随 submit 队列落盘，认领时写入 daemon 数据目录的 `manual-models.json`（不进 capsule），本机 daemon 重启、恢复、`pf resume` 重新认领均沿用，WI 终态（wrapped/failed/canceled）时删除；在另一台机器上重新认领则没有该列表，使用那台机器自己的设置）> 工作区 yaml 文件的 `steps.<project>.<step-id>.models`（polyforge#169：按步骤 id 配置，顶层键 `steps:` 取代 `models:`，旧 `models:` 不兼容、加载即拒绝并给迁移提示，同名步骤跨 bundle 共用一个键；非 `default` 的键必须是已安装 workflows 中的步骤 id，否则 DAEMON-414 拒绝认领、`pf doctor` 报告）> bundle 步骤自带 `model_candidates` > `steps.<project>.default.models` > bundle `defaults.model_candidates`（polyforge#164：步骤自带列表高于项目 default，项目 default 只兜底未自选模型的步骤）；推进/重置语义不变。见 `workspace/compose.go`。）
- 失败处理按死因表（上节）执行；`PolicyFor` / provider·host·operator·schema 分类已随 FaultClass 层删除（`driver/supervision.go`）。

### 2.4 阈值（已定）

- X（⓪②）、Y（③）、T（④）、N（driver_stalled）**全局默认**；step 级只能覆盖 T（见下条）。
- **T 的 step 级覆盖复用现有 `budget.wall_minutes`**，但它是**估计值而非硬上限**（polyforge#164：≤30 分钟的估计在下式下不起作用，交付 bundle 已全部删除，只剩 release bundle 的 `release: 45` → T=135）：声明了 `wall_minutes` 的 step，总时长界 **T = max(3 × wall_minutes, 全局 `--step-timeout` 默认)**；未声明则就是全局默认。不新增键；X、Y 没有 step 级覆盖（polyforge#147 C5 删除了 `silence_minutes` / `tool_minutes`，旧 bundle 中残留的这两个键被静默忽略）。覆盖值随 bundle 在领取时钉住，对之后领取的 WI 生效。
- X 默认按 harness 翻译器声明的**输出粒度**取：逐字粒度（思考时也有输出）可短（例 10 分钟）；整条粒度或模型不返回思考内容时放长（例 30 分钟）。"模型不返回思考内容"在模型配置中声明。
- Y 默认 30 分钟（#83 教训）；T 默认 60–90 分钟（#117 教训）；N 默认 10 分钟。
- **放宽**：每次放宽翻倍（放宽后的界 = 放宽前界 × 2），写入 `execution.json`（`steps.<id>.supervision.relaxed_wall_ms` / `relaxed_tool_ms`），重启后沿用。**只有超时类死因放宽 T**（`step_timeout_slow_model` 与 `step_timeout_long_task` 每次处置都放宽一次，第二次 `step_timeout_long_task` 也先翻倍再 blocked）；换模型、人工继续、限流退避一律不放宽（polyforge#147 D6）。
- T 按**整个步骤累计**（跨该步的多次派发、跨换模型、跨人工继续连续累计，polyforge#147 D6 起不再有任何"重开窗口"的 rebase——`runtime_base_ms` 已删）；计入 agent 实际运行时间与**限流退避的等待时间**（D7：同模型重试只受 T 约束，owner 裁定），等人回答（needs_input）的挂钟时间不计入。
- **rewind 重跑清零**：gate FAIL rewind 回到某步时，该步的累计 T 归零（旧窗口的记账随归档 attempt 留存）；发起 rewind 的 gate 步自身（不归档、保持 in_progress）的累计 T 同样归零，重入在新窗口下评审。

### 2.5 harness 翻译器

每个 harness 一个翻译器，把原生事件翻成标准信号：活着、在等模型、模型在输出（思考/正文）、工具开始（名称+参数）、工具结束、本轮结束（成功/出错）、**进程退出（退出码、最后一条错误）**。supervisor 只认标准信号。**这也是"按步骤选 harness"的接入点**。

| harness | 对接方式 | 思考时有输出 | 核实来源 |
|---|---|---|---|
| pi | `--mode rpc` | ✅ `thinking_delta` | 本机源码 `agent-loop.js` |
| Claude Code | `-p --output-format stream-json --verbose --include-partial-messages`（由 driver 一律加上） | ✅ | 2.1.296 二进制事件名 |
| Codex | **`codex app-server`**（JSON-RPC），**不用 `exec --json`** | ✅ `item/reasoning/textDelta` 等 | 0.162.1 源码；`exec --json` 会过滤掉回复/思考的开始事件且无增量 |

第一版只实现 pi 翻译器；Claude Code / Codex 随"按步骤选 harness"落地，接入时按当时最新版本复核协议。

### 2.6 结果信封、`context_policy` 与 `handoff`（polyforge#147 定稿）

**结果信封 = 最小契约**：step agent 把**一个** JSON 对象写到约定位置 `runtime-state/pending-results/<spawn_n>.json`（位置与命名不变），内容只有 `outcome` + `output`，`needs_input` 时另带 `question_id` + `question`，再加上派发身份围栏（`wi_id` / `step_id` / `spawn_n`）。`protocol_version`、`result_id`、`artifacts` 哈希清单、`handoff_ref` 摘要引用**全部删除**，driver 不再校验它们，缺这些旧字段 MUST NOT 判不合格；result_id 宽容等补全层一并删除，不留兼容分支。**V4 命令取证门禁保留**（owner 裁定：agent 不可信，取证门禁留在 driver）：欠取证的 step（`requires_command_evidence: true`，或 step id 为 `verification`）的 completed 结果另带 `command_evidence: [{argv, exit_code, duration_ms}]`——至少一条、argv 非空、`duration_ms > 0`，否则信封被拒（进程内重试车道）；字符串形态条目由 `command_evidence` 的宽容重解析剔除并在拒绝原因里点名；被接受的取证由 driver 写入 `output.commands`（B10，step 自写的同名键被覆盖）。其余 step 的信封不带该字段。**已提交测试文件门禁**（polyforge#156，任意 step）：completed 结果的 `output.test_files` 非空时，每条路径必须是 repo 相对路径、至少存在于一个 task repo，且在其存在的每个 task repo 中都已提交在 HEAD，否则信封被拒（同一进程内重试车道，拒绝原因点名路径）——resume 只按 `refs/pf-task/<name>`（HEAD）恢复 task repo，未提交文件会丢。**声明输出 schema 校验**（polyforge#164 H3）：step 定义文件 frontmatter 的 `output:`（draft-07 JSON Schema，YAML 书写）既注入提示词（放在 step 正文之前，"per the schema above" 成立），也由 driver 机械校验 completed 结果的 `output`（值为 `null` 的可选属性按缺省处理）。v2.5.0 为**只记录模式**（`driver/outputschema.go` 的 `enforceOutputSchemas=false`，唯一开关）：不合格时不拒信封，只记持久事件 `output_schema_violation`（step、schema 路径、截断的校验错误）——用近期真实 result.json 回放约 64% 不合格（agent 此前从未见过 schema），后续版本翻转开关后改为信封被拒（同一进程内重试车道）。声明了却加载失败的 schema 记 `output_schema_unloadable` 事件并写 stderr。**既有失败的边界**（#164 S6）：repro 检查、作者 step 的 `test_files`、bug-fix fix 的 `regression_test` 永远不能标为 `pre_existing`（它们在 base 上失败是设计使然，未修好的 bug 会被当成既有失败放过），pre_existing 检查的 detail 必须带 base sha，且至少有一条 `<repo 相对路径>: <测试名>` 形式的 `pre_existing_failures` 条目（路径按完整路径或目录前缀的路径分段精确比对，不做子串匹配）；bug-fix 的 repro 已复现时，verification 的 pass 必须带一条名为 repro、状态 pass 的检查；违者信封被拒（同一车道）。**已写测试不可改**（polyforge#164 H4）：声明 `test_files` 的 step（e2e-author / repro）被接受时，driver 在其 `output.test_files_commit` 记下各 task repo 的 HEAD（只由 driver 写，step 自写的同名键被丢弃）；之后每个 completed 的 step 都机械比对这些文件在该提交与 HEAD 之间是否一致（等价于 `git diff <author-commit>..HEAD -- <test_files>`，以两次 `git ls-tree -r` 实现，沿用 #156 的加固 git 调用），有差异即信封被拒——除非该 step 当前尝试链里有人工回答（step 认为测试本身错了，必须用 needs_input 提出），此时改动成为新基线（driver 把新的 HEAD 写回前面作者 step 的 `test_files_commit`，故该 step 之后被 rewind 也不丢），并记持久事件 `authored_tests_rebaselined`（列出被改的文件）。**`requires_human_answer` 取值**：`true` = 完成前必须有人工回答（feature-full grill-review）；`if_asked`（polyforge#164 H11，quick-fix grill）= 提出的问题只能由人回答，零问题可直接完成，但 driver 记一条持久事件 `human_gate_unasked`，watcher 据此通知 owner。

**`context_policy`（step 键，`inherit`\|`fresh`，缺省 `inherit`）**：声明一个 step 与上游的上下文关系。`inherit` = 本 step 续上游的活：派发时 driver 找**最近一个已 done 且声明 `handoff: true` 的更早 step**，按路径约定 `artifacts/handoff-<step-id>.md` 读出交接正文，原样拼进派发 prompt 的「Handoff from step …」小节；`fresh` = 从零开始，不注入交接正文，要读的上游产物按产物路径约定自取。`gate: true` 的 step 强制 `fresh`——闸只凭自己的输入判，不继承有倾向性的上下文。

**`handoff`（step 键，bool，缺省 false）**：`true` 的 step 必须在收尾前把交接内容写成 `artifacts/handoff-<step-id>.md`。**路径约定就是全部契约**：没有摘要、没有字节数、没有信封字段，driver 不校验交接文件的哈希、不比对清单；交接文件不在时，下游 inherit step 的派发只是少了那一段正文，不构成信封级失败。权威细节见 `01-workflow-skills.md#§4.4a`。

### 2.7 harness 适配层的隔离保证（owner 裁决 A4，2026-10-13）

**禁止 step agent 使用 harness 后台任务，是 harness 适配层的责任，不是写进提示词的纪律。** 后台任务会让一个步骤在它的派生进程之外留下活动，监督面（判死、step 结束、派生计数）全部失真；靠"规则里写了不许用"约束的只是诚实的 agent。

pi 适配层的具体机制：**专用 step agent 目录**。daemon 在自己的数据目录下维护 `<data-dir>/step-agent/`，派生每个 step 进程时注入 `PI_CODING_AGENT_DIR=<data-dir>/step-agent`（会话另置于 `PI_CODING_AGENT_SESSION_DIR=<data-dir>/step-agent-sessions/`，实时 transcript 定位器跟着同一个根走）。该目录是 operator agent 目录的**最小快照**——只有 `auth.json`（模型服务凭证）、`models.json`（模型服务配置，保留源文件权限）、`agents/`（子 agent 定义）、`extensions/subagent/`（子 agent **工具**本身），**没有 `settings.json`**，而用户级 package（例如提供后台任务工具的 `npm:pi-background-tasks`）正是在 `settings.json` 里声明的：没有声明就没有加载，后台任务工具对 step agent **机械不可见**，与提示词无关。

- **子 agent 工具如何送达**：它不是 pi 内建工具，而是 operator agent 目录下的扩展 `extensions/subagent/`；pi 只从 `<agent 目录>/extensions/` 自动发现全局扩展（`resource-loader` / `package-manager` 的全局 extensions 根）。所以快照**恰好**拷贝 `extensions/subagent/` 这一个子目录，其余扩展一律不带（例如 `extensions/polyforge` 是交互会话用的），`settings.json` 与 packages 永远不带。
- 拷贝而非链接：快照不跟随 operator 后续改动（包括全局重装），也没有任何符号链接把专用目录变成整个 operator 目录的窗口（树内符号链接跳过不拷）。**每次派生前**只刷新白名单成员（源文件大小+mtime 变化即重拷，源里没了就删）；清除白名单以外的条目**只在 daemon 启动时**做——那时本 daemon 没有存活的 step 进程，不会把文件从运行中的 step 进程脚下抽走。凭证不可读 = 大声拒绝派生，绝不降级运行。
- **凭证限制**：拷贝的 `auth.json` 支持 API-key 条目。OAuth 条目在 pi 刷新令牌之前可用；pi 会把刷新后的令牌写进**副本**，而下一次同步发现副本与 operator 文件不一致，就用 operator 的（旧）条目覆盖它——step 侧的刷新结果会丢失。没有为此设机制：step agent 应使用 API-key 凭证。
- 位置在数据目录下，**绝不在 capsule 内**：capsule 会提交并推送到 aihub，且对 agent 可写。
- `--no-extensions` 仍然**不传**：子 agent 工具就是上面那个扩展，关掉它等于关掉整条 step-agent 派发路径；隔离由目录（只带这一个扩展）而非开关完成。
- **任何未来的 harness 在接入前，必须提供等价的保证**（一个 harness 若无法从启动配置上关掉后台任务族，就不具备接入资格）——这是适配层的准入条件，写在这里而不是写在执行者规则里。
- 与 #135（独立 agent 用户）方向一致：该目录届时对 step agent 用户只读，daemon 仍是唯一写入者。

## 3. 人工协同：`pf watch`（已定）

### 3.1 形态

- Go 实现的 `pf watch`，**取代 `watch.sh` 与 `pf watch-probe`**；`pf observe` 并入为 `pf watch <wi> --once`。
- `pf watch --chat <id>`（缺省取 `PF_CHAT_ID`）**不因订阅的 WI 结束而退出**（保持常驻；D10 裁定，polyforge#150：以代码为准——只跳过已结束的 WI；仍会在父 harness 进程消失、stdout 关闭、或 `--wait-decision` 打印首批决策后退出，`watcher/engine.go`）；一个会话一个进程。
- 只产出两种信号：
  - **进展**：当前 step、已运行时长、正在执行的命令及时长、最近修改的文件、工具调用计数（由翻译器信号机械生成，**不调用模型**）。
  - **决策事件**：有问题要回答/gate、失败、blocked、完成、被判死并已处理、候选已用尽；以及状态/来源类事件（C13 裁定，polyforge#150，与 `watcher/decision.go` 的事件类别表对齐）：`canceled`（终态）、`paused`（非终态，可继续）、`submit_rejected`、`submit_dropped`、`daemon_restarted`；以及 `carrier_push_failed`（polyforge#158：capsule 链推送成功而某任务仓库的 carrier ref 推送失败或被拒，resume 可能丢该仓库的提交；提醒行带经字符集过滤的仓库名，不带错误文本；每个 job 每个仓库只提醒一次，直到该仓库推送再次成功）；以及 `human_gate_unasked`（polyforge#164 H11：`requires_human_answer: if_asked` 的人工关口——quick-fix grill——零问题完成，工作流照常继续，owner 收到一次提醒）。
- **订阅关系与读取游标存于 daemon**（按 chat id），不存在 agent 可写的位置。
- **重连 / daemon 重启后**：先以 `execution.json` 核对每个订阅 WI 的当前状态（权威），再按单调事件 id 补读事件——"WI 在 watch 不在时结束"也不会漏。
- **提交后、监控尚未注册**：显示"排队 / 准备中"，不当作错误（#98/#101 注册竞态）。
- daemon 不可达：明确提示"daemon 无法连接，进展暂停"，而不是静默循环。
- pidfile + 锁；**父进程（会话）消失即退出**，不留孤儿。

### 3.2 两条通道（已定）

| 信息 | 通道 | 进 agent 上下文？ |
|---|---|---|
| 进展 | 直接显示：Claude Code 用 `statusLine`（可多行，最快每秒刷新）；pi 用扩展 `setWidget`/`setStatus` | 否 |
| 决策事件 | 唤醒 agent，**只给一句由 daemon 按固定模板生成的短话** | 是（少而短） |
| 用户主动问进展 | agent 执行一次 `pf watch <wi> --once` | 仅此时 |

**安全（r2 补，#135 落地前即生效）**
- 唤醒语句**只用 daemon 生成的固定模板**（WI 编号、事件类型、问题 id），**不夹带任何 agent 可写的自由文本**——防止 step agent 借此对交互 agent 做提示注入。问题正文由人通过 `pf watch <wi> --once` 主动查看。
- 唤醒语句固定附带一句："这是需要人做的决定，agent 不得代答"（#122 教训）。
- 进展状态文件由 daemon 写在其数据目录，statusLine / 扩展通过 socket 读取，**不放在 agent 可写的路径**。

**不做**：tmux 分屏（owner 否决）；把进展原样经过 agent（污染上下文、耗 token）。

### 3.3 唤醒适配

| 交互 harness | 唤醒方式 |
|---|---|
| pi | 扩展托管常驻 `pf watch`，决策事件用 `sendMessage(..., {triggerTurn: true})` 直接唤醒 |
| Claude Code | 只能"进程退出即唤醒"：常驻部分负责状态；另起一个轻量等待命令以后台任务挂着，出现决策事件即退出唤醒，agent 处理后重新挂上。Monitor 工具有 30 分钟上限且每行耗 token，不用 |

### 3.4 待验证

- Claude Code 上下文压缩后 SessionStart（`compact`）是否触发（用于补注入提示）。
- `claude --resume` 后 harness session_id 是否不变（决定 chat 映射依据）；pi 的 resume 行为；`/clear`、fork 后的语义。
- pi 事件中 `tool_execution_start.args` 足以提取命令与文件路径。

## 4. 身份模型（已定）

| 概念 | 规则 |
|---|---|
| **daemon**（取代"机器"） | **沿用现有 machine-id 字符串作为 daemon id**（默认 `m-<hostname>`，不生成新值——aihub 注册新 id 需要 admin/maintainer，daemon key 没有该权限，`server.go:1050-1075`）；改为首次启动时写入 daemon 数据目录，之后以数据目录为准（不放 `cli.toml`：会被 install.sh 重写、会被复制到别的机器）；在途 WI 的 `session_machine` 因值不变而不受影响。另有仅供显示的名字（默认主机名）。aihub machines 表/心跳/凭证概念改称 daemon，表结构可后迁 |
| **chat**（原称 session，改名 `PF_CHAT_ID`，避免与 v1.5"session_id = 推送凭证"重名） | 由 **pf 生成**，会话启动时由 Claude Code SessionStart 钩子 / pi 扩展调用 pf 生成，经环境变量 **`PF_CHAT_ID`** 注入每条命令（CC：`CLAUDE_ENV_FILE`，按会话隔离，已核实 `~/.claude/session-env/<id>/`；pi：`tool_call` 钩子改写命令）。模型不经手；上下文只放一句提示，丢失不影响功能。**resume 同一对话 = 同一 chat**（harness 会话 → chat id 映射存于 daemon 数据目录） |
| 隔离 | 每会话独立进程；daemon 派生 step 时清除所有 `PF_*`（`runtime/spawn.go:115`），step agent 拿不到 chat id |
| 性质 | `PF_CHAT_ID` 是"通知谁"的**标签，不是凭证**，不得用于权限判断；不得写入任何共享或 agent 可写的文件 |
| **负责人** | 该 WI 的 **reporter**（创建它的认证用户；交互提交、自动领取与在途 WI 一律如此——aihub 没有比 reporter 更可信的身份来源，D5 裁定）。门范围：负责人可 continue（resume-blocked）与 abandon（cancel）；ForceEnd 等恢复门仍限 admin/maintainer |
| **任务来源 origin** | 每个 WI 的 daemon 侧记录：`chat`（含 chat id、负责人）或 `auto`（仅负责人） |
| 通知路由 | 有订阅 chat → 通知 chat；否则 → 通知负责人（其下次任一会话启动时提示，可选择订阅；将来经 #84 推外部渠道） |

"会话绑定 + adoption（接管 watcher）"整套规则被"负责人（必有）+ 订阅（可选）"取代。

## 5. 执行模式与提交（已定）

- **drain 暂不做**，但架构保留支持：领取与执行分离（已是）；人与任务的交互不绑死在执行机上（依赖 #84 跨 daemon 通信）。
- 形态（**已落地**，分支 `v2/m2-modes`）：`pf ops auto on|off` + `pf ops max N`，自动领取与手动提交并存。`auto off` = 只有显式 `pf submit`；`auto on` = daemon 另外自己拉 aihub 就绪队列（即原 drain 循环，origin 仍为 `auto`）。已删除 interactive/batch/drain 三模式、`abort-drain`、`batch-confirm`、`batch_submit`、`get_recommendation`（只是原样转发就绪队列，改用 `pf queue --ready`）、DAEMON-409「drain 是唯一建 job 入口」拒绝。`status` 头为 `auto: on|off  max: N`；heartbeat 体仍带 mode 字符串（`auto` / `manual`）以兼容 aihub。daemon 旗标 `-auto`（`-mode` 别名与 `daemon-state.json` 旧 `mode` 字段的迁移均已删除，见第 262 行「兼容垫片」：v2 不留向后兼容代码）。drain 作为产品能力仍属「暂不做」范围（跨 daemon 通信 #84 未到），但架构上已可用。
- **名额已满时的提交**：daemon 排队队列**持久化到 daemon 数据目录**（方案 B），重启不丢。重放规则（r2 补）：
  - 条目带 origin 与提交时间；
  - 只重放普通提交；
  - 重放前先查 aihub 状态（已被领取 / 已结束则丢弃）；
  - 与 B1 claiming journal 重放去重，避免同一 WI 双重领取；
  - 被拒一律发决策事件给订阅者 / 负责人。
- `pf submit` 只在本机执行；跨机器使用方式 = 建票进就绪队列由执行方领取（auto 打开后）。

## 6. 恢复与重启

**原则**：session 是唯一的锁；`execution.json` 是唯一的进度账本；推到 aihub git 才算保存；**绝不悄悄从远端重建本地现场**（#103）。

**已定**
- 某 WI 的 capsule 在重启时缺失 / 项目未映射（cli.toml 无该项目的 workspace 映射）：**只该 WI 被置为普通的 aihub `blocked`**——经与其他 blocked 结局相同的 bounded-fail 收尾发 SessionEnd(blocked)，原因备注写明成因（"capsule missing at <path> …" / "project <p> is not in the daemon's [workspace] mapping …"）；**本地不停放任何东西**（无本地 job 记录、无槽位），仍写 `capsule_missing` / `project_unmapped` 事件（`pf watch` 可见）；daemon 照常启动、其余 WI 照常恢复。人（如需）先修复成因（补映射），再 `pf resume <wi>`：这是普通的 blocked 门（resume-blocked + daemon claim）= 从 aihub capsule 链 + task-repo 载体 ref（`refs/pf-task/<name>`）**重新领取**；死现场（如有）被隔离，已完成的步骤不重跑，最后一次推送之后发布的工作丢失。blocked 的两个出口就是 **`pf resume <wi>`（继续）/ `pf cancel <wi>`（放弃）**；`pf wrap --fail <wi>` 不是 blocked 的门，对 parked 工作流不再有任何 hub 门（§6.4 收紧）：daemon 以 DAEMON-409 拒绝并点名这两条路；daemon 无法判定时（本机无 job，DAEMON-404）CLI 只打印这两条路的提示，不调用 hub、不终结任何东西（审计减法 pass B，C9）。session-end 只接受 wrapped|failed|blocked|paused。
- **重启时 aihub 不可达（repo review 2026-10-16 B-2 裁定，取代未成文的 "C6 audit-subtraction" 姿态）**：重启时 SessionVerify 的**传输错误**（连不上、超时、5xx）不是裁决，**不做任何 hub 转换**：保留本地记录（它持有 session 秘钥的唯一副本和尚未推送的 capsule 提交），先回收孤儿 step 进程（单写者），把在跑的 job 登记为 `recovering`（`pf status` 可见；其间 `pf wrap`/`pf pause` 答 DAEMON-409），并在 `goRetry` 退避车道上重试 verify；首个应答走普通对账路径——WI 无需 `pf resume` 即在原 capsule 里续跑，不丢提交。aihub 的 typed 拒绝（4xx）是应答：走原有路径（blocked 并写明原因）。sealing/paused 记录保持其状态，verify 应答后照常对账。关停停止重试，记录持久化，下次重启重新 verify。
- **end_pending 先推链再收尾（同一裁定）**：bounded-fail 在裁决前 commit 脏 capsule；若当时 push 失败（多半正是让 SessionEnd 失败的同一次中断），end_pending 车道的每次重试**先用 session 凭证推送本地链，再发 SessionEnd(blocked)**，push 失败即整体重试；仅当 verify 表明 session 已不再持有该 WI（凭证已死，推不上去）时不带推送收尾。
- **终态关闭的 SessionEnd 失败（同一评审 B-4）**：job 停在 SEALING 时，`goRetry("seal:"+wi)` 在 opMu 下以记录的结论重跑与重启收尾器相同的 `stopOp(alreadySealing)`；typed `STALE_SESSION` 停止重试并本地收尾（job 本地 Done，事件 `seal_finished_locally` 注明 hub 状态以管理员/先前已落地的关闭为准）。
- **不加租约**（保留 v1.5 裁决：领取永不驱逐活着的持有者，接管是单独的特权操作）。改为 WI 级"失联"提示：**WI 失联 = 其持有 daemon 的心跳超时（统一一个阈值，替代 aihub 展示层的 5 分钟与本文早先的 15 分钟两套），或该 WI 从该 daemon 心跳上报的在跑列表中消失**；在 `pf queue`/`pf watch` 提示"可接管"并发事件。
- 同一步失败计数上限（2.3）；driver 异常退出界（2.3）。

**曾经的缺陷（均已修复，保留作回归清单）**
1. 重新领取时克隆失败会退化为本地 init，`execution.json` 丢失，工作流从第 1 步重来（`runtime/runtime.go:1143-1153`）→ 改为本地"待 blocked"，保留现场，**持续重试 SessionEnd**（克隆失败多半因 aihub 不可达，此时 SessionEnd 也会失败，不能假设立刻 blocked 成功）；修复须保留 #108 的 task-ref 拉取（`FetchTaskRef`）。
2. 同机重新领取接上旧 git-dir（`gitops.go:649-656`，ENOTEMPTY 被 `os.IsExist` 视为已存在）。
3. daemon 崩溃后 step 进程及其子进程不回收（`recover.go:534-545`）→ 重启时按 spawn journal 中的 pid 与 `PF_WI_ID` + `PF_DAEMON_ID` 双环境标记（同机另一 daemon 的同 WI 进程不受影响）回收（四层清理对 pid=0 也执行）。
4. 部分 bounded-fail 分支不杀 driver、不 commitDirty（`recover.go:62-75`）。
5. bounded-fail 时 SessionEnd 失败仍删除本地记录 → 孤儿（`recover.go:483-499`）。
6. 正常关停白等 ~60s 的 flush(shutdown)（#136）。
7. 重启后 `pf observe` 只读内存事件环，历史从头开始（由 2.2 的持久事件 + 单调 id 解决）。
8. 重启后无任何"daemon 重启过"的信号给会话（→ 决策事件）。

**事实澄清**：正常关停后 WI 在 aihub 上保持 `running`（session 保留），下次 daemon 启动时恢复。

## 7. 生命周期简化（已定方向）

- "resume"现有 5 种含义 → 对人只保留 **暂停 / 继续**；系统内部自动处理统一称 **恢复（recover）**，不再叫 resume。
- blocked 两扇门（unblock / resume-blocked）→ **继续 / 放弃**两个动作（terminate 门已删，polyforge#149）。
- **暂停后的"继续"（wi_seWTgHeKtaa8）**：`pf resume <wi>`（WI 为 paused）先在 aihub 清 `clean_pause`，再调本机 daemon 的 `POST /jobs/{wi}/resume-paused`：本机 daemon 若持有该 paused 记录，**立即原地继续**——与重启时相同的流程（session verify → SessionAttach → 恢复引擎续跑当前步；未答问题重新提出并等待人回答），不必等 daemon 重启。daemon 回答 `attached`（原地继续）/ `already_live`（已在跑，幂等）/ `closed`（已 attach，但恢复续跑当场把 job 收尾——终态裁决或重拉耗尽——不再原地继续）；拒绝：DAEMON-404（本机不持有，CLI 提示到持有机器上 resume）、DAEMON-409（`clean_pause` 未清 / session 已不持有 / capsule 缺失则置 blocked）、DAEMON-503（aihub 不可达，未继续）。hub 侧已清 flag 而 daemon 拒绝或不可达时，flag 保持清除，持有 daemon 下次启动的重启对账照常 attach。
- **"继续"的范围（r2 补）**：默认由**原持有 daemon 原地继续**；跨 daemon 继续前，必须确认 aihub 上的 capsule 版本等于原 daemon 封存时的版本，否则拒绝（#116 教训：防止拿到 blocked 之前的旧状态）。
- 继续 / 放弃的权限（已定）：**任务负责人、项目 maintainer、admin** 均可；顺带修 resume-blocked 只查 key scope 不查成员。
- WI 依赖关系保留（#120 已解冻）。

## 8. 减法清单

| 档 | 内容 | 状态 |
|---|---|---|
| 一（直接删） | `v0/`（已打 tag `v0-archive`，workflow digest 测试向量已迁入活测试）；根目录 `fakepi` 二进制；旧答案缓存 `observe/answers.go`+`answerfile.go`；`tools/gate-metrics`、`tools/capsule-audit.go` | **集成分支已完成**（`3a036b9`、`90d95bf`、`34fdf86`）；deadcode 函数清扫**已完成**（`EffectiveModel` 属 CORE 模型候选特性，保留） |
| 二（#106 残骸） | flush 车道全套、answer 缓存/ledger、recall/remember 代理路由、step 进程 stdio 控制通道、`PF_DAEMON_SOCKET`（**仅作为 step 环境变量**删除；CLI/daemon 的 `--socket`/`PF_DAEMON_SOCKET` 是用户面配置，保留）、`PF_AGENT_ROLE`（#81）；步骤文本旧协议（#137） | **集成分支已完成**（`34fdf86`、`5f4fc3c`、`4c4564f`）；`agents/` 布局兼容（#136）**未单独核对** |
| 三（改逻辑，随本设计） | supervisor 猜测型车道（hang/idle/probe/wall/turn_duration/工具豁免/问题豁免/ack veto）→ 2.3 规则（**M4 重验保留**）；watch.sh → `pf watch`；三模式 → 第 5 节 | `pf watch`（Go）、watch.sh / watch-probe 删除：**已完成**（`f642e06`、`c01bd94`）；supervisor 车道重写：**已完成**（`internal/polyforge/supervise`、`driver/supervision.go`）；三模式简化：**已完成**（`v2/m2-modes`：`pf ops auto on|off` + `pf ops max N`，守护进程旗标 `-auto`，无兼容别名） |
| 四 | takeover/adopt-session 简化（先验证"结束会话后重新领取能从断点继续"）；心跳保留并修权限漏洞 | takeover 简化：**已验证并完成**——验收测试 `test/v2_m3_end_session_recovery_test.go`（A 完成两步后被 SIGKILL → 管理员强制结束会话 → B 普通领取，从中断步继续、A 的任务仓提交经 `refs/pf-task` 还原、旧密钥写入被拒、A 重启后释放不复活）通过后，删除 `pf takeover`、`pf submit --takeover`、`pf verify-session`、daemon `takeover`/`adopt-session` 路由、`runtime/adopted.go` 托管、aihub `force-takeover` 端点；恢复路径 = 管理员/维护者 `pf ops end-session <wi> --reason ...`（aihub `POST /work-items/{id}/force-end-session`，WI→queued，审计事件 `session_superseded`）+ 任意 daemon 普通 `pf submit`；**M4 重验保留**。心跳权限漏洞：未动 |
| 已完成 | release-intent 门禁（#139，commit a159c8c）；driver 机械保证（needs_input 校验、仅 feature-full grill-review 的 `requires_human_answer`、V4 command_evidence 门禁，`f00b398`）；重新领取两个 bug（git-dir 隔离、克隆失败拒绝 InitLocal / DAEMON-516）；身份模型（daemon-id 文件、`pf chat init`、`PF_CHAT_ID`、origin）；持久化提交队列 + 待修复停放 + `pf resume` 续跑（DAEMON-517；此后已并入 blocked，见第6节）+ `daemon_restarted`/`submit_*` 事件；bounded-fail 杀树 + 推送、end_pending 重试、隔离原子性、继续/放弃走人的 key（`pf resume`/`pf cancel`/`pf wrap --fail`）、`unblock` 动词移除、blocked 封存跨 daemon 校验（DAEMON-518）；待修复 WI 的放弃路径（此后已并入 blocked，见第6节）；孤儿 step 进程回收；`atomicWrite` 修复 | 已完成（`v2/final`）；含孤儿回收的双标记边界（`PF_WI_ID` + `PF_DAEMON_ID`，同机多 daemon 互不误杀）|
| 审计减法 A（polyforge#147） | 结果信封瘦身为最小契约（`protocol_version`/`result_id`/`artifacts` 清单/`handoff_ref` 摘要及 result_id 宽容层全删，§2.6；V4 `command_evidence` 门禁按 owner 裁定保留，仅欠取证的 step 携带）；拒绝重启层删除（3 次进程内重试仍不合格 → blocked `result_invalid`）；未用 frontmatter 键删除（`inputs`/`outputs`+`schema`/`silence_minutes`/`tool_minutes`/`include_verdict`/`on_fail: terminate`）；三个 bundle 的 release step 删除（与 #134 一致；driver 的 release/deploy verdict binding 按 #134 保留，供 `workflows/release` bundle 使用，作用于最小信封的 `output`）；driver 死代码（`Driver.Rewind`/`Terminate`、`Budget.USD`/`Tokens`、stdLogger 兜底、调试输出）删除；T 累计语义归位（`runtime_base_ms` 删）+ 超时类死因放宽翻倍（D6）；限流类死因独立成类、原模型退避重试（D7） | **已完成**（本票；交付清单见 WI capsule 的 `artifacts/code-summary.md`） |
| workflow 结构精简（polyforge#165） | feature-full 11→9 步：`e2e-baseline` 并入 `e2e-author`（作者提交测试后在实现前的树上只跑作者文件、证明逐场景断言级红，同一步内修接线 / 删除已有行为的场景（`already_satisfied`）/ 重写空断言；只有"全部通过"或"环境跑不起来"才 needs_input；新增 `requires_command_evidence`），`e2e-run` 并入 feature-full `verification`（输出 `authored_titles` + `scenarios`；全量 bar 没覆盖作者测试时补一条定向命令进 `test_bar`）；所有交付 bundle 统一为 `verification → review → deliver`（chore 调换顺序）；e2e-author 只写新行为场景、软上限新增 400 行测试，超出填 `size_justification` 由 review 判定（driver 不检查）；review 读完整 diff `git diff $(git merge-base HEAD origin/HEAD)..HEAD`（含 e2e-author / repro 的测试提交），测试错误或空断言 = blocking（code 走 H4 的 needs_input 重新定基线），冗余或超规模 = non-blocking；删除无生产调用方的操作员 rewind（`TransRewind` / `applyRewindLocked`），blocked 只有 `pf resume` / `pf cancel`；加载期校验补 kind 集合与 on_fail（只在 gate 上、目标存在且更早），01 文档 V 表改为只列代码执行的规则（#118） | **已完成**（本票） |
| 删除步骤 kind（polyforge#169） | 步骤 `kind` 概念整体删除：bundle frontmatter 的 `kind` 字段、`KindVocabulary`/`ValidKind`、driver 的 Kind 字段与 #165 的 kind 校验；`.polyforge.yaml` 改为 `steps.<project>.<default|step-id>.models`（每个条目目前只允许 `models` 键，#159 再加 `harness`；顶层 `models:` 不兼容、拒绝），拼写保护改为"键必须是已安装 workflows 的步骤 id"（DAEMON-414 拒绝认领 + `pf doctor` 报告）；旧 bundle 快照里遗留的 `kind:` 被宽松解析忽略，同 daemon 恢复不受影响；迁移对照表见 `docs/upgrade.md` | **已完成**（本票） |
| 迁移代码 | v0→v1 迁移（`migrate-memories`/`migrate-work-items`、`core/import.go`、`tools/v0export`、`seed-v1.sh` 等）：owner 裁定提前删除，不再设观察期 | **已完成**（`v2/m1-migrate`） |
| 兼容垫片 | owner 裁定 v2 不留向后兼容代码：`-mode` 别名、`daemon-state.json` 的 `mode` 迁移、`pf observe` 别名、`pf unblock` 提示桩、aihub `unblock` 端点与 `WIManager.Unblock`、`[watch.*]`/`watch_*` 配置容忍、`project_scope` 别名、对遗留 `takeover` 队列项的专名处理 | **已完成**（`v2/final`）；配置加载现对未知 section（含 `[watch.*]`）报错，未知顶层平铺键同样报错（含已移除的 `url` 别名；`533e032`） |

## 9. 快速落地顺序

不做新旧并行观察，按依赖关系直接替换：

1. **止血**：去掉 release-intent 门禁（已完成待部署）；修步骤文本旧协议（#137）；driver 人工确认保证（#138）；**重新领取路径的两个 bug（第 6 节 1、2）**——已暂停的 #130、#134 恢复依赖它们，修复前不要 resume。
   - git-dir 修法：隔离 capsule 时把外置 git-dir 一并移入隔离区；目标已存在时报错而非复用（含跨文件系统 EXDEV 合并情形）；加"同机隔离后重新领取 → HEAD 等于 aihub 最新提交"的测试。
   - **V4 验证证据门禁移入 driver**（已定）：driver 接受 verification 步骤的 completed 结果时，要求结构化命令证据（argv、exit_code、duration_ms），缺失则走信封拒绝的进程内重试；旧 flush 路径上的同名检查随 #136 删除。
   - 步骤 1 完成后：**验证"结束会话 + 重新领取能从断点继续"**，通过则按第 8 节四档简化 takeover / adopt-session / verify-session（**已完成**，见第 8 节四档）。
2. **事件底座**：driver 事件写入事件日志；单调 id、轮转；`execution.json` 权威。
3. **监督替换（同一批上线）**：driver ↔ supervisor 契约 + 判死规则 + 死因处理 + 失败计数 + 阈值 + 恢复引擎的 driver 异常退出界 + **孤儿 step 进程回收（第 6 节 bug 3，必须同批——否则"重启后同模型重派"会与孤儿进程双写同一 capsule，#99 教训）**。
4. **`pf watch`**：Go 实现、双通道、固定模板唤醒、订阅/游标存 daemon、Claude Code statusLine 与唤醒适配、pi 扩展；删除 watch.sh / watch-probe。
5. **身份模型**：daemon id 存放迁移、`PF_CHAT_ID`、负责人、origin、通知路由。
6. **恢复与重启其余 bug**（第 6 节 4、5、6、8）+ 坏任务置 blocked 后 `pf resume` 重新领取（原"待修复"原地续跑，此后已并入 blocked，见第6节） + 持久化排队队列（含重放规则）+ "继续"范围校验。
7. **减法一、二档**（可与 2–6 并行）。
8. 之后：权限隔离（#135）、按步骤选 harness、drain + 跨 daemon 通信（#84）。

## 10. 现有 WI 映射

| WI | 去向 |
|---|---|
| #119 | 已取消（本文即其产出） |
| #102 | 已取消（残留） |
| #87、#88 | 被已失联的 `cli-opencode` 持有自 10-06；接管路径（`pf ops end-session <wi> --reason ...` + 任意 daemon 普通 `pf submit`）已落地，**现在可清理** |
| #122 | 由 #137/#138/#135 与 3.2 的唤醒约束修复后复现验证关闭 |
| #139 | 由门禁删除分支解决（已落在集成分支） |
| #136、#137、#138 | 归入第 9 节 1、7；集成分支已落：#137（步骤文本，`5f4fc3c`）、#138（driver 人工确认保证，`f00b398`）、#136 的 flush/stdio/`PF_AGENT_ROLE` 部分（`34fdf86`）；`agents/` 布局兼容未单独核对 |
| #135 | 第 9 节 8；在其之前由 3.2 的安全约束兜底（见文末「已知缺口」(b)） |
| #84 | 重要性上升：drain 与跨机器回答的前提；仍冻结，范围待改写 |
| #120 | 已解冻 |
| #130（执行 #126）、#134（执行 #133） | owner 已于 2026-10-12 暂停（paused，进度已推到 aihub）；重新领取 bug 已在集成分支修复（git-dir 隔离、克隆失败 DAEMON-516），**部署后**再 resume；#134 的 **epic/task 概念保留**（已定） |
| #71、#73、#74、#118、#132、#133 | 不受影响 |
| #78、#121、#125、#126、#128 | 继续冻结 |

## 11. owner 裁决（2026-10-12，原待定项全部已定）

1. **V4 验证证据门禁**：移入 driver（第 9 节步骤 1）。
2. **`PF_AGENT_ROLE`**：删除（第 8 节二档）。
3. **历史设计文档**：00–08、v1-*、v1.5-* 开头加注"部分内容已被本文取代，冲突时以本文为准"，不逐篇改写。
4. **takeover 简化**：**已完成**（先验证"结束会话 + 重新领取能从断点继续"，通过后删除 takeover；见第 8 节四档）。
5. **#87、#88**：保留至"失联提示 + 接管"落地后再清理；接管（`pf ops end-session`）已落地，现可清理。
6. **epic/task 概念（#133/#134）**：保留。
7. **继续 / 放弃权限**：任务负责人、项目 maintainer、admin。

## 12. polyforge#140 裁决（2026-10-13，拆分 a：wi-kind 契约——父子与 bundle 规则）

拆分 a（aihub 端 + 设计文档）落地 grill-review 的调整 1–6（w140 裁决记录；拆分 b 承接客户端面：CLI/skills/daemon submit 预检）。接口细节（字段、reason 清单、响应形状、部署与回滚）权威在 `wi-kind-contract.md`；本节登记裁决本身：

1. **epic 由数据派生，不落列**：不引入 `work_items.kind` 列、无迁移、无 PARENT_NOT_EPIC/EPIC_NO_BUNDLE 等新错误码；有子票（`parent_id` 指向它）即 epic，可选 `epic` label 仅展示，服务端不据此强制任何行为。
2. **父子规则对称化**：两条对称规则取代旧“父票必须 epic”的单向表述——①有子票的票不可绑定 bundle（create 与 PATCH 共用同一校验路径，`CORE-400`，`reason=HAS_CHILDREN_NO_BUNDLE`）；②已绑 bundle 的票不可作父票（沿用 `PARENT_NOT_REGISTRY` 语义，不留旧名别名）。bundle 只落在叶子上，epic/父票永远是登记票。
3. **R2 窗口**：`bundle_name` 的固化时点从“建票即冻结”改为**首次认领冻结**（R2 窗口：status=queued ∧ 从未认领；waiting_on 非空不单独拒绝）；窗口内 `PATCH bundle_name` 三向变更（NULL→X / X→Y / X→NULL 解绑），与 create 共用同一落库路径（引用即名字本身——digest 链已删，polyforge#149 B9：不再派生 stand-in，也不接受显式 digest）；`repo_inventory` 不触碰。
4. **待选 bundle 分段**：ready queue 新增“待选 bundle”分段——bundle 为空 ∧ 无子票 ∧ queued 的票，与可执行分段同样排除 waiting_on 非空与依赖未完成的票；epic 不出现在任何分段；响应形状只做加法（不改名不删减旧字段）。
5. **父票完成状态由子票派生、不写终态**：全部子票终态且 ≥1 wrapped → 派生“完成”，否则“进行中”（附子票计数：总数/已终态/wrapped）；父票库内 status 保持 queued，任何终态不写；新增子票自动回到“进行中”（纯读派生，无重开写路径）；暴露只做加法。
6. **执行中发现的问题不挂子票**（本票及后续执行期行为约束）：范围内→顺手修（改动重新声明进 touched files）；越界不阻塞→新票 + `related`；阻塞→新票 + `blocks` 依赖；方向错误→`wrap --fail`/`cancel` 后新开 task。四分支均不向正在执行的票挂子票。

**并发（两组，均有测试覆盖）**：只靠单写者事务串行化加各自的 CAS，不另设仲裁（不用内存闩、不用定时等待）。PATCH bundle vs Claim：绑定或改名的 PATCH 不会让 Claim 失败（解绑若排在前，Claim 按无 bundle 票被拒）；PATCH 排在 Claim 之前则 Claim 冻结新 bundle，排在之后则被拒且不写库（`CORE-400 reason=IMMUTABLE`，或 `CORE-409 reason=RACE_LOST`）——两种次序的终态都正确。绑定 bundle vs 新增子票：恰好一方成功，终态不同时出现“有子票 ∧ bundle 非空”。

## 13. 审计裁定（2026-10-13，polyforge#150：文档对齐——以代码为准）

来源：`audit-2026-10-13-invented-logic.md` §三/§七 + owner 同日补登（claim 幂等记录保留期），划归本票的全部 11 条裁定集中登记于此。每条一行：编号、一句话裁定、现文档落点（本次修订均已落盘）。

| # | 裁定（一句话） | 文档落点 |
|---|---|---|
| D1 | 就绪队列（`GET /api/v1/projects/{project}/ready-queue`）响应为 `{items, running, paused, pending_bundles}` 四段（`internal/aihub/core/queue.go`）；无 `blocked` 数组，等依赖的票不出现 | `02-aihub-core.md#§5.1b`（重写）及 §5.1 行 4 / §4.2a 行 26 / §7 / §10 关联句；`00` §3.1 组件表、§3.4b/§3.4c 与 `07` §9.2 同步最小修订 |
| D2 | `seq` 项目内唯一（`UNIQUE(project_id, seq)`），非全局 | `02-aihub-core.md#§4.2` DDL |
| D4 | claim 需该 project 的 writer/maintainer membership 或全局 admin（`hSessionClaim`→`requireWrite`，403 otherwise）；key scope 不构成认领授权 | `02-aihub-core.md#§4.10`、`#§8`；`04` §6.1 关联句 |
| D8 | 先推 capsule 链，再推任务仓库 carrier ref（`refs/pf-task/<name>`）；任务仓库推送失败不回滚 capsule 链（polyforge#158 起另发 `carrier_push_failed` 决策提醒，见 `05` §7.5） | `v1-spec-full.md`（单行最小修订）+ 本表登记（“或”：两处都做） |
| D10 | watcher 不因订阅的 WI 结束而退出（保持常驻），只跳过已结束的 WI；父 harness 消失、stdout 关闭或 `--wait-decision` 打印后仍退出 | 本文 §3.1 |
| D14 | `paused` 的 WI 保留会话，可直接 wrap/fail/block，无需先 resume（`End()` 同一 session-hash fence，通过后清空会话） | `02-aihub-core.md#§2.5` 表行与相邻文字、§5.1 行 9 |
| C5 | `context_policy` / `handoff` 保留并写入文档 | **已由 #147 落地**，见 `01-workflow-skills.md#§4`/`§4.4a` 与本文 §2.6（本票不重复修改） |
| C11 | job 级收尾前活转录同步保留（`runtime/pitranscript.go`） | 本文 §2.2 |
| C13 | 决策类别补 `canceled`/`paused`/`submit_dropped`（另含 `submit_rejected`/`daemon_restarted`，与 `watcher/decision.go` 代码表对齐） | 本文 §3.1 |
| C16 | `pf file create` 在 cwd 恰落一个映射 workspace 且该 workspace 恰映射一个 project 时推断 `--project`；其余一切情形要求显式 | `00-architecture.md#§3.1` |
| 补登 | claim 幂等记录永不过期、无任何清理任务（缓存应答只含公开 fingerprint，永久保留无安全问题） | `02-aihub-core.md#§4.3`、`#§6.6` |

其余审计条目（B*、C1–C4、C6–C10、C12、C14–C15、D3/D5–D7/D9/D11–D13/D15 等）属其他票（#144/#145/#146/#147 等），不在本票范围。

---

## 附录 A：r2 深度审查漏洞清单与处置

| # | 严重度 | 漏洞 | 处置（本文位置） |
|---|---|---|---|
| 1 | 高 | 退出未交结果一律同模型重试（F4 回归），"机器问题"无来源 | 2.3 细分表；2.5 翻译器上报进程退出 |
| 2 | 高 | 一次派发多次判定、kill 后再计一次失败；多规则优先级未定义 | 2.2 围栏与幂等；2.3 规则按编号优先 |
| 3 | 高 | T 作用域矛盾，放宽值无处持久化 | 2.2、2.4：步骤累计、写入 execution.json |
| 4 | 高 | 失败计数未定义 | 2.3 失败计数 |
| 5 | 高 | driver 异常退出无人管、重启不计失败致无界循环、M4 无归属 | 2.1 恢复引擎；2.3 driver 异常退出界；8 三档保留 M4 |
| 6 | 高 | 事件日志 best-effort、seq 不跨重启 | 2.2 execution.json 权威 + 单调 id；3.1 重连先核对状态 |
| 7 | 高 | daemon id 新生成会被 aihub 拒绝注册 | 4：沿用 machine-id 字符串 |
| 8 | 高 | 孤儿回收晚于"重启同模型重派"（#99 回归） | 9：同批上线 |
| 9 | 高 | 坏任务 blocked 后继续会重新克隆（#103 回归）；克隆失败时 SessionEnd 也会失败 | 6：置 blocked + `pf resume` 重新领取（原待修复原地续跑，此后已并入 blocked，见第6节）；bug 1 持续重试 |
| 10 | 高 | watch 状态/唤醒/映射/队列文件 agent 可写 | 3.1、3.2、4、5：存 daemon 数据目录、固定模板 |
| 11 | 中 | 工具超时重试不放宽（#83 回归）；阈值受 bundle 钉住 | 2.3 tool_hung 放宽；2.4 覆盖对之后领取的 WI 生效 |
| 12 | 中 | 零事件只能靠 T 兜底（#114 退化） | 2.3 规则 ⓪ |
| 13 | 中 | 重试形态未定义 | 2.3 重试形态 |
| 14 | 中 | 持久化队列重放与 aihub 分歧、takeover 重放抢占 | 5 重放规则 |
| 15 | 中 | 步骤之间卡住无人管 | 2.3 driver_stalled |
| 16 | 中 | watch 生命周期、注册竞态 | 3.1 |
| 17 | 中 | 负责人未定义 | 4 负责人 |
| 18 | 中 | 候选用尽语义 | 2.3 候选用尽 |
| 19 | 中 | 阻塞投递与非阻塞循环冲突 | 2.2 无界队列、不在 opMu 下做长操作 |
| 20 | 中 | T 与 `budget.wall_minutes` 关系、命名撞车 | 2.4 复用 wall_minutes；改名 step_timeout_* |
| 21 | 中 | 跨机继续拿到 blocked 前旧状态（#116 回归） | 7 继续范围 |
| 22 | 中 | 继续/放弃角色未定 | 11 第 7 条 |
| 23 | 低 | PF_SESSION_ID 与 session_id 凭证重名 | 4 改名 PF_CHAT_ID |
| 24 | 低 | 转录同步来源未写明 | 2.2 |
| 25 | 低 | 失联阈值两套、WI 级失联定义 | 6 |

## 附录 B：历史问题回归对照

| 历史问题 | 新方案覆盖机制 |
|---|---|
| #44 agent 死无人知 | driver 退出检测 + ②③④ |
| #48 活动量误判、incident 风暴 | "真实输出"定义 + 失败计数上限 |
| #83 长工具被误杀 | ③ 默认 30 分钟 + 重试放宽 Y |
| #97 快速死亡死循环 | 恢复引擎 driver 异常退出界（连续恢复失败计数器） |
| #99 等人期间假卡死 | 等人期间无进程、不计时 |
| #99 双写者 | 孤儿回收与监督替换同批 |
| #103 绝不悄悄重建 | 坏任务置 blocked，人显式 `pf resume` 才重新领取（原"待修复"原地续跑，此后已并入 blocked，见第6节） |
| #108 task-repo 持久 | 保留 FetchTaskRef |
| #114 探针洗白 / 零事件 | 无探针 + 规则 ⓪ + ④ |
| #116 blocked 前状态 | 继续范围校验 |
| #117/#124 trickle-wedge | ④ 步骤累计 T；状态按派发、不跨步 |
| #122 agent 自答 | #138 + 唤醒固定附带"不得代答" + 唤醒不夹带自由文本 |
| F3 转录 | step_started.transcript_path + commit 前同步 |
| F4 模型 pin 失败 | 退出细分：模型类 → 换候选 |
| F5 致命错误后永久 idle | ① 1 分钟宽限 |
| F13 上下文超限 | 第二次起重开对话 |
| F15 UI 噪声掩盖卡死 | "真实输出"定义 |
| F16/B2 僵尸 flush | spawn_n 围栏 |
| F18/F22 推送风暴 | 事件触发推送保留退避与熔断 |
| B1 领取日志 | 不变；与持久化队列重放去重 |
| M4 僵尸会话发现 | 保留在 supervisor |
| NEW-1/2/3 | 指纹围栏、判定不丢、待关闭回放不变 |
| #92 孙进程持管道 | 组杀不变 |
| #107 信封拒绝 | 进程内重试不变，不计入失败计数 |
| watch 四次修补 | Go 实现、不调模型、进展不进上下文 |

---

## 附录 C：已知缺口（集成分支，2026-10-12）

- **(a) bundle digest 描述与代码不一致**：01 §4.5 描述的是 RFC 8785 canonical-JSON bundle digest，但在线的 `Digest` 实现是"路径 + sha + 长度"拼接。**以代码为准**（`internal/common/workflow`，golden 向量在活测试中），01 §4.5 待更正。
- **(b) 人工确认保证只约束诚实的 agent，直到 #135 落地**：driver 的 `requires_human_answer` 与 needs_input 校验是进程内的机械保证，但不可信的 step agent 仍可写 capsule，从而绕过。daemon 的 UDS 面（polyforge#122 起于 answer 路由，#164 并入；repo review 2026-10-16 V-1 扩到全部变更路由）按 UDS 对端 SO_PEERCRED pid 拒绝来自本 daemon step 进程树的一切非读请求（DAEMON-403）：对端、其祖先或其进程组 leader 带有 `PF_WI_ID` + `PF_DAEMON_ID=<本 daemon>` 标记即拒；取不到对端凭据也拒（fail closed）。拒绝面是一个包住整张路由表的中间件：`/jobs/{wi}/answer|claim|resume-blocked|resume-paused|pause|wrap`、`/ops/config`、`/shutdown`、`/chat/init`、`/chats/{id}/subscribe|unsubscribe|ack|heartbeat`；开放的只有读：`GET /status`、`/jobs/{wi}/watch`、`/chats/{id}/subscriptions`、`/chats/{id}/attention`。answer 路由保留其 #122 拒绝文案。残余：`setsid` 脱离进程组且清空环境的进程不在此检查内。真正的隔离仍是 #135（第 9 节 8）。（曾有的 abandon-repair 路由随 repair_pending 一并删除，此实例已不存在。）
- **(c) `pf watch` 进展字段**：已随监督重写合入——daemon watch 路由从 `Observe.Progress(wi)` 填充命令、命令时长、最近文件、工具调用数（见附录 D）。

## 附录 D：监督替换落地对照（第 9 节步骤 2–3，分支 `v2/a-supervision`）

**代码位置**
- 契约、翻译器、判死规则：`internal/polyforge/supervise`（`contract.go` 事实/判定/无界队列/每 WI 的 `Link`；`translator.go` + `pi.go` 翻译器接口与 pi 实现；`tracker.go` 每次派发一份的合并状态；`rules.go` 规则 ⓪–④ 的纯函数 `Judge`；`defaults.go` 全局阈值）。
- driver 一侧：`internal/polyforge/driver/supervision.go`（判定围栏、死因表、失败计数、放宽、事实发送）；`TransStepFailed` 在 `transition.go`，一次原子写入记录 attempt（带死因子类）、计数、放宽值、下一次派发形态，必要时写 blocked。
- supervisor 一侧：`internal/polyforge/observe/monitor.go`（消费事实、每派发一份状态、判定、`driver_stalled`、事件触发推送、M4 保留）；事件底座 `observe/events.go`（逐行带 id、按大小轮转、重启后续号、历史从文件读）。
- 失败计数等持久字段：`capsule.Step.Supervision`、`Attempt.FailureCause`、`Execution.BlockedReason`。

**规则 ① 与 T 的后续修订（集成分支）**：规则 ① 扩展到正常结束的轮次（`ended_without_result`，见 2.3 表）；T 取 `max(3 × wall_minutes, 全局默认)`（见 2.4）。

**阈值配置名**：旗标 `--silence-window`（X）、`--tool-window`（Y）、`--step-timeout`（T）、`--stall-window`（N）、`--model-error-grace`（规则 ① 宽限）；step 级覆盖在 workflow.md 只剩 `budget.wall_minutes`（T）——polyforge#147 C5 删除了从未被 bundle 使用的 `silence_minutes` / `tool_minutes` 键，X、Y 只有全局默认。旧旗标 `--hang-window/--idle-window/--probe-timeout/--wall-window` 已删，传入即解析失败。

**与正文的实现差异（均为补充，不改契约语义）**
- 事实集合多一条 `exec_committed{version}`：事件触发推送需要"driver 提交了新 Version"的提示，否则只能回到轮询 `execution.json`。
- 事实/判定走 `supervise.Link`（进程内）；driver 与 supervisor 之外的"harness 海量输出"由翻译器直接折叠进每派发一份的 `Tracker`（只保留最新状态，不排队）。
- `driver_stalled`：supervisor 同时向 driver 发判定，并经 `OBNeedsRecovery(reason=driver_stalled)` 交给恢复引擎——卡死的 driver 无法自己消费判定；恢复引擎杀树并重拉，计入连续恢复失败计数器。当前 driver 自身不做 git/推送（推送在 supervisor 的推送环里，受 60s 单次上界约束），所以该规则管的是"driver 在步骤之间的任何卡死"。
- 前一次 transcript 作为"只读参考"仅在 capsule 里读得到时才附上（capsule 内的转录靠提交边界同步，进程刚死时可能还没有）；声明的 resume 路径不可读时，resume 退化为 fresh。
