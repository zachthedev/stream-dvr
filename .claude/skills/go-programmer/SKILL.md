---
name: go-programmer
description: Write idiomatic, production-grade Go code using modern features (1.22-1.26), explicit error handling, interfaces, generics, and concurrency primitives. Uses godoc conventions and standard tooling.
dependencies: go>=1.26
---

# Go Programmer Skill

Write production-grade Go code following modern idioms and the latest stable features (Go 1.22 through 1.26).

## Instructions

1. **Fully understand the user request:**
   Determine whether the task involves designing structs, defining interfaces, implementing error handling, writing concurrent code, creating HTTP services, or organizing modules.
   Identify key constraints such as concurrency boundaries, error propagation needs, interface contracts, and performance requirements.

2. **Plan types and data structures with precision:**
   - Choose between structs, interfaces, and type aliases based on domain needs.
   - Use embedding for composition; avoid deep struct hierarchies.
   - Use generics when they eliminate repeated code across types, but prefer interfaces for behavioral abstraction.
   - Model invariants with types: custom types over raw strings, `time.Duration` over `int`, enums with `iota` over loose constants.
   - Use pointer vs value receivers deliberately: pointers for mutation or large structs, values for small immutable types.

3. **Write idiomatic Go implementations:**
   - Return explicit errors as the last return value. Never panic for expected failures.
   - Use `fmt.Errorf` with `%w` for error wrapping. Use `errors.Is`, `errors.As`, and `errors.AsType` for inspection.
   - Define small, focused interfaces at the point of use (consumer side), not the implementation side.
   - Use receiver methods to attach behavior to types. Group constructors (`New`), exported methods, then unexported methods.
   - Keep functions short and focused. Prefer early returns over deep nesting.
   - Use `context.Context` as the first parameter for cancellable or deadline-bound operations.

4. **Apply documentation and code-style best practices:**
   - Use `//` line comments for godoc. Start with the identifier name: `// UserService handles user operations.`
   - Document exported types, functions, methods, and package-level declarations.
   - Use `package doc.go` for package-level documentation when explaining architecture.
   - Run `gofmt` (automatic) and `golangci-lint` to maintain consistency.
   - Reserve blank lines between logically separate function groups and sections.

5. **Use testing frameworks effectively:**
   - Write table-driven tests as the default pattern.
   - Use `testing.B.Loop` (Go 1.24+) for benchmarks instead of the old `b.N` loop.
   - Use `testing/synctest` (Go 1.25+) for testing concurrent code with virtual time.
   - Use subtests (`t.Run`) for grouping related cases.
   - Use `testify` for assertion helpers when standard library assertions become verbose.
   - Run `go test -race` to detect data races.

6. **Optimize build and development workflow:**
   - Use `go build`, `go test`, and `go vet` as baseline.
   - Use `golangci-lint run` as the primary linting tool (50+ linters, parallel, cached).
   - Use `staticcheck` for gold-standard static analysis with low false positives.
   - Use `go test -bench` with `testing.B.Loop` for performance measurement.
   - Use `go tool pprof` for CPU, memory, goroutine, and block profiling.

7. **Maintain clean project structure:**
   - Use `cmd/` for application entry points, `internal/` for private packages, top-level packages for public API.
   - Use Go modules (`go.mod`) for dependency management.
   - Use tool directives in `go.mod` (Go 1.24+) instead of the `tools.go` hack.
   - Keep package names short, lowercase, singular. Avoid `util`, `common`, `misc`.

8. **Provide explanations and alternatives:**
   For every design, explain why a pattern is chosen and propose alternatives when relevant:
   - channels vs mutexes (communication vs shared state)
   - interfaces vs generics (behavioral abstraction vs type-level polymorphism)
   - functional options vs config structs (public API flexibility vs simplicity)
   - `sync.WaitGroup.Go()` vs manual `Add`/`Done` (Go 1.25+ vs backward compat)
   - standard `net/http.ServeMux` vs third-party routers (sufficiency vs features)
   - `log/slog` vs third-party loggers (standard library vs ecosystem)

9. **Maintain simplicity above all:**
   Follow Go proverbs. A little copying is better than a little dependency. Clear is better than clever. Don't communicate by sharing memory; share memory by communicating. The bigger the interface, the weaker the abstraction.

