# 架构总览

[English](../../architecture/overview.md) | **简体中文**

> 英文页面是权威版本；本页与之同步。若两者冲突，以英文为准。

PatchProof 将区域控制面与执行集群分离。控制面负责身份、策略、Run 状态、调度和报告；
执行集群只拥有当前正在运行的 attempt。

## 控制面

| 包 | 职责 |
|---|---|
| `internal/api` | HTTP 路由、认证、租户限流、problem 文档、attempt 回调。 |
| `internal/app/runservice` | Run 提交、幂等指纹、列表/查询/取消/报告读取。 |
| `internal/app/orchestrator` | 校验、快照、策略评估、规划、聚合和终态迁移。 |
| `internal/planner` | 将策略和变更文件转换为步骤 DAG 与 claim。 |
| `internal/policy` | 内置硬策略：隔离下限、protected paths、来源限制、可信策略、网络规则。 |
| `internal/app/scheduler` | 分片所有权、Ready 步骤租约、放置、执行器分发、租约过期。 |
| `internal/app/attempts` | Worker 回调的带 fencing 的 start/heartbeat/complete/fail。 |
| `internal/app/proof` | Verdict 计算、证据校验、报告构造、Merkle root 和可选签名。 |
| `internal/app/outbox` | 将事务 outbox 记录投递到 NATS。 |
| `internal/app/admin` | 租户、项目、策略管理；集群列表。 |
| `internal/domain` | ID、枚举、状态机、证据/报告类型、fencing 校验。 |
| `internal/ports` | 元数据存储、artifact 存储、事件总线、执行器和 Planner 接口。 |

除 CLI 和本地开发使用的 `trusted-local` 执行器外，控制面进程不执行仓库代码。

## 执行面

| 包 | 职责 |
|---|---|
| `internal/adapters/executors/kubernetes` | 每个 attempt 创建一个加固 Job；观察 Job 并归一化失败。 |
| `internal/adapters/executors/local` | `trusted-local` 和本地开发使用的进程内执行器。 |
| `internal/adapters/executors/remote` | 访问 cluster agent 的 HTTP 客户端。 |
| `internal/clusteragent` | 提供 `/v1/executions`、刷新集群注册、持有 Kubernetes executor。 |
| `internal/worker` | 按 digest 获取输入、准备 workspace、调用引擎、上传输出。 |
| `engines/goanalyzer` | Go AST/SSA 分析与影响面证据。 |
| `engines/gobuild` | `go build` 与 `go test` 回归执行。 |
| `engines/godifferential` | base/head harness 的差分执行。 |
| `engines/gofuzz` | 原生 Go fuzz target 执行。 |

Worker 只收到一个步骤 spec 和非敏感环境（角色、区域、内存元数据、关闭的事件总线、
CAS 地址、工作目录）。它不访问 PostgreSQL、不枚举其他 attempt、不持有静态 S3 key，
也不持有集群凭据。直连模式下，Worker 会收到回调 URL 和短时 attempt token。

当 `artifact_transport=brokered-offline-io` 时，Kubernetes executor 会创建 attempt 卷
和三个 Job：可信 stage-in broker 将快照写入 attempt 本地 CAS；execute worker 在无网络、
无 attempt token、无 CAS 凭据的条件下执行；可信 stage-out broker 上传输出并提交结果。
execute Pod 带有 `patchproof.io/phase=execute`，chart 的 NetworkPolicy 对该标签实施
完整的 ingress/egress 拒绝。详见 [brokered-io.md](../../architecture/brokered-io.md)。

## 状态归属

| 存储 | 内容 | 权威性 |
|---|---|---|
| PostgreSQL | 租户、项目、策略、Run、步骤、attempt、证据、反例、报告、outbox、集群、分片租约、审计日志 | 唯一业务状态权威。 |
| NATS JetStream | 领域事件通知 | 派生数据。可从 outbox 重建；消费者必须容忍重复。 |
| CAS | 不可变 blob 和快照 | 内容权威是 digest，不是对象 key。 |
| Worker 文件系统 | 单个 attempt 的 workspace | 可丢弃；attempt 之外不得依赖。 |
| 内存存储 | 开发和单元测试 | 生产环境永不作为权威。 |

