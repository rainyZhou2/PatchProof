# Contributing

## Developer certificate of origin

Every commit must be signed off with `git commit -s`, certifying the Developer
Certificate of Origin 1.1 (https://developercertificate.org/). Contributions
without a matching `Signed-off-by` trailer are not merged.

## Before opening a pull request

1. Read `architecture/overview.md` and the ADRs that touch the area you are
   changing. State ownership, fencing and evidence-format changes need an ADR.
2. Keep changes scoped. Do not edit generated files or `go.mod`/`go.sum` as a
   side effect of an unrelated change.
3. Add tests at the package that owns the behaviour. External adapters use the
   fakes in the same package; the unit suite must not require PostgreSQL, NATS,
   S3 or Kubernetes.
4. Run the local checks. In this repository the Go toolchain may be provided by
   `hack/go`, which the Makefile detects automatically:

   ```sh
   make fmt
   make verify
   ```

   On a normal workstation with Go 1.26 on `PATH`, `make` uses `go` directly.
5. Update the contract you changed: OpenAPI in `api/openapi/v1`, protobuf in
   `api/proto`, Helm values/schema, and the relevant runbook. A behaviour change
   without its contract is incomplete.

## Commit and review conventions

- Use imperative subject lines (`scheduler: reject stale generation on commit`).
- Describe invariants and failure modes in the body when they are not obvious.
- Do not include credentials, tenant data, customer repository names or
  generated SBOMs in commits.
- At least one reviewer who did not write the change must approve changes to
  `internal/security`, `internal/policy`, `internal/app/attempts`,
  `internal/domain/evidence.go` or report signing.

## Generated code

`api/proto` is the source of truth for plugin and attempt contracts. Generated
Go bindings are produced with `make generate` (Buf preferred, `protoc` supported)
and are not checked in at this stage. Do not hand-edit generated output.

`api/openapi/v1/openapi.yaml` is authored by hand and validated by CI. It is
versioned with `/v1` in the path; breaking changes require a new path version.

## Schema and API changes

- PostgreSQL migrations are append-only files in `migrations/postgres`. Follow
  expand/contract as described in `migration/schema-expand-contract.md`.
- Never drop or rename a column in the same release that stops writing it.
- API fields are additive within `v1`. Removing a field or changing its meaning
  requires a new major path or a documented deprecation window.
- Evidence and report schema identifiers (`patchproof.evidence/v1`,
  `patchproof.report/v1`) change only with a new verifier path.

## Deployment asset changes

- Helm: update `values.yaml`, `values.schema.json` and the chart README together.
  `helm lint` and `helm template` must succeed with the CI values.
- Kustomize: keep secrets out of git; use `secretKeyRef` only. Production
  overlays pin images by digest.
- Dockerfiles: non-root UID 65532, read-only root filesystem, no runtime shell
  requirement, `CGO_ENABLED=0` for control-plane binaries.
- Compose is an integration harness, not a production or untrusted-execution
  environment.

## License

By contributing you agree that your contribution is licensed under the Apache
License 2.0. See `../LICENSE`.
