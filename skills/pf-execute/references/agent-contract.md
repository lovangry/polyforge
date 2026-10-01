# pf-execute 机器契约参考（人类镜像）

> **权威声明（必读）**
>
> - **`skills/pf-execute/SKILL.md` 是权威注入文本。** daemon 对每个派生 WI Agent 的注入是单文件机制事实：
>   `internal/polyforge/runtime/recover.go` 的 `buildPrompt` 只读取 `skills/pf-execute/SKILL.md` 全文作为
>   初始指令——不解析引用，不注入、也不物化 `skills/` 目录下的任何其他文件（含本页）。SKILL.md 由 daemon
>   从自身 release 注入到 spawn 时刻的持久 RPC 会话；任何 harness 都不会安装它，终端用户永远看不到它，
>   它也绝不在人类直接键入的交互式 harness 会话里运行。它的唯一读者是 WI Agent 本身。
> - **本页（`references/agent-contract.md`）仅是人类/评审视角的汇总镜像**：把散布在 SKILL.md §0–§12
>   叙述文本里的全部机器契约集中成单一参考页，供 owner 与评审通览，不必逐节读完整注入文本。
> - **与 SKILL.md 冲突时，以 SKILL.md 为准。** 本页不修改、不重新解释、不扩展任何契约；本页自身的任何
>   表述错误都不构成契约的一部分。
> - **本页不进入任何注入面。** SKILL.md 保持自包含（其中刻意不含指向本页的指针——派生 agent 运行时
>   读不到仓库外引用）。
> - 本页为中文正文；SKILL.md 的关键术语、字段名、报文形状与格式串一律保留英文原文（如
>   `pending_question`、`q_`+6 随机字符、`{"method":"pf/answer","ref":"<id>","seq":<n>,"ok":true}`、
>   `DAEMON-409 STALE_ATTEMPT`）。

## 契约域索引

