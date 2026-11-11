# Examples

`policy.yaml` is a complete trusted verification policy for a local Go
repository. It requires compilation, the repository test suite, protected-path
checks and one behavior-preservation contract.

`contracts/normalize.yaml` is a differential contract for:

```go
package demo

func Normalize(value int) int
```

The contract declares a pure-function input domain, seed inputs and the
`json-differential/v1` oracle. To try it:

1. Create a Git repository containing the package and the contract file at
   `contracts/normalize.yaml`.
2. Copy `policy.yaml` to the repository root.
3. Run:

```sh
patchproof verify --base HEAD~1 --head HEAD --policy policy.yaml
```

Local verification runs the all-in-one pipeline with `isolationTier:
trusted-local`. It executes code inside the control-plane process and must only
be used with trusted repositories. For untrusted code, schedule the run through
a cluster that advertises `untrusted-gvisor` or `untrusted-vm` and set
`execution.isolationTier` accordingly.
