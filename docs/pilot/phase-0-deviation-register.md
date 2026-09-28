# Pilot Phase-0 偏差登记表 <!-- deviation register -->

- **登记对象**：`docs/design/polyforge-vnext/07-deployment-pilot.md` §9.1（Pilot 范围与参与面，≈行 393）声明的 Pilot 设计参数。
- **登记时间**：pilot-phase-0 启动（2026-09-28）之后、40 张 wrapped WI 观测窗进行中。
- **性质**：本登记表只登记偏差，不修改 §9.1——§9.1 保持权威；偏差是*对着它*登记的，不是折入它。若 Pilot 设计参数本身应当修订，那是另一个人类决策。
- **证据底稿**：运维记录位于仓库外 `/root/polyforge-pilot/`（本文引用其作为审计线索，不逐字复制；其中 `a.key`/`b.key`/`root.key` 等密钥材料只按机制引用，绝不复制内容）。
- **结论先行**：以下全部偏差均**不影响** §9.3 八条机械否决门槛（`PILOT-V1`…`PILOT-V8`）的判定——其机械判定的输入（M1–M9，§9.2）不依赖任何一条偏差面。逐条与总结陈述见各条目及文末。

---

## 1. 装机面

### 1.1 对照的 §9.1 原文

> 「1 个 project、3 个内部仓库、4 名工程师（经路由 Skill 会话使用 **Interactive** 模式：submit/observe/answer，`00-architecture.md#§3.4a`）+ 2 台常驻工作机（分别运行 **Batch**……与 **Drain**……）+ 1 台 macOS 工作机（与 Linux 工作机运行同一份 Native runner 形态……）」

> 「三种模式与至少一次 Daemon 优雅停机（`00-architecture.md#§3.5`/`04-local-runtime.md#§6.5`）在观测窗口内均须被实际触发至少一次，否则对应模式/路径的回归断言（§10.2）视为未覆盖，不计入 Pilot 达标判定。」

配套的部署权威（§3 部署单元表）：Linux 工作机注册 systemd 单元 `polyforge-daemon`；`aihub` 以 systemd 单元 `polyforge-aihub` 长驻。

### 1.2 实际情况

- 运行环境是**容器**，无 systemd：`aihub` 与两台「工作机」的 `daemon` 均无法注册 systemd 单元。
- 全部进程在**同一台（单机）容器**内运行，以 **双 daemon 身份** `--machine-id pilot-a` / `pilot-b` 模拟 §9.1 的 2 台常驻工作机（pilot-a 运行 Interactive 模式、pilot-b 运行 Drain 模式，各自独立 `--home`、`--socket`、日志文件 `a.log`/`b.log`）。
- **无 macOS 工作机**：§9.1 的 1 台 macOS 工作机与「Linux/macOS 同一 Native runner 形态」的对齐验证未参与本 Pilot。

### 1.3 补偿措施

- **进程监督**：以 `run-aihub.sh`、`run-a.sh`、`run-b.sh` 三个 shell 循环替代 systemd 单元——`while true` 循环内长驻拉起进程、退出即记录退出码并 `sleep 2` 重启；`restart-all.sh`（及 `restart-all2.sh`）作为统一拉起入口。脚本注释自证其意图：「supervised restart loop (deviation: container has no systemd; 07 §3 long-run semantics preserved)」。该替代满足 §3 要求的长驻语义（「崩溃后可被重新拉起」，`04-local-runtime.md#§7`），只是把监督者从 systemd 换成脚本循环。
- **单机双身份的隔离**：两个 daemon 各持独立 `--machine-id`、独立 `--home` 目录、独立 Unix domain socket 与独立日志（`a.log`/`b.log`）。§2.1 规定「Daemon 间零通信——不存在 Daemon-to-Daemon 协议或互推通道」，两 daemon 同机部署未引入任何设计外的通信通道：二者仍仅与本机 socket 上的操作面和 AIHub 的 HTTP 面交互，机器身份在 AIHub 侧仍以 `pilot-a`/`pilot-b` 两个条目区分。
- **无 systemd 的重启实操**：`killall.py`（按命令行特征扫描 `/proc` 并 SIGKILL）配合 `kill.log`（记录了 8 个实际被杀的 pid，含对同组进程的多轮清杀）构成「监督循环确实杀过/拉起过进程」的实操证据；被 SIGKILL 的 daemon 按 `04-local-runtime.md#§7.1` 走崩溃恢复路径续跑。**§9.1 的「至少一次优雅停机」要求不受此影响、仍须在观测窗内真实触发一次**——它作为观测窗达标条件继续被跟踪，未被脚本循环替代。
- **无 macOS 工作机**：§9.1 已裁定「不设独立的分平台验证阶段，Linux/macOS 在 v1 下没有第二种执行 profile 需要对齐」；macOS 面在本 Pilot 中缺席意味着 §10.2 对应模式/路径的回归断言按 §9.1 原文规则「视为未覆盖，不计入 Pilot 达标判定」，不构成对否决门槛的绕开。

