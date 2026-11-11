# Action cache

The action cache reuses completed results for hermetic analysis and build steps.
It is not a correctness shortcut: only successful, positive evidence is cached,
and every reuse is re-fenced as a new attempt in the new run.

## Key

`internal/actioncache` hashes a canonical document containing:

- tenant id and cache epoch (`scheduler.cacheEpoch`, default service version),
- step kind, engine name, engine version, input manifest digest,
- snapshot digest, base and head archive references,
- policy digest,
- architecture, isolation tier, subject network, runtime class,
- worker image reference,
- all step options in sorted order.

Changing any of these produces a different key. Cross-tenant reads are
impossible because the store keys entries by `(tenant_id, action_key)`.

## Eligibility

Only `analyze`, `impact` and `compile` steps are eligible, and only when the
planner declares `cache.enabled=true`. Test, differential, fuzz and security
steps always execute. A result is stored only when:

- the outcome is `SUCCEEDED`,
- there are no counterexamples,
- every evidence result is `TESTED_WITHIN_SCOPE` or `PROVEN_WITHIN_MODEL`.

## Read and write path

1. The scheduler computes the key before placing a READY step.
2. A hit acquires a new lease (incrementing the generation), clones the cached
   result with fresh evidence ids, run/step ids and timestamps, and commits it
   through `CommitAttempt`. Downstream steps are promoted by the store.
3. A miss dispatches the step normally. After a successful commit the scheduler
   writes the entry. Workers that commit through the callback API write the entry
   in `attempts.Service` so Kubernetes executions are cached too.
4. Expired entries are purged hourly and ignored on read.

## Invalidation

- Engine version, worker image and step options invalidate naturally via the key.
- Bump `scheduler.cacheEpoch` for changes not captured by those fields.
- Disable with `scheduler.actionCacheEnabled=false`.
- TTL is `scheduler.actionCacheTtl` (default 168h).
- Changing inputs, policy, architecture or isolation invalidates naturally via
  the key.

## Limits

The compile cache assumes the step is hermetic: `go-build` runs with a
workspace-local module cache and `GOPROXY` disabled unless a step option enables
a proxy. If a deployment enables network access for compilation, bump the cache
epoch per dependency change or disable the cache. Static analysis is cached as
hint evidence, not as a correctness verdict.
