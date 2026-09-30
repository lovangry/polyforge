# v1 Pilot Phase-0 偏差登记表 <!-- v1 phase-0 deviation register -->

- **登记对象**：`docs/design/polyforge-vnext/07-deployment-pilot.md` §9.1（Pilot 范围与参与面）及其配套部署权威——§2.1（新仓部署物与旧系统的并存裁定）、§2.2（本文新增不变量）。§9.1/§2.1 保持权威；偏差是*对着它*登记的，不是折入它。若 Pilot 设计参数本身应当修订，那是另一个人类决策。
- **登记时间**：v1 pilot phase-0 启动（2026-09-30：v1.0.0-alpha 构建、`boot.sh` 首次拉起 aihub `:8421` 与 pilot-a/pilot-b 双 daemon 均在当日凌晨；可引证锚点：v1 aihub 首条日志 2026-09-30T00:58:41Z）之后、40 张 wrapped WI 观测窗进行中。**计数起点 = 2026-09-30 v1 装机，重新计数**：v0 pilot 已 wrap 的 7 张不计入 v1 窗口（那是旧系统代码跑出的结果）；门槛口径本身延续 v0 phase-0 的八条否决门槛精神。
- **性质**：同 v0 登记表（`docs/pilot/phase-0-deviation-register.md`）——本登记表只登记偏差，不修改 §9.1/§2.1/§2.2——它们保持权威原文。
- **证据底稿**：仓库外 `/root/polyforge-pilot-v1/`（`a/`、`b/`、`boot.sh`、`releases/v1.0.0-alpha/`、`aihub.log` 等；本文引用其作为审计线索，不逐字复制；其中密钥材料只按机制引用，绝不复制内容）。
- **结论先行**：以下偏差均**不改变** §9.3 八条机械否决门槛（`PILOT-V1`…`PILOT-V8`）的判定公式与输入定义（M1–M9，§9.2）——但**公式不变不等于读数不受影响**：缺陷存续期间实际发生的失败样本（如 F2/F3 的恢复/发布失败、F5 的卡死）按既有口径如实计入，可能影响实测指标值乃至 Pilot 判定结果；本登记表不据此预先豁免任何样本，也**不在观测数据之外认证任何 Pilot 通过**。其中 F1–F5 是观测窗内已修复落 main 的实现缺陷，F6 已裁定待实现、其影响面是 lane 的输入面；F7（第 10 条）已由 owner 对本 capsule 手动补偿、产品修复待实现。逐条与总结陈述见各条目及文末。

---

## 1. 装机面——双 daemon 同机

### 1.1 对照的原文

> 「2 台常驻工作机（分别运行 **Batch**——人显式确认 wave 后成批派发，`00-architecture.md#§3.4b`——与 **Drain**——反复向 AIHub 调度器（就绪队列）取用下一批可执行 WI 且无需逐条人工确认，`00-architecture.md#§3.4c`）」——`07-deployment-pilot.md#§9.1`

> 「每台工作机 + CI 机器：**Linux** 注册为 systemd 单元 `polyforge-daemon`（ExecStart 指向 `<binary> daemon`，随开机自启、由 systemd 监督重启）」；`aihub`：「单主机，systemd 单元 `polyforge-aihub`」——`07-deployment-pilot.md#§3` 部署单元表（systemd 单元权威）

### 1.2 实际情况

- 运行环境是单机容器，无 systemd：aihub 与两台「工作机」的 daemon 均无法注册 systemd 单元。
- 全部进程在**同一台（单机）容器**内运行，以双 daemon 身份 `--machine-id pilot-a` / `pilot-b` 模拟 §9.1 的 2 台常驻工作机：pilot-a 运行 Interactive 模式、pilot-b 运行 Drain 模式，各自独立 data-dir、UDS socket 与日志；由 `boot.sh` 起停（owner 确认的口径是 start/stop，不含崩溃后自动重启监督，见 1.3）。
- 与 v0 登记表装机面的差异点：v0 以 `run-a.sh`/`run-b.sh` 两个 shell 循环监督，v1 改为 `boot.sh` 统一起停——监督者身份仍非 systemd，但起停形态从分身份脚本循环换成了单一启动脚本。

### 1.3 补偿措施