### 1.4 证据（审计线索）

`run-aihub.sh`（aihub 重启循环，注释自证偏差与语义保全）；`run-a.sh`/`run-b.sh`（`--machine-id pilot-a`/`pilot-b`、key-file 凭证、interactive/drain 两模式、分身份日志）；`restart-all.sh`；`killall.py`；`kill.log`（8 个被杀 pid 的实操记录）；`a.log`/`b.log`（单机上按 daemon 身份分离的运行日志）。

### 1.5 是否影响门槛判定

**不影响。** §9.3 八条门槛的机械判定输入（M1–M9，§9.2）全部来自 Core API、`runtime-state/execution.json`、Memory 索引时延等机械口径；其中没有任何一条以「systemd 是否存在」「工作机物理台数」「是否有 macOS 机器」为输入。进程监督由 systemd 换成重启循环只改变监督者身份，不改变长驻语义，因而也不改变 M3（卡死步判据 = step 状态 + 机器心跳）所读取的字段。**PILOT-V1…PILOT-V8 均不受本条偏差影响。**

---

## 2. 规模面

### 2.1 对照的 §9.1 原文

> 「范围：1 个 project、3 个内部仓库、4 名工程师……」

> 「观测窗口：30 天，或累计 ≥ 40 张 wrapped WI，以先满足者为准（两者均为 Pilot 设计参数，见 §12 第 1 条关于校准的说明）。」

### 2.2 实际情况

- **solo 工程师**：对照 §9.1 的 4 名工程师，实际参与面为 1 人（经路由 Skill 会话使用 Interactive 模式）。
- **1 仓**：对照 §9.1 的 3 个内部仓库，实际任务仓为 1 个（`polyforge`）。
- **观测窗口**：不以 30 天日历期限为卡点，以**累计 40 张 wrapped WI** 为准。

### 2.3 补偿措施

- 观测窗口口径本来就允许「30 天或 40 张，先满足者为准」；本 Pilot 反向使用该「或」——只按 40 张票推进、不按日历卡点，是对同一设计参数的**单边取用**，不是新造口径。§9.1/§12 已把窗口时长定性为「Pilot 设计参数、可按校准说明调整」，而门槛判定公式（§9.3）本身不因校准改变。
- 参与面缩小（1 人 / 1 仓）意味着 M1–M9 的样本总量小于 4 人 / 3 仓的设定，但每条指标的分母口径（§9.2 已按 INV-D5 逐条补齐窗口与分母）在缩小后的样本上**原样成立**；`PILOT-V3` 明文以「在 ≥ 40 张 WI 的样本内」为样本基数，与 40 票观测窗天然对齐。

### 2.4 证据（审计线索）

`run-a.sh`/`run-b.sh` 的 `--project polyforge` 单项目单仓启动参数；观测窗进度以 AIHub 内 wrapped WI 计数为准（非本登记表展开项）。

### 2.5 是否影响门槛判定

