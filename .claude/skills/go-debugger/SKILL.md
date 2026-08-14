---
name: go-debugger
description: Debug Go applications using modern diagnostic tools, profiling, pattern-based analysis, and systematic troubleshooting techniques. Handles concurrency, memory, and performance issues.
dependencies: go>=1.24
---

# Go Debugger Skill

Diagnose and fix Go runtime errors, goroutine issues, data races, performance problems, and logic bugs with clear explanations and idiomatic solutions.

## Instructions

1. **Understand the problem:**
   Identify whether the issue is a nil pointer dereference, goroutine leak, data race, deadlock, interface nil confusion, slice aliasing, context misuse, or incorrect logic. Determine which Go concepts (goroutines, channels, mutexes, interfaces, slices, error handling, context) are involved.

2. **Reproduce reliably:**
   Write a minimal test case that triggers the bug. Use `go test -run TestName` for focused reproduction. Enable the race detector with `-race` for concurrency issues. Use `testing/synctest` (Go 1.25+) for timing-dependent bugs.

3. **Measure before guessing:**
   Use the right diagnostic tool for the problem class:
   - `go vet` and `staticcheck` for static analysis
   - `go test -race` for data races
   - `go tool pprof` for CPU, memory, goroutine, and block profiling
   - `go tool trace` for execution timeline analysis
   - `delve` (dlv) for interactive debugging
   - `runtime/trace.FlightRecorder` (Go 1.25+) for lightweight production tracing
   - Goroutine leak profiles (Go 1.26, experimental) for leak detection

4. **Identify the root cause:**
   Provide a detailed explanation grounded in Go's runtime behavior:
   - why a nil pointer was dereferenced (uninitialized field, unchecked error, nil interface)
   - why a goroutine leaked (blocked channel, missing cancellation, forgotten close)
   - why a race condition occurred (shared state without synchronization)
   - why a deadlock happened (lock ordering, channel direction, select starvation)
   - why an interface comparison behaved unexpectedly (nil interface vs typed nil)
   - why a slice mutation affected unexpected data (shared backing array)
   - why context cancellation wasn't propagated (missing parent chain)

5. **Isolate aggressively:**
   Narrow the problem to the smallest possible scope:
   - Extract the failing logic into a standalone test
   - Use subtests to isolate specific inputs
   - Use `t.Parallel()` to expose concurrency assumptions
   - Add temporary `slog.Debug` calls at boundaries

6. **Provide an idiomatic and minimal fix:**
   Offer a correction that follows Go best practices:
   - Check errors before using return values
   - Use `context.Context` for cancellation propagation
   - Prefer channels for communication, mutexes for state protection
   - Use `sync.WaitGroup.Go()` (Go 1.25+) to avoid `Add`/`Done` mismatches
   - Use `errgroup.Group` for concurrent error aggregation
   - Use value receivers for small immutable types, pointer receivers for mutation
   - Wrap errors with `fmt.Errorf("context: %w", err)` for traceability

7. **Verify the fix:**
   - Run `go test -race ./...` to confirm no races
   - Run `go vet ./...` and `staticcheck ./...`
   - Add a regression test for the specific bug
   - Verify with `golangci-lint run` for broader checks

## Diagnostic Tools

### go vet - Built-in Static Analysis

```bash
# Check all packages
go vet ./...
```

Detects: printf format mismatches, unreachable code, shadowed variables, struct tag issues, and more.

### staticcheck - Gold Standard Static Analysis

```bash
# Install
go install honnef.co/go/tools/cmd/staticcheck@latest

# Run
staticcheck ./...
```

Low false positive rate. Catches ineffectual assignments, unnecessary type conversions, deprecated API usage, and subtler issues go vet misses.

### Race Detector

```bash
# Run tests with race detection
go test -race ./...

# Build with race detection
go build -race ./cmd/myapp
```

Detects unsynchronized reads and writes to shared memory at runtime. Always run tests with `-race` in CI.

### pprof - CPU, Memory, Goroutine Profiling

