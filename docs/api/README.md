# API documentation

- [OpenAPI 3.1 contract](../../api/openapi/v1/openapi.yaml) — public REST API.
- [Plugin protocol](../../api/proto/patchproof/plugin/v1/plugin.proto) and its
  stdio SDK in [pkg/plugin](../../pkg/plugin).
- [Worker callback contract](../../api/proto/patchproof/worker/v1/worker.proto)
  — internal attempt callbacks authenticated with short-lived attempt tokens.

Public endpoints are under `/api/v1`; `/internal/v1/attempts/*` is reserved for
workers and is intentionally excluded from the public OpenAPI document.

## Public endpoints

| Method and path | Purpose |
|---|---|
| `POST /api/v1/runs` | Submit a run; `Idempotency-Key` required. |
| `GET /api/v1/runs` | Cursor-paginated run list. |
| `GET /api/v1/runs/{run_id}` | Run status and links. |
| `GET /api/v1/runs/{run_id}/steps` | Steps, dependency edges and claims. |
| `POST /api/v1/runs/{run_id}:cancel` | Request cancellation. |
| `GET /api/v1/runs/{run_id}/report?format=json\|markdown\|sarif` | Render the report. |
| `GET /api/v1/runs/{run_id}/evidence` | Evidence documents. |
| `GET /api/v1/runs/{run_id}/counterexamples` | Reproduced counterexamples. |
| `POST /api/v1/reproductions` | Create a controlled replay run. |
| `POST /api/v1/projects`, `GET /api/v1/projects/{project_id}` | Project management. |
| `POST /api/v1/policies`, `POST /api/v1/policies:validate` | Trusted policy management. |
| `GET /api/v1/clusters` | Registered execution clusters. |
| `POST /api/v1/tenants`, `GET /api/v1/tenants/{tenant_id}` | Tenant management. |
| `GET /api/v1/version`, `/healthz`, `/readyz` | Version and probes. |

Roles are `viewer`, `operator`, `admin` and `platform-admin`. The tenant is always
derived from the authenticated token, never from a path or body field.

## Conventions

- Authentication: `Authorization: Bearer <OIDC JWT or dev token>`.
- Tenant scope is taken from the token tenant claim; it is never accepted from
  the request body or path.
- `POST /api/v1/runs` requires an `Idempotency-Key` header. Replaying the same
  key with identical frozen inputs returns the existing run; different inputs
  return `409`.
- Errors use `application/problem+json` with `type`, `title`, `status`,
  `detail`, `code`, `request_id` and `instance`.
- List endpoints use an opaque `cursor` and a bounded `limit`.
- Reports are available as `json`, `markdown` and `sarif`; integrity headers
  `X-PatchProof-Merkle-Root` and `X-PatchProof-Attestation` accompany every
  report response.

## Offline verification

A report is verified without the control plane:

```sh
patchproof proof verify --report report.json --cas-root /var/lib/patchproof/cas \
  --key release=/etc/patchproof/release.pub --policy policy.yaml
```

Exit codes: `0` `VALID_INTEGRITY`, `1` `INVALID`, `2` `UNVERIFIABLE`, `3` usage
error. This validates bytes, signature identity, schema, manifest and policy
requirements; it does not re-execute the engines.
