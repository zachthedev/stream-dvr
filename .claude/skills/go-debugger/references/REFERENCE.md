# Go Debugger Reference

Common error patterns, diagnostic techniques, and debugging strategies for Go applications.

## Common Error Patterns and Fixes

### Nil Pointer Dereference

```go
// Pattern: using result before checking error
resp, err := http.Get(url)
body, _ := io.ReadAll(resp.Body) // PANIC if resp is nil

// Fix: check error, then use result
resp, err := http.Get(url)
if err != nil {
    return fmt.Errorf("fetching %s: %w", url, err)
}
defer resp.Body.Close()
body, err := io.ReadAll(resp.Body)
if err != nil {
    return fmt.Errorf("reading response: %w", err)
}
```

### Goroutine Leak Patterns

```go
// Pattern 1: blocked on unbuffered channel
func leak() {
    ch := make(chan int)
    go func() {
        result := work()
        ch <- result // blocks forever if nobody reads
    }()
    // function returns without reading
}

// Fix: buffered channel + context
func noLeak(ctx context.Context) (int, error) {
    ch := make(chan int, 1)
    go func() {
        ch <- work()
    }()
    select {
    case v := <-ch:
        return v, nil
    case <-ctx.Done():
        return 0, ctx.Err()
    }
}

// Pattern 2: infinite loop without exit
func leak2() {
    go func() {
        for {
            doWork() // runs forever
        }
    }()
}

// Fix: use context or done channel
func noLeak2(ctx context.Context) {
    go func() {
        for {
            select {
            case <-ctx.Done():
                return
            default:
                doWork()
            }
        }
    }()
}

// Pattern 3: ticker not stopped
func leak3() {
    ticker := time.NewTicker(time.Second)
    // missing: defer ticker.Stop()
    for range ticker.C {
        doWork()
    }
}

// Fix: always stop ticker
func noLeak3(ctx context.Context) {
    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            doWork()
        case <-ctx.Done():
            return
        }
    }
}
```

### Map Concurrent Access Panic

```go
// Pattern: concurrent map write
// fatal error: concurrent map writes
var m = map[string]int{}

func inc(key string) {
    m[key]++ // PANIC under concurrent access
}

// Fix 1: sync.Mutex
var (
    m  = map[string]int{}
    mu sync.Mutex
)

func inc(key string) {
    mu.Lock()
    m[key]++
    mu.Unlock()
}

// Fix 2: sync.Map for high-read, low-write scenarios
var m sync.Map

func inc(key string) {
    val, _ := m.LoadOrStore(key, new(atomic.Int64))
    val.(*atomic.Int64).Add(1)
}
```

### Channel Direction Errors

```go
// Pattern: writing to closed channel
ch := make(chan int)
close(ch)
ch <- 1 // PANIC: send on closed channel

// Fix: only close from the sender side, use sync.Once if multiple senders
type SafeChannel struct {
    ch   chan int
    once sync.Once
}

func (sc *SafeChannel) Close() {
    sc.once.Do(func() {
        close(sc.ch)
    })
}
```

### Loop Variable Capture (pre-1.22 codebases)

```go
// Pattern (Go <1.22): loop variable captured by closure
for _, url := range urls {
    go func() {
        fetch(url) // all goroutines see the LAST url
    }()
}

// Fix (Go <1.22): shadow the variable
for _, url := range urls {
    url := url // shadow
    go func() {
        fetch(url) // each goroutine has its own copy
    }()
}

// Go 1.22+: loop variables are per-iteration, so this just works
for _, url := range urls {
    go func() {
        fetch(url) // correct in Go 1.22+
    }()
}
```

## Diagnostic Shell Scripts

### Full Project Health Check

```bash
#!/bin/bash
set -e

echo "=== go vet ==="
go vet ./...

echo "=== staticcheck ==="
staticcheck ./...

echo "=== golangci-lint ==="
golangci-lint run ./...

echo "=== tests with race detector ==="
go test -race -count=1 -timeout=5m ./...

echo "=== coverage ==="
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1

echo "=== all checks passed ==="
```

### Goroutine Leak Detection Script

```bash
#!/bin/bash
# Monitor goroutine count over time
# Requires: import _ "net/http/pprof" in your server

HOST="${1:-localhost:6060}"
INTERVAL="${2:-5}"

echo "Monitoring goroutines on $HOST every ${INTERVAL}s..."
echo "Time,Goroutines"

while true; do
    COUNT=$(curl -s "http://$HOST/debug/pprof/goroutine?debug=1" | head -1 | grep -oP 'goroutine profile: total \K[0-9]+')
    echo "$(date +%H:%M:%S),$COUNT"
    sleep "$INTERVAL"
done
```

### Memory Profile Snapshot

