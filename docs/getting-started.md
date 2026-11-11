# Getting started

PatchProof has three entry points: local verification, a regional control plane,
and a Kubernetes execution cluster. Start with local verification to learn the
policy and report model.

Requirements: Go 1.26+, `git`, `make`.

## 1. Build

```sh
git clone https://github.com/patchproof/patchproof.git
cd patchproof
make build
```

Binaries are written to `bin/`:

| Binary | Purpose |
|---|---|
| `patchproof` | CLI: init, verify, run, report, reproduce, proof verify. |
| `patchproof-server` | All-in-one local server (API, controller, scheduler, worker). |
| `patchproof-api` | Stateless API and worker callback service. |
| `patchproof-controller` | Controller, orchestrator and scheduler. |
| `patchproof-worker` | One-attempt worker. |
| `patchproof-broker` | Trusted stage-in/stage-out broker. |
| `patchproof-cluster-agent` | Kubernetes execution cluster agent. |
| `patchproof-proof` | Offline report verification command. |

## 2. Local verification

Local mode runs the planner, scheduler, worker and proof service in one process
with a filesystem CAS under `.patchproof/`. It only accepts `trusted-local`
policy. Never point it at untrusted pull requests.

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

`patchproof init` writes a policy template. Review `contracts`,
`execution.isolationTier` and `protectedPaths` before using it in a real project.
Local verification writes its CAS, generated signing key and reports under
`.patchproof/`, which is ignored by git.

## 3. Verify the report offline

```sh
bin/patchproof proof verify --report report.json \
  --cas-root .patchproof/cas \
  --key local=.patchproof/keys/signing.pub \
  --policy patchproof.yaml
```

Exit codes: `0` `VALID_INTEGRITY`, `1` `INVALID`, `2` `UNVERIFIABLE`, `3` usage.

## 4. Run a control plane

The Compose stack wires PostgreSQL, NATS JetStream, MinIO, the API and the
controller:

```sh
docker compose -f deploy/compose/docker-compose.yaml up --build
curl -sS http://localhost:8080/healthz
```

It intentionally registers **no execution cluster**, so submitted runs queue but
do not execute. Use the all-in-one server or a Kubernetes cluster to execute
work. See [deploy/compose/README.md](../deploy/compose/README.md).

## 5. Kubernetes

The Helm chart and staging values are under
[deploy/helm/patchproof](../deploy/helm/patchproof) and
[deploy/staging](../deploy/staging). The acceptance matrix is in
[staging-verification.md](operations/runbooks/staging-verification.md).

## Next

- [Verdicts and evidence levels](concepts/verdicts.md)
- [CLI reference](reference/cli.md)
- [Configuration reference](reference/configuration.md)
- [Architecture overview](architecture/overview.md)
- [Action cache](architecture/action-cache.md)
- [Plugins](architecture/plugins.md)
- [Brokered offline IO](architecture/brokered-io.md)
