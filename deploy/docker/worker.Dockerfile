# syntax=docker/dockerfile:1.7
# PatchProof worker image used by per-attempt Kubernetes Jobs.
#
# The runtime layer is the Go toolchain image golang:1.26.0-bookworm. The Go
# toolchain and git are required because the bundled Go engines invoke
# `go build`, `go test` and `go list` inside the staged snapshot. That tag is
# intentionally the same minor as go.mod (go 1.26.0); pin the published image
# by digest in Helm values rather than by tag.
#
# Build from the repository root:
#   docker build -f deploy/docker/worker.Dockerfile .
FROM golang:1.26.0-bookworm AS build

ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src
ENV CGO_ENABLED=0

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/patchproof-worker ./cmd/patchproof-worker

FROM golang:1.26.0-bookworm

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="patchproof-worker" \
      org.opencontainers.image.description="PatchProof step worker with Go toolchain" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/patchproof/patchproof" \
      org.opencontainers.image.licenses="Apache-2.0" \
      io.patchproof.worker.toolchain="golang:1.26.0-bookworm"

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 patchproof \
    && useradd --uid 65532 --gid 65532 --create-home --home-dir /home/patchproof --shell /usr/sbin/nologin patchproof \
    && mkdir -p /workspace /scratch /run/spec \
    && chown -R 65532:65532 /workspace /scratch /home/patchproof

COPY --from=build --chown=65532:65532 /out/patchproof-worker /usr/local/bin/patchproof-worker

ENV HOME=/home/patchproof
ENV TMPDIR=/scratch
ENV PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# The worker receives its step spec at /run/spec/step.json and writes only to
# /workspace and /scratch, both mounted as emptyDir by the executor. The runtime
# user has no write access to the image filesystem, so readOnlyRootFilesystem is
# compatible with this image.
WORKDIR /workspace
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/patchproof-worker"]