```bash
#!/bin/bash
# Capture and analyze heap profile from running server

HOST="${1:-localhost:6060}"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
PROFILE="heap_${TIMESTAMP}.prof"

echo "Capturing heap profile from $HOST..."
curl -s "http://$HOST/debug/pprof/heap" > "$PROFILE"

echo "Top allocations:"
go tool pprof -top "$PROFILE" 2>/dev/null | head -20

echo ""
echo "Profile saved: $PROFILE"
echo "Interactive: go tool pprof $PROFILE"
echo "Web UI:      go tool pprof -http=:8080 $PROFILE"
```

## pprof Analysis Techniques

### CPU Profiling

```bash
# From tests
go test -cpuprofile=cpu.prof -bench=BenchmarkHotPath ./...
go tool pprof cpu.prof

# From running server (30 second sample)
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

```
# Inside pprof
(pprof) top 20           # top functions by CPU time
(pprof) top -cum 20      # top by cumulative time (includes callees)
(pprof) list MyFunction  # line-by-line annotation
(pprof) web              # flamegraph in browser
(pprof) peek MyFunction  # callers and callees
```

### Memory (Heap) Profiling

```bash
# Current allocations (inuse_space)
go tool pprof http://localhost:6060/debug/pprof/heap

# All allocations since start (alloc_space)
go tool pprof -alloc_space http://localhost:6060/debug/pprof/heap
```

```
# Inside pprof
(pprof) top -inuse_space   # current live objects
(pprof) top -alloc_space   # total allocations (GC pressure)
(pprof) top -inuse_objects # count of live objects
(pprof) list NewUser       # allocations inside function
```

### Goroutine Profiling

```bash
# Goroutine stacks (text dump)
curl http://localhost:6060/debug/pprof/goroutine?debug=2

# Goroutine profile for pprof
go tool pprof http://localhost:6060/debug/pprof/goroutine
```

```
# Inside pprof
(pprof) top              # where goroutines are blocked
(pprof) traces           # full stack traces grouped by count
```

### Block Profiling

```go
// Enable block profiling in your server
import "runtime"

func init() {
    runtime.SetBlockProfileRate(1) // capture all blocking events
}
```

```bash
go tool pprof http://localhost:6060/debug/pprof/block
```

Shows where goroutines spend time waiting on synchronization primitives.

## Race Detector Output Interpretation

```
==================
WARNING: DATA RACE
Read at 0x00c0000a6000 by goroutine 7:     ← the reader
  main.handler()
      /app/server.go:42 +0x38              ← file:line of the read

Previous write at 0x00c0000a6000 by goroutine 6:  ← the writer
  main.updateCache()
      /app/cache.go:15 +0x64               ← file:line of the write

Goroutine 7 (running) created at:
  net/http.(*Server).Serve()
      ...

Goroutine 6 (running) created at:
  main.startCacheUpdater()
      /app/cache.go:8 +0x3c                ← where the writing goroutine was spawned
==================
```

Key fields:
- **Read/Write addresses**: same address means shared variable
- **File:line**: exact source location of the racy access
- **Created at**: where the goroutines were spawned (helps trace ownership)

Fix checklist:
1. Identify the shared variable at the given address
2. Add synchronization: mutex, channel, or atomic
3. Re-run with `-race` to confirm fix

## Delve Commands Reference

```
# Execution
continue (c)       - run to next breakpoint
next (n)           - step over
step (s)           - step into
stepout (so)       - step out of current function
restart (r)        - restart program

# Breakpoints
break (b) file:line     - set breakpoint
break funcname          - break on function entry
condition bp_num expr   - conditional breakpoint
clear bp_num            - remove breakpoint
clearall                - remove all breakpoints
breakpoints (bp)        - list breakpoints

# Inspection
print (p) expr    - evaluate expression
locals            - show local variables
args              - show function arguments
whatis expr       - show type of expression
display -a expr   - show expr at every stop

# Goroutines
goroutines (grs)  - list all goroutines
goroutine (gr) N  - switch to goroutine N
stack (bt)        - show call stack
frame N           - switch to stack frame N

# Advanced
call func(args)   - call function (Go 1.11+)
rewind            - reverse execution (replay mode)
```

## Testing Concurrent Code

### synctest (Go 1.25+)

```go
import "testing/synctest"

func TestRateLimiter(t *testing.T) {
    synctest.Run(func() {
        limiter := NewRateLimiter(10, time.Second) // 10 per second

        // Use all tokens
        for range 10 {
            if !limiter.Allow() {
                t.Fatal("should allow first 10 requests")
            }
        }

        // 11th should be rejected
        if limiter.Allow() {
            t.Fatal("should reject 11th request")
        }

        // Advance virtual time
        synctest.Sleep(time.Second)

        // Should allow again
        if !limiter.Allow() {
            t.Fatal("should allow after refill")
        }
    })
}

