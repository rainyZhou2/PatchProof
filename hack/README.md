# Toolchain helper

`hack/go` is a thin `go` shim used by the Makefile. By default it executes the
Go toolchain from `PATH` and honors the normal Go environment.

The isolated build environment used during development sets `PP_SANDBOX=1`,
which points `GOPATH`, `GOMODCACHE` and `GOCACHE` at writable temporary
directories. Do not set that variable on a developer workstation or CI runner;
the normal Go cache and module proxy are preferred there.
