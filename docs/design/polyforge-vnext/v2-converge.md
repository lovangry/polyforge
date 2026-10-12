# v2 收敛设计：减法、监督重构、watch 与身份模型

> 状态：**r3，owner 裁决已全部纳入，待审阅**（2026-10-12）。
> 来源：2026-10-12 owner 与 review 会话的完整讨论；r2 吸收了一次独立深度审查（25 个漏洞 + 历史回归对照，附录 A/B），owner 已同意其 18 条修补默认值。
> 本文取代的范围：04 §6.2 监督车道、v1.5-session-model 中 watch/adoption 相关描述、`skills/polyforge/scripts/watch.sh`。
> 落地进度（集成分支 `v2/converge`）：见第 8 节状态列、第 10 节映射与文末「已知缺口」；监督重写（第 2 节、`pf watch` 进展字段）在另一分支进行。
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
| 主线 | 安装与升级 | install.sh、发版、aihub 部署、`pf upgrade`（#130） |
| 主线 | 团队与权限 | 用户、key、项目、成员角色、agent 信任边界 |
| 主线 | 任务管理与生命周期 | WI 状态机、依赖、就绪队列、领取（session 锁）、blocked 出口 |
| 主线 | 记忆 | 存储与检索、recall/remember、领取时快照 |
| 主线 | workflow 定义 | bundle、step、gate/rewind、按步骤选模型（将来：harness） |
| 主线 | 执行 | daemon、capsule、driver、并发、工作区映射 |
| 主线 | 交付 | ship → git 分支 / PR |
| 横切 | 人工协同 | 提问/回答、暂停/继续、进展可见（`pf watch`） |
| 横切 | 可靠性 | 监督（判死）、恢复、session 隔离、接管 |
| 横切 | 运维 | doctor、backup、ops status、事件日志 |

## 2. 执行与监督

### 2.1 分工（已定）

| 组件 | 职责 | 不做什么 |
|---|---|---|
| **driver** | 执行 WI：按步骤派发 step 进程、收结果文件、推进 `execution.json`、按死因处理、维护失败计数 | 不判断"活着但不推进" |
| **supervisor**（原 observe 组件，改名以免与命令混淆） | 观察执行动态、**判断 step 是否已死**、判断 **driver 是否卡在步骤之间**、M4 僵尸会话重验、记录事件、推送进度到 aihub、提供查询接口 | 不推进执行 |
| **恢复引擎**（runtime） | driver 自身异常退出后的重拉；保留 storm guard 语义（短时间多次崩溃 → blocked） | 不判 step 死活 |
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
- 转录同步（F3）以最近一次 `step_started.transcript_path` 为源，commit 前同步。

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
| ① | 本轮结束后停住 | harness 发出"本轮结束"，宽限 1 分钟仍无结果文件：原因出错 → `model_error`（换候选）；正常结束 → `ended_without_result`（同模型重试） | `model_error` / `ended_without_result` |
| ② | 等模型太久 | 在等模型回复，超过 X 无任何输出 | `model_hung` |
| ③ | 工具太久 | 某工具超过 Y 未结束 | `tool_hung` |
| ④ | 总时长超 | 本步**累计**运行超过 T | `step_timeout_long_task`（主要耗在工具上）或 `step_timeout_slow_model`（主要耗在等模型 / 单次回复异常慢） |
| — | 步骤之间卡住 | driver 不在任何 step、也不在等人，超过 N 分钟（如推送 git 子进程卡死仍持锁） | `driver_stalled` |

等人回答期间没有 step 进程，**不计时**。

**"退出未交结果"的细分（driver 自己发现，r2 补）**

| 细分 | 判据 | 处理 |
|---|---|---|
| 模型类 | 退出前最后信号为"本轮出错"，或退出码 / 错误文本命中模型类特征（模型名不可解析、401/鉴权失败、配额等） | 同 `model_error`：换模型（F4 教训：模型 pin 失败必须推进候选） |
| 机器类 | driver 自身 I/O 错误（ENOSPC、EACCES 等）或翻译器上报的宿主错误 | 机器问题 |
| 其余 | — | 同模型重试 |

**driver 按死因处理（已定）**