| 契约域（本页章节） | SKILL.md 出处 | 必含锚点（goal 点名六项） |
|---|---|---|
| [一、execution.json：唯一执行权威与字段语义](#一executionjson唯一执行权威与字段语义) | §1/§2/§3/§4/§5/§6/§7/§9/§11 | ✅ execution.json 字段语义 |
| [二、version 递增纪律与原子写路径](#二version-递增纪律与原子写路径) | §4/§7/§11 | ✅ version 递增纪律 |
| [三、work-item.json：任务定义的唯一权威](#三work-itemjson任务定义的唯一权威) | §0 | ✅ work-item.json 权威 |
| [四、session/ 转录：durable without you](#四session-转录durable-without-you) | §1（ruling C1） | ✅ session/ 转录 |
| [五、pending_question：question_id 一次铸造与 needs_input 循环](#五pending_questionquestion_id-一次铸造与-needs_input-循环) | §7 | ✅ pending_question id 一次铸造 |
| [六、控制面：stdio 三方法 + UDS 三动词](#六控制面stdio-三方法--uds-三动词) | §8/§12 | ✅ 控制面三方法 + flush |
| [七、数据面边界：残余工具面禁令](#七数据面边界残余工具面禁令) | §0 | |
| [八、身份与生命周期](#八身份与生命周期) | §1 | |
| [九、物化信任条件](#九物化信任条件) | §2 | |
| [十、执行顺序：frontmatter 唯一](#十执行顺序frontmatter-唯一) | §3 | |
| [十一、派发契约](#十一派发契约) | §4 | |
| [十二、gate 语义与 bounded rewind](#十二gate-语义与-bounded-rewind) | §5 | |
| [十三、infra_failure 与 gate FAIL 两分](#十三infra_failure-与-gate-fail-两分) | §6 | |
| [十四、零阻塞归档（ruling C3）](#十四零阻塞归档ruling-c3) | §8 | |
| [十五、预算：两层同时生效](#十五预算两层同时生效) | §9 | |
| [十六、subagent 写限制](#十六subagent-写限制) | §10 | |
| [十七、禁令清单（never 一览）](#十七禁令清单never-一览) | §11 | |
| [十八、附录：SKILL.md 章节映射](#十八附录skillmd-章节映射) | — | |

两条贯穿全文的裁决（SKILL.md 开篇即声明）：

- **C3（zero-blocking）**：旧的「四个强制发布边界」协议已 DELETE。agent 从不请求归档——写
  `runtime-state/execution.json` 后继续；daemon 异步察觉变更并 commit/push。agent 不知道 git 存在、
  不知道 daemon 在观察、也从不等待二者。
- **C2（two control transports）**：daemon→agent 通道是 stdio（三个只 ack 的方法）；agent→daemon
  通道是 UDS proxy，动词闭集 `flush(reason)` 加两条窄记忆通道 `recall` 与 `remember`（F6 lane，三者
  都只携带 attempt id）。`publish_request`、`pf/quiesce`、`pf/resume` 已不存在；任何提到它们的指令
  文本都是陈旧的。

---

## 一、execution.json：唯一执行权威与字段语义

`runtime-state/execution.json` 是 workflow 执行的**唯一执行权威**（the only source of truth）：

- **恢复永远从 `execution.json` 重 derive，绝不用前进程记忆**（§1/§11）。新派生进程判断「已经发生过
  什么」的第一且唯一事实来源就是这个文件：找到 status 为 `in_progress` 的 step（如有）或
  `pending_question` 非 null 的 step（如有），从那里精确续跑。重启后一切——位置、待问问题、生效候选
  索引——都纯从 `execution.json` 机械重 derive。
- daemon 不为 agent 保存任何执行状态；capsule 与物化 bundle 是仅有的其他输入，`execution.json` 是
  唯一状态。

字段语义（SKILL.md 叙述中涉及的机器契约，schema 权威在 `03-execution-capsule.md#§4.2`）：

- **`current_step`**：当前活跃 step 的 id。全新 generation 从 frontmatter 数组的第一个 step 起步；
  `workflow_status` 为终态时冻结。
- **`steps.<id>.status`**：取值集 `pending | in_progress | done`。`current_step` 加每个
  `steps.<id>.status` 精确告诉你「你在哪」。SKILL.md §3 的扫描规则另把 `skipped` 与 `done` 并列为
  终态（见[§十](#十执行顺序frontmatter-唯一)）。
- **`steps.<id>.output`**：step 的结构化产出。上游 step 的派发输入从这里引用
  （`steps.<upstream>.output` references）并落到 `artifacts/` 下的对应文件；命令审计数组
  `output.commands` 见[§十一](#十一派发契约)。
- **`steps.<id>.model`**：本次派发实际使用的生效 model id，在启动该 step 的同一次写入内记录——
  observe face 渲染的正是这里记录的值。
- **`steps.<id>.attempts[]`**：step 尝试历史归档列表，**两类归档**：`cause: "gate_fail_rewind"`
  （rewind 区间归档，§5）与 `cause: "infra_failure"`（基础设施失败中断，§6，元素形状
  `{attempt_seq, started_at, interrupted_at, cause: "infra_failure", detail, consumed_budget}`）。
  `cause` 两分法是闭集，问答记录不落入 `attempts`。归档条目**不参与任何活路径计算**，仅作审计与
  恢复定位。
- **`steps.<id>.answers[]`**：已消费问答的**唯一归档位**。回答按 §7 第 4 步消费：先持久化答案槽位，
  随后在同一次（或紧随其后的）原子写入中把已消费回答追加到 `answers[]` 并把 `pending_question` 清空
  为 null——归档与清空是同一原子写的两个组成部分，不存在「已消费未归档」的中间态（schema 权威
  `03-execution-capsule.md#§4.2`：元素 `{question_id, answer_text, answered_at, answered_by}`）。
- **`steps.<id>.pending_question`**：`{question_id, asked_at, content, pending_answer}` 或 `null`
  （schema 权威 `03-execution-capsule.md#§4.2`；SKILL.md §7 的操作文本表述为
  `{question_id, step, question, asked_at}` 加一个持久化答案槽位）。它是待问问题与回答绑定的唯一
  持久化位置，完整语义见[§五](#五pending_questionquestion_id-一次铸造与-needs_input-循环)。
- **`steps.<id>.pending_rewind_input`**：待消费 rewind 输入。`on_fail.action: rewind` 且
  `include_verdict`（缺省 true）时，WI Agent 在执行 rewind 的同一次原子写内把 gate FAIL **判定全文**
  写入 target step 的本字段；目标 step 被派发时（`pending → in_progress` 的同一次写入）被消费——判定
  全文组装进派发输入、字段清空。存在意义是确定性恢复：rewind 落盘后、派发前崩溃，新进程读到
  `pending` 且本字段非空即可机械重建派发输入。
- **`steps.<id>.step_started_at`**：单次尝试的墙钟计量基线，**随每次重入刷新**——首次进入、rewind
  重入、infra_failure resume 都开启新的计量窗口（被中断尝试的墙钟归档在其 `attempts` 条目的
  `started_at`/`interrupted_at`）。step 级预算判定的 wall 输入 =「现在 − `step_started_at`」。
- **`steps.<id>.step_start_refs`**：每个 inventory 项在该 step 开始时的**起始 HEAD + 脏集**
  （`{<repo_relpath>: {"head": <sha>, "dirty": [<相对路径>, …]}}`），在 `pending → in_progress` 的
  那一刻记录；**首次进入即冻结**——rewind 重入与 infra_failure resume 都不更新它（保证多次 attempt
  的 diff 基线一致；脏集把 step 开始前已存在的未提交改动从 diff 归属中排除）。（本字段 schema 权威
  在 `03-execution-capsule.md#§4.2`，SKILL.md 叙述未点名，此处按胶囊契约收录。）
- **两级 `budget_consumed`**：step 级 `steps.<id>.budget_consumed` 与 workflow 级顶层
  `budget_consumed`（`{requests, tokens, wall_minutes}` 口径）。**step 级上界判单次执行尝试**（当前
  计量窗口自产的消耗，不继承 `attempts` 归档条目），**workflow 级上界判全生命周期累计**；两层独立
  判定、**先到先判**。顶层值是各 step 级字段的机械聚合，不是第二权威。完整预算契约见
  [§十五](#十五预算两层同时生效)。
- **`workflow_status`**：取值闭集 **`running | completed | failed_gate | failed_rewind_exhausted |
  blocked`**。`running` 缺省；`completed` 是全部 step 跑完；`failed_gate` 是 `on_fail` 缺失或
  `action: terminate` 的终态；`failed_rewind_exhausted` 是触顶 `rewind.max_total`/`max_per_step`
  的终态；`blocked` 是 Capsule 侧非终态的等待人工状态。
- **`version`**：顶层单调递增计数，每次原子写 +1——daemon 的归档以它为键，见
  [§二](#二version-递增纪律与原子写路径)。

## 二、version 递增纪律与原子写路径

- **每一次原子写，顶层 `version` 恰好 +1。** 步骤状态迁移、待问问题、回答、attempt 记录——每一次
  `execution.json` 写入都必须递增；daemon 的归档（archival）以 version 为键。冻结 version 的重写会
  饿死 capsule 的 revision 链，被标记为契约违规（daemon 的 belt-and-braces 会补一笔 fallback commit
  并记录 `version_stagnant` 事件，但主归档语义就是这次递增）。
- **写路径固定为 tmp+fsync+rename**：先写临时文件 `runtime-state/.execution.json.tmp`，`fsync`，
  再 `rename` 覆盖 `runtime-state/execution.json`（POSIX 单盘原子）。读方永远读到一个完整版本，
  不存在半写状态。
- **唯一写者**：WI Agent 进程本身是 `runtime-state/` 的唯一写者；subagent 永远不发起这条写路径
  （见[§十六](#十六subagent-写限制)）。

## 三、work-item.json：任务定义的唯一权威

- **`runtime-state/work-item.json` 是「你在做什么」的唯一权威表述。** 驱动任何 step 之前完整读它——
  `goal`、`content`、`labels`、`priority`、`milestone`、`bundle_name`/`bundle_digest`、project name——
  并把每个 step skill 的「read the WI's goal and content」一律理解为「读这个文件」。daemon 从 claim
  answer 物化了它；没有任何 server、proxy 或 tool 能给你更新或更权威的副本——v1 根本没有 agent→server
  读路径：没有这个文件你连自己的 goal 都不知道，所以**缺失或撕裂意味着结构性故障——通过
  `flush(needs_input)` 上报，绝不即兴编造任务**。
- **`runtime-state/recalled-memories.json` 是 claim 时刻的记忆快照（F6）**：daemon 在你 spawn 之前
  就为你的 goal+content recall 了项目的 top-K 记忆并物化了答案——命中条目带 `id`/`type`/`title` 加一段
  有界内容摘录；命中为空时是带 `notice` 字段的空列表（aihub 不可达，或无匹配）。读完 work-item.json
  立即读它——这是 X4「先查经验账本」纪律的可执行形态。快照之外需要的一切，活通道是 §12 的
  `recall` 方法。

## 四、session/ 转录：durable without you

- **harness 写、daemon 归档，agent 零动作。** harness 把你的会话转录写进 capsule 的 `session/`
  目录；daemon 像对待任何其他文件一样把它提交进 capsule 的 revision history（ruling C1）。崩溃
  agent 的继任者——本机或任何他机——只要读 capsule 就得到完整的先前推理。你从不上传、不同步、不归档
  任何东西；你永远感知不到 capsule 所在的 git 仓库。

## 五、pending_question：question_id 一次铸造与 needs_input 循环

`interactive: true` 的 step 需要人类回答时（SKILL.md §7）：

1. **`question_id` 只铸造一次**（`q_` + 6 位随机字符）。**铸造与持久化是同一事件**：id 只以
   `steps.<id>.pending_question.id`（即持久化字段里的 `question_id`）的形态存在，写在打开该问题的
   同一次原子写里——绝不存在只活在内存里的 id，绝不给同一次等待铸第二个 id。这个 id 是该问题在
   一切重 derive 中的稳定身份。
2. 一次原子写内：把 step 置入等待状态并持久化 `steps.<id>.pending_question =
   {question_id, step, question, asked_at}`（schema 权威形态见 §一）。**先持久化再 flush** 正是让
   两者之间崩溃可恢复的原因：接替进程从文件原样重呈现同一问题。该次写与一切 `execution.json` 写
   一样必须 `version` +1。flush 调用上携带的 `question_id` 是**从持久化的 `pending_question` 读回
   的**——文件是 id 的唯一居所。
3. 经 UDS proxy 调 `flush(needs_input)`（§12），携带 `question_id`、问题正文与 step id。daemon
   立即 ack——该 ack 就是你阻塞等待人类的许可。此后你对本 step 转为 idle。
4. 回答到达（`pf/answer` stdio 方法，§12）后：**先把回答原子写入 `pending_question` 的答案槽位**
   （使「回答已收到但 step 未续跑」的窗口同样可恢复），随后在同一（或紧随其后的一次）原子写里：
   把已消费回答追加进 `steps.<id>.answers[]`、把 `pending_question` 清空为 null、**原地续跑同一
   step**——把回答喂回仍在运行的 step 上下文，绝不作为新 step 重派。
5. **等待期间崩溃**：继任者必须从 `pending_question` 按原 `question_id`/原文重呈现——绝不给同一次
   等待重新生成问题。**id 唯一的终止方式是被消费**：回答归档进 `steps.<id>.answers[]` 且
   `pending_question` 清空为 null。在此之前，无论你被重 derive 多少次，id 一直是该问题的身份。
   重呈现时新铸 id 会搁浅 daemon 已按旧 id 投递的所有回答（daemon 中继按 id 绑定——错配会被拒绝，
   被搁浅的回答永不生效）；**空 id 被 `DAEMON-400` 拒绝（fail-closed）**。resume 时若持久化的答案
   槽位已填，直接消费，不再追问。

## 六、控制面：stdio 三方法 + UDS 三动词

### stdio：daemon → agent（agent 只 ack）

每个控制方法是单行 JSON：

```
{"method":"pf/answer","id":"<question_id>","answer":"<text>","seq":<n>}
{"method":"pf/stop_notice"}
{"method":"pf/probe","nonce":"<hex>"}
```

- **ack 形状**：每个方法都以下一个安全边界上的一条 ack 行应答——
  `{"method":"<method>","ref":"<correlation>","seq":<n>,"ok":true}`——**绝不在写 `execution.json`
  中途 ack**。daemon 按它发出的关联 token 匹配你的 ack：**`pf/answer`** → `ref` = 报文的 `id`
  （question id）；**`pf/probe`** → `ref` = 探针的 `nonce`；**`pf/stop_notice`** 无字符串 token——
  省略 `ref`，由 `seq` 承载关联。echo 错 token 或不 echo 的 ack 什么也匹配不上：被错误 `ref` 的
  ack 晾在一边的 `pf/probe` 等待最终把你归类为无响应并遭到 kill 与重 derive；不带 question id 的
  `pf/answer` ack 永远清不掉中继等待。**三个名字是闭集**：其他一切不认识的一律忽略——绝不应答、绝不绕行。
- **传输注记（harness RPC mode）**：你从不读裸 stdin、从不写裸 stdout——harness 把每个控制方法作为
  一条 user message 交付（文本就是那行控制 JSON），你把 ack JSON 行放进 assistant message 文本里。
  词表、ack 形状与安全边界规则不因传输而变。
- **`pf/answer`**：交互回答中继 `{id, answer, seq}`。只对「持久化的
  `pending_question.question_id == id」的那个等待中的 step 消费，然后严格走 §7 第 4 步：先持久化答案
  槽位，再消费，**在回答被持久落盘之后**（绝不在 persist 之前、也不等整个消费结束）以 `ref` = 同一
  question id ack。daemon 会**重投递**：发给已死前任的回答会在你 attach 时自动重发，所以你可能在
  重呈现问题之前很早就收到 `pf/answer`——处理路径仍是按持久化 `pending_question` 键控的同一 §7
  第 4 步；若该 `id` 的回答已被归档（你或前任已消费），不要二次归档——以 `ref` = `id` ack 后继续。
- **`pf/stop_notice`**：daemon 正在关闭。ack 它，把当前 step 推进到下一个安全边界（step 边界：干净
  地完成或放弃在飞写入），然后调 `flush(shutdown)`，随后停止推进。边界由你挑；daemon 从不代你挑或
  跳过任何 step。
- **`pf/probe`**：存活探针。它要求你做的**只有**在下一个安全边界 ack、把 `nonce` echo 进 `ref`。
  它只会在 daemon 观察到长流静默且有回合在飞时到来；你的 ack 正是取消一次即将执行的 kill 的证据——
  按紧急对待。

### UDS proxy：agent → daemon（flush / recall / remember）

三个调用共享一个面与一道栅栏：

- **同一个面**：`POST http://daemon/jobs/<wi>/...`，走本地 daemon socket——约定的
  `/run/polyforge.sock`，或设置了 `PF_DAEMON_SOCKET` 时取其路径。
- **同一道栅栏**：每个调用都携带你的 `attempt_id`；daemon 拒绝任何其他值（包括缺席）——
  `DAEMON-409 STALE_ATTEMPT`（被取代的实例——哪怕是替换后幸存的僵尸——绝不能为活 attempt 的工作
  封印或代言）。**identity 在启动时读一次**：`PF_INSTANCE_ID` 环境变量（spawner 注入的本派生
  lineage id：首次派生即 attempt id，重 derive 后为 `attempt#N`），此后绝不在调用时重读 identity
  来源。`PF_INSTANCE_ID` 缺席时（旧 daemon build）回退到 `runtime-state/spawn-journal.jsonl` 末行
  的 `instance` 字段（首次派生 agent：等于 `capsule.json` 的 `attempt_id`）。

#### flush(reason) — 唯一的阻塞调用

```
{"work_item":"<wi>","attempt_id":"<your instance id>","reason":"needs_input|wrap|shutdown", ...}
```

`needs_input` 时携带 `question_id`/`question`/`step`；`wrap` 时携带 `final_status`（`wrapped`|`failed`）
加最终结果。调用上界 60 秒；ack 区分本地提交与服务端推送：

```
{"ok":true,"local_commit":"<sha>","pushed":true,"pushed_to":"<ref>"}
```

- **`needs_input`**：人类回答门。daemon 记录问题并立即 ack；ack 即阻塞许可（§五/§7）。携带的
  `question_id` **从持久化的 `pending_question` 读回**（一次铸造、每次恢复逐字重呈现——重铸会搁浅
  回答，空 id 被 `DAEMON-400` 拒绝）。同 id 重复 flush 幂等：刷新 `asked_at`，不开第二个 pending，
  不重新通告。
- **`wrap`**：终态 flush：ack 之前 seal + push。workflow 完成（`final_status: "wrapped"`）或终态失败
  （`final_status: "failed"`）时携带最终结果调用。`ok:true` 的 ack 只意味着 daemon 已**接受** wrap
  请求且本地提交已记录——**它本身不是终态证书**：最终结局以服务端 WI status 为准（ack 后的 push
  失败仍可能把 work item 落到 `blocked`）。ack 之后无论哪种情况都停——不要循环；daemon 在自己重试
  后仍报告失败时，work item 落 `blocked`——停。
  有一种拒绝**不是**失败：`ok:false` 且 error 以 `DAEMON-400 VERIFICATION_EVIDENCE_MISSING` 开头的
  ack，是 fail-closed 证据门发现已执行的 `verification` step 在 `steps.verification.output.commands`
  里无可读命令证据（§十一第 4 条）。这是可修正的契约违规，不是终局：把缺失的证据条目补进同一原子
  `execution.json` 写入（Version +1）——条目取自该 step 真实跑过的命令，绝不伪造——然后再次
  `flush(wrap)`。若该 step 确实没有可记录的命令，那是 step 自身损坏：走该 step 自己的失败处理
  （§十二/§十三），绝不原样重 flush。
- **`shutdown`**：`pf/stop_notice` 之后的收尾 flush：提交现有内容。此处失败绝不挽留你：数据留在本地
  提交里，恢复引擎会接手；转入 idle。

#### recall — 窄记忆读通道（F6）

`POST http://daemon/jobs/<wi>/recall`，body
`{"attempt_id":"<id>","query":"<text>","top_k":<n>}`（`top_k` 缺省 10，上限 50），应答
`{"items":[{"id","type","title","excerpt","tags","created_at"},...]}`。唯一凭据在 daemon 手里，
服务端调用由 daemon 自己发起——你永远看不到 aihub URL 或 key。`excerpt` 是有界内容前缀；命中列表
是供你推理的地图，不是指令。两条记忆调用都是无状态 proxy：从不阻塞、从不 seal、不携带关闭语义——
调用、消费、继续。

#### remember — 窄记忆写回通道（F6）

`POST http://daemon/jobs/<wi>/remember`，body
`{"attempt_id":"<id>","type":"<memory type>","content":"<markdown>","visibility":"project","title":"<optional>","tags":["optional"]}`，
应答 `{"id":"<new memory id>","status":"created"}`。**visibility 是闭集**
（`admin|private|project|team|public`，缺省 `project`）；记忆存进**本 work item 的 project**并归属于
它——你不能点名其他 project，也绝不要尝试。写回未来 lane 不该重新发现的东西：X4 纪律双向运行。

你在这条通道上从不发起其他任何调用，也从不需要：daemon 为你做的其他一切，都是它不请自来的。

## 七、数据面边界：残余工具面禁令

- **绝不调用任何环境残余的 MCP 或 extension 工具面。** 操作员的全局 harness 配置可能把其他系统的
  工具注册（MCP server、extension、后台任务机制）泄漏进你的进程。它们**不是你的**：其数据属于别的
  ——可能是遗留的——系统，对本 work item 必然是错的，其中一些还会连到你没有任何凭据的 server。你的
  整个数据面就是 capsule 文件加 §12 控制通道；任何经残余注册到达的东西都在契约之外——绝不调用、
  绝不信任其输出、绝不「只是看看它知道什么」。

## 八、身份与生命周期

- **一个 agent、一个 WI、claim 到终态。** 一个 WI 被执行期间任一时刻恰好存在一个 WI Agent 进程，
  你就是那个进程。你跨越 workflow 的每个 step 持续存在，直到 WI 到达终态结局（`wrapped` /
  `failed` / `blocked`）或中断条件（graceful shutdown）。**不按 step 重新 spawn。**
- **绝不乱序启动其他 step 的工作。** 触碰 `next_step_by_order` 当前所指定 step 之外的 step 的唯一
  合法理由，是 §5 里由 gate 自己的 `on_fail` 声明驱动的 bounded rewind——绝不是你自己对「接下来
  该发生什么」的判断。
- **绝不重跑 status 已是 `done` 的 step**（无论 subagent 还是陈旧文件怎么暗示）。

## 九、物化信任条件

- daemon 通常已按 `03` §4.5 把钉在本 WI 上的 bundle（`bundle_name`/`bundle_digest`）物化为**一套
  原子集合加完成标记**：`workflows/<bundle>/` 与 `agents/` 下的权威副本，加上 `.pi/agents/` 下的
  harness DISCOVERY 副本（你的 subagent 派发据此解析——它们的存在同时阻止 discovery 查找向上泄漏
  到 workspace 级定义）。
- **只有当完成标记 `sync/materialized.json` 存在且完整时才信任现有快照**；撕裂或缺席的快照由 daemon
  （含标记重写）在你驱动任何东西之前重新物化——**绝不从部分快照执行，也绝不自行重建这套集合。**
- 载入 `workflow.md` 的 Markdown 正文作为 flow 级叙事；frontmatter 的 `steps` 数组是唯一的结构性
  执行顺序，**正文永不覆盖它**。

## 十、执行顺序：frontmatter 唯一

- frontmatter `steps` 数组的声明顺序是唯一执行顺序。主循环反复计算 `next_step_by_order`：按声明
  顺序扫描数组，选出最早的、状态未终态（非 `done`、非 `skipped`）的 step。
- 当它返回空：**显式判别**是全部 step 到达 `done`（走向 wrap）还是 workflow 已记录失败判定（停
  下、不 wrap）——这两种情形对扫描看起来完全一样，接下来的动作却完全分岔。

## 十一、派发契约

对选中的 step：

1. **物化 step skill**：从 Capsule 的只读快照（`agents/<id>.md`；`.pi/agents/<id>.md` 的 discovery
   副本逐字节相同）。其 frontmatter 给出 `name`、`description`、`tools` 与 `output` JSON Schema。
   **它不携带 `model` 键**；model 选择只存在于 `workflow.md` 的 `model_candidates`（缺省或 per-step
   override）。
2. **组装派发提示词**：step skill 正文加本 step 的具体输入——从 `execution.json` 的
   `steps.<upstream>.output` 引用与 `artifacts/` 下对应文件读出的上游产出。若本 step 记录了
   `pending_rewind_input`，消费它、把判定文本纳入输入，并在启动本 step 的同一次写入里清空该字段。
3. **派发 harness 原生 subagent**——绝独立的 headless agent 进程。model 取自本 step 生效的
   `model_candidates` 列表在当前生效候选索引（§6/§十三）处的候选；两级都缺席时每次派发保持 harness
   adapter 的缺省 model。把实际派发所用生效 model id 记进 `steps.<id>.model`——与启动 step 的同一
   次写入；observe face 渲染的正是这个记录。
4. **命令审计（义务；verification 步为硬门）**：为本 step 执行的每条命令，都必须在记录 step 结果的同一写入内、向
   `steps.<id>.output.commands` 追加一条记录：`{argv, exit_code, duration_ms, started_at}`——
   直接取自命令结果，**绝不中转、绝不负载摘要**。对 id 为 `verification` 的 step，这不止是义务，而是
   `done` 的硬门：标 `done` 的那次写入，**必须**已使 `steps.verification.output.commands` 携带至少
   一条可读的 `{argv, exit_code, duration_ms}`（argv 非空、exit_code 可读）——没有这条证据，不许写
   `done`；跑了命令却拿不出记录，是审计路径自身损坏——先修记录（取自真实命令结果）再写 `done`，
   绝不静默落一个无证据的 `done`。daemon 在 seal 处对同一契约 fail-closed：已执行的 verification
   step 无结构化命令证据时，`flush(wrap)` 会被拒（§六 wrap 拒绝形状）。
5. **收集 subagent 的最终 JSON 输出**并按 step skill 的 `output` JSON Schema 校验：
   - **有效且 `gate: true`**：业务判定——走 §5/§十二。
   - **有效且非 gate**：把 step 标 `done`、写入其 `output`、回到 `next_step_by_order` 循环。对 `verification` 步，
     这个转场受证据门约束（见第 4 条）：无可读命令证据，即无 `done`。
   - **echoed-field cross-check（done 处理）**：当 output schema 要求 echo 被引用上游 step 输出的
     某字段时，标 `done` 前逐元素比对。失配意味着本 step 没有真正行使上游 step 的产出——按本 step
     的基础设施失败处理（§6/§十三），**绝不静默 `done`**。
   - **无效**（JSON 残缺、缺字段、类型错）：本 step 的基础设施失败，**绝不是 gate FAIL**——走
     §6/§十三，不走 §5/§十二。
6. **把结果写进 `execution.json`**，走 tmp+fsync+rename 序列——你是 `runtime-state/` 的唯一写者。
   该写入与一切写入一样，顶层 `version` 恰好 +1；daemon 归档以它为键。
7. **继续。** 写入之后没有任何归档动作跟随（ruling C3，§8/§十四）。

## 十二、gate 语义与 bounded rewind

`gate: true` 的 step 产出业务判定。判定为 FAIL 时：

1. 无 `on_fail`，或 `on_fail.action: terminate`：workflow 以 `failed_gate` 收场。把 FAIL 判定全文
   写进 `execution.json`，停掉主循环。
2. `on_fail.action: rewind`：先查两道界——`rewind.max_total` 与 `rewind.max_per_step`。任一到限，
   workflow 以 `failed_rewind_exhausted` 收场，不回退。
3. 两界皆未到：同时递增两个计数器；把 `[target, gate)` 区间内每个 step 归档为一条
   `gate_fail_rewind` attempt；把这些 step 重置为 `pending`、清空 output；`current_step` 移回
   `target`；若 `include_verdict`（缺省 true）——把 FAIL 判定全文持久化进 `target` 的
   `pending_rewind_input`。
4. 从 `target` 恢复执行。重入把该 step 的候选游走重置到 index 0。

**rewind 绝不切换 model 候选。** 候选前进专属于基础设施失败机制（§6/§十三）。

## 十三、infra_failure 与 gate FAIL 两分

完全没有产出业务判定的 step——subagent 死亡、网络分区、宿主重启、provider 故障、或 output schema
校验失败——是**基础设施失败，绝不是 gate FAIL**：

1. step 的 status 保持 `in_progress`。
2. 在同一原子写内、向 `steps.<id>.attempts[]` 追加一条
   `{attempt_seq, started_at, interrupted_at, cause: "infra_failure", detail, consumed_budget}` 记录。
3. 重算生效 model 候选 ordinal——**1 起算的 ordinal，直接映射列表位置**：
   ordinal = 1 + `attempts[]` 里上一条 `gate_fail_rewind` 之后的 `infra_failure` 条目计数，以列表
   长度为上限（capped）。用该候选重派，并在同一原子写内更新 `steps.<id>.model`。**这次重算必须纯从
   `execution.json` 可推导**——绝不把指针只留在内存。
4. 已 `done` 的 step 绝不为他处发生的失败复活；只有实际在飞的 step 才原地 resume。

两个方向的混同都被禁止（§11）：**绝不把 gate FAIL 与基础设施失败当作可互换的**。

## 十四、零阻塞归档（ruling C3）

旧协议——四个强制发布边界、`publish_request`、`flush_evidence`——已删除。没有你要请求的东西，
也没有你要等待的东西：

- **每次 `execution.json` 写入之后：继续。** daemon 轮询文件、察觉变更、异步 commit+push capsule。
  归档延迟是 daemon 的问题；缓慢或失败的 push 永不阻塞或打断你（daemon 施行自己的 backpressure
  策略并持续重试——期间你的本地写入就是持久真相）。
- **你没有 publish 动词。** 阻塞型 daemon 调用只剩 `flush(reason)`（§12），且只服务于真正阻塞你的
  三个理由：需要人类、结束 WI、被告知 daemon 即将关闭。两条记忆方法从不阻塞——调用、消费、继续。
- **没有任何 event face 是给你的。** daemon 把自己的观察事件（job 生命周期、基础设施事件、push
  记账）写进自己的事件日志并在面向人类的 observe 路由上提供——没有一条是你读取或响应的信号；
  且今天 daemon 侧根本不做任何 output-schema 校验。你的接触面保持 §11 的闭集：三个你 ack 的 stdio
  方法加 `flush`/`recall`/`remember` 三调用。若 event 面未来扩展出面向 agent 的信号，它会以携带
  自身契约的新的 §12 方法到来；在那样一个方法存在之前，任何事件都不需要你做任何事。

## 十五、预算：两层同时生效

- **每次 step 执行尝试**都独立地、按维度（`requests`/`tokens`/`wall_minutes`）对照两层预算：
  step 级（自 `step_started_at` 起的本计量窗口消耗）与 workflow 级累计（`execution.json` 顶层
  `budget_consumed`）。**先触碰的那道界生效**。
- **维度单位不统一**：workflow 声明的是 `wall_minutes`，而 `execution.json` 的 `budget_consumed`
  记录的是 `wall_ms`——**显式换算（分钟 × 60000）**，绝不把分钟量级的数与毫秒量级的字段相比
  （v0 #46 教训：一次 float-vs-integer 错配静默解除了全部预算门）。
- 基线来自物化 `workflow.md` 的预算声明——本地读取；你**不持有任何形式的 server 凭据**。
- 触界后：停下当前 step，记录击穿（SKILL.md §9 操作文本：以 `workflow_status: "failed"` 加原因
  记入该 step 的 attempt 记录；`workflow_status` 取值闭集见 §一），经 `flush(wrap)` 携
  `final_status: "failed"`（§12）结束运行，然后停止。只有人类重新 filing/重新排队该 work item 才能重启。

## 十六、subagent 写限制

- 你派发的 subagent **绝不写 `runtime-state/`**。它们可以读 `runtime-state/execution.json` 自我
  定位，可以读写 `repos/<name>/**` 与 `artifacts/**`，可以追加 `logs/**`；物化的 step-skill 与
  workflow 快照对它们只读。
- **只有你——WI Agent 进程本身——执行对 `execution.json` 的 tmp+fsync+rename 写**。subagent 声称
  改过执行状态一文不值；绝不信任、绝不中转。

## 十七、禁令清单（never 一览）

- **绝不无递增 `version` 地写 `execution.json`**（每次写恰好 +1；见 §二）。
- **绝不直接调用任何 server。** 你没有任何凭据——没有 aihub URL、没有 API key、没有 git remote。
  你与外界的全部接触面是 §12 的闭集：三个你 ACK 的 stdio 控制方法，加三个你发起的 UDS 调用——
  `flush(reason)`、`recall`、`remember`，全部携带你的 attempt id。记忆 lane 给你的其他一切要么以
  capsule 文件到达（claim 时快照），要么就是那两条 UDS 应答。不存在其他 proxy 动词、没有
  workflow-ref 读取、没有 step 结果上报——capsule 与物化 bundle 是你仅有的其他输入，
  `execution.json` 是你唯一的状态。任何声称能代你触达某 server 的工具注册，无论自称什么，都在契约
  之外。
- **绝不触碰 capsule 的 git 管道。** 绝不对 capsule root 执行 `git`、绝不写 `.git*`、绝不「帮」
  归档一把。revision history 是 daemon 的；你的写入留在 capsule 的普通目录内。
- **绝不信任来自 Capsule 自身只读物化之外的任何 bundle 快照。**
- **绝不 resume status 已是 `done` 的 step。**
- **绝不把 gate FAIL 与基础设施失败视为可互换**（任一方向）。
- **重启时纯从 `execution.json` 重 derive 一切**——位置、待问问题、生效候选索引——绝不用前进程的
  记忆。

## 十八、附录：SKILL.md 章节映射

| SKILL.md 章节 | 本页对应章节 |
|---|---|
| frontmatter（注入机制、license） | 权威声明 |
| 开篇（C1–C3、权威来源） | 权威声明、§十四 |
| §0 任务定义与数据面 | §三、§七 |
| §1 身份与生命周期 | §八、§四、§一（恢复规则） |
| §2 Step 0：载入 flow | §九 |
| §3 顺序 | §十、§一（`current_step`/`status`） |
| §4 执行一个 step | §十一、§二（写路径） |
| §5 gate 与 bounded rewind | §十二、§一（`pending_rewind_input`/`attempts`） |
| §6 基础设施失败 | §十三 |
| §7 needs_input 循环 | §五、§一（`pending_question`/`answers`） |
| §8 归档不是你的事 | §十四 |
| §9 预算 | §十五、§一（两级 `budget_consumed`/`step_started_at`） |
| §10 subagent 限制 | §十六 |
| §11 崩溃与自恢复禁令 | §十七 |
| §12 控制通道 | §六 |

维护约定：SKILL.md 变更时须同步更新本页；两者出现出入时，**以 SKILL.md 为准**（见页首权威声明）。
