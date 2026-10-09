# operator-skills/

Two FACES — install the right one, not both.

## 用户面(团队成员):只装一个

**`polyforge/`**(意图路由,自带 13 动词 fragments)+ pf 二进制即完整可用:
`cp -r polyforge ~/.pi/agent/skills/`

## 操作员面(AI 执行/维护 harness):9 个

`polyforge/` + `using-polyforge/`(IR 规则;per-WI 观察器纪律完整版)+ \
`pf-init/`(装机/升级)+ `pf-doctor/` + `pf-help/` + `pf-status/` + \
`pf-retro/` + `pf-crystallize/` + `pf-release/`

同步方向:先改 harness 侧(~/.pi/agent/skills),变更后镜像回本目录。

## 2026-10-03 精简(owner 裁定):19 → 9

砍掉的 10 个是 v0 工作树工作流的遗产,胶囊模型(v1.0.0)已取代其工作面:
pf-work(领取=路由动词+hub;观察器纪律移入 using-polyforge)、
pf-spec/pf-plan(执行由 daemon 的 Go driver 驱动)、pf-revise、pf-sync(v1 无此
机制)、pf-update(v0 role 机制)、pf-project/pf-user(CLI 薄壳)、
pf-stop(动词 fragments 已带)。git 历史可追溯。
