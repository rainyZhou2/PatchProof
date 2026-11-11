# syntax=docker/dockerfile:1.7
# PatchProof controller: orchestration, scheduling, attempt accounting, proof
# aggregation and report signing. Build from the repository root:
#   docker build -f deploy/docker/controller.Dockerfile .
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
    -o /out/patchproof-controller ./cmd/patchproof-controller

FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="patchproof-controller" \
      org.opencontainers.image.description="PatchProof orchestrator and scheduler" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/patchproof/patchproof" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build --chown=65532:65532 /out/patchproof-controller /usr/local/bin/patchproof-controller

USER 65532:65532
EXPOSE 9090
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/patchproof-controller"]