```bash
# CPU profile from tests
go test -cpuprofile=cpu.prof -bench=. ./...
go tool pprof cpu.prof

# Memory profile
go test -memprofile=mem.prof -bench=. ./...
go tool pprof mem.prof

# HTTP pprof (add to running server)
import _ "net/http/pprof"
go tool pprof http://localhost:6060/debug/pprof/heap
go tool pprof http://localhost:6060/debug/pprof/goroutine

# Inside pprof interactive mode
(pprof) top 20
(pprof) list FunctionName
(pprof) web              # generate SVG flamegraph
(pprof) traces           # show full call stacks
```

### go tool trace - Execution Timeline

```bash
# Capture trace
go test -trace=trace.out ./...

# View in browser
go tool trace trace.out
```

Shows goroutine scheduling, GC pauses, syscalls, and blocking events on a timeline.

### Delve (dlv) - Interactive Debugger

```bash
# Install
go install github.com/go-delve/delve/cmd/dlv@latest

# Debug test
dlv test ./internal/user/ -- -test.run TestCreateUser

# Debug binary
dlv debug ./cmd/myapp

# Attach to running process
dlv attach <pid>
```

Common delve commands:
```
break main.go:42       # set breakpoint
break mypackage.MyFunc # break on function
continue               # run to breakpoint
next                   # step over
step                   # step into
print myVar            # inspect variable
goroutines             # list goroutines
goroutine 5            # switch to goroutine
stack                  # show call stack
```

### FlightRecorder (Go 1.25+)

```go
import "runtime/trace"

// Lightweight always-on tracing for production
fr := trace.NewFlightRecorder()
fr.SetPeriod(5 * time.Second)
if err := fr.Start(); err != nil {
    log.Fatal(err)
}

// On incident, snapshot the last 5 seconds
f, _ := os.Create("incident.trace")
fr.WriteTo(f)
f.Close()
// Then: go tool trace incident.trace
```

## Common Issue Patterns

### Nil Pointer Dereference

```go
// Problem: unchecked error before using result
user, err := repo.FindByID(ctx, id)
fmt.Println(user.Name) // PANIC if err != nil and user is zero value

// Fix: always check error first
user, err := repo.FindByID(ctx, id)
if err != nil {
    return fmt.Errorf("finding user: %w", err)
}
fmt.Println(user.Name)

// Problem: nil interface method call
var handler http.Handler // nil
handler.ServeHTTP(w, r) // PANIC

// Fix: check or initialize
if handler == nil {
    handler = http.DefaultServeMux
}
```

### Goroutine Leaks

```go
// Problem: goroutine blocked on channel forever
func process(ctx context.Context) {
    ch := make(chan int)
    go func() {
        result := expensiveWork()
        ch <- result // blocks forever if nobody reads
    }()
    // function returns without reading from ch
}

// Fix: use context for cancellation
func process(ctx context.Context) (int, error) {
    ch := make(chan int, 1) // buffered so sender won't block
    go func() {
        ch <- expensiveWork()
    }()

    select {
    case result := <-ch:
        return result, nil
    case <-ctx.Done():
        return 0, ctx.Err()
    }
}

// Problem: forgotten HTTP response body close
resp, err := http.Get(url)
if err != nil {
    return err
}
// missing: defer resp.Body.Close()
// leaks connection goroutine

// Fix: always close response body
resp, err := http.Get(url)
if err != nil {
    return err
}
defer resp.Body.Close()
```

### Data Races

```go
// Problem: unsynchronized map access
var cache = map[string]string{}

func handler(w http.ResponseWriter, r *http.Request) {
    key := r.URL.Query().Get("key")
    if val, ok := cache[key]; ok { // RACE: concurrent map read
        fmt.Fprint(w, val)
        return
    }
    val := computeExpensive(key)
    cache[key] = val // RACE: concurrent map write
    fmt.Fprint(w, val)
}

// Fix 1: sync.RWMutex
var (
    cache   = map[string]string{}
    cacheMu sync.RWMutex
)

func handler(w http.ResponseWriter, r *http.Request) {
    key := r.URL.Query().Get("key")

    cacheMu.RLock()
    val, ok := cache[key]
    cacheMu.RUnlock()

    if ok {
        fmt.Fprint(w, val)
        return
    }

    val = computeExpensive(key)

    cacheMu.Lock()
    cache[key] = val
    cacheMu.Unlock()

    fmt.Fprint(w, val)
}

// Fix 2: sync.Map (better for mostly-read workloads)
var cache sync.Map

func handler(w http.ResponseWriter, r *http.Request) {
    key := r.URL.Query().Get("key")
    if val, ok := cache.Load(key); ok {
        fmt.Fprint(w, val)
        return
    }
    val := computeExpensive(key)
    cache.Store(key, val)
    fmt.Fprint(w, val)
}
```

