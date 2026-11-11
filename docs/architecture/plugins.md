# External plugins

`execution.plugins` registers out-of-process engines. A plugin is an executable
inside the worker image that speaks the stdio protocol implemented in
`pkg/plugin` (protocol 1.1). The parent worker stages inputs into the sandbox,
starts one plugin process per step, and uploads the files the plugin names; the
plugin never receives CAS credentials.

```yaml
execution:
  allowedLanguages: [go, python]
  plugins:
    - name: python-analyzer
      command: ["/opt/patchproof/plugins/python-analyzer"]
      env:
        PYTHONDONTWRITEBYTECODE: "1"
      languages: [python]
      stepKinds: [analyze, impact-analysis]
      timeout: 5m
```

The policy selects a plugin per step slot:

```yaml
verification:
  engines:
    analyze: {name: python-analyzer, language: python}
    impact:  {name: python-analyzer, language: python}
```

## Contract

- Request: step identity, fencing generation, policy digest, options, workspace,
  output directory and staged input paths plus CAS references.
- Response: outcome/error class/retryable, evidence drafts, counterexample
  drafts, output files and metrics.
- The host creates evidence/counterexample identifiers, validates every enum and
  path, rejects traversal/symlinks/duplicates, caps output at 256 files and
  4 GiB, then uploads files into CAS and returns `engine.Result`.
- Raw evidence JSON from a plugin is rejected; only drafts plus host-uploaded
  files are accepted so artifacts cannot reference missing blobs.

## Failure and security

- A protocol major mismatch is rejected before execution; a missing binary or
  crash is classified as an engine failure and never as a passing verdict.
- The plugin runs inside the same sandbox as the worker; isolation comes from
  the executor (RuntimeClass, read-only root, no network for subject code).
- Plugins are trusted images. Pin them by digest and include them in SBOM and
  signature verification. There is no OCI auto-loader and no generated gRPC
  client in this tree.
