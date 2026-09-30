# v1 Pilot Wrap 登记簿 <!-- v1 wrapped register -->

- **性质**：v1 pilot 观测窗（#11 起）全部 wrapped 票的**逐票证据登记簿**——每票一段、四个固定节拍（**目标一句 / 交付物 / 关键裁定 / 发现**），段尾附机械证据引用。本文是 phase-0 报告底稿 `docs/pilot/v1-phase-0-report.md`（M1–M9 读数全为「待填」）的**证据索引**：只登记证据落点，**不产生任何 M1–M9 读数、不判定任何 `PILOT-V1`…`PILOT-V8` 门槛、不在观测数据之外认证 Pilot 通过或失败**（与报告底稿、#11 偏差登记表同姿态）。
- **权威边界（照述，不越界）**：判定公式与指标口径的唯一权威 = `docs/design/polyforge-vnext/07-deployment-pilot.md` §9.1（Pilot 范围与观测窗口）/§9.2（M1–M9 口径，INV-D5 窗口/分母）/§9.2a（M4/M5 机械证据链）/§9.3（八条否决门槛表与 M9 观测项收尾注）；本文不校准、不改写、不重排任何门槛公式（§12 第 1 条：校准不改判定式）。v1 偏差权威 = `docs/pilot/v1-phase-0-deviation-register.md`（#11）；`docs/pilot/phase-0-deviation-register.md` 是 **v0** 登记表，不是 v1 的登记权威，本文不以其为内容来源。v1 窗口计数起点 = 2026-09-30 v1 装机重新计数（报告底稿 §3）；v0 已 wrap 的 7 张不入窗。
- **数据源（仓库外，只读）**：① capsule 面 `/root/.local/share/polyforge/workspace/capsules/<wi_id>/`——`runtime-state/work-item.json`（slug/goal/bundle）、`runtime-state/execution.json`（`steps.<id>.status/model/answers/output`、`rewind_stats`、`budget_consumed`、`workflow_status`）、`artifacts/`（各步 memo）；② 裸仓链 `/root/polyforge-pilot-v1/b/git/wi_<id>.git`；③ main 仓历史（`Merge pull request #<n> from silgrid/polyforge/wi_<id>` merge commit 与其合入内容）。
- **取数快照时点**：2026-09-30T13:46Z（code 步执行时点重跑 §1 机械枚举；§2/§3 证据于同一次 code 步会话内抽取）。快照声明：daemon 仍在运行、capsule/裸仓面是活面（计划期 #47 的裸仓即在两次检查之间新出现）；本判定式产出的是**取数时点的快照**——若此后某张原排除票按同一判定式达成三条，其补录属后续更新，不在本登记簿内追改。
- **无裸仓的 4 张**：#11（wi_RrKTmUqYzift）、#22（wi_3uxqBceRVcFh）、#23（wi_Xah1Y0REajOb）、#44（wi_ibYL3MtSq0Lp）无裸仓——裸仓归档面在 v1 中途才建立/回补；此 4 票按 merged PR + capsule `execution.json` 取证，§2 证据表逐票如实标注「无」。这是登记事实，不是本登记簿要修的缺陷。

---

## 1. 机械判定式与票集

**判定式（三条同时成立）**：ticket polyforge#N 属于 v1 wrapped 集 ⇔

1. **capsule 在位**：`/root/.local/share/polyforge/workspace/capsules/wi_<id>/` 存在，且其 `runtime-state/work-item.json` 的 slug = `polyforge#N`，**N ≥ 11**（v1 窗口起点；#9 已 cancel、#10 未执行，均不入窗）。
2. **v1 期 merged PR 在案**：main 仓 `git log` 含 `Merge pull request #<pr> from silgrid/polyforge/wi_<id>`，且该 merge commit 日期 **≥ 2026-09-30**（v1 装机日）。此日期过滤机械排除 4 个 v0 期同名分支式 merge：PR #49（wi_Aeg09AdVrWKV，`e866325`，2026-09-28）、PR #50（wi_MDRnpGncmOGi，`e5270bd`，2026-09-28）、PR #51（wi_RRwQr7XwI2go，`e791703`，2026-09-29）、PR #54（wi_xwPgC69A0Xsx，`035fb2e`，2026-09-29）——其 capsule 均不在本工作区（反向核对：窗内 34 个 merge 全部有本地 capsule，无一缺位）。
3. **capsule 步面终态**：`execution.json` 的 `steps` 中每个已记录 step 的 status 均为 `done`（无 `in_progress`/`pending`）。`workflow_status` 字段值只作参考、§2 照录原值——本地文件可滞后于服务端状态（如 #22 照录为 `running` 而步面已全 `done` 且 PR 已合并）；**步面终态 + merged PR 才是机械判据**。