- **监督者身份不同（长驻语义不作超出口径的推断）**：与 v0 登记表装机面同构的只是「监督者从 systemd 换成脚本」这一身份变化。照实限定：owner 确认 `boot.sh` 承担的是进程**起停**（start/stop）；「由 `boot.sh` 起停」**不得推断为**它提供崩溃后自动重启监督——§3/`04-local-runtime.md#§7` 要求的长驻语义（「崩溃后可被重新拉起」）在 v1 是否成立、以何种方式成立，以实际运维记录为准，本登记表不超出 owner 口径作推断。
- **单机双身份的隔离、Daemon 间零通信不变**：两个 daemon 各持独立 `--machine-id`、独立 data-dir、独立 Unix domain socket 与独立日志。§2.1 规定「Daemon 间零通信——不存在 Daemon-to-Daemon 协议或互推通道」，双 daemon 同机部署未引入任何设计外的通信通道：二者仍仅与本机 socket 上的操作面和 AIHub 的 HTTP 面交互，机器身份在 AIHub 侧仍以 `pilot-a`/`pilot-b` 两个条目区分。
- **与 v0 的差异照实登记**：监督脚本从 v0 的 `run-a.sh`/`run-b.sh` 循环换为 `boot.sh`，是同一补偿模式内的形态变化，不改变上述补偿论证。

### 1.4 证据（审计线索）

`/root/polyforge-pilot-v1/` 下：`a/`、`b/`（两 daemon 身份各自的 data-dir/socket/日志落位）；`boot.sh`（起停监督脚本）；`releases/v1.0.0-alpha/`（v1.0.0-alpha 构建发行物）；`aihub.log`（v1 aihub 运行日志，首条 2026-09-30T00:58:41Z）。

### 1.5 是否影响门槛判定

**不影响。** §9.3 八条门槛的机械判定输入（M1–M9，§9.2）中没有任何一条以「systemd 是否存在」「工作机物理台数」「监督脚本形态」为输入；监督者从 systemd 换成 `boot.sh` 只改变监督者身份，也不改变 M3（卡死步判据 = step 状态 + 机器心跳）所读取的字段（自动重启监督是否成立，见 1.3 的照实限定——它同样不是任何 M 的输入变量）。**PILOT-V1…PILOT-V8 的判定公式均不受本条偏差影响。**

---

## 2. 装机/协议面——8421 端口并存（同机双栈）

### 2.1 对照的原文

> 「二者**并存运行**：旧 aihub 系统继续作为生产权威，vNext AIHub 只服务 Pilot 范围（§9.1）；二者不共 `<data_dir>`、不共 systemd 单元名（§3 的 `polyforge-aihub`）、不共监听端口，之间**不迁移执行状态**……」——`07-deployment-pilot.md#§2.1`（新仓部署物与旧系统的并存裁定）

该裁定的假定是独立机器与正式端口分配。

### 2.2 实际情况

- v0 pilot 的 vNext aihub 仍监听 `127.0.0.1:8420`（仍在跑）；v1 pilot aihub 监听本机 `:8421`；生产权威位是**远程** legacy aihub，本机不占其端口。
- 偏差点：同一台机器上两代 vNext 栈（8420/8421）并行服务——§2.1 裁定覆盖「legacy 与 vNext 并存」却未覆盖「v0-pilot vNext 与 v1 vNext 同机并存」；端口分配是事实分配而非正式分配。

### 2.3 补偿措施

- **两栈零共享**：v0-pilot vNext 栈与 v1 栈各自独立 data-dir、端口与日志，互不共享任何数据面或状态。
- **v1 只服务 Pilot 范围**（§9.1）、**不迁移执行状态**——§2.1 对 legacy/vNext 并存的隔离要求同构地适用于 v0-pilot vNext 与 v1 vNext 两代栈之间。
- 照实登记为**覆盖面偏差**：§2.1 的并存裁定未预设「两代 vNext 同机并行」这一形态；本条把该形态登记为已知偏差，使同机双栈期间任何面的问题可定位为已知偏差而非未知事故。

### 2.4 证据（审计线索）