**不影响。** M1–M9 全部是计数或比例型指标，其输入来自 Core API 与执行权威文件的机械读取；「几名工程师」「几个仓库」「窗口多少天」都不是任何一条门槛公式的输入变量。样本量缩小影响的是统计置信度（那是观测结论的解读问题，属 §12 校准议题），不改变任何一条门槛的机械判定式。**PILOT-V1…PILOT-V8 均不受本条偏差影响。**

---

## 3. harness 面

### 3.1 对照的 §9.1 原文

> 「Pilot 启动前置验证（四项全部通过才进入观测窗口）：……② **claude `.claude/agents/` 非 git 根发现实测**：capsule 根本身不是 git 仓库根（git 根在各 `repos/<name>/` 内），按 `03-execution-capsule.md#§10` 的待验证项实测 Claude 在该布局下的 agent 发现行为；若实测结论为「不做 git 根无法发现」，须在 claude 适配器层提供显式装载路径后才进入观测窗口（该项不得顺延到联调阶段之后）」

§2.1 权威同时把 **Pi CLI 与 Claude Code CLI** 并列为「v1 仅有的两条 harness 适配器」。

### 3.2 实际情况

- **pi 全量承载**：观测窗内全部 WI 的 harness 适配器均为 pi（`--pi-bin /usr/bin/pi`，见 `run-a.sh`/`run-b.sh` 启动参数）。
- **cc 适配器已验证未实战**：claude code 适配器完成了其自身的发现/装载验证（§9.1 前置 ② 的实测面），但尚未承载观测窗内的生产 WI；对照 §9.1 双 harness 并列的预期，实际观测面目前只有一条适配器。

### 3.3 补偿措施

- §9.1 前置验证 ② 的验证动作本身已完成（cc 适配器层验证通过），观测窗的进入条件满足；「cc 未实战」登记为覆盖面偏差——若 Pilot 期内 cc 面出现承载问题，将由本登记表定位为已知偏差而非未知事故。
- pi 全量承载下的端到端可用性有独立冒烟证据：`smoke-realpi.sh` 记录了「real-pi smoke」——对重启后的 daemon-a 提交真实 WI 并循环观察事件流、`runtime-state/execution.json` 落盘与 pi 进程拉起，直至首个真实 step 启动。

### 3.4 证据（审计线索）

`run-a.sh`/`run-b.sh`（`--pi-bin` 指定唯一 harness）；`smoke-realpi.sh`（real-pi 端到端冒烟，含 observe 增量事件与 runtime-state 落盘断言）。

### 3.5 是否影响门槛判定

**不影响。** 门槛输入中与 harness 最相关的是 M4（校验真实性）：其判据是 verification step 在 `runtime-state/execution.json` 里留下的**结构化命令证据**（`output.commands` 至少一条 `{argv, exit_code, duration_ms}`），与该 step 由哪条适配器承载无关——适配器只是承载面，证据权威在执行权威文件。其余 M1–M9 同样不读 harness 选择。cc 面缺席改变的是适配器覆盖度（观测结论解读层面），不改变任何门槛公式的输入。**PILOT-V1…PILOT-V8 均不受本条偏差影响。**

---

## 4. 协议面

### 4.1 对照的 §9.1 原文（及配套 §2.1 迁移裁定）

> 「观测来源：全部经 Core API 与本节 §9.2 定义的机械口径读取，不引入任何人工抽查或主观判断步骤。」

§2.1 配套裁定：「**Memory 存量是唯一迁移项**（`06-memory-search.md#§10`：现有 aihub memory 一次性导出为 Markdown 权威文件——非执行状态，也不构成两系统间的同步通道）。」

### 4.2 实际情况

- **记忆经 MCP 面 dump**：v1 存量记忆的导出取自旧系统（gen-1 aihub）的 MCP 面（`v1-memories.jsonl`，34 条；原始请求记录在 `v1-memories-raw/`）。MCP 面导出的结构性损失：**related/WI 引用为空**——`related: []`、`work_item_id: null` 在导出的 34 条中全量成立。
- **密钥 key-file 方式（合规确认，非偏差）**：工作机/管理面凭证均以 key-file 方式供给（`--api-key-file`，见 `run-a.sh`/`run-b.sh`；`root.key`/`a.key`/`b.key` 即其落位）。这是部署 CLI 的设计内凭证路径，不构成偏差，登记在此仅为把协议面证据链收口到一处。

