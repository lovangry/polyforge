# v1 Pilot 值守/演练脚本盘点 <!-- ops-scripts as-is inventory -->

- **性质**：as-is 盘点——只描述 `/root/polyforge-pilot-v1/` 下各值守/演练脚本**今天实际做什么**，不给整合、重构或归档建议；全文仅有的两个评注面是已取代脚本的标注（§4、§5）与文末「运维注意事项」。
- **盘点范围**：`/root/polyforge-pilot-v1/` 下 8 个脚本（清单见总览表，即本文全部段落）。`boot-b-only.sh` 不在 8 段范围内、不设独立段，仅在「运维注意事项」中指认为现成的单 daemon 重启手段。
- **证据底稿**：仓库外 `/root/polyforge-pilot-v1/*.sh` 脚本原文（沿用偏差登记表 `docs/pilot/v1-phase-0-deviation-register.md` 的证据引用先例，一律按绝对路径引用；盘点时点 2026-09-30，逐一阅读脚本原文后照录）。
- **结论先行**：**所有值守脚本在各设计路径——needs_input 确认、终态、轮询上限——均以退出码 0 结束**，结果靠 stdout 输出行区分；脚本没有区分性退出码，操作员要区分结果须读输出行，不能只看退出码。本文退出语义均为对实际代码的照录，非规范性期望。

## 0. 总览表

| 脚本（绝对路径） | 类别 | 一句话职责 | 状态 |
|---|---|---|---|
| `/root/polyforge-pilot-v1/boot.sh` | 启动基础设施 | 全栈幂等重启：kill 旧 aihub + 双 daemon 后以 setsid 全部重拉 | 在用 |
| `/root/polyforge-pilot-v1/watch-smoke.sh` | 值守（单票） | 轮询固定 smoke 票 `polyforge#10` 至 needs_input 或终态 | 在用（票面固定为 #10） |
| `/root/polyforge-pilot-v1/watch-batch.sh` | 值守（批次） | 经 daemon-b 盯整批在跑票，needs_input 连续两轮确认后报告 | 在用 |
| `/root/polyforge-pilot-v1/watch-round3.sh` | 值守（单票） | 轮询 `polyforge#11` 至 needs_input 或终态（grep/sed 解析） | **已被 3c 取代** |
| `/root/polyforge-pilot-v1/watch-round3b.sh` | 值守（单票） | 同 round3 形态的 #11 续播值守（grep/sed 解析） | **已被 3c 取代** |
| `/root/polyforge-pilot-v1/watch-round3c.sh` | 值守（单票） | 轮询 `polyforge#11`：JSON 解析 + needs_input 两次防抖确认 | 在用（现行） |
| `/root/polyforge-pilot-v1/night-watch.sh` | 值守（夜巡/全队列） | 自动 ship wrapped 票 + 双 daemon needs_input 防抖汇总 | 在用 |
| `/root/polyforge-pilot-v1/drill-shutdown.sh` | 演练（停机） | graceful shutdown 演练：提交慢票→停 daemon-a→核孤儿与在册状态 | 在用（演练） |

---

## 1. `/root/polyforge-pilot-v1/boot.sh`（类别：启动基础设施）

- **触发条件**：操作员手动执行；无自动触发面。用于整套 v1 pilot 栈（aihub + 双 daemon）的拉起/重启。
- **职责**：幂等全栈启动。先按 pid 文件 kill 旧实例——`aihub.pid`、`a/daemon.pid`、`b/daemon.pid`（精确路径，无通配，kill 失败静默吞掉）——sleep 1 后以 `setsid` 依次重拉：aihub（`-listen :8421`）、daemon-a（interactive 模式，`--machine-id m-pilot-a`）、daemon-b（drain 模式，`--machine-id m-pilot-b`），各持独立 data-dir/socket/日志；最后回显三个进程的 PID。
- **退出语义**：固定序列跑完即结束，正常路径退出码 0；脚本不做健康检查、不做重试，末尾 echo 回显 PID 即为完成面。照实指出：它是**全栈重启器**，没有单点重启能力——重启单个 daemon 不要用它（见文末运维注意事项）。