| 死因 | 处理 |
|---|---|
| 退出未交结果（其余） | 同模型重试 |
| `harness_silent` | 同模型重试 |
| `ended_without_result` | 同模型重试；重试的 prompt 明确告知"上一轮已结束但没有写结果信封"。进程在正常结束一轮后以退出码 0 自行退出且无结果，也归此类（被杀 / 崩溃仍是退出其余） |
| `model_error`、`model_hung`、`step_timeout_slow_model`、退出（模型类） | 换下一个候选模型 |
| `tool_hung` | 同模型重试，并**放宽 Y**（同 T 的放宽规则） |
| `step_timeout_long_task` | 同模型重试并放宽 T；再超时 → blocked |
| 机器问题 | blocked，通知人，不换模型 |
| daemon 重启 | 同模型重新派发，**不计失败** |
| `driver_stalled` | 终止当前 git/推送子进程，driver 重拉（计入 driver 异常退出界） |
| driver 自身异常退出 | 恢复引擎重拉；**短时间多次（保留 storm guard 语义）→ blocked**——防止"崩溃 → 重启不计失败 → 再崩溃"无界循环（#97 教训） |
| 同一步失败计数达上限（默认 3） | blocked，通知人 |

**候选用尽**：指针停在末尾，后续"换模型"按同模型重试计数；决策事件注明"候选已用尽"。

**重试形态**：第一次重试接着原对话（resume 原 transcript）；第二次起重开对话（fresh），把上次 transcript 作为只读参考输入——避免上下文超限类错误在任何模型上都重复失败（F13 一族）。

**失败计数（r2 补）**
- 存放：`execution.json` 该 step 的字段（attempts 记录带 cause 子类）。
- 计入：上表除"daemon 重启"外的所有死因。**信封被拒（#107 进程内重试，独立上限）不计入。**
- 清零：gate rewind 回到该步、换到新候选模型、人工"继续"。
- daemon 重启触发的中断**不归档为失败、不推进候选指针**（修正现状：`ClassifyFault` 默认 provider 导致重启切模型，`driver/candidates.go:201`）。
- `PolicyFor` 中 host→block、schema→report 当前未接入生产，按上表落实。

### 2.4 阈值（已定）

- X（⓪②）、Y（③）、T（④）、N（driver_stalled）**全局默认，step 可覆盖**。
- **T 的 step 级覆盖复用现有 `budget.wall_minutes`**，但它是**估计值而非硬上限**（bundle 里现有的 15–30 分钟都是估计）：声明了 `wall_minutes` 的 step，总时长界 **T = max(3 × wall_minutes, 全局 `--step-timeout` 默认)**；未声明则就是全局默认。不新增键；X、Y 的 step 级覆盖在 frontmatter 新增。覆盖值随 bundle 在领取时钉住，对之后领取的 WI 生效。
- X 默认按 harness 翻译器声明的**输出粒度**取：逐字粒度（思考时也有输出）可短（例 10 分钟）；整条粒度或模型不返回思考内容时放长（例 30 分钟）。"模型不返回思考内容"在模型配置中声明。
- Y 默认 30 分钟（#83 教训）；T 默认 60–90 分钟（#117 教训）；N 默认 10 分钟。
- **放宽**：每次放宽翻倍，写入 `execution.json`，重启后沿用。
- T 按**整个步骤累计**（跨该步的多次派发），不是每次派发重置。

### 2.5 harness 翻译器

每个 harness 一个翻译器，把原生事件翻成标准信号：活着、在等模型、模型在输出（思考/正文）、工具开始（名称+参数）、工具结束、本轮结束（成功/出错）、**进程退出（退出码、最后一条错误）**。supervisor 只认标准信号。**这也是"按步骤选 harness"的接入点**。

| harness | 对接方式 | 思考时有输出 | 核实来源 |
|---|---|---|---|
| pi | `--mode rpc` | ✅ `thinking_delta` | 本机源码 `agent-loop.js` |
| Claude Code | `-p --output-format stream-json --verbose --include-partial-messages`（由 driver 一律加上） | ✅ | 2.1.296 二进制事件名 |
| Codex | **`codex app-server`**（JSON-RPC），**不用 `exec --json`** | ✅ `item/reasoning/textDelta` 等 | 0.162.1 源码；`exec --json` 会过滤掉回复/思考的开始事件且无增量 |

第一版只实现 pi 翻译器；Claude Code / Codex 随"按步骤选 harness"落地，接入时按当时最新版本复核协议。

