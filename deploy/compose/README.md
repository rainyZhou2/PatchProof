# Compose integration stack

This stack runs the PatchProof control plane against PostgreSQL 16, NATS
JetStream 2.10 and MinIO. It exists to exercise API, database, event-bus and
object-store wiring on one host.

It is not a production deployment and it is not a sandbox. The API and
controller run as trusted processes; `trusted-local`/in-process execution is not
enabled here, but the containers are still not a place to run untrusted pull
requests. Use the Helm chart and an isolated execution cluster for that.

## Services

| Service | Address | Purpose |
|---|---|---|
| `patchproof-api` | http://localhost:8080 | Public API (`/api/v1`, `/healthz`, `/readyz`) |
| `patchproof-api` metrics | http://localhost:9090/metrics | Prometheus metrics |
| `patchproof-controller` metrics | http://localhost:9091/metrics | Controller metrics |
| `postgres` | internal `postgres:5432` | Metadata authority |
| `nats` | internal `nats:4222`, http://localhost:8222 | JetStream event bus and monitoring |
| `minio` | http://localhost:9000, console http://localhost:9001 | S3-compatible CAS |
| `createbuckets` | one-shot | Creates the `patchproof` bucket and leaves it private |
| `seed` | one-shot | Inserts the development tenant used by dev auth |

The development tenant is fixed:

```text
tenant_id: 11111111-1111-4111-8111-111111111111
token:     patchproof-dev-token
```

These values exist only in `deploy/compose/config/patchproof.yaml` and the
Compose environment. Do not reuse them outside this harness.

## Start

```sh
docker compose -f deploy/compose/docker-compose.yaml up --build
```

Wait until the API and controller logs show the services listening. Then:

```sh
curl -sS http://localhost:8080/healthz
# {"status":"ok"}

curl -sS -H 'Authorization: Bearer patchproof-dev-token' \
  http://localhost:8080/api/v1/version
# {"region":"region-a","version":"compose"}
```

`/readyz` checks PostgreSQL, the S3 bucket and NATS; it returns `503` with a
problem document until all three are reachable.

## Create a project and inspect runs

The seed inserts the tenant row; project creation is done through the API.

```sh
curl -sS -X POST http://localhost:8080/api/v1/projects \
  -H 'Authorization: Bearer patchproof-dev-token' \
  -H 'Content-Type: application/json' \
  -d '{
    "provider": "git",
    "repository_uri": "https://git.example.org/team/repo.git",
    "default_branch": "main",
    "allowed_regions": ["region-a"],
    "classification": "standard"
  }'
```

List runs, always with a cursor-aware limit:

```sh
curl -sS -H 'Authorization: Bearer patchproof-dev-token' \
  'http://localhost:8080/api/v1/runs?limit=20'
```

Submitting a run requires a committed project, a policy digest and two distinct
commits. The repository URL must be reachable from the container network.

## What this stack does not do

The standalone `patchproof-controller` role runs the orchestrator, scheduler,
outbox relay and proof aggregation, but it does not host an in-process step
executor and no execution cluster is registered by this compose file. A run
therefore reaches planning/queueing and does not execute steps here. There is
no worker service in the stack.

To execute steps you need one of:

- the all-in-one `patchproof-server` binary on a trusted workstation, which
  enables the local executor (trusted code only), or
- a Kubernetes execution cluster with a registered cluster agent and a worker
  image; see `deploy/helm/patchproof/README.md`.

The compose stack is still useful for contract tests: authentication, tenant
scope, project and policy validation, pagination, problem documents, database
migrations and readiness checks.

## Operate

```sh
docker compose -f deploy/compose/docker-compose.yaml ps
docker compose -f deploy/compose/docker-compose.yaml logs -f patchproof-api
docker compose -f deploy/compose/docker-compose.yaml down
docker compose -f deploy/compose/docker-compose.yaml down -v   # remove data
```

PostgreSQL applies the embedded migrations when the API or controller starts
with `database.migrateOnStart: true`; the advisory lock serializes concurrent
startups. MinIO data, PostgreSQL data and JetStream data live in named volumes.

## Limits and known constraints

- No TLS. The API listens on plain HTTP and trusts the local network.
- Development auth accepts one static bearer token and one static tenant id.
- The S3 bucket is private; pre-signed URLs are issued with a 10 minute TTL.
- Rate limiting is per process and in-memory (`apiRpsPerTenant: 20`).
- The MinIO and NATS images are pinned by release tag here, not by digest.
  Production assets in `deploy/helm` require image digests.
