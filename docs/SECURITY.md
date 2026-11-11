# Security policy

## Reporting a vulnerability

Report suspected vulnerabilities through GitHub Security Advisories for the
repository that hosts this source tree. Do not open a public issue, and do not
include exploit details, tenant data, signing keys or credentials in a public
forum.

Include:

- affected component and version or commit,
- a reproduction that does not require access to another tenant,
- the observed impact and any known preconditions,
- whether the issue is already public.

You will receive an acknowledgement after triage. This project does not operate
a paid bug bounty programme.

## Supported versions

Security fixes are produced for the latest minor release line. The current
development line is `0.1.x`; no older line is supported. Container images and
Helm charts are supported only when pinned by digest to a build produced by the
release pipeline for this repository.

## Scope

In scope:

- the public API under `/api/v1` and the attempt callback API under `/internal/v1`,
- the controller, scheduler, proof aggregation and report signing paths,
- the cluster agent and the Kubernetes Job execution contract,
- the worker and bundled Go engines (`engines/`),
- the filesystem and S3 CAS adapters,
- policy evaluation and protected-path enforcement (`internal/policy`),
- the CLI and offline proof verifier (`cmd/patchproof`, `internal/proofverify`),
- deployment assets in `deploy/` when used with the documented configuration.

Out of scope unless a PatchProof control fails first:

- compromise of cluster administrator credentials, the Kubernetes API server,
  the container runtime, the host kernel or the cloud account,
- a runtime escape that is reproducible only with a correctly isolated
  `untrusted-gvisor` or `untrusted-vm` profile and no PatchProof misconfiguration,
- denial of service from a tenant that already holds valid credentials and
  exceeds its configured quotas,
- vulnerabilities in the Go toolchain, Git or base images that have no
  PatchProof-specific exploitation path; report those upstream as well.

## Security model summary

PatchProof trusts the control plane (T0) and signed engine images (T1). Source
code, patches, test output, plugin output and Git-provider metadata are treated
as untrusted (T2/T3). Isolation depends on the configured runtime: the
`trusted-local` tier executes code in the control-plane process and must not be
used for untrusted pull requests. See `security/threat-model.md` and the
isolation guidance in `../README.md`.

Report signatures attest that a report was produced by a holder of the signing
key and that the referenced evidence is internally consistent. They do not
prove that the verified program is correct.