## 3. 人工协同：`pf watch`（已定）

### 3.1 形态

- Go 实现的 `pf watch`，**取代 `watch.sh` 与 `pf watch-probe`**；`pf observe` 并入为 `pf watch <wi> --once`。
- `pf watch --chat <id>`（缺省取 `PF_CHAT_ID`）常驻到其所订阅的 WI 全部结束；一个会话一个进程。
- 只产出两种信号：
  - **进展**：当前 step、已运行时长、正在执行的命令及时长、最近修改的文件、工具调用计数（由翻译器信号机械生成，**不调用模型**）。
  - **决策事件**：有问题要回答/gate、失败、blocked、完成、被判死并已处理、候选已用尽。
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
| **负责人** | 交互提交的 WI：提交人（`PF_USER_ID`）；自动领取的 WI：reporter；无来源记录的在途 WI：reporter |
| **任务来源 origin** | 每个 WI 的 daemon 侧记录：`chat`（含 chat id、负责人）或 `auto`（仅负责人） |
| 通知路由 | 有订阅 chat → 通知 chat；否则 → 通知负责人（其下次任一会话启动时提示，可选择订阅；将来经 #84 推外部渠道） |

"会话绑定 + adoption（接管 watcher）"整套规则被"负责人（必有）+ 订阅（可选）"取代。

## 5. 执行模式与提交（已定）

- **drain 暂不做**，但架构保留支持：领取与执行分离（已是）；人与任务的交互不绑死在执行机上（依赖 #84 跨 daemon 通信）。
- 形态（**已落地**，分支 `v2/m2-modes`）：`pf ops auto on|off` + `pf ops max N`，自动领取与手动提交并存。`auto off` = 只有显式 `pf submit`；`auto on` = daemon 另外自己拉 aihub 就绪队列（即原 drain 循环，origin 仍为 `auto`）。已删除 interactive/batch/drain 三模式、`abort-drain`、`batch-confirm`、`batch_submit`、`get_recommendation`（只是原样转发就绪队列，改用 `pf queue --ready`）、DAEMON-409「drain 是唯一建 job 入口」拒绝。`status` 头为 `auto: on|off  max: N`；heartbeat 体仍带 mode 字符串（`auto` / `manual`）以兼容 aihub。daemon 旗标 `-auto`（`-mode interactive|drain` 作一版弃用别名，打印告警）；`daemon-state.json` 的旧 `mode` 字段加载时迁移（drain → auto on，其余 off），下次持久化改写为 `auto`。drain 作为产品能力仍属「暂不做」范围（跨 daemon 通信 #84 未到），但架构上已可用。
- **名额已满时的提交**：daemon 排队队列**持久化到 daemon 数据目录**（方案 B），重启不丢。重放规则（r2 补）：
  - 条目带 origin 与提交时间；
  - **只重放普通提交，不重放 takeover**；
  - 重放前先查 aihub 状态（已被领取 / 已结束则丢弃）；
  - 与 B1 claiming journal 重放去重，避免同一 WI 双重领取；
  - 被拒一律发决策事件给订阅者 / 负责人。
- `pf submit` 只在本机执行；跨机器使用方式 = 建票进就绪队列由执行方领取（auto 打开后）。

## 6. 恢复与重启

**原则**：session 是唯一的锁；`execution.json` 是唯一的进度账本；推到 aihub git 才算保存；**绝不悄悄从远端重建本地现场**（#103）。

**已定**
- 某 WI 的 capsule 在重启时缺失 / 项目未映射：**只该 WI 进入"待修复"**——本地标记、**session 保留**、写明原因并发决策事件；daemon 照常启动、其余 WI 照常恢复。人恢复目录 / 补映射后，由**同一 session 原地续跑**（不走重新领取，避免隔离 + 克隆把人刚恢复的本地现场丢掉）。人也可选择"放弃"：`pf cancel <wi>` / `pf wrap --fail <wi>` 先用**人自己的 key** 调 aihub 只读授权探针（`GET /work-items/{id}/close-auth`：负责人 / maintainer / admin），再由 daemon 用保存的 session secret 结束 session（`canceled` / `failed`；失败走 end_pending 持续重试），丢弃待修复记录并写 `repair_abandoned` 事件。
- **不加租约**（保留 v1.5 裁决：领取永不驱逐活着的持有者，接管是单独的特权操作）。改为 WI 级"失联"提示：**WI 失联 = 其持有 daemon 的心跳超时（统一一个阈值，替代 aihub 展示层的 5 分钟与本文早先的 15 分钟两套），或该 WI 从该 daemon 心跳上报的在跑列表中消失**；在 `pf queue`/`pf watch` 提示"可接管"并发事件。
- 同一步失败计数上限（2.3）；driver 异常退出界（2.3）。