`/root/polyforge-pilot-v1/` 下：`aihub.log`（v1 aihub `:8421` 监听与运行记录）；v0 pilot 侧 vNext aihub `127.0.0.1:8420` 仍在运行的事实（其底稿见 v0 登记表所引 `/root/polyforge-pilot/`）；两栈各自独立的 data-dir 落位。

### 2.5 是否影响门槛判定

**不影响。** M1–M9 无一以「监听端口号」或「同机栈数」为输入：其读数来源是 Core API、`runtime-state/execution.json`、§9.2a 机械信号、vNext 自身 Memory 索引样本与已发生边界同步请求——全部从 v1 栈自身的权威面机械读取，与 v0-pilot 栈是否同机并行无关。端口是事实分配而非正式分配，改变的是部署覆盖面，不改变任何门槛公式的输入。**PILOT-V1…PILOT-V8 均不受本条偏差影响。**

---

## 3. 规模面——solo 工程师 + 1 仓 + 窗口重计

### 3.1 对照的 §9.1 原文

> 「范围：1 个 project、3 个内部仓库、4 名工程师……」

> 「观测窗口：30 天，或累计 ≥ 40 张 wrapped WI，以先满足者为准（两者均为 Pilot 设计参数，见 §12 第 1 条关于校准的说明）。」

### 3.2 实际情况

- **solo 工程师**：对照 §9.1 的 4 名工程师，实际参与面为 1 人（经路由 Skill 会话使用 Interactive 模式）。
- **1 仓**：对照 §9.1 的 3 个内部仓库，实际任务仓为 1 个（`polyforge` 单仓）。
- **观测窗口**：不以 30 天日历期限为卡点，以**累计 40 张 wrapped WI** 为准；且 **v1 装机（2026-09-30）后重新计数**——v0 pilot 已 wrap 的 7 张不计入 v1 窗口（那是旧系统代码跑出的结果）。

### 3.3 补偿措施

- 观测窗口口径本来就允许「30 天或 40 张，先满足者为准」；本 Pilot 反向使用该「或」——只按 40 张票推进、不按日历卡点，是对同一设计参数的**单边取用**，不是新造口径（同 v0 登记表规模面）。
- **窗口重计的口径依据**：v1 是新仓部署物的首次真机运行，v0 窗口的 7 张 wrapped 由旧系统代码跑出，不构成 v1 栈的观测样本；以 2026-09-30 装机为计数起点保证 40 张样本全部来自 v1 栈。门槛口径本身延续 v0 phase-0 八条否决门槛精神，不因重计改变。
- 参与面缩小（1 人 / 1 仓）意味着 M1–M9 的样本总量小于 4 人 / 3 仓的设定，但每条指标的分母口径（§9.2 已按 INV-D5 逐条补齐窗口与分母）在缩小后的样本上**原样成立**；`PILOT-V3` 明文以「在 ≥ 40 张 WI 的样本内」为样本基数，与重计后的 40 票观测窗天然对齐。

### 3.4 证据（审计线索）

`/root/polyforge-pilot-v1/` 下 `boot.sh` 与 daemon 启动参数（单 project 单仓）；观测窗进度以 v1 AIHub 内 wrapped WI 计数为准（计数起点 2026-09-30，非本登记表展开项）。

### 3.5 是否影响门槛判定

**不影响。** M1–M9 全部是计数或比例型指标，其输入来自 Core API 与执行权威文件的机械读取；「几名工程师」「几个仓库」「窗口多少天」都不是任何一条门槛公式的输入变量。样本量缩小影响的是统计置信度（观测结论的解读问题，属 §12 校准议题），不改变任何一条门槛的机械判定式。**PILOT-V1…PILOT-V8 均不受本条偏差影响。**

---

## 4. 真机发现面——F1：goal 从未到达 agent

### 4.1 对照的原文（教训台账条目）

> 「C2  Work-item context delivery: v0 delivered goal/content via daemon proxy_call (read_workflow_ref). When a contact surface is deleted, enumerate the business it carried before deleting it.」——`docs/design/polyforge-vnext/v0-lessons-ledger.md`（C2；另见该台账「v1 pilot findings」F1 条目）

### 4.2 实际情况

v1 删掉 v0 的 daemon proxy_call（`read_workflow_ref`）时未枚举其承载的业务（台账 C2）：agent 无任何 agent→server 读路径，无法知道自己的目标（goal）。

