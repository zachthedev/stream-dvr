# Quality Gates

Automated enforcement of code quality. All gates run in CI and locally
via `make check`. When adding new code, ensure it passes all gates before
committing.

## Coverage Enforcement

Coverage thresholds are enforced by
[go-test-coverage](https://github.com/vladopajic/go-test-coverage) via
`.testcoverage.yml`. Run locally with `make coverage`.

### Thresholds

- `internal/` packages: 80% per-package minimum
- `cmd/` packages: no minimum (CLI wiring)
- Overall: 70%

### Comment Conventions

These comments are documentation conventions for humans and LLMs. They
explain why coverage is lower in specific places. The enforcement mechanism
is `.testcoverage.yml` thresholds, not the comments.

```go
// coverage:ignore (exec wrapper; requires external binary)
func runExternalTool(...) { ... }

return fmt.Errorf("error: %w", err) // coverage:partial (table always fresh; branch unreachable)
```

- `coverage:ignore` above a function: entire function is untestable (external
  I/O, exec wrappers). Explain why.
- `coverage:partial` inline on a specific line: this branch is unreachable by
  design. Explain the invariant that prevents it.

When adding `coverage:ignore`, consider whether the package's threshold
override in `.testcoverage.yml` needs adjusting.

### Adding New Packages

New `internal/` packages inherit the 80% threshold. If a package has
legitimate untestable code, add an override to `.testcoverage.yml`:

```yaml
override:
  - threshold: 50
    path: internal/my-new-package/
```

## Declaration Ordering

Enforced by [decorder](https://pkg.go.dev/gitlab.com/bosi/decorder) via
golangci-lint (`.golangci.yml`). Run locally with `make lint`.

### Required Order (linted)

Within each Go file, top-level declarations must appear in this order:

1. `type` declarations (with associated const/var enum values nearby)
2. `const` declarations
3. `var` declarations
4. `func` declarations (init first, then exported, then unexported)

This is enforced by `decorder`. Multiple `const`/`var` blocks are allowed
(grouped by concern). The linter also enforces that `init()` comes first
among functions.

### Within Functions (convention, not linted)

Within the `func` section, group by concern:

1. Methods on a type (grouped by receiver)
2. Constructor functions (`New*`)
3. Utility/helper functions

## Test Sync

Enforced by `go tool testpair`. Run locally with `make testpair`.

### Rules

- **1:1 file pairing**: every `foo.go` in `cmd/` or `internal/` must have
  a `foo_test.go`. No orphan test files without a source companion.
- **Naming sync**: test functions must follow `TestSourceFunc_Case` where
  `SourceFunc` is an exported symbol in the corresponding source file.

This catches drift when functions are renamed or files are reorganized.
LLMs should run `make testpair` after renaming functions or moving code.

## Gates

Gates are organized into phases. `make check` runs them in this order;
each gate is also callable standalone.

| Phase          | Gate            | Tool               | Config              | Make target      |
| -------------- | --------------- | ------------------ | ------------------- | ---------------- |
| Canonical form | Module tidy     | go mod tidy        |                     | `make tidy`      |
|                | Generated files | cmd/generate       |                     | `make generate`  |
|                | Formatting      | gofumpt, goimports | `.golangci.yml`     | `make fmt`       |
| Static         | Lint            | golangci-lint      | `.golangci.yml`     | `make lint`      |
|                | Test sync       | go tool testpair   | `.allow.testpair`   | `make testpair`  |
|                | Dead code       | go tool deadcode   | `.allow.deadcode`   | `make deadcode`  |
| Security       | Vulnerabilities | govulncheck        |                     | `make vulncheck` |
| Behavior       | Tests           | go test            |                     | `make test`      |
|                | Coverage        | go-test-coverage   | `.testcoverage.yml` | `make coverage`  |
| Artifact       | Build           | go build           |                     | `make build`     |
|                | Release layout  | go build           |                     | `make dist`      |

Run all gates: `make check`, which is every row above in that order.

`make fmt` is not one of them. It rewrites files, and a gate that edits
what it is checking cannot fail. Formatting is enforced by `make lint`,
which reads.

Generation is invoked as `cmd/generate` directly, never through
`go generate ./...`. That would run every `//go:generate` directive the
checked-out tree declares, and CI builds a fork's branch, so it is an
arbitrary-command primitive bought for nothing. Do not "fix" the Makefile
to use it.

## CI Pipeline

The GitHub Actions workflow (`.github/workflows/check.yml`) runs each gate
as its own job. Jobs are grouped by phase in the file but all run in
parallel. Coverage, `test` and `build` all run on the full OS matrix:
a threshold met on one platform and missed on another is a gate that
passes or fails by which runner reported first.

The aggregate `check` job goes green when no job failed. Every job is
listed in `allowed-skips`, so a run where the `gate` job skipped all of
them is green too. That is deliberate: a pull request touching only
`CHANGELOG.md` or `.release-manifest.json` changes no code. Such a run
verifies nothing itself, so `check` instead requires the base commit's
own push run to have passed, and fails when no run for it exists.

A skip therefore never fails the aggregate, so a gate that misfires and
skips still produces a green `check`. Read the job list, not just the
badge.
