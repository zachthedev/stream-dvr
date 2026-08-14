# Test Style

Go test conventions for this project. Load /go-programmer before writing
or editing any test file.

## Structure

- Test files are 1:1 with source files. `foo.go` has exactly one test file
  `foo_test.go`. No gap files, no scatter, no combined test files.
- File organization: constants and helpers at top, test functions below,
  grouped by the source function they test.
- Table-driven tests as the default pattern.

## Naming

- Test functions: `TestFunctionName_CaseDescription` (underscore-separated)
- Table test variable: `tt` in the loop (`for _, tt := range tests`)
- Table struct fields: `name` (test case name), `input`, `want*`, `wantErr`
- Assertion variables: `got` and `want`

## Table-Driven Tests

```go
tests := []struct {
    name    string
    input   string
    want    int
    wantErr bool
}{
    {name: "valid input", input: "42", want: 42},
    {name: "invalid", input: "xyz", wantErr: true},
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        got, err := Parse(tt.input)
        if (err != nil) != tt.wantErr {
            t.Fatalf("Parse(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
        }
        if got != tt.want {
            t.Errorf("Parse(%q) = %v, want %v", tt.input, got, tt.want)
        }
    })
}
```

## Assertions

- `t.Fatal(err)` or `t.Fatalf(...)` for setup failures (DB open, file write)
- `t.Errorf(...)` for test assertions (lets remaining checks run)
- Format: `t.Errorf("field = %v, want %v", got, want)`
- Use `%q` for strings, `%d` for counts
- Substring checks: `strings.Contains(got, want)` with clear error on failure
- Negative checks: `if strings.Contains(got, unwanted) { t.Errorf(...) }`

## Error Messages

Error messages must derive from the same source as the assertion. Never
hardcode the expected value in the format string:

```go
// Bad: "want 42" will drift if the expected value changes
t.Errorf("Count = %v, want 42", got)

// Good: format string and assertion use the same variable
t.Errorf("Count = %v, want %v", got, want)
```

## Isolation

- `t.TempDir()` for all file-based tests; no writes outside temp dirs
- No real I/O, no network, no side effects outside temp dirs
- `defer` immediately after resource acquisition (`defer s.Close()`)

## Helpers

- All test helpers must call `t.Helper()` as the first line
- Helpers live at the top of the test file, before test functions
- Panic recovery for testing panics:
  ```go
  defer func() {
      if r := recover(); r == nil {
          t.Error("expected panic")
      }
  }()
  ```