func TestTimeout(t *testing.T) {
    synctest.Run(func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        // Advance virtual time past the deadline
        synctest.Sleep(6 * time.Second)

        if ctx.Err() == nil {
            t.Fatal("context should be expired")
        }
    })
}
```

### Race Detector in Tests

```go
func TestConcurrentAccess(t *testing.T) {
    cache := NewCache()
    var wg sync.WaitGroup

    // Write goroutines
    for i := range 100 {
        wg.Go(func() {
            cache.Set(fmt.Sprintf("key-%d", i), i)
        })
    }

    // Read goroutines
    for i := range 100 {
        wg.Go(func() {
            cache.Get(fmt.Sprintf("key-%d", i))
        })
    }

    wg.Wait()
}

// Run with: go test -race -run TestConcurrentAccess ./...
```

### Testing for Goroutine Leaks

```go
import "runtime"

func TestNoGoroutineLeak(t *testing.T) {
    before := runtime.NumGoroutine()

    // Exercise the code under test
    svc := NewService()
    svc.Start()
    svc.DoWork()
    svc.Stop()

    // Allow goroutines to wind down
    time.Sleep(100 * time.Millisecond)

    after := runtime.NumGoroutine()
    if after > before+1 {
        t.Errorf("goroutine leak: before=%d after=%d", before, after)
        // Dump goroutine stacks for debugging
        buf := make([]byte, 1<<20)
        n := runtime.Stack(buf, true)
        t.Logf("goroutines:\n%s", buf[:n])
    }
}
```

## Benchmark Analysis Techniques

### Comparing Before and After

```bash
# Install benchstat
go install golang.org/x/perf/cmd/benchstat@latest

# Run benchmarks with sufficient count
go test -bench=BenchmarkProcess -benchmem -count=10 ./... > old.txt

# Make your optimization
# ...

go test -bench=BenchmarkProcess -benchmem -count=10 ./... > new.txt

# Compare
benchstat old.txt new.txt
```

Output:
```
name          old time/op    new time/op    delta
Process-8     45.2µs ± 2%    12.1µs ± 1%   -73.23%  (p=0.000 n=10+10)

name          old alloc/op   new alloc/op   delta
Process-8     4.82kB ± 0%    1.20kB ± 0%   -75.10%  (p=0.000 n=10+10)

name          old allocs/op  new allocs/op  delta
Process-8       15.0 ± 0%       4.0 ± 0%   -73.33%  (p=0.000 n=10+10)
```

### Writing Effective Benchmarks

```go
// Use b.Loop (Go 1.24+) — compiler-aware, prevents dead code elimination
func BenchmarkMarshalJSON(b *testing.B) {
    user := User{ID: "1", Name: "Alice", Email: "alice@example.com"}
    b.ResetTimer()
    for b.Loop() {
        json.Marshal(user)
    }
}

// Sub-benchmarks for comparing approaches
func BenchmarkLookup(b *testing.B) {
    data := generateTestData(10000)

    b.Run("map", func(b *testing.B) {
        m := make(map[string]int, len(data))
        for i, d := range data {
            m[d.Key] = i
        }
        b.ResetTimer()
        for b.Loop() {
            _ = m[data[rand.IntN(len(data))].Key]
        }
    })

    b.Run("linear", func(b *testing.B) {
        b.ResetTimer()
        for b.Loop() {
            target := data[rand.IntN(len(data))].Key
            for _, d := range data {
                if d.Key == target {
                    break
                }
            }
        }
    })
}

// Memory-focused benchmark
func BenchmarkAllocation(b *testing.B) {
    b.ReportAllocs()
    for b.Loop() {
        buf := make([]byte, 1024)
        _ = buf
    }
}
```

## Error Pattern Quick Reference

| Pattern | Symptom | Fix |
|---------|---------|-----|
| Nil pointer | `runtime error: invalid memory address` | Check errors before using results |
| Concurrent map | `fatal error: concurrent map writes` | Use `sync.Mutex` or `sync.Map` |
| Send on closed | `panic: send on closed channel` | Close only from sender side, use `sync.Once` |
| Index out of range | `runtime error: index out of range` | Validate bounds, use `len()` checks |
| Goroutine leak | Goroutine count grows over time | Use context cancellation, buffered channels |
| Deadlock | `fatal error: all goroutines are asleep` | Fix lock ordering, add select with timeout |
| Interface nil | Nil check passes but method panics | Return explicit untyped `nil` |
| Slice alias | Mutation affects unexpected slice | Use full slice expression `[:n:n]` |
| Context ignored | Operation not cancelled on shutdown | Derive from parent ctx, check `ctx.Done()` |
| Race condition | `-race` flag reports data race | Add mutex, channel, or atomic |