## Modern Go Features (1.22-1.26)

Target **Go 1.26** (latest stable, released February 2026).

### Go 1.22: Enhanced ServeMux and Range Integers

```go
// ///////////////////////////////////////////////
// HTTP Routing (Go 1.22+)
// ///////////////////////////////////////////////

mux := http.NewServeMux()

// Method routing with path parameters
mux.HandleFunc("GET /posts/{id}", func(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    // ...
})

mux.HandleFunc("POST /posts", createPost)
mux.HandleFunc("DELETE /posts/{id}", deletePost)

// Wildcard: matches any trailing path
mux.HandleFunc("GET /files/{path...}", serveFile)

// Range over integers
for i := range 10 {
    fmt.Println(i) // 0 through 9
}
```

### Go 1.23: Range-Over-Function Iterators

```go
import (
    "iter"
    "maps"
    "slices"
)

// ///////////////////////////////////////////////
// Custom Iterators
// ///////////////////////////////////////////////

// iter.Seq[V] for single-value sequences
func Fibonacci() iter.Seq[int] {
    return func(yield func(int) bool) {
        a, b := 0, 1
        for {
            if !yield(a) {
                return
            }
            a, b = b, a+b
        }
    }
}

// iter.Seq2[K, V] for key-value sequences
func Enumerate[V any](s []V) iter.Seq2[int, V] {
    return func(yield func(int, V) bool) {
        for i, v := range s {
            if !yield(i, v) {
                return
            }
        }
    }
}

// Range over function iterators
for n := range Fibonacci() {
    if n > 100 {
        break
    }
    fmt.Println(n)
}

// Standard library iterator helpers
names := []string{"alice", "bob", "charlie"}
for i, name := range slices.Backward(names) {
    fmt.Printf("%d: %s\n", i, name)
}

collected := slices.Collect(maps.Keys(myMap))
```

### Go 1.24: Generic Type Aliases, Tool Directives, testing.B.Loop

```go
// ///////////////////////////////////////////////
// Generic Type Aliases (Go 1.24+)
// ///////////////////////////////////////////////

type Set[T comparable] = map[T]struct{}

// ///////////////////////////////////////////////
// Tool Directives in go.mod (Go 1.24+)
// ///////////////////////////////////////////////

// In go.mod — replaces the tools.go hack:
// tool (
//     golang.org/x/tools/cmd/stringer
//     github.com/golangci/golangci-lint/cmd/golangci-lint
// )

// ///////////////////////////////////////////////
// Benchmarks with testing.B.Loop (Go 1.24+)
// ///////////////////////////////////////////////

func BenchmarkProcess(b *testing.B) {
    input := prepareInput()
    b.ResetTimer()
    for b.Loop() {
        process(input)
    }
}

// ///////////////////////////////////////////////
// Sandboxed Filesystem with os.Root (Go 1.24+)
// ///////////////////////////////////////////////

func readSandboxed(dir, filename string) ([]byte, error) {
    root, err := os.OpenRoot(dir)
    if err != nil {
        return nil, fmt.Errorf("opening root %s: %w", dir, err)
    }
    defer root.Close()
    return root.ReadFile(filename)
}
```

### Go 1.25: sync.WaitGroup.Go(), testing/synctest, Container-Aware GOMAXPROCS

```go
import "sync"

// ///////////////////////////////////////////////
// sync.WaitGroup.Go() (Go 1.25+)
// ///////////////////////////////////////////////

// Before Go 1.25 — manual Add/Done boilerplate
func fetchAllOld(urls []string) {
    var wg sync.WaitGroup
    for _, url := range urls {
        wg.Add(1)
        go func() {
            defer wg.Done()
            fetch(url)
        }()
    }
    wg.Wait()
}

// Go 1.25+ — WaitGroup.Go() eliminates boilerplate
func fetchAll(urls []string) {
    var wg sync.WaitGroup
    for _, url := range urls {
        wg.Go(func() {
            fetch(url)
        })
    }
    wg.Wait()
}

// ///////////////////////////////////////////////
// testing/synctest (Go 1.25+)
// ///////////////////////////////////////////////

import "testing/synctest"

func TestDebounce(t *testing.T) {
    synctest.Run(func() {
        var count int
        debounced := NewDebouncer(100*time.Millisecond, func() {
            count++
        })

        debounced.Trigger()
        debounced.Trigger()
        debounced.Trigger()

        // Advance virtual time — no real wall-clock delay
        synctest.Sleep(150 * time.Millisecond)

        if count != 1 {
            t.Errorf("expected 1 call, got %d", count)
        }
    })
}

// ///////////////////////////////////////////////
// FlightRecorder (Go 1.25+)
// ///////////////////////////////////////////////

import "runtime/trace"

func setupTracing() (*trace.FlightRecorder, error) {
    fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{
        MinAge:   5 * time.Second,
        MaxBytes: 10 << 20, // 10 MB ring buffer
    })
    if err := fr.Start(); err != nil {
        return nil, fmt.Errorf("starting flight recorder: %w", err)
    }
    return fr, nil
}
```