`internal/ports.MetadataStore` 是持久化契约。PostgreSQL adapter 和内存 adapter 实现相同
状态迁移和 fencing 检查，handler 无法绕过。

## Run 生命周期

1. `POST /api/v1/runs` 进入 `internal/api.handleCreateRun`，要求 `Idempotency-Key`，
   解析认证租户后调用 `runservice.Submit`。
2. `Submit` 加载活跃租户和项目，按 digest 或引用解析策略，校验预算，计算冻结输入指纹，
   调用 `CreateRun`。Run 行和 `patchproof.run.created.v1` outbox 事件在同一 PostgreSQL
   事务中写入。相同幂等键但指纹不同返回 `409 IDEMPOTENCY_CONFLICT`。
3. controller 的 orchestrator 被事件唤醒，同时轮询。它通过
   `internal/adapters/repos/git` 获取 base/head 快照，评估可信策略，用
   `internal/planner` 规划 DAG。`PlanRun` 在一个事务中写步骤、依赖和 claim。
4. 每个步骤按 `step.shard = hash(run, key) % shardCount` 分片。controller 副本在
   PostgreSQL 获取分片租约，并列出自己所有分片中的 Ready 步骤。租用步骤会增加
   `generation`、创建 `attempts` 行、记录租约截止时间，并签发绑定 attempt id 和
   generation 的短时 token。
5. Scheduler 构造带快照、策略和 contract 输入 CAS 引用的 `StepSpec`，调用选中的
   executor。Kubernetes executor 创建包含 spec 的 ConfigMap、包含 attempt token 的
   Secret，以及 Job。
6. Worker 下载输入、校验每个 digest、执行引擎、上传 artifact 到 CAS，然后调用回调。
   `internal/app/attempts` 加载当前步骤和 attempt，在接收结果前调用
   `domain.ValidateCommitFence`。
7. 成功或失败的步骤事务会更新步骤、记录证据和反例、提升新就绪的依赖步骤，并可能将
   Run 迁移到 `AGGREGATING`。Proof 服务校验证据、计算 verdict、存储报告，在配置签名器
   时签名，并发出报告事件。

## Outbox 与事件

事件总线不是事实来源。每个需要被其他组件观察的状态变更，都在同一 PostgreSQL 事务中
写入 outbox 行。`internal/app/outbox` 轮询待投递记录，发布到 JetStream，然后标记已投递。
投递语义是 at-least-once：

- 发布成功后、标记前崩溃，事件可能重复；
- 消费者必须幂等；
- `PATCHPROOF` stream 通过 `CreateOrUpdateStream` 幂等创建；
- NATS 故障不会丢状态，只会延迟通知；outbox 会持续重试直到 broker 恢复。

消费者负责唤醒 orchestrator；正确性不依赖收到某个特定事件，因为 orchestrator 也会从
PostgreSQL 对账。

## Fencing 与 attempt

Fencing 按步骤单调递增：

- 新 attempt 被租用时 `step.generation` 增加；
- attempt token 内嵌 `attempt_id` 和 `generation`；
- generation 不匹配或 attempt 不是当前 attempt 时，start/heartbeat/complete/fail
  回调被拒绝；
- `CommitAttempt` 在修改任何状态前校验步骤、attempt、generation、当前 attempt id、
  `LEASED`/`RUNNING` 状态、非终态 attempt 和合理时间戳；
- 过期 generation 的迟到结果只作为诊断丢弃，不能成为 Run 证据。

重试只在一个地方决定（`domain.NextStepStatus`），使用有界指数退避
（`domain.RetryDelay`，上限五分钟）。Kubernetes Job 使用 `backoffLimit: 0`；重试是
PatchProof 的 attempt，不是 Job 重启。

## CAS

`internal/ports.ArtifactStore` 由 `SecurityScope` 和 `CASRef`（`sha256`、digest、size）
寻址。实现：

- `internal/adapters/cas/s3`：边哈希边写临时文件，上传对象，读取时重新校验 size 和
  digest；对象不可变。