## 2. `/root/polyforge-pilot-v1/watch-smoke.sh`（类别：值守——单票）

- **触发条件**：操作员手动启动；面向固定票 `polyforge#10`（`wi_gyXc0y9lX7gk`，v1 smoke 票），经 daemon-a socket（`a/daemon.sock`）观察。
- **职责**：每 30 秒执行一次 `pf --json observe polyforge#10` 轮询该票，直到 needs_input（输出中出现 `"pending_question"`）或终态（status 脱离 running/needs_input/sealing），终态时附带打印 queue 前 4 行。
- **退出语义**：三条设计路径均以退出码 0 结束：检出 needs_input（打印 `NEEDS_INPUT detected:` 及 observe 输出前 600 字节）、检出终态（打印 `TERMINAL status=<st>` 及 queue 摘要）、轮询达上限 240 次（约 2 小时，打印 `watch cap reached, still running`）。结果只能靠 stdout 输出行区分，无区分性退出码。

## 3. `/root/polyforge-pilot-v1/watch-batch.sh`（类别：值守——批次）

- **触发条件**：操作员手动启动；经 daemon-b（drain）socket（`b/daemon.sock`）观察，面向该 daemon 承接的全部在跑票。
- **职责**：每 60 秒读一次 `pf --json ops status`，以 python3 做 JSON 解析，汇总 needs_input 的 pending question 与在跑 job 数；任何 needs_input **连续两轮命中同一问题集合**（防抖）才报告。
- **退出语义**：两条设计路径均以退出码 0 结束：needs_input 经两轮确认（打印 `NI <question ids>`）、轮询达上限 400 次（约 6 小时 40 分，打印 `batch watch cap reached`）。无区分性退出码。

## 4. `/root/polyforge-pilot-v1/watch-round3.sh`（类别：值守——单票）——**已被 3c 取代（3/3b 用脆弱的 grep/sed，3c 改 JSON 解析+防抖）**

- **触发条件**：操作员手动启动；面向固定票 `polyforge#11`，经 daemon-a socket 观察。
- **职责**：每 30 秒 `pf --json observe polyforge#11` 轮询该票至 needs_input 或终态；needs_input 用 grep 匹配 `"pending_question"`、status 用 sed 从输出文本里提取——解析面脆弱（见本节取代标注），无防抖。
- **退出语义**：三条设计路径均以退出码 0 结束：检出 needs_input（打印 `NEEDS_INPUT:` 及 observe 输出前 800 字节）、检出终态（打印 `TERMINAL status=<st>` 及 queue 第 3–4 行）、轮询达上限 300 次（约 2.5 小时，打印 `watch cap reached, still running`）。无区分性退出码。**本脚本已被 `watch-round3c.sh` 取代**，按 as-is 立场留存于目录、仅作标注，不在本文建议清理。

## 5. `/root/polyforge-pilot-v1/watch-round3b.sh`（类别：值守——单票）——**已被 3c 取代（3/3b 用脆弱的 grep/sed，3c 改 JSON 解析+防抖）**

- **触发条件**：操作员手动启动；同样面向固定票 `polyforge#11`，经 daemon-a socket 观察，是 round3 形态的续播值守版本。
- **职责**：与 `watch-round3.sh` 同构：每 30 秒 observe `polyforge#11` 至 needs_input 或终态，同样用 grep/sed 解析、无防抖；与 round3 的差异仅在输出细节（needs_input 打印 `NEEDS_INPUT:`、终态摘要行）。
- **退出语义**：三条设计路径均以退出码 0 结束：needs_input、终态、轮询达上限 300 次（约 2.5 小时，打印 `watch cap reached, still running`）。无区分性退出码。**本脚本已被 `watch-round3c.sh` 取代**，按 as-is 立场留存于目录、仅作标注，不在本文建议清理。

## 6. `/root/polyforge-pilot-v1/watch-round3c.sh`（类别：值守——单票，现行）