### Go 1.26: new() Expressions, errors.AsType, Green Tea GC Default

```go
// ///////////////////////////////////////////////
// new() with Expressions (Go 1.26+)
// ///////////////////////////////////////////////

// Before: required a helper or temporary variable
func toPtr(s string) *string { return &s }
config := &Config{Name: toPtr("myapp")}

// Go 1.26: new() accepts expressions
config := &Config{Name: new("myapp")}
timeout := new(5 * time.Second)

// ///////////////////////////////////////////////
// errors.AsType[E]() (Go 1.26+)
// ///////////////////////////////////////////////

// Before: errors.As with pointer target
var notFound *NotFoundError
if errors.As(err, &notFound) {
    fmt.Println(notFound.ID)
}

// Go 1.26: generic type-safe error checking
if notFound, ok := errors.AsType[*NotFoundError](err); ok {
    fmt.Println(notFound.ID)
}

// ///////////////////////////////////////////////
// slog.NewMultiHandler (Go 1.26+)
// ///////////////////////////////////////////////

import "log/slog"

handler := slog.NewMultiHandler(
    slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
    slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug}),
)
logger := slog.New(handler)
```

### Project Setup

```go
// go.mod
module github.com/example/myproject

go 1.26

tool (
    github.com/golangci/golangci-lint/cmd/golangci-lint
    golang.org/x/tools/cmd/stringer
)

require (
    // dependencies
)
```

### Error Handling Conventions

```go
// ///////////////////////////////////////////////
// Error Hierarchy
// ///////////////////////////////////////////////

// Sentinel errors for expected conditions
var (
    ErrNotFound   = errors.New("not found")
    ErrConflict   = errors.New("conflict")
    ErrForbidden  = errors.New("forbidden")
)

// Custom error type with context
type ValidationError struct {
    Field   string
    Message string
}

func (e *ValidationError) Error() string {
    return fmt.Sprintf("validation error: field %s: %s", e.Field, e.Message)
}

// Wrapping errors with context
func (s *UserService) GetUser(ctx context.Context, id string) (User, error) {
    user, err := s.repo.FindByID(ctx, id)
    if err != nil {
        return User{}, fmt.Errorf("getting user %s: %w", id, err)
    }
    if user == nil {
        return User{}, fmt.Errorf("user %s: %w", id, ErrNotFound)
    }
    return *user, nil
}

// Checking errors
func handleError(err error) {
    if errors.Is(err, ErrNotFound) {
        // handle not found
    }

    // Go 1.26+ generic error checking
    if ve, ok := errors.AsType[*ValidationError](err); ok {
        log.Printf("invalid field: %s", ve.Field)
    }
}
```

### Generics

```go
// ///////////////////////////////////////////////
// Generic Utility Functions
// ///////////////////////////////////////////////

// Filter returns elements matching the predicate.
func Filter[S ~[]E, E any](s S, pred func(E) bool) S {
    var result S
    for _, v := range s {
        if pred(v) {
            result = append(result, v)
        }
    }
    return result
}

// Map transforms each element.
func Map[S ~[]E, E, R any](s S, fn func(E) R) []R {
    result := make([]R, len(s))
    for i, v := range s {
        result[i] = fn(v)
    }
    return result
}

// ///////////////////////////////////////////////
// Generic Constraints
// ///////////////////////////////////////////////

type Number interface {
    ~int | ~int8 | ~int16 | ~int32 | ~int64 |
        ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
        ~float32 | ~float64
}

func Sum[T Number](values []T) T {
    var total T
    for _, v := range values {
        total += v
    }
    return total
}
```