**裸仓链为佐证而非必要条件**：34 张中 30 张的裸仓 HEAD 为 `wrap: wrapped`（最强 wrap 标记）；上述 4 张无裸仓（见头部要点）。

**执行时点枚举结果（34 张，slug 升序）**——与计划期枚举**完全一致**，无新纳入、无剔除变动：

#11（wi_RrKTmUqYzift）、#12（wi_Pr1snK01qDS8）、#13（wi_DLzwgbXI2YG2）、#15（wi_cnTF84S2eEpd）、#16（wi_xpI1NzK8C36O）、#17（wi_cVWghIMEIuiQ）、#18（wi_PstymWtmKFAt）、#19（wi_qxRRoSbpFblJ）、#20（wi_hPl82OPb7V3q）、#21（wi_0k05ak9v95dS）、#22（wi_3uxqBceRVcFh）、#23（wi_Xah1Y0REajOb）、#24（wi_g3QsCdv1joZ8）、#25（wi_5x0CNtmiRMXJ）、#26（wi_3WWHkJNT5tiA）、#27（wi_xzrlpQQWRyLY）、#28（wi_qRJFlbdZOuEf）、#29（wi_av0LZhpUbqRY）、#30（wi_fE9931dzUMv2）、#31（wi_f50ZPPKYiXrI）、#32（wi_tEnGWCxYEXzw）、#33（wi_MuyoyHwOewS7）、#34（wi_XlhUr5fXLjO1）、#35（wi_dDCz1MjpczvZ）、#36（wi_ihbMI4rH43er）、#37（wi_cVhdv3IInCRq）、#38（wi_jwVtx8w5XDXE）、#39（wi_izqGJFHcCaeH）、#40（wi_L2ZyiXbEectN）、#41（wi_s11m8pp8Pdw9）、#42（wi_hVYd0WqGUrPq）、#43（wi_TobxjUMungXl）、#44（wi_ibYL3MtSq0Lp）、#45（wi_8cGon3rDrypq）。

**排除表（执行时点逐票核验照录）**：

| 票 | capsule | 排除理由（执行时点照录） | 证据锚点 |
|---|---|---|---|
| #9 | 目录 `polyforge#9/`（slug 拼写目录；`runtime-state/` 下仅 `spawn-journal.jsonl`，无 work-item.json/execution.json） | v0 期、已 cancel；判定式第 1 条不成立（capsule 内无 slug 权威文件，#9 亦 < 11） | capsule 目录 `polyforge#9/runtime-state/`；#11 偏差登记表 §9.2（F6）提及「#9（已 cancel）」 |
| #10 | wi_gyXc0y9lX7gk | v0 期未执行：无 `runtime-state/execution.json`、main 仓无该 wi 的 merged PR、无裸仓 | capsule 目录；main 仓 `git log`（无 `from silgrid/polyforge/wi_gyXc0y9lX7gk`） |
| #14 | wi_ZiKzuDaiGpgZ | quick-fix 进行中：grill `in_progress`、fix/verification/release `pending`；无 merged PR；裸仓 HEAD `seal: paused`——paused 非 wrapped | `execution.json` steps；裸仓 `wi_ZiKzuDaiGpgZ.git` HEAD |
| #46 | wi_DNsIyHZwZQB5 | bug-fix running：repro `in_progress`、后续步 `pending`；无 merged PR | `execution.json` steps；裸仓 HEAD `observe: execution v1` |
| #47 | wi_7ORyHe0iLg2H | chore running：plan `done`、code `in_progress`、verification `pending`；无 merged PR。计划期为「仅 spawn 无 execution.json」；执行时点 capsule 已有 execution.json 但 code 步仍在跑——三条判据仍不成立，维持排除 | `execution.json` steps；裸仓 HEAD `observe: execution v2` |
| #48 | wi_VCyBYf7uYeb2 | quick-fix running：fix `in_progress`、verification/release `pending`；无 merged PR | `execution.json` steps；裸仓 HEAD `observe: execution v7` |
| #49 | wi_pyBtx4KHYyFV | quick-fix running：grill `in_progress`、后续步 `pending`；无 merged PR | `execution.json` steps；裸仓 HEAD `observe: execution v6` |
| #50 | wi_340KBOZg8gXm | 本 WI（chore，本登记簿自身），running：plan `done`、code `in_progress` | `execution.json` steps；裸仓 HEAD `observe: execution v2` |