### 4.3 补偿措施

claim answer 的任务定义**全量物化**为 `runtime-state/work-item.json`（capsule 内唯一权威），恢复路径修复缺失副本。

### 4.4 证据（审计线索）

修复 commit `39dfd1c`（分支 v1-fix-pilot1，merge 为 `1059da5`，git 历史）；台账条目 C2 与「v1 pilot findings」F1（`docs/design/polyforge-vnext/v0-lessons-ledger.md`）。

### 4.5 是否影响门槛判定

**不影响。** F1 是观测窗内已修复落 main 的实现缺陷；其读数面是 capsule 内任务定义的权威落位（`runtime-state/work-item.json`），M1–M9 无一以该文件为输入，`runtime-state/execution.json` 的证据口径不变。**PILOT-V1…PILOT-V8 均不受本条偏差影响。**

---

## 5. 真机发现面——F2：slug 拼出的 git 路径

### 5.1 对照的原文（教训台账条目）

> 「F2  Slug-built git paths. ... the claim answer's canonical work-item id is the authority; every path (capsule dir, external git-dir, push remote, session id, spawn journal) is keyed by it, the submitted spelling is registered as an alias only」——`docs/design/polyforge-vnext/v0-lessons-ledger.md`（「v1 pilot findings」F2 条目：路径权威 = canonical work-item id）

### 5.2 实际情况

提交侧的 `<project>#<seq>` 拼写泄漏进 push remote URL（`#` 是 URL fragment 分隔符，推送 404）与 capsule 目录名——路径按 slug 拼写而非 canonical id 键控。

### 5.3 补偿措施

canonical work-item id 为唯一权威：所有路径（bare repo 名、remote URL、capsule 目录、session id、spawn journal）一律按它键控，slug 仅注册为别名；Restart 原地迁移存量。

### 5.4 证据（审计线索）

修复落点同 F1：commit `39dfd1c`（v1-fix-pilot1，merge `1059da5`，git 历史）；台账条目「v1 pilot findings」F2。

### 5.5 是否影响门槛判定

**公式与输入定义不变；实测值与 Pilot 判定结果不排除受影响。** F2 是观测窗内已修复落 main 的实现缺陷；其读数面是 ship/publish 链路的路径键控——M7/M8 只对已真实发生的扫描/同步请求如实计分，门槛公式与输入定义不变。但缺陷存续期间由 slug 路径造成的发布失败（推送 404）样本会按既有口径如实计入 M7/M8 的分母与分子，可能压低实测指标值乃至影响 Pilot 判定结果——**公式不变不等于读数不受影响**，本登记表不预先豁免这些历史失败样本，最终以观测窗数据判定。

---

## 6. 真机发现面——F3：transcript 从未进 capsule

### 6.1 对照的原文（教训台账条目）

> 「P3  PI_SESSION_DIR does NOT control pi's transcript location. ... pi CAN resume from any .jsonl path via --session.」——`docs/design/polyforge-vnext/v0-lessons-ledger.md`（P3）

> 「C1' (v0 lesson, superseded by v1 C1 ruling) transcripts are valuable: in v1 they must actually REACH the capsule git (sync mechanism was the missing piece — see P3).」——同台账（C1'；另见「v1 pilot findings」F3 条目）

### 6.2 实际情况

v0 移植的 `PI_SESSION_DIR` 环境变量对 transcript 落位是死的（台账 P3）：新派生的 agent 把 transcript 写进 harness 自己的 session store，没有任何东西进 capsule git（C1'）。

### 6.3 补偿措施

修复（daemon 侧）：spawn 后定位活 transcript，在每个 commit 边界前把它拷入 capsule（`session/session-<instance>.jsonl`，先于 commit、随链入库）；resume 优先用 capsule 副本、活文件兜底。

### 6.4 证据（审计线索）

修复落点同 F1：commit `39dfd1c`（v1-fix-pilot1，merge `1059da5`，git 历史）；台账条目 P3、C1' 与「v1 pilot findings」F3。

### 6.5 是否影响门槛判定