- `internal/adapters/cas/fs`：本地使用，按 digest 派生路径存储。
- `internal/adapters/cas/memory`：测试使用。
- `internal/adapters/cas/localfs`：brokered attempt 使用的本地 CAS。

Key 按租户和信任域隔离（`pkg/cas.BlobKey`），一个租户的私有源码 blob 不能被其他租户
scope 寻址。S3 adapter 可签发带 scope 的预签名 GET/PUT URL；调用方应把 URL 视为在其
TTL 内有效的 bearer capability。

Action Cache 将声明为 hermetic 的 analyze/impact/compile 成功结果存入 PostgreSQL，key
覆盖租户、cache epoch、快照、策略、引擎、选项、架构和隔离等级。条目按租户隔离、会过期，
绝不保存反例或非正向证据。Scheduler 和 Worker 回调路径都会写入；命中会作为新的 fencing
attempt 提交并生成新的 evidence id。详见
[action-cache.md](../../architecture/action-cache.md)。

## 扩展点

当前有两个扩展缝：

- 进程内 `engine.Engine` 在 `internal/engine` 注册，按 `(step kind, language, engine
  name)` 选择；内置 Go analyzer、build、differential 和 fuzz 引擎由此接入。
- `pkg/plugin` 实现 stdio 插件协议（4 字节长度前缀 + JSON），消息与
  `api/proto/patchproof/plugin/v1` 对应；`internal/engine/pluginengine` 将配置的可执行
  文件适配为 `engine.Engine`。插件通过 `execution.plugins` 注册，由策略
  `verification.engines` 选择。

没有自动 OCI 插件加载器。插件必须存在于 Worker 镜像中并在配置中注册才可达。`Executor`、
`ArtifactStore`、`EventBus` 和 `MetadataStore` 是基础设施扩展缝；替换 PostgreSQL、NATS、
S3 或 Kubernetes 只需实现接口，不修改 orchestrator。

## 区域模型

每个区域运行自己的 PostgreSQL、JetStream 和 CAS。租户状态不是全局的：

- `tenant.home_region` 决定哪个控制面接受该租户写入；
- `tenant.active_writer_epoch` 是单写者 fence；只有旧写者冻结后才能提升新 epoch；
- `UpdateTenantEpoch` 使用 expected-epoch CAS 更新 epoch 和 home region；
- 执行集群按区域划分并声明 `cas_region`；放置优先选择 CAS 区域与 Run 匹配的集群。

仓库不实现全局租户目录、跨区域复制、自动 failover 或 active-active 写入。区域迁移是
人工演练流程，见 [region-migration.md](../../migration/region-migration.md) 和区域迁移
runbook。同一 epoch 下，一个租户绝不会被两个区域同时写入。

## 信任边界

| 边界 | 可信侧 | 不可信侧 | 执行手段 |
|---|---|---|---|
| T0 | API、controller、scheduler、PostgreSQL、策略、签名密钥 | — | 网络隔离、RBAC、secret 引用。 |
| T0→T1 | 签名的引擎和 Worker 镜像、内置分析器 | 引擎输入数据 | digest 固定镜像、CAS digest 校验、资源限制。 |
| T1→T2 | Worker 进程和沙箱运行时 | 补丁代码、测试、生成文件、工具输出 | Job security context、RuntimeClass、默认无 service account token、只读根、NetworkPolicy。 |
| T2→T3 | 平台 adapter | Git 提供方、CI webhook、第三方报告 | 提供方 allowlist、大小限制、schema 校验、无 LLM 决策权。 |

源码快照即使由可信分析器处理，也仍视为不可信数据；解析、路径处理、符号链接处理和资源
耗尽在 analyzer、worker 和 sandbox 中都属于 T2 关注点。

## 失败语义

- PostgreSQL 事务提交后，Run 才算持久接受。
- 步骤分发是 at-least-once；重复 Job 或重复投递必须由 attempt fencing 容忍。
- Worker 结果只能针对当前 generation 和租约提交。
- NATS 事件缺失只会延迟进度，不会丢失已接受的 Run。
- CAS 故障以可重试基础设施错误使 attempt 失败，绝不产生通过结论。
- 签名失败使报告保持 `PENDING_ATTESTATION`，绝不作为已签名报告展示。