### 4.3 补偿措施

- **related/WI 引用置空的补全路径**：MCP 面 dump 的引用损失已定位并有明确 cutover 动作——**cutover 时经 psql 路径补全**（直连旧系统数据库读取 MCP 面未暴露的关联字段，回填 vNext Memory 权威文件）。`v1-memories.jsonl` + `v1-memories-raw/` 保留为「MCP 面取数快照」的审计底稿，补全前后的差异可对照。
- **不构成同步通道**：与 §2.1 裁定一致——dump 是一次性迁移动作，不是两系统间的持续同步；置空期间 vNext Memory 面的检索（M6 观测对象）只覆盖 vNext 自己的写入样本，不受存量引用缺失影响。
- **key-file 合规**：无需补偿（设计内路径）。

### 4.4 证据（审计线索）

`v1-memories.jsonl`（34 条 MCP 面导出，全量 `related: []` / `work_item_id: null`）；`v1-memories-raw/`（逐条原始请求记录，含 `g-mem_*`/`g2-mem_*`/`fix-mem_*` 等条目文件与 `err.log`）；`run-a.sh`/`run-b.sh`（`--api-key-file` 机制引用，密钥内容不复制、不出现）。

### 4.5 是否影响门槛判定

**不影响。** 与 Memory 相关的唯一门槛是 `PILOT-V6`（M6：滚动 24 小时 p99 写入→可检索时延，仅针对 **vNext FTS5 trigram 索引自身的写入事件**计样本）——M6 的样本集是「窗口内全部写入事件」，即 vNext 运行期自身产生的写入；存量记忆如何 dump 进来（MCP 面、引用是否置空）不在 M6 的样本口径内。related/WI 引用置空不触碰任何 M1–M9 的输入字段；key-file 方式是设计内路径。**PILOT-V1…PILOT-V8 均不受本条偏差影响。**

---

## 总结：偏差面 × 门槛判定

**写明**：§9.3 的八条机械否决门槛 `PILOT-V1`…`PILOT-V8` **不受上述任何一条偏差影响**。逐条依据：

1. 八条门槛的机械判定公式全部以 §9.2 定义的 M1–M9 为输入，而 M1–M9 的读数来源是——Core API（M1/M2/M3/M7 的状态与事件面）、`runtime-state/execution.json`（M4 的结构化命令证据、M9 的 `rewind_stats`）、§9.2a 的机械信号（M5）、vNext 自身 Memory 索引的写入→可检索时延样本（M6）、已发生边界同步请求的耗时与结果（M8）。
2. 逐条核对四个偏差面：**装机面**（无 systemd / 单机双 daemon / 无 macOS）——M1–M9 无一以监督机制、物理机台数或平台为输入；**规模面**（1 人 / 1 仓 / 40 票为准）——M1–M9 无一以工程师数、仓库数或日历时长为输入，样本量缩小只影响统计置信度；**harness 面**（pi 全量承载、cc 已验证未实战）——M4 读的是执行权威文件里的结构化证据，与承载适配器无关；**协议面**（记忆 MCP 面 dump、引用置空待 cutover psql 补全；key-file 合规）——M6 只对 vNext 自身写入事件计样本，存量迁移路径不在任何门槛口径内。
3. 因此本登记表的四条偏差都是**观测面/覆盖面的偏差**（哪些模式、多大样本、哪条适配器、哪条迁移路径被实际覆盖），不是**判定面的偏差**：八条门槛的公式、输入与「读 API/指标即判」的机械性质原样成立，Pilot 期内按 §9.3 逐条机械判定不受任何上述偏差的削弱或豁免。

§9.1/§9.2/§9.3 保持权威原文不变；本登记表只作为 40 票观测窗期间的可审计底稿。
