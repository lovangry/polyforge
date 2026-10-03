# operator-skills/

Two FACES live here — install the right one, not both.

## 用户面(团队成员):只装一个技能

设计就是单一入口:用户 harness 只安装 **`polyforge/`**(意图路由技能),
它自带 13 个动词的全部 fragments。配合 pf 二进制 + config 即完整可用。

```sh
# 团队成员机器(装完 /pf-init 或手工):
cp -r polyforge  ~/.pi/agent/skills/
# 日常:自然语言即可("看下队列""答这题""切 drain""这票交付了")
```

## 操作员面(AI 执行/维护 harness):19 件套

pf-* 系列(pf-work/pf-status/pf-execute/pf-stop/pf-init/pf-doctor/…)
+ using-polyforge(IR 规则、per-WI 观察器对账)是操作员/维护者的
完整 harness,不是给普通用户的。同步方向:先改 harness 侧
(~/.pi/agent/skills),变更后镜像回本目录。

## Inventory

- `polyforge/`        ← 用户唯一入口(13 动词路由,含 ops)
- `pf-init/`          ← 装机(用户机/执行机初始化,可选)
- `pf-*/` 其余         ← 操作员 harness
- `using-polyforge/`  ← 会话常驻规则(操作员 harness 用)