- **触发条件**：操作员手动启动；面向固定票 `polyforge#11`，经 daemon-a socket 观察。
- **职责**：每 30 秒 observe `polyforge#11`，以 python3 做 **JSON 解析**：needs_input 须**连续两轮命中同一 question id**（防抖确认）才报告；解析失败（`PARSE_ERR`）只重置防抖、不退出；status 离开 running/needs_input/sealing 即报终态。
- **退出语义**：三条设计路径均以退出码 0 结束：needs_input 防抖确认（打印 `NEEDS_INPUT (confirmed 2 polls): <qid>`）、终态（打印 `TERMINAL status=<st>`）、轮询达上限 300 次（约 2.5 小时，打印 `watch cap reached, still running`）。无区分性退出码。轮询 `polyforge#11` 时应使用本脚本，不再使用 round3/3b。

## 7. `/root/polyforge-pilot-v1/night-watch.sh`（类别：值守——夜巡/全队列）

- **触发条件**：操作员手动启动（加速值守场景，如夜间放人后）；同时照看整个 polyforge 队列与 a、b 双 daemon。
- **职责**：每 60 秒一轮，每轮做两件事：(1) 对 queue 中所有 wrapped 票逐张 `pf ship`（输出含 shipped/reused 即打印 `SHIPPED <slug>`，自动发布面）；(2) 分别经 `a/daemon.sock`、`b/daemon.sock` 读 `pf --json ops status`（python3 JSON 解析），汇总双 daemon 的全部 needs_input，**连续两轮命中同一集合**（防抖）才报告。
- **退出语义**：两条设计路径均以退出码 0 结束：needs_input 经防抖确认（打印 `NEEDS_INPUT (confirmed): …`）、轮询达上限 480 轮（约 8 小时，打印 `night watch cap reached`）。没有终态退出路径——队列全部跑完时它只继续轮询直到上限。无区分性退出码。

## 8. `/root/polyforge-pilot-v1/drill-shutdown.sh`（类别：演练——停机）

- **触发条件**：操作员手动启动；用于 pilot checklist 的 graceful shutdown 演练。
- **职责**：按固定序列演练停机：file 一张慢票（quick-fix bundle，goal 即本次「值守脚本盘点」文档任务——本 WI 就是该 drill 票）→ 提交到 daemon-a → sleep 15 → 显示运行中状态 → 经 daemon-a socket 下发 `pf ops shutdown` → 等待 daemon-a 退出（墙钟预算 330 秒，超限打印 `SHUTDOWN OVER BUDGET` 后继续序列）→ 扫描 environ 含 `PF_WI_ID` 且 PPid=1 的孤儿进程 → 核对 aihub 上该 WI 仍为 running（未 wrap）→ 打印 daemon-a 日志尾 3 行。注意：演练**实际关停的是 daemon-a**。
- **退出语义**：固定序列一路跑完，正常路径退出码 0（末尾命令为 `tail -3 a/daemon.log`）；各检查项的结果（停机耗时行、orphan scan、aihub 侧 WI 状态行、`SHUTDOWN OVER BUDGET`）全部靠 stdout 输出行呈现，无区分性退出码。

---

## 运维注意事项

- **boot.sh 是全栈重启器**：它先按 pid 文件 kill 旧 aihub 和 a、b 双 daemon，再把三者全部重拉。**重启单个 daemon 不要用它**——它会连带停掉 aihub 与另一个 daemon。要重启单个 daemon，用单独的 `setsid` 启动该 daemon。
- **该误伤今天（2026-09-30）刚发生过一次**：操作员想重启单个 daemon，用了 `boot.sh`，结果整个 v1 pilot 栈（aihub + 双 daemon）被一并重启。此教训即本文的成文起因。
- **现成单 daemon 重启手段**：`/root/polyforge-pilot-v1/boot-b-only.sh`（2026-09-30 11:46 创建，即上述误伤的当日补救脚本）——只 kill daemon-b、轮询等待 `b/daemon.sock` 释放（最长 120 秒），再以 setsid 单独重启 daemon-b 并回显新 PID。重启 daemon-b 时优先用它；重启 daemon-a 可按同型方式单独 setsid 启动。