**待修 bug（已核实或读码得出）**
1. 重新领取时克隆失败会退化为本地 init，`execution.json` 丢失，工作流从第 1 步重来（`runtime/runtime.go:1143-1153`）→ 改为本地"待 blocked"，保留现场，**持续重试 SessionEnd**（克隆失败多半因 aihub 不可达，此时 SessionEnd 也会失败，不能假设立刻 blocked 成功）；修复须保留 #108 的 task-ref 拉取（`FetchTaskRef`）。
2. 同机重新领取接上旧 git-dir（`gitops.go:649-656`，ENOTEMPTY 被 `os.IsExist` 视为已存在）。
3. daemon 崩溃后 step 进程及其子进程不回收（`recover.go:534-545`）→ 重启时按 spawn journal 中的 pid 与 `PF_WI_ID` + `PF_DAEMON_ID` 双环境标记（同机另一 daemon 的同 WI 进程不受影响）回收（四层清理对 pid=0 也执行）。
4. 部分 bounded-fail 分支不杀 driver、不 commitDirty（`recover.go:62-75`）。
5. bounded-fail 时 SessionEnd 失败仍删除本地记录 → 孤儿（`recover.go:483-499`）。
6. 正常关停白等 ~60s 的 flush(shutdown)（#136）。
7. 重启后 `pf observe` 只读内存事件环，历史从头开始（由 2.2 的持久事件 + 单调 id 解决）。
8. 重启后无任何"daemon 重启过"的信号给会话（→ 决策事件）。

**文档对齐**：正常关停后 WI 在 aihub 上是 `running`（不是文档所说 paused）；resume-blocked 是从 aihub 重新克隆（不是"原地继续"）。

## 7. 生命周期简化（已定方向）

- "resume"现有 5 种含义 → 对人只保留 **暂停 / 继续**；系统内部自动处理统一称 **恢复（recover）**，不再叫 resume。
- blocked 三扇门（unblock / resume-blocked / terminate-blocked）→ **继续 / 放弃**两个动作。
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
| 已完成 | release-intent 门禁（#139，commit a159c8c）；driver 机械保证（needs_input 校验、仅 feature-full grill-review 的 `requires_human_answer`、V4 command_evidence 门禁，`f00b398`）；重新领取两个 bug（git-dir 隔离、克隆失败拒绝 InitLocal / DAEMON-516）；身份模型（daemon-id 文件、`pf chat init`、`PF_CHAT_ID`、origin）；持久化提交队列 + 待修复停放 + `pf resume` 续跑（DAEMON-517）+ `daemon_restarted`/`submit_*` 事件；bounded-fail 杀树 + 推送、end_pending 重试、隔离原子性、继续/放弃走人的 key（`pf resume`/`pf cancel`/`pf wrap --fail`）、`unblock` 动词移除、blocked 封存跨 daemon 校验（DAEMON-518）；待修复 WI 的放弃路径；孤儿 step 进程回收；`atomicWrite` 修复 | 已完成（`v2/final`）；含孤儿回收的双标记边界（`PF_WI_ID` + `PF_DAEMON_ID`，同机多 daemon 互不误杀）|
| 迁移代码 | v0→v1 迁移（`migrate-memories`/`migrate-work-items`、`core/import.go`、`tools/v0export`、`seed-v1.sh` 等）：owner 裁定提前删除，不再设观察期 | **已完成**（`v2/m1-migrate`） |
| 兼容垫片 | owner 裁定 v2 不留向后兼容代码：`-mode` 别名、`daemon-state.json` 的 `mode` 迁移、`pf observe` 别名、`pf unblock` 提示桩、aihub `unblock` 端点与 `WIManager.Unblock`、`[watch.*]`/`watch_*` 配置容忍、`project_scope` 别名、对遗留 `takeover` 队列项的专名处理 | **已完成**（`v2/final`）；配置加载现对未知 section（含 `[watch.*]`）报错，未知顶层平铺键仍被忽略 |

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
6. **恢复与重启其余 bug**（第 6 节 4、5、6、8）+ 坏任务"待修复"原地续跑 + 持久化排队队列（含重放规则）+ "继续"范围校验。
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
| 9 | 高 | 坏任务 blocked 后继续会重新克隆（#103 回归）；克隆失败时 SessionEnd 也会失败 | 6：待修复 + session 保留 + 原地续跑；bug 1 持续重试 |
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
| #97 快速死亡死循环 | 恢复引擎 driver 异常退出界（保留 storm guard 语义） |
| #99 等人期间假卡死 | 等人期间无进程、不计时 |
| #99 双写者 | 孤儿回收与监督替换同批 |
| #103 绝不悄悄重建 | 坏任务"待修复"原地续跑 |
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
- **(b) 人工确认保证只约束诚实的 agent，直到 #135 落地**：driver 的 `requires_human_answer` 与 needs_input 校验是进程内的机械保证，但不可信的 step agent 仍可写 capsule、可触达 daemon 的 UDS socket，从而绕过。真正的隔离是 #135（第 9 节 8）。
- **(c) `pf watch` 进展字段**：已随监督重写合入——daemon watch 路由从 `Observe.Progress(wi)` 填充命令、命令时长、最近文件、工具调用数（见附录 D）。

