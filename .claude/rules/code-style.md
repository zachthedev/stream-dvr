# Code Style

Load the /go-programmer skill when writing or editing Go files.
Load the /go-debugger skill when diagnosing Go issues.
Load the /toml-programmer skill when editing TOML config files.

## General

- Never use em dashes; use colons, semicolons, commas, or parentheses
- Comments explain why, not what
- When verifying Go builds, use `go build -o /dev/null ./cmd/...` to avoid
  dropping `.exe` files in the repo root

## Go Naming

### Verb prefixes

Use these consistently for function/method names:

- **Load**: read from disk or DB
- **Compute**: derive from in-memory data
- **Build**: construct a new struct
- **Decode**: parse from bytes

### Type suffixes

- **Result**: single-item outcome
- **Report**: aggregate collection
- **Opts**: input parameter struct

### Package-qualified readability

Check how exported names read at the callsite (`pkg.Name`). If the package
name already provides context, the function name shouldn't repeat it.
`config.Load` reads cleanly (not `config.LoadConfig`).

## Go Error Handling

- Return explicit errors as the last return value
- Wrap with context: `fmt.Errorf("loading config: %w", err)`
- Use `errors.Is` and `errors.As` for inspection, never string matching
- No silent error swallowing; at minimum log to `slog`

## Go Package Structure

- `cmd/` contains only CLI wiring: flag parsing, output formatting.
  No business logic. If a function could be imported by another package, it
  belongs in `internal/`.
- `internal/` contains all pure logic. Functions accept data and return data;
  I/O happens at the call site in `cmd/`.
- One concern per package. If changing a behavior requires edits in multiple
  packages, consolidate.
