# 快速开始

[English](../getting-started.md) | **简体中文**

PatchProof 有三种使用方式：本地验证、区域控制面和 Kubernetes 执行集群。建议先完成
本地验证，理解策略与报告模型。

依赖：Go 1.26+、`git`、`make`。

## 1. 构建

```sh
git clone https://github.com/patchproof/patchproof.git
cd patchproof
make build
```

二进制输出到 `bin/`：

| 二进制 | 用途 |
|---|---|
| `patchproof` | CLI：init、verify、run、report、reproduce、proof verify。 |
| `patchproof-server` | 本地 all-in-one 服务（API、controller、scheduler、worker）。 |
| `patchproof-api` | 无状态 API 与 Worker 回调服务。 |
| `patchproof-controller` | Controller、orchestrator 与 scheduler。 |
| `patchproof-worker` | 单 attempt Worker。 |
| `patchproof-broker` | 可信 stage-in/stage-out broker。 |
| `patchproof-cluster-agent` | Kubernetes 执行集群 agent。 |
| `patchproof-proof` | 离线报告验证命令。 |

## 2. 本地验证

本地模式在单进程内运行 Planner、Scheduler、Worker 和 Proof 服务，CAS 位于
`.patchproof/`，只接受 `trusted-local` 策略。**不要用于不可信 PR。**

```sh
mkdir demo && cd demo
git init
git config user.email dev@example.org
git config user.name dev
printf 'module example.org/demo\n\ngo 1.26.0\n' > go.mod
printf 'package demo\n\nfunc Add(a, b int) int { return a + b }\n' > add.go
git add . && git commit -qm base

printf 'package demo\n\nfunc Add(a, b int) int { return a - b }\n' > add.go
git add . && git commit -qm change

../bin/patchproof init
../bin/patchproof verify --repo . --base HEAD~1 --head HEAD \
  --policy patchproof.yaml --format markdown
```

`patchproof init` 会生成策略模板。用于真实项目前请检查 `contracts`、
`execution.isolationTier` 和 `protectedPaths`。本地验证产生的 CAS、签名私钥和报告
位于 `.patchproof/`，已被 git 忽略。

## 3. 离线验签

```sh
bin/patchproof proof verify --report report.json \
  --cas-root .patchproof/cas \
  --key local=.patchproof/keys/signing.pub \
  --policy patchproof.yaml
```

退出码：`0` `VALID_INTEGRITY`、`1` `INVALID`、`2` `UNVERIFIABLE`、`3` 用法错误。

## 4. 运行控制面

Compose 栈包含 PostgreSQL、NATS JetStream、MinIO、API 和 controller：

```sh
docker compose -f deploy/compose/docker-compose.yaml up --build
curl -sS http://localhost:8080/healthz
```

它**不注册执行集群**，因此 Run 会排队但不会执行。需要执行时使用 all-in-one server
或真实 Kubernetes 集群。参见 [deploy/compose/README.md](../../deploy/compose/README.md)。

## 5. Kubernetes

Helm chart 与 staging 资产位于
[deploy/helm/patchproof](../../deploy/helm/patchproof) 和
[deploy/staging](../../deploy/staging)。验收矩阵见
[staging-verification.md](../operations/runbooks/staging-verification.md)。

## 下一步

- [Verdict 与证据等级](concepts/verdicts.md)
- [CLI 参考](reference/cli.md)
- [配置参考](reference/configuration.md)
- [架构总览](architecture/overview.md)
- [Action Cache](../architecture/action-cache.md)
- [插件](../architecture/plugins.md)
- [Brokered IO](../architecture/brokered-io.md)
