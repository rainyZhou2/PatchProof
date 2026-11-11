# syntax=docker/dockerfile:1.7
# PatchProof cluster agent: exposes one execution cluster over the authenticated
# /v1/executions API and publishes cluster heartbeats. Build from the repository
# root:
#   docker build -f deploy/docker/cluster-agent.Dockerfile .
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
    -o /out/patchproof-cluster-agent ./cmd/patchproof-cluster-agent

FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="patchproof-cluster-agent" \
      org.opencontainers.image.description="PatchProof execution cluster agent" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/patchproof/patchproof" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build --chown=65532:65532 /out/patchproof-cluster-agent /usr/local/bin/patchproof-cluster-agent

USER 65532:65532
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/patchproof-cluster-agent"]
