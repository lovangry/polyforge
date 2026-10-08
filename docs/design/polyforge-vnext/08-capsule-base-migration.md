# 08 — Capsule 基底迁移运行手册（人工流程）<!-- 权威 -->

| 权威项 | 值 |
|---|---|
| 文档定位 | Capsule 基底（workspace）的人工迁移流程：换机/换盘/迁移到新基底时的 shutdown → `mv` → 重启序列、`capsules/` 的 `.gitignore` 指引 |
| 本文是唯一权威 | Daemon **没有**、也永远不会有自动迁移：没有迁移子命令、没有迁移旗标、启动时不搬任何 capsule。本文是唯一一个命名旧基底默认路径的地方 |

## 1. 背景 <!-- §1 -->

Daemon 的 Capsule 基底是 **`<workspace>/capsules/<wi_id>/`**。workspace 由操作者在启动时经
**必填**的 `-workspace <path>` 旗标（或 `POLYFORGE_WORKSPACE` 环境变量）指定：

- **没有默认值**——两者皆缺时 Daemon 拒绝启动，报错会点名 `-workspace` 并给出一条示例命令。
- 旧版的缺省基底 `<data-dir>/workspace`（即
  `~/.local/share/polyforge/workspace/capsules/<wi_id>/`）**已随旧旗标一并删除**：`-workspace-root`
  旗标与 `POLYFORGE_WORKSPACE_ROOT` 环境变量不存在了，传旧旗标就是普通的「未知旗标」启动失败。
- Daemon 启动时校验：workspace 目录必须**已存在**（Daemon 绝不代建它），且
  `<workspace>/capsules/` 可创建、可写。Daemon 在 workspace 内只写 `capsules/` 子树。
- 运行中的 WI 若在其应处的基底上找不到 capsule 目录，Daemon **硬失败并点名该 WI**
  （`mid-flight capsule missing at <path>`），**绝不**从远端 git 链重建——未发布的本地
  capsule 状态不允许被静默丢弃。唯一的补救就是把 capsule 目录移回
  `<workspace>/capsules/<wi_id>/`，再重启。
- 终态（已 wrap/fail/cancel）的 capsule 留在原地不动；Daemon 工具面（`pf ship` 等）不会
  回落去读旧基底——找不到就是找不到，只能人工处理。

## 2. 迁移序列（人工，按序执行）<!-- §2 -->

```
# 0. 目标 workspace 先建好（Daemon 不代建它）
mkdir -p /srv/polyforge/workspace

# 1. 优雅停掉本机 daemon
pf ops shutdown
# （systemd 机器：systemctl stop polyforge-daemon）

# 2. 把「进行中」的 capsule 目录整体移动到新基底的 capsules/ 下（目录名 = wi_id，原样搬）
mv ~/.local/share/polyforge/workspace/capsules/wi_xxx  /srv/polyforge/workspace/capsules/
#    只搬还在进行中的 WI；终态 capsule 不搬（留在旧基底，需要时人工取用）

# 3. 用新旗标重启 daemon
pf daemon -workspace /srv/polyforge/workspace ...（其余旗标不变）
```

重启后用 `pf ops status` 核对 `workspace:` 一行指向新基底；被搬迁的进行中 WI 会在
Daemon 重启对账时按既有恢复语义续跑。

## 3. `capsules/` 的 `.gitignore` 指引 <!-- §3 -->

workspace 常位于 git 仓库或被 git 跟踪的目录内。**Daemon 从不创建、追加或改写
`<workspace>/.gitignore`**——忽略 `capsules/` 是装机者/操作者自己的动作：

```
# 在 workspace 的 .gitignore 里加一行：
capsules/
```

`tools/install.sh` 会在安装时替你加（并打印它做了什么）；按本手册手工迁移的机器请自行补上。
