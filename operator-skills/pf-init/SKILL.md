---
name: pf-init
description: >
  Use when setting up a polyforge machine on v1.0.0 for the first time:
  installing the pf binary, writing the client config, verifying server
  connectivity, and optionally booting the local execution daemon.
  Also use for binary upgrades (version check → download → graceful
  restart) and repair when connectivity or daemon health checks fail.
---

# pf-init — v1.0.0 机器安装 / 升级 / 修复

## 你需要先有这两样(向管理员要,skill 自己不生成)

1. **服务器地址**:当前生产 `http://10.146.0.34:8080`
2. **个人 API key**(pf_k1_…):管理员签发。**绝不用 root/admin key
   装机**,也绝不把 key 写进仓库、消息或工单。

## Step 1 — 装 pf 二进制(从发布服务器,零认证)

```
REL=<GitHub Releases,见 Step 2;还没配就先按 Step 2 写好再回来>
VER=$(curl -s "$REL/latest.txt" | awk '{print $1}')
SHA=$(curl -s "$REL/latest.txt" | awk '{print $2}')
curl -s -o ~/bin/pf "$REL/pf-$VER" && chmod +x ~/bin/pf
echo "<下载后文件的 sha256>  -" | sha256sum -c 2>/dev/null || \
  sha256sum ~/bin/pf   # 必须等于 $SHA
pf --version           # 应输出 $VER
```

## Step 2 — 写客户端配置(含发布地址)

```
mkdir -p ~/.polyforge
cat > ~/.polyforge/config.toml <<EOF
url = "http://10.146.0.34:8080"
api_key = "<你的个人 key>"
EOF
chmod 600 ~/.polyforge/config.toml
```

读取规则:本 skill 的一切下载/版本比对只认 **`GitHub Releases`**;
config 缺该字段时,用 `url` 的主机部分 + `:8090` 推导,但正式装机
应显式写明。

## Step 3 — 连通性验证

```
pf queue --project polyforge
#   通:打印队列(空也算通)
#   AUTH-401 → key 错或没签发;连不上 → 检查网络/服务器
```

到此为止:纯使用机(开票/答题/看队列)装完了。以下仅执行机需要。

## Step 4 —(执行机)首次起 daemon

```
pf daemon -server $(grep ^url ~/.polyforge/config.toml | cut -d'"' -f2) \
  -api-key $(grep ^api_key ~/.polyforge/config.toml | cut -d'"' -f2) \
  -machine-id m-<你的名字> \
  -mode interactive -project polyforge \
  -data-dir ~/.polyforge/daemon-data \
  -socket ~/.polyforge/daemon.sock \
  -max-concurrent-jobs 4 \
  > ~/.polyforge/daemon.log 2>&1 &
```

- **机器注册是管理员动作**(安全门:心跳首注册只认 admin)
- 60 秒后验证:`pf machines`(你的 m-xxx 在列,心跳新鲜)
- 日常运转归 `/polyforge ops`(status/mode/shutdown/升级前自检)

## Step 5 —(AI 操作员机)技能包

操作员技能(pf-* 全套 + using-polyforge)从仓库 `operator-skills/`
安装到 pi 会话技能目录;用户面只需 `polyforge/` 一个。

## 升级循环(已装机机器)

```
REL=<GitHub Releases from config>
LATEST=$(curl -s "$REL/latest.txt"); VER=$(echo "$LATEST"|awk '{print $1}')
RUNNING=$(pf --version)          # 旧二进制无此输出 = 需要升级
if [ "$RUNNING" != "$VER" ]; then
  # 优雅停(仅执行机;纯 CLI 机跳过)
  /polyforge ops shutdown        # 或 curl --unix-socket ~/.polyforge/daemon.sock \
                                 #   -X POST http://localhost/ops/shutdown
  curl -s -o ~/bin/pf.new "$REL/pf-$VER" && sha256sum ~/bin/pf.new  # 校验
  mv ~/bin/pf.new ~/bin/pf && chmod +x ~/bin/pf
  # 重启(执行机):重跑 Step 4 的 daemon 命令;systemd 托管的机器 systemctl restart
  pf --version && pf machines    # 版本对 + 心跳回来 = 升级完成
fi
```

## 修复模式(连接不上/daemon 病了)

按序:config 存在且 600 → key 有效(pf queue)→ daemon 活着
(`curl --unix-socket ~/.polyforge/daemon.sock http://localhost/status`)
→ daemon 日志尾三行。逐层修,修不了转 /pf-doctor。
