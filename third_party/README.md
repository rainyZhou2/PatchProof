# Third-party dependency inventory

`licenses.csv` lists direct runtime dependencies and their licenses. It is an
inventory, not legal advice. Regenerate the full transitive inventory from the
module graph before a release:

```sh
go mod graph
go list -m -json all
go run github.com/google/go-licenses@latest check ./...
```

The release pipeline also publishes an SBOM and checks it against the image
digests. If a dependency is added or upgraded, update this file and the SBOM.