**公式与输入定义不变；实测值与 Pilot 判定结果不排除受影响。** F3 是观测窗内已修复落 main 的实现缺陷；其读数面是跨机恢复的 transcript 可得性——修复后 transcript 随 capsule git 链传播，不改 M4 的 `runtime-state/execution.json` 证据口径（M4 读的是 `output.commands` 结构化命令证据，与 transcript 无关），也不改其余任何 M 输入。但缺陷存续期间 transcript 不在 capsule 内所造成的恢复/续跑失败样本会按既有口径如实计入相应 M 指标，可能影响实测值乃至 Pilot 判定结果——本登记表不预先豁免这些历史失败样本，最终以观测窗数据判定。

---

## 7. 真机发现面——F4：candidate chain 是摆设 + settings 污染敞开

### 7.1 对照的原文（教训台账条目）

> 「P6  Model selection is per-step bundle business (model_candidates with ordinal advancement on INFRA failure only — business FAIL never advances). The agent must not silently run on the operator's global default model/settings (settings pollution: ...)」——`docs/design/polyforge-vnext/v0-lessons-ledger.md`（P6；配合 I2：「Candidate advancement must be bounded and the ordinal persisted in execution state (crash-safe)」；另见「v1 pilot findings」F4 条目）

### 7.2 实际情况

bundle 的 model_candidates 从未被消费（台账 P6 + I2）：spawn 不钉模型（agent 静默跑在 operator 全局默认上）、ordinal 不推进不持久化、业务 FAIL/rewind 语义未接线、observe 面 StepView.Model 恒空。

### 7.3 补偿措施

最小 frontmatter 读面回补（defaults + per-step candidates）；materialize 把 defaults 链抽到 job；每次派生 spawn 按**持久化 ordinal** 钉 argv（实测：rpc 模式下 capsule 级 project settings 被信任门挡掉，见 spawn.go buildCmd）；**仅** provider 类恢复推进 ordinal（有界、可绕尽、INV-1g）；executor 把有效模型记进 `steps.<id>.model`。

### 7.4 证据（审计线索）

修复 commit `d18ae73`（分支 v1-lessons-fix，base 1059da5，git 历史）；台账条目 P6、I2 与「v1 pilot findings」F4。

### 7.5 是否影响门槛判定

**不影响。** F4 是观测窗内已修复落 main 的实现缺陷；其读数面是 observe 面 StepView.Model 的渲染与模型选择治理（candidate chain 消费、ordinal 持久化推进）——M1–M9 无一以「spawn 钉了哪个模型」或「StepView 渲染了什么」为输入，不改任何 M 读数字段。**PILOT-V1…PILOT-V8 均不受本条偏差影响。**

---

## 8. 真机发现面——F5：fatal provider error 处置与 idle 看门狗

### 8.1 对照的原文（教训台账条目）

> 「I1  Provider 400/503/connection-hangs are normal weather. Detection: message.stopReason=='error' + errorMessage records.」——`docs/design/polyforge-vnext/v0-lessons-ledger.md`（Provider/infra class，I1）

该条承载的裁定：provider 级错误属常规天气，须被识别并分类处置。fatal 级 provider 错误此前被按普通中断处理（不自恢复、不能及时判死），长静默步无看门狗，未满足该分类处置要求。

### 8.2 实际情况

fatal 级 provider 错误按普通中断处理：不自恢复、不能及时判死；长时间静默的 step 无任何看门狗机制。

### 8.3 补偿措施

fatal provider error 即刻恢复（instant recovery）；idle watchdog（10 分钟窗口 + probe）。

### 8.4 证据（审计线索）

修复 commit `2a0ab4a`（分支 v1-f5-watchdog，merge 为 `d84115b`，git 历史）；台账 Provider/infra class 条目 I1。按现行口径，F5 及后续新发现先落 repo 台账文件（见 9.3）。

### 8.5 是否影响门槛判定

**公式与输入定义不变；实测值与 Pilot 判定结果不排除受影响。** F5 是观测窗内已修复落 main 的实现缺陷；其读数面是卡死判死与恢复的及时性——修复不改变 M3 所读取的字段口径（step 状态 + 机器心跳），其余 M 指标不读看门狗。但缺陷存续期间 fatal provider error 未自恢复与长静默步无看门狗所产生的卡死、超时与墙钟消耗样本会按既有口径如实计入相应 M 指标，可能影响实测值乃至 Pilot 判定结果——本登记表不预先豁免这些历史失败样本，最终以观测窗数据判定。