### Deadlocks

```go
// Problem: lock ordering violation
func transfer(from, to *Account, amount int) {
    from.mu.Lock()
    defer from.mu.Unlock()
    to.mu.Lock() // DEADLOCK if another goroutine locks in opposite order
    defer to.mu.Unlock()
    from.Balance -= amount
    to.Balance += amount
}

// Fix: consistent lock ordering
func transfer(from, to *Account, amount int) {
    first, second := from, to
    if from.ID > to.ID {
        first, second = to, from
    }
    first.mu.Lock()
    defer first.mu.Unlock()
    second.mu.Lock()
    defer second.mu.Unlock()
    from.Balance -= amount
    to.Balance += amount
}

// Problem: unbuffered channel deadlock
ch := make(chan int)
ch <- 42 // blocks forever — no receiver yet

// Fix: buffer or separate goroutine
ch := make(chan int, 1)
ch <- 42
```

### Interface Nil vs Typed Nil

```go
// Problem: typed nil satisfies interface but is not == nil
type MyError struct{ Msg string }
func (e *MyError) Error() string { return e.Msg }

func getError() error {
    var err *MyError // typed nil pointer
    return err       // returns non-nil interface holding nil *MyError
}

func main() {
    err := getError()
    if err != nil {
        // This EXECUTES even though the underlying pointer is nil
        fmt.Println(err) // PANIC: nil pointer dereference in Error()
    }
}

// Fix: return the interface zero value explicitly
func getError() error {
    var err *MyError
    if err == nil {
        return nil // return untyped nil
    }
    return err
}
```

### Slice Shared Backing Array

```go
// Problem: append to sub-slice mutates original
original := []int{1, 2, 3, 4, 5}
sub := original[:3]
sub = append(sub, 99) // overwrites original[3]!
fmt.Println(original) // [1 2 3 99 5] — not [1 2 3 4 5]

// Fix: use full slice expression to limit capacity
sub := original[:3:3] // length=3, capacity=3
sub = append(sub, 99) // allocates new backing array
fmt.Println(original) // [1 2 3 4 5] — unchanged
```

### Context Cancellation Misuse

```go
// Problem: creating context.Background() inside a function
// that should respect the caller's cancellation
func (s *Service) Process(ctx context.Context, items []Item) error {
    for _, item := range items {
        // BUG: ignores ctx cancellation
        childCtx := context.Background()
        s.processItem(childCtx, item)
    }
    return nil
}

// Fix: derive from the parent context
func (s *Service) Process(ctx context.Context, items []Item) error {
    for _, item := range items {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
        s.processItem(ctx, item)
    }
    return nil
}
```

## Memory and Performance Debugging

### Escape Analysis

```bash
# See what escapes to heap
go build -gcflags="-m" ./...

# More verbose
go build -gcflags="-m -m" ./...
```

Common escapes to watch:
- Returning pointers to local variables
- Closures capturing local variables
- Interface conversions (value boxed on heap)
- Slices that grow beyond stack size

### Benchmark Analysis

```go
// Use testing.B.Loop (Go 1.24+) for accurate benchmarks
func BenchmarkJSON(b *testing.B) {
    data := loadTestData()
    b.ResetTimer()
    for b.Loop() {
        json.Marshal(data)
    }
}

// Compare allocations
func BenchmarkStringConcat(b *testing.B) {
    b.Run("plus", func(b *testing.B) {
        for b.Loop() {
            _ = "hello" + " " + "world"
        }
    })
    b.Run("builder", func(b *testing.B) {
        for b.Loop() {
            var sb strings.Builder
            sb.WriteString("hello")
            sb.WriteString(" ")
            sb.WriteString("world")
            _ = sb.String()
        }
    })
}
```