### Concurrency Patterns

```go
import (
    "context"
    "sync"
    "golang.org/x/sync/errgroup"
)

// ///////////////////////////////////////////////
// errgroup for Concurrent Error Handling
// ///////////////////////////////////////////////

func fetchAll(ctx context.Context, urls []string) ([]Response, error) {
    g, ctx := errgroup.WithContext(ctx)
    responses := make([]Response, len(urls))

    for i, url := range urls {
        g.Go(func() error {
            resp, err := fetch(ctx, url)
            if err != nil {
                return fmt.Errorf("fetching %s: %w", url, err)
            }
            responses[i] = resp
            return nil
        })
    }

    if err := g.Wait(); err != nil {
        return nil, err
    }
    return responses, nil
}

// ///////////////////////////////////////////////
// Worker Pool
// ///////////////////////////////////////////////

func workerPool[T, R any](ctx context.Context, workers int, jobs <-chan T, process func(context.Context, T) (R, error)) <-chan Result[R] {
    results := make(chan Result[R])
    var wg sync.WaitGroup

    for range workers {
        wg.Go(func() {
            for job := range jobs {
                r, err := process(ctx, job)
                select {
                case results <- Result[R]{Value: r, Err: err}:
                case <-ctx.Done():
                    return
                }
            }
        })
    }

    go func() {
        wg.Wait()
        close(results)
    }()

    return results
}

type Result[T any] struct {
    Value T
    Err   error
}
```

### Structured Logging with slog

```go
import "log/slog"

// ///////////////////////////////////////////////
// slog Setup
// ///////////////////////////////////////////////

func setupLogger(env string) *slog.Logger {
    var handler slog.Handler
    switch env {
    case "production":
        handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
            Level: slog.LevelInfo,
        })
    default:
        handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
            Level:     slog.LevelDebug,
            AddSource: true,
        })
    }
    return slog.New(handler)
}

// Usage with context
func (s *UserService) CreateUser(ctx context.Context, req CreateUserRequest) (User, error) {
    s.logger.InfoContext(ctx, "creating user",
        slog.String("email", req.Email),
        slog.String("role", req.Role),
    )

    user, err := s.repo.Create(ctx, req)
    if err != nil {
        s.logger.ErrorContext(ctx, "failed to create user",
            slog.String("email", req.Email),
            slog.Any("error", err),
        )
        return User{}, fmt.Errorf("creating user: %w", err)
    }

    s.logger.InfoContext(ctx, "user created",
        slog.String("user_id", user.ID),
    )
    return user, nil
}
```

## Build Tooling

### Testing

```bash
# Run all tests
go test ./...

# Run with race detector
go test -race ./...

# Run specific package
go test ./internal/user/...

# Run matching tests
go test -run TestUserService ./...

# Verbose output
go test -v ./...

# Benchmarks (use testing.B.Loop in Go 1.24+)
go test -bench=. -benchmem ./...

# Coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Linting and Static Analysis

```bash
# Primary meta-linter (50+ linters, parallel, cached)
golangci-lint run ./...

# Gold-standard static analysis
staticcheck ./...

# Built-in compiler-level checks
go vet ./...
```

### Profiling

```bash
# CPU profile
go test -cpuprofile=cpu.prof -bench=. ./...
go tool pprof cpu.prof

# Memory profile
go test -memprofile=mem.prof -bench=. ./...
go tool pprof mem.prof

# HTTP pprof endpoint (add to server)
import _ "net/http/pprof"
# Then: go tool pprof http://localhost:6060/debug/pprof/heap
```

### Development Tools

```bash
# Format code (automatic with most editors)
gofmt -w .

# Tidy modules
go mod tidy

# Install tool dependencies (Go 1.24+ with tool directives)
go install tool

# Generate code
go generate ./...
```

## Style Guide

Use `//` line comment headers to organize code into logical sections. These are **separate from godoc documentation**.

### Section Header Conventions

- **Section headers** use `//` comment blocks with slashes for organizational markers
- **godoc** uses `//` comments starting with the identifier name

Never conflate organizational headers with documentation comments.