---

## 9. 真机发现面——F6：goal 携带的 recall 指令不可执行

### 9.1 对照的原文（教训台账条目）

> 「X4  The memory ledger exists precisely to prevent re-hits — every implementation/review lane MUST recall it first (v1 lanes did not; that is why F1/F3/F4 happened).」——`docs/design/polyforge-vnext/v0-lessons-ledger.md`（X4）

配套裁定：v1 pf-execute 契约禁用 ambient MCP/extension 面（台账 P5 的隔离裁定的落地），agent 无任何 agent→server memory 读路径。

### 9.2 实际情况

goal 书写惯例（v0 实践）仍带「recall 记忆」类指令，而 v1 pf-execute 契约禁用 ambient MCP/extension 面、agent 无任何 agent→server memory 读路径——该指令 in-contract 不可执行。暴露实例：#9（已 cancel）与本 WI（#11，goal 明文要求开工前 recall 记忆「教训台账」）。

### 9.3 补偿措施

- **现行补偿**：repo 内台账（`docs/design/polyforge-vnext/v0-lessons-ledger.md`）充当 recall 面——F5 及后续新发现先落 repo 台账文件；v1 记忆系统的 recall 面等 F6 落地后才对 agent 生效（台账就在 repo 里）。
- **修复方向已拍板、尚未实现，照实登记为「已裁定待实现」**：静态快照（materialize 时按 goal 语义检索 top-K 记忆进 capsule）+ UDS 窄通道（`POST /jobs/{id}/recall`，与 flush 同面同围栏）+ 同面 remember 写回。

### 9.4 证据（审计线索）

台账条目 X4（in-repo）；暴露实例 #9（已 cancel）与本 WI（#11）的 goal 原文（AIHub 内 WI 记录）；修复方向的裁定记录（owner 确认，见本登记表的 grill 问答底稿）。

### 9.5 是否影响门槛判定

**不影响。** F6 影响的是 lane 的输入面——agent 拿到的上下文完备性，不改任何 M 输入；本 WI 靠 in-repo 台账 + grill 问答补齐 recall 面。M1–M9 的读数来源（Core API、`runtime-state/execution.json`、Memory 索引时延样本等）不因 recall 面缺口改变口径。**PILOT-V1…PILOT-V8 均不受本条偏差影响。**

---

## 10. 真机发现面——F7：步定义物化错位与完成标记缺失

### 10.1 对照的原文

> 「WI Agent 被派生后、驱动任何 step 之前的**第 0 步**，从本机 aihub checkout 的 `workflows/<bundle>/` 物化三项目标：`agents/*.md`（subagent 定义文件）→ `<capsule>/.pi/agents/`（pi 适配器）……」——`03-execution-capsule.md#§4.5`（物化协议第 1 条）

> 「capsule 必建空 `.pi/agents/`：pi 从 subagent 的 cwd 向上查找最近的 `.pi/agents`；若 capsule 内不存在该目录，查找会**向上泄漏**到工作区级（或更上层）的 `.pi/agents/`，把与本 WI 无关的定义带入解析面。」——同文 §4.5（防冲突三层第 3 条）

配套口径：物化完成须有完成标记（completion marker）才可信——「trust the existing materialized snapshot only when its completion marker is present」（`skills/pf-execute/SKILL.md` 契约；owner 按 v0 `03` §4.5 语义确认）。

### 10.2 实际情况

- v0→v1 移植遗漏：v1 物化把步定义复制到 capsule 的 `agents/` 目录，而不是 Pi 的发现目录 `.pi/agents/`——发现目录错位，pi 从 capsule cwd 找不到项目级步定义；同时未写物化完成标记。
- 缺口存续期间的历史事实（owner 确认按当时环境如实记录、不追溯视为合规）：本 WI 此前的步派发走的是**全局 `step-executor` 定义**——这是事实性的历史 workaround，不追溯表述为项目级发现命中了 pinned 步定义。

### 10.3 补偿措施