```bash
# Run with allocation stats
go test -bench=. -benchmem ./...

# Compare benchmarks
go install golang.org/x/perf/cmd/benchstat@latest
go test -bench=. -count=10 > old.txt
# make changes
go test -bench=. -count=10 > new.txt
benchstat old.txt new.txt
```

### Green Tea GC Tuning (Go 1.25+ experimental, default in 1.26)

```bash
# Check GC behavior
GODEBUG=gctrace=1 ./myapp

# Tune GC target (default 100 = trigger at 2x live heap)
GOGC=200 ./myapp          # less frequent GC, more memory
GOGC=50 ./myapp           # more frequent GC, less memory

# Set memory limit (soft)
GOMEMLIMIT=512MiB ./myapp
```

## Quick Diagnostics Commands

```bash
# Full diagnostic pass
go vet ./...
staticcheck ./...
go test -race -count=1 ./...
golangci-lint run ./...

# Profile goroutines (running server)
curl http://localhost:6060/debug/pprof/goroutine?debug=2

# Profile heap
go tool pprof http://localhost:6060/debug/pprof/heap

# Trace execution
go test -trace=trace.out -run TestSlowTest ./...
go tool trace trace.out

# Find goroutine leaks (check goroutine count over time)
curl http://localhost:6060/debug/pprof/goroutine?debug=1 | head -1

# Deadlock detection (if server hangs)
kill -SIGQUIT <pid>  # dumps all goroutine stacks to stderr
```

## Debugging Decision Tree

```
Bug reported
├── Compile error?
│   ├── Type mismatch → check interface satisfaction, pointer vs value
│   ├── Undefined → check exports, imports, package scope
│   └── Cannot use → check type assertions, conversions
│
├── Panic at runtime?
│   ├── nil pointer → trace initialization, check error returns
│   ├── index out of range → validate slice/array bounds
│   ├── interface conversion → use comma-ok or type switch
│   ├── concurrent map access → add mutex or use sync.Map
│   └── send on closed channel → restructure channel lifecycle
│
├── Incorrect output?
│   ├── Loop variable capture → check closures in goroutines
│   ├── Slice aliasing → use full slice expression [:n:n]
│   ├── Interface nil confusion → return explicit nil
│   └── Wrong error checked → use errors.Is/errors.As/errors.AsType
│
├── Deadlock / hang?
│   ├── Channel blocked → check buffer size, select with timeout
│   ├── Mutex ordering → enforce consistent order by ID
│   ├── Context not cancelled → propagate from parent
│   └── WaitGroup mismatch → use wg.Go() (1.25+)
│
├── Performance?
│   ├── High CPU → pprof CPU profile, check hot loops
│   ├── High memory → pprof heap, escape analysis, check allocations
│   ├── High goroutines → pprof goroutine, check for leaks
│   ├── GC pressure → benchmem, reduce allocations, tune GOGC
│   └── Slow I/O → go tool trace, check blocking events
│
└── Data race?
    ├── Map → sync.RWMutex or sync.Map
    ├── Slice → copy or use mutex
    ├── Struct field → mutex or channel
    └── Global var → sync.Once or init()
```

## When to Use This Skill

Invoke this skill when:
- Investigating runtime panics or nil pointer dereferences
- Debugging goroutine leaks or deadlocks
- Tracking down data races
- Profiling CPU, memory, or goroutine usage
- Debugging context cancellation issues
- Investigating interface behavior or type assertion failures
- Analyzing benchmark results

## Design Pattern Reference

See references/REFERENCE.md for:
- Common error patterns and fixes
- Diagnostic shell scripts
- pprof analysis techniques
- Race detector output interpretation
- Delve commands reference
- Testing concurrent code with synctest
- Benchmark analysis techniques
