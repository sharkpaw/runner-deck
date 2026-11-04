# detect-go-module-root

Composite action: find the Go module root (a directory containing both `go.mod` and `cmd/runner-manager`) in the repo root or a subdirectory.

- **Input**: `working_dir` (default `.`); first check whether `cmd/runner-manager` exists in that directory.
- **Output**: `root` (module root path), `found` (`true`/`false`).

Reused by `ci-manager.yml`, `ci-agent.yml`, etc., so a single repo or a monorepo can share the same CI setup.