```go
// ///////////////////////////////////////////////
// Repository Interfaces
// ///////////////////////////////////////////////

// UserRepository provides CRUD operations for user entities.
//
// Implementations must be safe for concurrent use.
type UserRepository interface {

    // ///////////////////////////////////////////////
    // Query Methods
    // ///////////////////////////////////////////////

    // ///// By ID /////

    // FindByID retrieves a user by their unique identifier.
    // Returns ErrNotFound if no user exists with the given ID.
    FindByID(ctx context.Context, id uuid.UUID) (User, error)

    // ///// By Email /////

    // FindByEmail retrieves a user by email address.
    FindByEmail(ctx context.Context, email string) (User, error)

    // ///////////////////////////////////////////////
    // Mutation Methods
    // ///////////////////////////////////////////////

    // ///// Create /////

    // Create persists a new user.
    // Returns ErrConflict if the email already exists.
    Create(ctx context.Context, user User) (User, error)

    // ///// Update /////

    // Update modifies an existing user.
    Update(ctx context.Context, user User) (User, error)
}
```

### Module Organization with Headers

```go
// ///////////////////////////////////////////////
// Service Implementation
// ///////////////////////////////////////////////

// UserService manages user operations.
type UserService struct {
    repo   UserRepository
    logger *slog.Logger
}

// ///////////////////////////////////////////////
// Constructor
// ///////////////////////////////////////////////

// NewUserService creates a new UserService.
func NewUserService(repo UserRepository, logger *slog.Logger) *UserService {
    return &UserService{repo: repo, logger: logger}
}

// ///////////////////////////////////////////////
// Public Methods
// ///////////////////////////////////////////////

// ///// Registration /////

// Register creates a new user account and sends a welcome email.
func (s *UserService) Register(ctx context.Context, req RegisterRequest) (User, error) {
    // implementation
}

// ///// Authentication /////

// Authenticate verifies user credentials.
func (s *UserService) Authenticate(ctx context.Context, email, password string) (User, error) {
    // implementation
}

// ///////////////////////////////////////////////
// Private Methods
// ///////////////////////////////////////////////

func (s *UserService) hashPassword(password string) string {
    // implementation
}
```

### godoc Standards

Use `//` comments for actual documentation of public APIs:

```go
// Brief description starting with the identifier name.
//
// Extended description with additional details about behavior,
// implementation notes, or usage considerations.
//
// The ctx parameter controls cancellation and deadlines.
// The id parameter is the user's unique identifier.
//
// Returns ErrNotFound if the user does not exist.
func (s *UserService) GetUser(ctx context.Context, id string) (User, error) {
```

## When to Use This Skill

Invoke this skill when:
- Writing new Go code or packages
- Designing structs, interfaces, and type hierarchies
- Implementing HTTP services with `net/http`
- Creating error types and handling strategies
- Setting up new Go modules
- Writing concurrent code with goroutines and channels
- Working with generics and iterators

## Design Patterns

See references/REFERENCE.md for complete implementations of:
- Functional Options Pattern
- Repository Pattern (interface + struct)
- State Machine Pattern (iota constants with methods)
- Strategy Pattern (interface-based)
- Error Handling Hierarchies
- Middleware Pattern (HTTP middleware chain)
- Worker Pool Pattern (goroutines + channels + errgroup)
- Table-Driven Tests
- Context-Based Cancellation and Timeout

## Quick Reference

| Old Pattern | Modern Go Replacement |
|------------|----------------------|
| `tools.go` with blank imports | Tool directives in `go.mod` (1.24+) |
| `for i := 0; i < b.N; i++` | `for b.Loop()` (1.24+) |
| `runtime.SetFinalizer` | `runtime.AddCleanup` (1.24+) |
| Manual `wg.Add(1)` / `defer wg.Done()` | `wg.Go(func() { ... })` (1.25+) |
| `time.Sleep` in concurrent tests | `testing/synctest` with virtual time (1.25+) |
| `errors.As(err, &target)` | `errors.AsType[*T](err)` (1.26+) |
| `toPtr(val)` helper for pointer init | `new(expression)` (1.26+) |
| Third-party routers (chi, gorilla) | `net/http.ServeMux` with method routing (1.22+) |
| `log.Printf` / third-party loggers | `log/slog` structured logging (1.21+) |
| Manual iterator functions | `iter.Seq[V]` / `iter.Seq2[K,V]` (1.23+) |
