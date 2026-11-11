#!/usr/bin/env bash
# Staging acceptance runner. It submits real verification runs against a deployed
# staging release and verifies the signed report offline.
set -euo pipefail

required() {
  local name="$1"
  if [ -z "${!name:-}" ]; then
    echo "error: $name is required" >&2
    exit 2
  fi
}

required PATCHPROOF_API
required PATCHPROOF_TOKEN
required PATCHPROOF_PROJECT
required PATCHPROOF_REPO
required PATCHPROOF_BASE
required PATCHPROOF_HEAD
required PATCHPROOF_POLICY
required PATCHPROOF_CLI
required PATCHPROOF_PUBLIC_KEY

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

run_case() {
  local name="$1" policy="$2" format="${3:-json}"
  echo "== $name =="
  "$PATCHPROOF_CLI" verify \
    --api "$PATCHPROOF_API" --token "$PATCHPROOF_TOKEN" \
    --project "$PATCHPROOF_PROJECT" --repo "$PATCHPROOF_REPO" \
    --base "$PATCHPROOF_BASE" --head "$PATCHPROOF_HEAD" \
    --policy "$policy" --format "$format" --timeout 30m \
    >"$WORKDIR/$name.json" 2>"$WORKDIR/$name.log" || {
      status=$?
      echo "verification failed with exit code $status" >&2
      tail -40 "$WORKDIR/$name.log" >&2
      exit "$status"
    }
  echo "report: $WORKDIR/$name.json"
}

verify_report() {
  local report="$1" policy="$2"
  echo "== offline verification =="
  cas_args=()
  if [ -n "${PATCHPROOF_CAS_ROOT:-}" ]; then
    cas_args+=(--cas-root "$PATCHPROOF_CAS_ROOT")
  elif [ -n "${PATCHPROOF_S3_BUCKET:-}" ]; then
    cas_args+=(--s3-bucket "$PATCHPROOF_S3_BUCKET")
    [ -n "${PATCHPROOF_S3_ENDPOINT:-}" ] && cas_args+=(--s3-endpoint "$PATCHPROOF_S3_ENDPOINT")
    [ -n "${PATCHPROOF_S3_REGION:-}" ] && cas_args+=(--s3-region "$PATCHPROOF_S3_REGION")
  else
    echo "error: set PATCHPROOF_CAS_ROOT or PATCHPROOF_S3_BUCKET for offline verification" >&2
    exit 2
  fi
  "$PATCHPROOF_CLI" proof verify --report "$report" --key "staging=$PATCHPROOF_PUBLIC_KEY" \
    --policy "$policy" --require-signature "${cas_args[@]}"
}

run_case direct "$PATCHPROOF_POLICY"
verify_report "$WORKDIR/direct.json" "$PATCHPROOF_POLICY"

if [ "${PATCHPROOF_BROKERED:-0}" = "1" ]; then
  run_case brokered deploy/staging/policy-brokered.yaml
  verify_report "$WORKDIR/brokered.json" deploy/staging/policy-brokered.yaml
fi

if command -v kubectl >/dev/null 2>&1 && [ -n "${PATCHPROOF_EXECUTION_NAMESPACE:-}" ]; then
  echo "== execute job security assertions =="
  jobs="$(kubectl -n "$PATCHPROOF_EXECUTION_NAMESPACE" get jobs -l patchproof.io/phase=execute -o name || true)"
  if [ -n "$jobs" ]; then
    for job in $jobs; do
      token_env="$(kubectl -n "$PATCHPROOF_EXECUTION_NAMESPACE" get "$job" \
        -o jsonpath='{.spec.template.spec.containers[0].env[*].name}' || true)"
      case "$token_env" in
        *PATCHPROOF_ATTEMPT_TOKEN*|*PATCHPROOF_S3_SECRET_KEY*|*AWS_SECRET_ACCESS_KEY*)
          echo "error: $job exposes credentials to the execute container" >&2
          exit 1
          ;;
      esac
      automount="$(kubectl -n "$PATCHPROOF_EXECUTION_NAMESPACE" get "$job" \
        -o jsonpath='{.spec.template.spec.automountServiceAccountToken}' || true)"
      if [ "$automount" = "true" ]; then
        echo "error: $job automounts a service account token" >&2
        exit 1
      fi
    done
    echo "execute jobs do not expose credentials"
  else
    echo "no execute jobs found (direct or cached run)"
  fi
fi

echo "staging acceptance completed"
