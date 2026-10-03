---
name: pf-init
description: >
  Use when setting up a polyforge machine on v1.0.0 for the first time:
  installing the pf binary, writing the client config, verifying server
  connectivity, and optionally booting the local execution daemon.
  Also use for repair when connectivity or daemon health checks fail.
---

# pf-init — v1.0.0 机器安装与初始化

## 你需要先有这两样(向管理员要,skill 自己不生成)

1. **服务器地址**:当前生产 `http://10.146.0.34:8080`
2. **个人 API key**(pf_k1_…):管理员逐人签发。**绝不用 root/admin key
   装机**,也绝不把 key 写进仓库、消息或工单。

## Step 1 — 装 pf 二进制

```
# 发布产物(linux amd64):
scp <发布机>:/root/release-v1.0.0/pf  ~/bin/pf && chmod +x ~/bin/pf
# 校验(发布机:/root/release-v1.0.0/SHA256SUMS):
sha256sum ~/bin/pf   # 必须一致
# Mac 机器:向发布机要 darwin/arm64 交叉编译产物(或让发布机补编)
pf 2>&1 | head -1    # 应打印 pf — polyforge v1 client CLI
```

## Step 2 — 写客户端配置

```
mkdir -p ~/.polyforge
cat > ~/.polyforge/config.toml <<EOF
url = "http://10.146.0.34:8080"
api_key = "<你的个人 key>"
EOF
chmod 600 ~/.polyforge/config.toml
```

## Step 3 — 连通性验证

```
pf queue --project polyforge
#   通:打印队列(空也算通)
#   AUTH-401 → key 错或没签发;连不上 → 检查网络/服务器
```

到此为止:纯使用机(开票/答题/看队列)装完了。以下仅执行机需要。

## Step 4 —(执行机)首次起 daemon

```
pf daemon -server http://10.146.0.34:8080 \
  -machine-id m-<你的名字> \
  -mode interactive -project polyforge \
  -data-dir ~/.polyforge/daemon-data \
  -socket ~/.polyforge/daemon.sock \
  -max-concurrent-jobs 4 \
  > ~/.polyforge/daemon.log 2>&1 &
```

注意:
- **机器注册是管理员动作**(v1.0.0 安全门:心跳首注册只认 admin;
  管理员先注册机器身份,你的 key 照常使用)
- 60 秒后验证:`pf machines`(你的 m-xxx 应在列表,心跳新鲜)
- 日常运转(状态/切模式/优雅停机)从此归 `/polyforge ops` 管,
  本 skill 不再插手

## Step 5 —(AI 操作员机)技能包

操作员技能(pf-work/pf-status/pf-execute/pf-stop… + using-polyforge
的 IR 规则/per-WI 观察器纪律)安装到 pi 会话环境后,`/pf-*` 命令即可用。
技能包随发布分发(见发布产物说明)。

## 修复模式(连接不上/daemon 病了)

按顺序检查:config 存在且 600 → key 有效(pf queue)→ daemon 活着
(`curl --unix-socket ~/.polyforge/daemon.sock http://localhost/status`)
→ daemon 日志尾三行。逐层修,修不了报告用户转 /pf-doctor。
