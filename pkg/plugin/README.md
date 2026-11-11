# Plugin SDK (stdio)

This package is the reference implementation of the plugin transport used for
out-of-process analysis and verification engines. It mirrors the messages in
`api/proto/patchproof/plugin/v1/plugin.proto` and does not require generated
protobuf code.

There are two ways a plugin can receive inputs and return outputs:

- **Sandbox-native (recommended).** The worker stages inputs inside the
  sandbox and passes their paths in `StepRequest.StagedInputs`. The plugin
  writes output files into `StepRequest.OutputDir` and returns
  `StepResponse.Files` plus `EvidenceDrafts`/`CounterexampleDrafts`. The host
  uploads the files into CAS; the plugin never sees CAS credentials.
- **Remote CAS.** The plugin receives `StepRequest.Inputs` as CAS references,
  fetches them itself, and returns `StepResponse.Artifacts` plus raw `Evidence`
  or `Counterexamples` JSON.

## Wire format

Each frame is a 4-byte big-endian payload length followed by UTF-8 JSON:

```json
{"id":"...","type":"capabilities"|"execute"|"response"|"error"|"shutdown","payload":{},"error":{"code":"...","message":"..."}}
```

Frames are capped at 16 MiB by default. A server replies with `response` or
`error` carrying the request `id`. An `execute` payload contains the step
identity, fencing generation, policy digest, step kind, language, engine,
options and deadline, plus either staged input paths or CAS references.
Responses carry the outcome, error class, metrics, outputs, artifact references,
uploaded file descriptors, evidence drafts and counterexample drafts.

### Step request (protocol 1.1)

```json
{
  "run_id": "...", "step_id": "...", "attempt_id": "...",
  "fencing_generation": 3, "policy_digest": "sha256:...",
  "step_kind": "analyze", "language": "go", "engine": "my-plugin",
  "workspace": "/sandbox/workspace",
  "output_dir": "/sandbox/workspace/.patchproof/plugin/<step>-<attempt>",
  "staged_inputs": {
    "base": {"ref": {"algorithm": "sha256", "digest": "...", "size_bytes": 12},
             "path": "/sandbox/workspace/inputs/base", "kind": "file"}
  },
  "options": {"claim.id": "compile"},
  "deadline": "2026-01-01T00:00:00Z"
}
```

`workspace` and `output_dir` are absolute paths inside the worker sandbox.
`output_dir` already exists, is writable, and is emptied before every attempt.
`staged_inputs[*].path` points at a file or an extracted directory the plugin
may read.

### Step response (protocol 1.1)

```json
{
  "attempt_id": "...", "fencing_generation": 3, "outcome": "SUCCEEDED",
  "files": [
    {"name": "report", "path": "report.json", "media_type": "application/json"}
  ],
  "evidence_drafts": [{
    "claim_id": "compile", "kind": "COMPILE", "result": "TESTED_WITHIN_SCOPE",
    "scope": {"input_domain": "go", "seed_count": 4, "generated_cases": 17,
              "elapsed_ms": 31, "budget": "small", "details": {"toolchain": "go1.26"}},
    "assumptions": ["..."], "limits": ["..."],
    "artifact_names": ["report"], "input_names": ["base"]
  }],
  "counterexample_drafts": [{
    "case_id": "case-1", "claim_id": "compile", "scope": "package example.com/x",
    "input_codec": "json", "input_file": "diff",
    "base_observation_file": "report", "head_observation_file": "diff",
    "reproduced": true, "minimized": true, "nondeterminism": ["clock"]
  }],
  "metrics": {"cpu_seconds": 1.25, "execution_ms": 12},
  "outputs": {"confidence": "high"}
}
```

File rules enforced by the host:

- `path` is relative to `output_dir`. Absolute paths, `..` escapes, symbolic
  links and non-regular files are rejected.
- `name` must be unique; at most 256 files and 4 GiB in total are accepted.
- Files are uploaded by the host with its own `ArtifactStore`. The plugin must
  not attempt to contact CAS.

Draft rules enforced by the host:

- `kind` and `result` must parse as `domain.EvidenceKind` and
  `domain.ClaimResult`; `claim_id` is required.
- `artifact_names` must name entries in `files`; `input_names` must name staged
  inputs. Unresolved names reject the whole response.
- Drafts cannot set tenant, run, step, engine identity, environment or
  timestamps; the host fills those in and validates the resulting
  `domain.Evidence`/`domain.Counterexample`.
- At most 512 evidence and 512 counterexample drafts are accepted.
- The legacy raw `evidence`/`counterexamples` fields are only supported by
  remote-CAS adapters; the sandbox adapter rejects them in favour of drafts.

## Server

```go
server := &plugin.Server{Service: myEngine}
if err := server.Serve(ctx, connection); err != nil {
    log.Fatal(err)
}
```

`Server.ExecuteTimeout` bounds an execution when the request has no deadline.
The service must honor `ctx`; the server does not kill the caller's process.

## Client

```go
client, err := plugin.Dial([]string{"/opt/patchproof/engines/my-engine"}, plugin.CommandConfig{})
if err != nil {
    return err
}
defer client.Close()

caps, err := client.Capabilities(ctx)
if err != nil {
    return err
}
response, err := client.Execute(ctx, plugin.StepRequest{
    RunID: runID, StepID: stepID, AttemptID: attemptID,
    FencingGeneration: generation, StepKind: "analyze", Language: "go",
    Workspace: workspace, OutputDir: outputDir,
    StagedInputs: map[string]plugin.StagedInput{
        "base": {Ref: baseRef, Path: basePath, Kind: "file"},
    },
})
```

`internal/engine/pluginengine` implements this translation: one plugin process
per step, staged input paths, host-side CAS uploads, and domain validation of
drafts. The SDK itself stays transport-only.

## Compatibility

`protocol_major` must match; `protocol_minor` is additive. A client rejects an
unknown major before executing. `StepResponse.AttemptID` and
`FencingGeneration`, when present, must match the request. Protocol 1.1 adds
`workspace`, `output_dir`, `staged_inputs`, `files`, `evidence_drafts`,
`counterexample_drafts` and `metrics`; 1.0 peers simply omit them.