## 2. 证据一览表

每票一行；34 个 merge 的日期全部为 2026-09-30（v1 装机当日，即全部落在窗内）；`workflow_status` 照录 execution.json 原值（含滞后值，见 §1 判定式第 3 条）；「裸仓链」列 = `/root/polyforge-pilot-v1/b/git/wi_<id>.git` 在否与 HEAD 终态。

| slug | capsule wi_id | merged PR / merge 短哈希 | 裸仓链（HEAD 终态） | workflow_status（照录） | bundle |
|---|---|---|---|---|---|
| polyforge#11 | wi_RrKTmUqYzift | PR #60 / `45276ac` | 无 | completed | quick-fix |
| polyforge#12 | wi_Pr1snK01qDS8 | PR #63 / `8231175` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#13 | wi_DLzwgbXI2YG2 | PR #62 / `b4c5222` | 在（`wrap: wrapped`） | wrapped | chore |
| polyforge#15 | wi_cnTF84S2eEpd | PR #61 / `eb7ba44` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#16 | wi_xpI1NzK8C36O | PR #67 / `1834347` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#17 | wi_cVWghIMEIuiQ | PR #71 / `831f880` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#18 | wi_PstymWtmKFAt | PR #66 / `633b312` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#19 | wi_qxRRoSbpFblJ | PR #70 / `af6b8de` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#20 | wi_hPl82OPb7V3q | PR #65 / `6945e9f` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#21 | wi_0k05ak9v95dS | PR #68 / `5086740` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#22 | wi_3uxqBceRVcFh | PR #64 / `73fff77` | 无 | running | chore |
| polyforge#23 | wi_Xah1Y0REajOb | PR #69 / `2db3aa4` | 无 | completed | quick-fix |
| polyforge#24 | wi_g3QsCdv1joZ8 | PR #72 / `ddd21d2` | 在（`wrap: wrapped`） | wrapped | chore |
| polyforge#25 | wi_5x0CNtmiRMXJ | PR #73 / `cbe9e89` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#26 | wi_3WWHkJNT5tiA | PR #74 / `c01a087` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#27 | wi_xzrlpQQWRyLY | PR #79 / `feaf127` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#28 | wi_qRJFlbdZOuEf | PR #75 / `63efe05` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#29 | wi_av0LZhpUbqRY | PR #77 / `5b6c396` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#30 | wi_fE9931dzUMv2 | PR #76 / `d50c410` | 在（`wrap: wrapped`） | all_done | quick-fix |
| polyforge#31 | wi_f50ZPPKYiXrI | PR #78 / `2ce4284` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#32 | wi_tEnGWCxYEXzw | PR #81 / `ff5923f` | 在（`wrap: wrapped`） | wrapped | feature-full |
| polyforge#33 | wi_MuyoyHwOewS7 | PR #80 / `660fa87` | 在（`wrap: wrapped`） | completed | bug-fix |
| polyforge#34 | wi_XlhUr5fXLjO1 | PR #85 / `ab6742e` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#35 | wi_dDCz1MjpczvZ | PR #88 / `bb23c57` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#36 | wi_ihbMI4rH43er | PR #87 / `60a27dc` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#37 | wi_cVhdv3IInCRq | PR #90 / `e3d7ea8` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#38 | wi_jwVtx8w5XDXE | PR #89 / `e87c36e` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#39 | wi_izqGJFHcCaeH | PR #86 / `a430cfe` | 在（`wrap: wrapped`） | wrapped | quick-fix |
| polyforge#40 | wi_L2ZyiXbEectN | PR #93 / `e1c8003` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#41 | wi_s11m8pp8Pdw9 | PR #83 / `ffb0100` | 在（`wrap: wrapped`） | completed | chore |
| polyforge#42 | wi_hVYd0WqGUrPq | PR #91 / `14aa35e` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#43 | wi_TobxjUMungXl | PR #92 / `fb4cd8a` | 在（`wrap: wrapped`） | completed | quick-fix |
| polyforge#44 | wi_ibYL3MtSq0Lp | PR #82 / `50180d5` | 无 | wrapped | quick-fix |
| polyforge#45 | wi_8cGon3rDrypq | PR #84 / `c4f4d06` | 在（`wrap: wrapped`） | wrapped | quick-fix |
