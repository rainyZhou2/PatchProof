# 文档

[English docs](../README.md) | **简体中文**

英文文档是权威版本。中文翻译位于 `docs/zh-CN/`；尚未翻译的页面会直接链接到英文
原文。代码标识符、协议名、配置键、API 字段和枚举值不翻译。

## 从这里开始

| 文档 | 阅读场景 |
|---|---|
| [项目 README](README.md) | 产品概览、快速开始、已知限制。 |
| [快速开始](getting-started.md) | 完整的本地验证流程。 |
| [Verdict 与证据等级](concepts/verdicts.md) | 结果的确切含义。 |
| [CLI 参考](reference/cli.md) | 命令、参数与退出码。 |
| [配置参考](reference/configuration.md) | 环境变量、YAML、插件与 brokered 配置。 |
| [架构总览](architecture/overview.md) | 了解包职责、状态归属、Run 生命周期、fencing、区域模型。 |
| [Action Cache](../architecture/action-cache.md) | 运维或排查跨 Run 结果复用。 |
| [插件](../architecture/plugins.md) | 接入进程外引擎。 |
| [Brokered IO](../architecture/brokered-io.md) | 在无网络、无凭据的执行域运行高安全任务。 |
| [威胁模型](../security/threat-model.md) | 评审信任边界、隔离等级和证据完整性。 |

## 语言与目录约定

```text
README.md                 英文入口（默认，仓库根目录）
docs/zh-CN/README.md      中文入口
docs/zh-CN/index.md       中文文档索引
docs/README.md            英文文档索引
docs/architecture/        英文架构文档（权威）
docs/security/            英文安全文档
docs/operations/          英文运维文档与 runbook
docs/zh-CN/getting-started.md  中文快速开始
docs/zh-CN/concepts/      中文概念文档
docs/zh-CN/reference/     中文 CLI/配置参考
docs/zh-CN/architecture/  中文架构翻译
```

规则：

- 英文页面按主题放在 `docs/` 下；中文翻译镜像到 `docs/zh-CN/` 对应路径。
- 英文页面发生实质变更时，同步更新中文翻译，或在中文索引中标记“待同步”。
- 代码、命令、环境变量、API 字段、枚举值和文件路径保持英文原样。
- 示例必须可执行，并写明所需环境变量或前置条件。

## 核心概念

### Verdict 与 claim

Run 级结论（`VERIFIED_WITHIN_SCOPE`、`COUNTEREXAMPLE_FOUND`、`INCONCLUSIVE`、
`POLICY_BLOCKED`、`INFRA_FAILED`、`CANCELLED`）由 claim 级证据聚合而来。
`VERIFIED_WITHIN_SCOPE` 只对冻结的提交、策略、输入域和预算成立，不是通用正确性证明。

### 权威状态与 fencing

PostgreSQL 是唯一业务状态权威。NATS JetStream 只传递通知，消费者必须容忍重复。
每次 lease 都会增加 `generation`；提交必须匹配当前 generation 和 attempt id，过期
Worker 的结果会被丢弃，不能成为证据。

### 证据与离线验签

报告包含规范化 Merkle manifest 和 Ed25519 签名。`patchproof proof verify` 会校验
schema、签名、manifest leaf 的 digest/size、证据文档、Run attestation、策略要求和
verdict 一致性，但不会重新执行引擎。

### 执行隔离

`trusted-local` 在控制面进程内执行，只能用于可信代码。不可信代码必须使用
Kubernetes + gVisor/VM 等隔离；brokered 模式下 execute 容器无网络、无 attempt token、
无 CAS 凭据。

## 常用命令

```sh
make build
make test
make verify

bin/patchproof init
bin/patchproof verify --repo . --base HEAD~1 --head HEAD --policy patchproof.yaml
bin/patchproof proof verify --report report.json --cas-root .patchproof/cas \
  --key local=.patchproof/keys/signing.pub --policy patchproof.yaml
```

## 翻译状态

| 页面 | 状态 |
|---|---|
| `docs/zh-CN/README.md` | 已同步 |
| `docs/zh-CN/architecture/overview.md` | 已翻译，英文为权威版本 |
| `docs/zh-CN/getting-started.md` | 已翻译 |
| `docs/zh-CN/concepts/verdicts.md` | 已翻译 |
| `docs/zh-CN/reference/cli.md` | 已翻译 |
| `docs/zh-CN/reference/configuration.md` | 已翻译 |
| `docs/architecture/action-cache.md` | 待翻译 |
| `docs/architecture/plugins.md` | 待翻译 |
| `docs/architecture/brokered-io.md` | 待翻译 |
| `docs/security/threat-model.md` | 待翻译 |
| `docs/operations/**` | 待翻译 |

需要新增翻译时，请在 `docs/zh-CN/` 下保持与英文相同的相对路径，并在本索引补充链接。
