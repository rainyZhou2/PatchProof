# Documentation

**English** | [简体中文](zh-CN/README.md)

English documentation is canonical. Chinese translations live under
[`zh-CN/`](zh-CN/README.md) and link back to the English source when a page is
not translated yet. Code identifiers, protocol names and configuration keys are
never translated.

## Start here

| Document | Read it when |
|---|---|
| [README](../README.md) | You want the product overview, quickstart and limitations. |
| [中文 README](zh-CN/README.md) | Chinese overview and entry point. |
| [中文文档索引](zh-CN/index.md) | Chinese documentation index. |
| [Getting started](getting-started.md) | You want a complete local walkthrough. |
| [Verdicts and evidence levels](concepts/verdicts.md) | You need the exact meaning of a result. |
| [CLI reference](reference/cli.md) | You need commands, flags and exit codes. |
| [Configuration reference](reference/configuration.md) | You need environment variables, YAML sections or plugin/broker settings. |
| [Architecture overview](architecture/overview.md) | You need packages, state ownership, lifecycle, fencing or region model. |
| [Action cache](architecture/action-cache.md) | You operate or debug cross-run result reuse. |
| [Plugins](architecture/plugins.md) | You add an out-of-process engine. |
| [Brokered IO](architecture/brokered-io.md) | You run high-security attempts without network or credentials. |
| [Threat model](security/threat-model.md) | You review trust boundaries, isolation or evidence integrity. |

## Getting started and reference

- [Getting started](getting-started.md): build, run, local demo, offline verify,
  control plane, Kubernetes.
- [Verdicts and evidence levels](concepts/verdicts.md): claim results, run
  verdicts and what `VERIFIED_WITHIN_SCOPE` means.
- [CLI reference](reference/cli.md): commands, remote/local examples, exit codes.
- [Configuration reference](reference/configuration.md): environment variables,
  YAML sections, action cache, plugins and brokered IO.

## Architecture

- [Overview](architecture/overview.md): control plane, execution plane, state
  ownership, run lifecycle, outbox, fencing, CAS, action cache, brokered IO,
  extensibility, regions and failure semantics.
- [Action cache](architecture/action-cache.md): key inputs, eligibility, read and
  write paths, invalidation and limits.
- [Plugins](architecture/plugins.md): stdio contract, configuration, security.
- [Brokered IO](architecture/brokered-io.md): stage-in/execute/stage-out,
  Kubernetes resources, invariants and limits.

## Security and contributing

- [Threat model](security/threat-model.md): assets, trust boundaries, abuse
  cases, plugin/broker/cache threats and controls.
- [Security policy](SECURITY.md): private vulnerability reporting and supported
  versions.
- [Contributing](CONTRIBUTING.md): development setup, tests, compatibility and
  review expectations.

## Operations

- [Operations index](operations/README.md): signals, dashboards and runbooks.
- Runbooks: [PostgreSQL failover](operations/runbooks/postgres-failover.md),
  [NATS outage](operations/runbooks/nats-outage.md),
  [CAS outage](operations/runbooks/cas-outage.md),
  [scheduler stuck](operations/runbooks/scheduler-stuck.md),
  [worker failures](operations/runbooks/worker-failures.md),
  [attestation failure](operations/runbooks/attestation-failure.md),
  [region migration](operations/runbooks/region-migration.md),
  [staging verification](operations/runbooks/staging-verification.md).

## Migration

- [Schema expand/contract](migration/schema-expand-contract.md)
- [Version compatibility](migration/version-compatibility.md)
- [CAS migration](migration/cas-migration.md)
- [Region migration](migration/region-migration.md)

## Decisions

- [ADR index](adr/README.md)

## API

- [API guide](api/README.md)
- [OpenAPI 3.1 contract](../api/openapi/v1/openapi.yaml)

## Documentation conventions

- `README.md` is the English entry point; `docs/zh-CN/README.md` is the Chinese
  entry point. Both must link to each other.
- `docs/zh-CN/index.md` is the Chinese documentation index; `docs/README.md` is
  the English index.
- English pages live next to their subject; translations mirror the path under
  `docs/zh-CN/`.
- When an English page changes materially, update its translation or mark the
  translation as stale in the Chinese index.
- Do not translate code, command names, environment variables, API fields,
  enum values or file paths.
- Examples must be runnable as written and must state required environment
  variables or preconditions.