- **手动补偿（本 capsule 已完成，owner 操作）**：四个步定义（grill/fix/verification/release）已从 capsule `agents/` 拷贝进 `.pi/agents/`——pi 的 subagent 工具可从 capsule cwd 发现项目级 agent；完成标记 `sync/materialized.json` 已写入，按 v0 `03` §4.5 语义记录 pinned bundle 身份（`bundle_name` = quick-fix、`bundle_digest` = `933ead50…`）与 targets 清单。
- **产品修复待实现**：v1 物化目标目录与完成标记的产品侧修复尚未实现，已作为 F7 登记待修——本登记 WI 只登记不修码。

### 10.4 证据（审计线索）

owner 答复（q_103mij，本 WI 的问答/执行记录底稿）；capsule 内 `sync/materialized.json`（完成标记）与 `.pi/agents/` 下的四个步定义副本。**本条不存在修复 commit**——不引用任何 git commit 作为修复证据。

### 10.5 是否影响门槛判定

**公式与输入定义不变；读数影响待观测窗数据判定。** F7 影响的是 lane 的输入面——缺口存续期间步执行实际运行在全局 `step-executor` 定义而非 pinned per-step 定义之下，这些历史样本是否受影响、受何种影响，属观测窗数据的解读问题，本登记表不预先豁免也不预先定罪；M1–M9 的公式与输入定义不因本条改变。Pilot 判定结果是否受影响，以观测窗数据按 §9.3 机械判定。

---

## 总结：偏差面 × 门槛判定

**写明**：§9.3 的八条机械否决门槛 `PILOT-V1`…`PILOT-V8` 的**判定公式与输入定义（M1–M9）不受上述任何一条偏差影响**；但这一结论**只覆盖公式面**——公式不变**不是**「缺陷未影响实测值」的证明，更不是对任何 Pilot 结果的认证。逐条依据：

1. 八条门槛的机械判定公式全部以 §9.2 定义的 M1–M9 为输入，而 M1–M9 的读数来源是——Core API（M1/M2/M3/M7 的状态与事件面）、`runtime-state/execution.json`（M4 的结构化命令证据、M9 的 `rewind_stats`）、§9.2a 的机械信号（M5）、vNext 自身 Memory 索引的写入→可检索时延样本（M6）、已发生边界同步请求的耗时与结果（M8）。
2. 逐条核对各偏差面：**装机面**（无 systemd / 单机双 daemon / `boot.sh` 起停——自动重启监督未由 owner 口径确认，见 1.3）——M1–M9 无一以监督机制、物理机台数或起停脚本形态为输入；**装机/协议面**（8420/8421 同机双栈并存）——M1–M9 无一以监听端口或同机栈数为输入，两栈零共享、v1 只服务 Pilot 范围、不迁移执行状态；**规模面**（1 人 / 1 仓 / 40 票为准、v1 装机重计）——M1–M9 无一以工程师数、仓库数或日历时长为输入，样本量缩小只影响统计置信度；**真机发现面 F1–F5**——观测窗内已修复落 main 的实现缺陷，逐条读数面见各条目；**F6**——影响的是 lane 的输入面（recall 缺口由 in-repo 台账 + grill 问答补齐）；**F7**——影响 lane 的输入面（物化错位期间步执行实际运行在全局定义下；本 capsule 已由 owner 手动补偿、产品修复待实现），不改任何 M 公式。
3. 因此本登记表的偏差都是**观测面/覆盖面/实现面的偏差**（哪些模式、多大样本、哪种起停形态、哪条实现缺陷被实际覆盖与修复），不是**公式面的偏差**：八条门槛的公式、输入与「读 API/指标即判」的机械性质原样成立。但**读数面照实保留历史失败样本**：缺陷存续期间实际发生的失败——特别是 F2 的发布 404、F3 的恢复/续跑失败、F5 的卡死与墙钟消耗——按 M 指标既有口径如实计入分母与分子，可能已经影响实测指标值，也可能由此影响 Pilot 判定结果；本登记表不豁免这些样本，也不在观测数据之外预先判定其影响方向。
4. **本登记表不认证任何 Pilot 通过**：`PILOT-V1`…`PILOT-V8` 的通过与否则只能由 40 票观测窗的实际读数按 §9.3 逐条机械判定得出；在观测数据齐全之前，本文既不宣布通过、也不宣布失败。

§9.1/§2.1/§2.2 保持权威原文不变；本登记表只作为 40 票观测窗期间的可审计底稿。
