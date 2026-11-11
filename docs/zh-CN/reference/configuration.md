# 配置参考

[English](../../reference/configuration.md) | **简体中文**

配置从 `PATCHPROOF_CONFIG` 指定的 YAML 加载，再被环境变量覆盖，最后统一校验。字段定义
在 [`internal/platform/config/config.go`](../../../internal/platform/config/config.go)；
未知 YAML 字段会被解码器拒绝。

## 常用环境变量

| 变量 | 示例 | 用途 |
|---|---|---|
| `PATCHPROOF_ROLE` | `api`、`controller`、`worker`、`cluster-agent`、`proof` | 进程角色。 |
| `PATCHPROOF_REGION` | `region-a` | 区域。 |
| `PATCHPROOF_EXTERNAL_URL` | `https://patchproof.example.org` | 公网 URL 与回调 base。 |
| `PATCHPROOF_POSTGRES_DSN` | `postgres://...` | 元数据权威库。 |
| `PATCHPROOF_NATS_URL` | `nats://nats:4222` | JetStream 地址。 |
| `PATCHPROOF_CAS_DRIVER` | `s3` 或 `fs` | 产物存储。 |
| `PATCHPROOF_S3_ENDPOINT`、`_REGION`、`_BUCKET` | | S3 配置。 |
| `PATCHPROOF_AUTH_MODE` | `oidc` 或 `dev` | 认证模式。 |
| `PATCHPROOF_OIDC_ISSUER`、`_AUDIENCE`、`_JWKS_URL` | | OIDC 校验。 |
| `PATCHPROOF_ATTEMPT_TOKEN_KEY` | ≥32 字符 | attempt token HMAC key。 |
| `PATCHPROOF_SIGNING_KEY`、`_SIGNING_KEY_ID` | `/etc/patchproof/signing.pem` | 报告签名。 |
| `PATCHPROOF_WORKER_IMAGE`、`_BROKER_IMAGE` | `repo@sha256:...` | 执行/broker 镜像。 |
| `PATCHPROOF_RUNTIME_CLASS` | `gvisor` | attempt Job 的 RuntimeClass。 |
| `PATCHPROOF_BROKERED_STORAGE_CLASS` | `standard-rwo` | brokered 卷存储类。 |
| `PATCHPROOF_ACTION_CACHE_ENABLED` | `true` | 启用 Action Cache。 |
| `PATCHPROOF_ACTION_CACHE_TTL` | `168h` | 缓存 TTL。 |
| `PATCHPROOF_CACHE_EPOCH` | `v1` | 引擎/镜像变化时使缓存失效。 |
| `PATCHPROOF_SHARD_COUNT` | `16` | 调度分片数，多副本必须一致。 |
| `PATCHPROOF_OTLP_ENDPOINT` | `otel-collector:4317` | Trace 导出。 |

## YAML 配置段

完整示例见
[`deploy/compose/config/patchproof.yaml`](../../../deploy/compose/config/patchproof.yaml)。

| 配置段 | 主要字段 |
|---|---|
| `service` | `name`、`region`、`version`、`externalUrl` |
| `api` | `addr`、`readHeaderTimeout`、`idempotencyTTL` |
| `postgres` | `dsn`、`maxConns`、`minConns`、`migrations` |
| `nats` | `urls`、`stream`、`subjectPrefix`、`maxAge` |
| `cas` | `driver`、`fs.root`、`s3.endpoint`、`s3.bucket`、`s3.region` |
| `auth` | `mode`、`oidc`、`dev`、`attemptTokenTTL` |
| `signing` | `keyId`、`privateKeyFile`、`require` |
| `scheduler` | `shardCount`、`leaseTTL`、`placement`、`actionCacheEnabled`、`actionCacheTTL`、`cacheEpoch` |
| `execution` | `workerImage`、`brokerImage`、`namespace`、`runtimeClass`、`isolation`、`plugins` |
| `limits` | 仓库、变更文件、预算与并发上限 |
| `telemetry` | `logLevel`、`logFormat`、`otlpEndpoint`、`sampleRatio` |

## Action Cache

`scheduler.actionCacheEnabled` 默认 `true`；`actionCacheTTL` 默认 `168h`；
`cacheEpoch` 默认取服务版本。引擎、编译器或 Worker 镜像变化并影响输出时，必须提升
cache epoch。详见 [Action Cache](../../architecture/action-cache.md)。

## 外部插件

平台配置：

```yaml
execution:
  allowedLanguages: [go, python]
  plugins:
    - name: python-analyzer
      command: ["/opt/patchproof/plugins/python-analyzer"]
      env:
        PYTHONDONTWRITEBYTECODE: "1"
      languages: [python]
      stepKinds: [analyze, impact-analysis]
      timeout: 5m
```

验证策略（独立文档，通过可信策略提交）：

```yaml
verification:
  engines:
    analyze: {name: python-analyzer, language: python}
```

插件二进制必须存在于 Worker 镜像中。详见 [插件](../../architecture/plugins.md)。

## Brokered 离线 IO

```yaml
execution:
  brokerImage: ghcr.io/example/patchproof-broker@sha256:...
  brokerServiceAccount: patchproof-broker
  brokeredStorageClass: standard-rwo
  workspaceSizeGiB: 20
```

execute Job 不获取 CAS 凭据或 attempt token，并受
`patchproof.io/phase=execute` egress-deny 策略约束。详见
[Brokered IO](../../architecture/brokered-io.md)。

## 密钥

不要把静态 S3 key 放进 Worker Job 或 YAML。使用 Worker/broker service account 的
workload identity，并从 Kubernetes Secret 挂载签名密钥和 attempt-token key。