## 附录 D：监督替换落地对照（第 9 节步骤 2–3，分支 `v2/a-supervision`）

**代码位置**
- 契约、翻译器、判死规则：`internal/polyforge/supervise`（`contract.go` 事实/判定/无界队列/每 WI 的 `Link`；`translator.go` + `pi.go` 翻译器接口与 pi 实现；`tracker.go` 每次派发一份的合并状态；`rules.go` 规则 ⓪–④ 的纯函数 `Judge`；`defaults.go` 全局阈值）。
- driver 一侧：`internal/polyforge/driver/supervision.go`（判定围栏、死因表、失败计数、放宽、事实发送）；`TransStepFailed` 在 `transition.go`，一次原子写入记录 attempt（带死因子类）、计数、放宽值、下一次派发形态，必要时写 blocked。
- supervisor 一侧：`internal/polyforge/observe/monitor.go`（消费事实、每派发一份状态、判定、`driver_stalled`、事件触发推送、M4 保留）；事件底座 `observe/events.go`（逐行带 id、按大小轮转、重启后续号、历史从文件读）。
- 失败计数等持久字段：`capsule.Step.Supervision`、`Attempt.FailureCause`、`Execution.BlockedReason`。

**规则 ① 与 T 的后续修订（集成分支）**：规则 ① 扩展到正常结束的轮次（`ended_without_result`，见 2.3 表）；T 取 `max(3 × wall_minutes, 全局默认)`（见 2.4）。

**阈值配置名**：旗标 `--silence-window`（X）、`--tool-window`（Y）、`--step-timeout`（T）、`--stall-window`（N）、`--model-error-grace`（规则 ① 宽限）；step 级覆盖在 workflow.md：`budget.wall_minutes`（T）、`silence_minutes`（X）、`tool_minutes`（Y）。旧旗标 `--hang-window/--idle-window/--probe-timeout/--wall-window` 已删，传入即解析失败。

**与正文的实现差异（均为补充，不改契约语义）**
- 事实集合多一条 `exec_committed{version}`：事件触发推送需要"driver 提交了新 Version"的提示，否则只能回到轮询 `execution.json`。
- 事实/判定走 `supervise.Link`（进程内）；driver 与 supervisor 之外的"harness 海量输出"由翻译器直接折叠进每派发一份的 `Tracker`（只保留最新状态，不排队）。
- `driver_stalled`：supervisor 同时向 driver 发判定，并经 `OBNeedsRecovery(reason=driver_stalled)` 交给恢复引擎——卡死的 driver 无法自己消费判定；恢复引擎杀树并重拉，计入 storm guard。当前 driver 自身不做 git/推送（推送在 supervisor 的推送环里，受 60s 单次上界约束），所以该规则管的是"driver 在步骤之间的任何卡死"。
- 前一次 transcript 作为"只读参考"仅在 capsule 里读得到时才附上（capsule 内的转录靠提交边界同步，进程刚死时可能还没有）；声明的 resume 路径不可读时，resume 退化为 fresh。
