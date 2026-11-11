# Kustomize deployment

`base` contains the namespace, generated ConfigMap, API and controller
Deployments and Services, service accounts and NetworkPolicies.
`overlays/production` sets replicas, resources and digest-pinned images, and
patches OIDC and signing configuration.

No Secret is stored in git. The base expects a Secret named
`patchproof-secrets` with these keys:

| Key | Purpose |
|---|---|
| `postgres-dsn` | PostgreSQL DSN. |
| `s3-access-key` | CAS access key. |
| `s3-secret-key` | CAS secret key. |
| `attempt-token-key` | At least 32 bytes for attempt tokens. |
| `dev-token` | Development bearer token; removed from use under the OIDC overlay. |

Create it in the target namespace:

```sh
kubectl create namespace patchproof-system
kubectl -n patchproof-system create secret generic patchproof-secrets \
  --from-literal=postgres-dsn="$PATCHPROOF_POSTGRES_DSN" \
  --from-literal=s3-access-key="$AWS_ACCESS_KEY_ID" \
  --from-literal=s3-secret-key="$AWS_SECRET_ACCESS_KEY" \
  --from-literal=attempt-token-key="$(openssl rand -hex 32)" \
  --from-literal=dev-token="$(openssl rand -hex 32)"
```

The base config is an integration starting point: it uses development auth with
the tenant `11111111-1111-4111-8111-111111111111`, points at
`nats://nats.patchproof-system.svc.cluster.local:4222`, an S3 bucket named
`patchproof`, and an empty `externalUrl`. Edit `base/config/patchproof.yaml`
before using it, and create the tenant row through the admin API or a seed.

## Render and apply the production overlay

The overlay expects two ConfigMaps supplied by the operator (see
`overlays/production/patch-auth-oidc.yaml`) and a signing Secret:

```sh
kubectl -n patchproof-system create configmap patchproof-oidc \
  --from-literal=issuer="$OIDC_ISSUER" \
  --from-literal=audience="$OIDC_AUDIENCE" \
  --from-literal=jwks-url="$OIDC_JWKS_URL"
kubectl -n patchproof-system create configmap patchproof-site \
  --from-literal=external-url="$PATCHPROOF_EXTERNAL_URL" \
  --from-literal=worker-image="$PATCHPROOF_WORKER_IMAGE"
kubectl -n patchproof-system create secret generic patchproof-signing \
  --from-file=signing.pem=./signing.pem --from-file=signing.pub=./signing.pub

kubectl apply -k deploy/kustomize/overlays/production
```

The committed overlay image digests are all-zero sentinels so an unrendered
overlay can never pull an image. Replace them from the release pipeline:

```sh
cd deploy/kustomize/overlays/production
kustomize edit set image ghcr.io/patchproof/patchproof-api@sha256:...
kustomize edit set image ghcr.io/patchproof/patchproof-controller@sha256:...
```

`kubectl kustomize deploy/kustomize/overlays/production` renders the result; the
CI workflow checks that both overlays render and parse.

## Configuration notes

- The Deployment mounts the generated ConfigMap at `/etc/patchproof` and sets
  `PATCHPROOF_CONFIG=/etc/patchproof/config.yaml`.
- Background jobs (migrations) are not part of the base. Apply the schema with
  `make migrate` or run a one-shot Job before starting the API; the API's
  `migrateOnStart` is disabled in the config.
- The base does not deploy a migration Job, a cluster agent, an HPA or worker
  Jobs. The Helm chart is the fuller production asset; Kustomize is provided for
  teams that compose their own overlays.
- NetworkPolicies are deny-by-default with explicit DNS, PostgreSQL, NATS,
  S3/HTTPS and OTLP rules. Narrow the `0.0.0.0/0` CIDRs to real destinations.
