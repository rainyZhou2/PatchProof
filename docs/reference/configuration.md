# Configuration reference

Configuration is loaded from the YAML file named by `PATCHPROOF_CONFIG`, then
overridden by environment variables, then validated. Fields are defined in
[`internal/platform/config/config.go`](../../internal/platform/config/config.go);
unknown YAML keys are rejected by the decoder.

## Common environment variables

| Variable | Example | Purpose |
|---|---|---|
| `PATCHPROOF_ROLE` | `api`, `controller`, `worker`, `cluster-agent`, `proof` | Process role. |
| `PATCHPROOF_REGION` | `region-a` | Region of this process. |
| `PATCHPROOF_EXTERNAL_URL` | `https://patchproof.example.org` | Public URL and callback base. |
| `PATCHPROOF_POSTGRES_DSN` | `postgres://...` | Metadata authority. |
| `PATCHPROOF_NATS_URL` | `nats://nats:4222` | JetStream servers. |
| `PATCHPROOF_CAS_DRIVER` | `s3` or `fs` | Artifact store. |
| `PATCHPROOF_S3_ENDPOINT`, `_REGION`, `_BUCKET` | | S3 settings. |
| `PATCHPROOF_AUTH_MODE` | `oidc` or `dev` | Authentication mode. |
| `PATCHPROOF_OIDC_ISSUER`, `_AUDIENCE`, `_JWKS_URL` | | OIDC validation. |
| `PATCHPROOF_ATTEMPT_TOKEN_KEY` | ≥32 characters | Attempt-token HMAC key. |
| `PATCHPROOF_SIGNING_KEY`, `_SIGNING_KEY_ID` | `/etc/patchproof/signing.pem` | Report signing. |
| `PATCHPROOF_WORKER_IMAGE`, `_BROKER_IMAGE` | `repo@sha256:...` | Execution/broker images. |
| `PATCHPROOF_RUNTIME_CLASS` | `gvisor` | RuntimeClass for attempt Jobs. |
| `PATCHPROOF_BROKERED_STORAGE_CLASS` | `standard-rwo` | Volume class for brokered IO. |
| `PATCHPROOF_ACTION_CACHE_ENABLED` | `true` | Enable the action cache. |
| `PATCHPROOF_ACTION_CACHE_TTL` | `168h` | Cache entry TTL. |
| `PATCHPROOF_CACHE_EPOCH` | `v1` | Invalidate cached results on engine/image changes. |
| `PATCHPROOF_SHARD_COUNT` | `16` | Scheduler shards; must match controller replicas. |
| `PATCHPROOF_OTLP_ENDPOINT` | `otel-collector:4317` | Trace export. |

## YAML sections

A complete example is
[`deploy/compose/config/patchproof.yaml`](../../deploy/compose/config/patchproof.yaml).

| Section | Key fields |
|---|---|
| `service` | `name`, `region`, `version`, `cluster` |
| `http` | `listenAddr`, `externalUrl`, `readHeaderTimeout`, `readTimeout`, `writeTimeout`, `idleTimeout`, `shutdownTimeout`, `maxBodyBytes`, `tlsCertFile`, `tlsKeyFile` |
| `database` | `driver`, `dsn`, `maxConns`, `minConns`, `maxConnLifetime`, `maxConnIdleTime`, `statementTimeout`, `migrateOnStart` |
| `events` | `driver`, `urls`, `stream`, `consumerPrefix`, `maxAckPending`, `batchSize`, `ackWait`, `outboxInterval`, `outboxBatch` |
| `artifacts` | `driver`, `root`, `endpoint`, `region`, `bucket`, `forcePathStyle`, `publicPrefix`, `presignTtl` |
| `auth` | `mode`, `devToken`, `issuer`, `audience`, `jwksUrl`, `tenantClaim`, `rolesClaim`, `groupsClaim`, `attemptTokenKey`, `attemptTokenTtl`, `internalToken`, `adminSubjects`, `devTenantId`, `devSubject`, `devRoles` |
| `execution` | `defaultIsolation`, `maxIsolation`, `allowedLanguages`, `runtimeClass`, `workerImage`, `engineImage`, `namespace`, `jobTtl`, `jobBackoffLimit`, `activeDeadline`, `maxConcurrent`, `supportedIsolationTiers`, `nodeSelector`, `tolerations`, `agentEndpoint`, `agentToken`, `clusterId`, `clusterRegion`, `reportBackUrl`, `automountWorkerToken`, `brokerImage`, `brokerServiceAccount`, `brokeredStorageClass`, `workspaceSizeGiB`, `plugins` |
| `scheduler` | `shardCount`, `leaseTtl`, `renewInterval`, `pollInterval`, `batchSize`, `maxStepsPerTenant`, `maxStepsPerCluster`, `clusterStaleAfter`, `placement`, `priorityBoostMax`, `actionCacheEnabled`, `actionCacheTtl`, `cacheEpoch` |
| `worker` | `workDir`, `controllerUrl`, `stepSpecFile`, `heartbeat`, `uploadRetries`, `maxOutputBytes`, `offlineOutputDir` |
| `signing` | `algorithm`, `keyId`, `privateKeyFile`, `publicKeyFile`, `require`, `kmsKeyUri` |
| `telemetry` | `logLevel`, `logFormat`, `metricsAddr`, `otlpEndpoint`, `otlpInsecure`, `sampleRatio`, `serviceNamespace` |
| `limits` | repository, changed-file, step-count, wall-clock, CPU, fuzz, concurrency and API limits |
| `state` | `driver` |

## Action cache

`scheduler.actionCacheEnabled` defaults to `true`; `actionCacheTTL` defaults to
`168h`; `cacheEpoch` defaults to the service version. Cache keys include the
engine version, the worker image and a toolchain-independent step description,
so an engine upgrade invalidates old entries automatically. Use `cacheEpoch` for
changes that are not captured by those fields. `scheduler.clusterStaleAfter`
defaults to `90s`; clusters that have not heartbeated within that window are not
scheduled. See [architecture/action-cache.md](../architecture/action-cache.md).

## External plugins

Platform configuration:

```yaml
execution:
  allowedLanguages: [go, python]
  plugins:
    - name: python-analyzer
      version: 1.0.0
      command: ["/opt/patchproof/plugins/python-analyzer"]
      env:
        PYTHONDONTWRITEBYTECODE: "1"
      languages: [python]
      stepKinds: [analyze, impact-analysis]
      timeout: 5m
```

Verification policy (separate document, submitted as the trusted policy):

```yaml
verification:
  engines:
    analyze: {name: python-analyzer, language: python}
```

Plugin binaries must be present in the worker image. See
[architecture/plugins.md](../architecture/plugins.md).

## Brokered offline IO

```yaml
execution:
  brokerImage: ghcr.io/example/patchproof-broker@sha256:...
  brokerServiceAccount: patchproof-broker
  brokeredStorageClass: standard-rwo
  workspaceSizeGiB: 20
```

The execute Job receives no CAS credentials or attempt token and is selected by
the `patchproof.io/phase=execute` egress-deny policy. See
[architecture/brokered-io.md](../architecture/brokered-io.md).

## Secrets

Never put static S3 keys in worker Jobs or YAML. Use workload identity for the
worker/broker service accounts, and mount signing and attempt-token keys from
Kubernetes Secrets.
