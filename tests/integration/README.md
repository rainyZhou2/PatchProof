# Integration tests

These tests exercise the real adapters and the all-in-one pipeline. They are
skipped by default so `go test ./...` stays hermetic.

```sh
# 1. Start PostgreSQL, NATS and MinIO (the Compose stack is enough for storage).
docker compose -f deploy/compose/docker-compose.yaml up -d postgres

# 2. Run the PostgreSQL-backed pipeline test.
PATCHPROOF_TEST_INTEGRATION=1 \
PATCHPROOF_TEST_POSTGRES_DSN='postgres://localhost:5432/patchproof_e2e?sslmode=disable' \
go test ./tests/integration/...
```

The test creates a temporary Git repository, submits a compile + regression
run, waits for completion, and verifies the signed report offline. It requires
`git` and a Go toolchain on `PATH`.

Adapter-level live tests for S3 and NATS live next to their packages and use:

- `PATCHPROOF_TEST_S3_ENDPOINT`, `PATCHPROOF_TEST_S3_BUCKET`,
  `PATCHPROOF_TEST_S3_REGION`, `PATCHPROOF_TEST_S3_ACCESS_KEY`,
  `PATCHPROOF_TEST_S3_SECRET_KEY`
- `PATCHPROOF_TEST_NATS_URL`
- `PATCHPROOF_TEST_POSTGRES_DSN`

Never point these variables at production infrastructure.
