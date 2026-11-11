# Container images

All images are built from the repository root and are multi-stage with
`CGO_ENABLED=0`:

```sh
docker build -f deploy/docker/api.Dockerfile \
  --build-arg VERSION=0.1.0 --build-arg COMMIT="$(git rev-parse HEAD)" \
  -t registry.example.org/patchproof/patchproof-api:0.1.0 .
```

`make docker` builds all six images with the repository version. In production,
pin by digest (`repo@sha256:...`); the Helm chart and the Kustomize production
overlay refuse or require digests.

| Dockerfile | Command | Runtime |
|---|---|---|
| `api.Dockerfile` | `cmd/patchproof-api` | distroless static, UID 65532 |
| `controller.Dockerfile` | `cmd/patchproof-controller` | distroless static, UID 65532 |
| `worker.Dockerfile` | `cmd/patchproof-worker` | `golang:1.26.0-bookworm` + git, UID 65532 |
| `cluster-agent.Dockerfile` | `cmd/patchproof-cluster-agent` | distroless static, UID 65532 |
| `proof.Dockerfile` | `cmd/patchproof-proof` | distroless static, UID 65532 |
| `cli.Dockerfile` | `cmd/patchproof` | distroless static, UID 65532 |

OCI labels include title, description, version, revision, source and license.
`VERSION` and `COMMIT` are build arguments; the process version is also set at
runtime through `PATCHPROOF_VERSION` and the chart's `appVersion`.

## Worker toolchain tag

The worker runtime layer is `golang:1.26.0-bookworm`, matching `go 1.26.0` in
`go.mod`. It provides the `go` binary for `go build`, `go test` and `go list`, and
git and CA certificates for repository and HTTPS access. Pin the published image
by digest in `execution.workerImage.digest`; the tag is not a deployment pin.

The image runs as UID/GID 65532 with `HOME=/home/patchproof`,
`TMPDIR=/scratch` and `WORKDIR=/workspace`. The Kubernetes executor mounts
`/workspace` and `/scratch` as emptyDir, so the S3 CAS adapter's temporary spool
file lands on writable scratch storage and the root filesystem can stay
read-only.

The Kubernetes executor injects non-secret worker environment (role, region,
in-memory metadata, disabled events, CAS driver/endpoint/bucket, work directory)
and the attempt token, callback URL, spec and engine image. Static S3 keys are
not injected; use workload identity, optionally with
`execution.automountWorkerToken=true`. See `deploy/helm/patchproof/README.md`.

## CLI image

The CLI image contains only the binary. Remote commands (`run`, `report`,
`case`, `proof`) work without extra tooling. Local `patchproof verify` shells
out to `git` and the Go toolchain, so use a host installation or the worker image
for trusted-local verification.

## Broker image

`broker.Dockerfile` builds `cmd/patchproof-broker`, the trusted stage-in/stage-out component for brokered offline IO. It does not execute repository code and needs object-store access through its own service account.
