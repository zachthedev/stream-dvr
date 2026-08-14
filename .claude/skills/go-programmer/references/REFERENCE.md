# Go Design Patterns Reference

Complete implementations of common design patterns using modern Go features (1.22-1.26).

## Functional Options Pattern

```go
// ///////////////////////////////////////////////
// Functional Options
// ///////////////////////////////////////////////

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithAddr sets the server listen address.
func WithAddr(addr string) ServerOption {
    return func(s *Server) {
        s.addr = addr
    }
}

// WithReadTimeout sets the read timeout.
func WithReadTimeout(d time.Duration) ServerOption {
    return func(s *Server) {
        s.readTimeout = d
    }
}

// WithWriteTimeout sets the write timeout.
func WithWriteTimeout(d time.Duration) ServerOption {
    return func(s *Server) {
        s.writeTimeout = d
    }
}

// WithLogger sets the structured logger.
func WithLogger(logger *slog.Logger) ServerOption {
    return func(s *Server) {
        s.logger = logger
    }
}

// WithMiddleware appends middleware to the chain.
func WithMiddleware(mw ...Middleware) ServerOption {
    return func(s *Server) {
        s.middleware = append(s.middleware, mw...)
    }
}

// Server is an HTTP server with configurable options.
type Server struct {
    addr         string
    readTimeout  time.Duration
    writeTimeout time.Duration
    logger       *slog.Logger
    middleware   []Middleware
    mux          *http.ServeMux
}

// NewServer creates a Server with the given options applied over defaults.
func NewServer(opts ...ServerOption) *Server {
    s := &Server{
        addr:         ":8080",
        readTimeout:  15 * time.Second,
        writeTimeout: 15 * time.Second,
        logger:       slog.Default(),
        mux:          http.NewServeMux(),
    }
    for _, opt := range opts {
        opt(s)
    }
    return s
}

// Usage
srv := NewServer(
    WithAddr(":9090"),
    WithReadTimeout(30*time.Second),
    WithLogger(slog.New(slog.NewJSONHandler(os.Stdout, nil))),
    WithMiddleware(loggingMiddleware, recoveryMiddleware),
)
```

## Repository Pattern

```go
// ///////////////////////////////////////////////
// Repository Interface
// ///////////////////////////////////////////////

// UserRepository defines data access operations for users.
//
// Implementations must be safe for concurrent use.
type UserRepository interface {
    FindByID(ctx context.Context, id string) (User, error)
    FindByEmail(ctx context.Context, email string) (User, error)
    FindAll(ctx context.Context, filter UserFilter) ([]User, error)
    Create(ctx context.Context, user User) (User, error)
    Update(ctx context.Context, user User) (User, error)
    Delete(ctx context.Context, id string) error
}

// UserFilter constrains user queries.
type UserFilter struct {
    Role      string
    Active    *bool
    CreatedAt *TimeRange
    Limit     int
    Offset    int
}

// TimeRange represents an inclusive time window.
type TimeRange struct {
    From time.Time
    To   time.Time
}

// ///////////////////////////////////////////////
// PostgreSQL Implementation
// ///////////////////////////////////////////////

// PostgresUserRepository implements UserRepository using PostgreSQL.
type PostgresUserRepository struct {
    db *sql.DB
}

// NewPostgresUserRepository creates a new PostgresUserRepository.
func NewPostgresUserRepository(db *sql.DB) *PostgresUserRepository {
    return &PostgresUserRepository{db: db}
}

// FindByID retrieves a user by ID.
// Returns ErrNotFound if the user does not exist.
func (r *PostgresUserRepository) FindByID(ctx context.Context, id string) (User, error) {
    var user User
    err := r.db.QueryRowContext(ctx,
        `SELECT id, email, name, role, active, created_at, updated_at
         FROM users WHERE id = $1`, id,
    ).Scan(&user.ID, &user.Email, &user.Name, &user.Role,
        &user.Active, &user.CreatedAt, &user.UpdatedAt)

    if errors.Is(err, sql.ErrNoRows) {
        return User{}, fmt.Errorf("user %s: %w", id, ErrNotFound)
    }
    if err != nil {
        return User{}, fmt.Errorf("querying user %s: %w", id, err)
    }
    return user, nil
}

// FindByEmail retrieves a user by email address.
func (r *PostgresUserRepository) FindByEmail(ctx context.Context, email string) (User, error) {
    var user User
    err := r.db.QueryRowContext(ctx,
        `SELECT id, email, name, role, active, created_at, updated_at
         FROM users WHERE email = $1`, email,
    ).Scan(&user.ID, &user.Email, &user.Name, &user.Role,
        &user.Active, &user.CreatedAt, &user.UpdatedAt)

    if errors.Is(err, sql.ErrNoRows) {
        return User{}, fmt.Errorf("user with email %s: %w", email, ErrNotFound)
    }
    if err != nil {
        return User{}, fmt.Errorf("querying user by email: %w", err)
    }
    return user, nil
}

// Create inserts a new user.
func (r *PostgresUserRepository) Create(ctx context.Context, user User) (User, error) {
    user.ID = uuid.NewString()
    user.CreatedAt = time.Now()
    user.UpdatedAt = user.CreatedAt

    _, err := r.db.ExecContext(ctx,
        `INSERT INTO users (id, email, name, role, active, created_at, updated_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7)`,
        user.ID, user.Email, user.Name, user.Role,
        user.Active, user.CreatedAt, user.UpdatedAt,
    )
    if err != nil {
        return User{}, fmt.Errorf("inserting user: %w", err)
    }
    return user, nil
}

// Update modifies an existing user.
func (r *PostgresUserRepository) Update(ctx context.Context, user User) (User, error) {
    user.UpdatedAt = time.Now()
    result, err := r.db.ExecContext(ctx,
        `UPDATE users SET email=$1, name=$2, role=$3, active=$4, updated_at=$5
         WHERE id=$6`,
        user.Email, user.Name, user.Role, user.Active,
        user.UpdatedAt, user.ID,
    )
    if err != nil {
        return User{}, fmt.Errorf("updating user %s: %w", user.ID, err)
    }
    rows, _ := result.RowsAffected()
    if rows == 0 {
        return User{}, fmt.Errorf("user %s: %w", user.ID, ErrNotFound)
    }
    return user, nil
}

// Delete removes a user by ID.
func (r *PostgresUserRepository) Delete(ctx context.Context, id string) error {
    result, err := r.db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, id)
    if err != nil {
        return fmt.Errorf("deleting user %s: %w", id, err)
    }
    rows, _ := result.RowsAffected()
    if rows == 0 {
        return fmt.Errorf("user %s: %w", id, ErrNotFound)
    }
    return nil
}

// FindAll queries users with optional filtering.
func (r *PostgresUserRepository) FindAll(ctx context.Context, filter UserFilter) ([]User, error) {
    query := `SELECT id, email, name, role, active, created_at, updated_at FROM users WHERE 1=1`
    args := []any{}
    argN := 1

    if filter.Role != "" {
        query += fmt.Sprintf(" AND role = $%d", argN)
        args = append(args, filter.Role)
        argN++
    }
    if filter.Active != nil {
        query += fmt.Sprintf(" AND active = $%d", argN)
        args = append(args, *filter.Active)
        argN++
    }

    query += " ORDER BY created_at DESC"
    if filter.Limit > 0 {
        query += fmt.Sprintf(" LIMIT $%d", argN)
        args = append(args, filter.Limit)
        argN++
    }
    if filter.Offset > 0 {
        query += fmt.Sprintf(" OFFSET $%d", argN)
        args = append(args, filter.Offset)
    }

    rows, err := r.db.QueryContext(ctx, query, args...)
    if err != nil {
        return nil, fmt.Errorf("querying users: %w", err)
    }
    defer rows.Close()

    var users []User
    for rows.Next() {
        var u User
        if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Role,
            &u.Active, &u.CreatedAt, &u.UpdatedAt); err != nil {
            return nil, fmt.Errorf("scanning user row: %w", err)
        }
        users = append(users, u)
    }
    return users, rows.Err()
}
```

## State Machine Pattern (iota with Methods)

```go
// ///////////////////////////////////////////////
// Order State Machine
// ///////////////////////////////////////////////

// OrderStatus represents the lifecycle state of an order.
type OrderStatus int

const (
    OrderPending   OrderStatus = iota
    OrderConfirmed
    OrderShipped
    OrderDelivered
    OrderCancelled
)

// String returns the string representation of the status.
func (s OrderStatus) String() string {
    switch s {
    case OrderPending:
        return "pending"
    case OrderConfirmed:
        return "confirmed"
    case OrderShipped:
        return "shipped"
    case OrderDelivered:
        return "delivered"
    case OrderCancelled:
        return "cancelled"
    default:
        return fmt.Sprintf("unknown(%d)", s)
    }
}

// IsTerminal reports whether the status is a final state.
func (s OrderStatus) IsTerminal() bool {
    return s == OrderDelivered || s == OrderCancelled
}

// validTransitions defines allowed state changes.
var validTransitions = map[OrderStatus][]OrderStatus{
    OrderPending:   {OrderConfirmed, OrderCancelled},
    OrderConfirmed: {OrderShipped, OrderCancelled},
    OrderShipped:   {OrderDelivered},
}

// CanTransitionTo reports whether a transition from s to target is allowed.
func (s OrderStatus) CanTransitionTo(target OrderStatus) bool {
    for _, allowed := range validTransitions[s] {
        if allowed == target {
            return true
        }
    }
    return false
}

// Order represents a customer order with lifecycle state management.
type Order struct {
    ID        string
    Items     []LineItem
    Status    OrderStatus
    CreatedAt time.Time
    UpdatedAt time.Time
}

// Confirm transitions the order to confirmed status.
func (o *Order) Confirm() error {
    return o.transitionTo(OrderConfirmed)
}

// Ship transitions the order to shipped status.
func (o *Order) Ship() error {
    return o.transitionTo(OrderShipped)
}

// Cancel transitions the order to cancelled status.
func (o *Order) Cancel() error {
    return o.transitionTo(OrderCancelled)
}

func (o *Order) transitionTo(target OrderStatus) error {
    if !o.Status.CanTransitionTo(target) {
        return fmt.Errorf("cannot transition from %s to %s: %w",
            o.Status, target, ErrInvalidTransition)
    }
    o.Status = target
    o.UpdatedAt = time.Now()
    return nil
}

var ErrInvalidTransition = errors.New("invalid state transition")
```

## Strategy Pattern (Interface-Based)

```go
// ///////////////////////////////////////////////
// Strategy Pattern
// ///////////////////////////////////////////////

// PricingStrategy computes the total for a given base price and quantity.
type PricingStrategy interface {
    Calculate(basePrice float64, quantity int) float64
    Name() string
}

// StandardPricing applies no discount.
type StandardPricing struct{}

func (StandardPricing) Calculate(basePrice float64, quantity int) float64 {
    return basePrice * float64(quantity)
}

func (StandardPricing) Name() string { return "standard" }

// VolumePricing applies a discount above a quantity threshold.
type VolumePricing struct {
    Threshold       int
    DiscountPercent float64
}

func (v VolumePricing) Calculate(basePrice float64, quantity int) float64 {
    total := basePrice * float64(quantity)
    if quantity >= v.Threshold {
        return total * (1.0 - v.DiscountPercent)
    }
    return total
}

func (VolumePricing) Name() string { return "volume" }

// PromotionalPricing applies a time-limited discount.
type PromotionalPricing struct {
    DiscountPercent float64
    ValidUntil      time.Time
}

func (p PromotionalPricing) Calculate(basePrice float64, quantity int) float64 {
    total := basePrice * float64(quantity)
    if time.Now().Before(p.ValidUntil) {
        return total * (1.0 - p.DiscountPercent)
    }
    return total
}

func (PromotionalPricing) Name() string { return "promotional" }

// CalculateOrderTotal uses the given strategy to compute total.
func CalculateOrderTotal(items []Item, strategy PricingStrategy) float64 {
    var total float64
    for _, item := range items {
        total += strategy.Calculate(item.Price, item.Quantity)
    }
    return total
}
```

## Error Handling Hierarchies

```go
// ///////////////////////////////////////////////
// Error Definitions
// ///////////////////////////////////////////////

// Sentinel errors for expected conditions.
var (
    ErrNotFound      = errors.New("not found")
    ErrConflict      = errors.New("conflict")
    ErrForbidden     = errors.New("forbidden")
    ErrUnauthorized  = errors.New("unauthorized")
    ErrInternalError = errors.New("internal error")
)

// ValidationError reports a field-level validation failure.
type ValidationError struct {
    Field   string
    Message string
}

func (e *ValidationError) Error() string {
    return fmt.Sprintf("validation: field %q: %s", e.Field, e.Message)
}

// ValidationErrors aggregates multiple validation failures.
type ValidationErrors []ValidationError

func (ve ValidationErrors) Error() string {
    msgs := make([]string, len(ve))
    for i, e := range ve {
        msgs[i] = e.Error()
    }
    return strings.Join(msgs, "; ")
}

// ///////////////////////////////////////////////
// Error Wrapping and Inspection
// ///////////////////////////////////////////////

// Wrapping with context
func (s *UserService) Create(ctx context.Context, req CreateRequest) (User, error) {
    if errs := req.Validate(); len(errs) > 0 {
        return User{}, ValidationErrors(errs)
    }

    existing, err := s.repo.FindByEmail(ctx, req.Email)
    if err != nil && !errors.Is(err, ErrNotFound) {
        return User{}, fmt.Errorf("checking existing user: %w", err)
    }
    if existing.ID != "" {
        return User{}, fmt.Errorf("email %s: %w", req.Email, ErrConflict)
    }

    user, err := s.repo.Create(ctx, User{
        Email: req.Email,
        Name:  req.Name,
    })
    if err != nil {
        return User{}, fmt.Errorf("creating user: %w", err)
    }
    return user, nil
}

// Inspecting errors at the handler level
func handleError(w http.ResponseWriter, err error) {
    switch {
    case errors.Is(err, ErrNotFound):
        http.Error(w, "not found", http.StatusNotFound)

    case errors.Is(err, ErrConflict):
        http.Error(w, "conflict", http.StatusConflict)

    case errors.Is(err, ErrForbidden):
        http.Error(w, "forbidden", http.StatusForbidden)

    case errors.Is(err, ErrUnauthorized):
        http.Error(w, "unauthorized", http.StatusUnauthorized)

    default:
        // Go 1.26+ generic error checking
        if ve, ok := errors.AsType[ValidationErrors](err); ok {
            w.WriteHeader(http.StatusBadRequest)
            json.NewEncoder(w).Encode(map[string]any{"errors": ve})
            return
        }

        slog.Error("unhandled error", slog.Any("error", err))
        http.Error(w, "internal server error", http.StatusInternalServerError)
    }
}
```

## Middleware Pattern (HTTP)

```go
// ///////////////////////////////////////////////
// Middleware Chain
// ///////////////////////////////////////////////

// Middleware wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware in order: first applied wraps outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
    for i := len(mws) - 1; i >= 0; i-- {
        h = mws[i](h)
    }
    return h
}

// ///////////////////////////////////////////////
// Logging Middleware
// ///////////////////////////////////////////////

func LoggingMiddleware(logger *slog.Logger) Middleware {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            start := time.Now()
            wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

            next.ServeHTTP(wrapped, r)

            logger.Info("request",
                slog.String("method", r.Method),
                slog.String("path", r.URL.Path),
                slog.Int("status", wrapped.statusCode),
                slog.Duration("duration", time.Since(start)),
            )
        })
    }
}

type responseWriter struct {
    http.ResponseWriter
    statusCode int
}

func (w *responseWriter) WriteHeader(code int) {
    w.statusCode = code
    w.ResponseWriter.WriteHeader(code)
}

// ///////////////////////////////////////////////
// Recovery Middleware
// ///////////////////////////////////////////////

func RecoveryMiddleware(logger *slog.Logger) Middleware {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            defer func() {
                if rec := recover(); rec != nil {
                    logger.Error("panic recovered",
                        slog.Any("panic", rec),
                        slog.String("path", r.URL.Path),
                    )
                    http.Error(w, "internal server error", http.StatusInternalServerError)
                }
            }()
            next.ServeHTTP(wrapped, r)
        })
    }
}

// ///////////////////////////////////////////////
// Auth Middleware
// ///////////////////////////////////////////////

type contextKey string

const userContextKey contextKey = "user"

func AuthMiddleware(verifier TokenVerifier) Middleware {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            token := r.Header.Get("Authorization")
            if token == "" {
                http.Error(w, "unauthorized", http.StatusUnauthorized)
                return
            }

            user, err := verifier.Verify(r.Context(), token)
            if err != nil {
                http.Error(w, "unauthorized", http.StatusUnauthorized)
                return
            }

            ctx := context.WithValue(r.Context(), userContextKey, user)
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}

// UserFromContext extracts the authenticated user from the request context.
func UserFromContext(ctx context.Context) (User, bool) {
    user, ok := ctx.Value(userContextKey).(User)
    return user, ok
}

// Usage
mux := http.NewServeMux()
mux.HandleFunc("GET /api/users", listUsers)
mux.HandleFunc("POST /api/users", createUser)

handler := Chain(mux,
    LoggingMiddleware(logger),
    RecoveryMiddleware(logger),
    AuthMiddleware(tokenVerifier),
)

srv := &http.Server{
    Addr:    ":8080",
    Handler: handler,
}
```

## Worker Pool Pattern

```go
// ///////////////////////////////////////////////
// Generic Worker Pool
// ///////////////////////////////////////////////

// Result holds a value or error from a worker.
type Result[T any] struct {
    Value T
    Err   error
}

// Pool runs work across a fixed number of goroutines.
func Pool[T, R any](ctx context.Context, workers int, jobs []T, process func(context.Context, T) (R, error)) ([]R, error) {
    g, ctx := errgroup.WithContext(ctx)
    g.SetLimit(workers)

    results := make([]R, len(jobs))

    for i, job := range jobs {
        g.Go(func() error {
            r, err := process(ctx, job)
            if err != nil {
                return err
            }
            results[i] = r
            return nil
        })
    }

    if err := g.Wait(); err != nil {
        return nil, err
    }
    return results, nil
}

// ///////////////////////////////////////////////
// Channel-Based Worker Pool
// ///////////////////////////////////////////////

func ChannelPool[T, R any](ctx context.Context, workers int, jobs <-chan T, process func(context.Context, T) (R, error)) <-chan Result[R] {
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

// Usage
urls := []string{"https://a.com", "https://b.com", "https://c.com"}

results, err := Pool(ctx, 5, urls, func(ctx context.Context, url string) (Response, error) {
    req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
    if err != nil {
        return Response{}, fmt.Errorf("creating request for %s: %w", url, err)
    }
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return Response{}, fmt.Errorf("fetching %s: %w", url, err)
    }
    defer resp.Body.Close()
    // parse response...
    return parsed, nil
})
```

## Table-Driven Tests

```go
// ///////////////////////////////////////////////
// Table-Driven Test Pattern
// ///////////////////////////////////////////////

func TestParseAge(t *testing.T) {
    tests := []struct {
        name    string
        input   string
        want    int
        wantErr bool
    }{
        {name: "valid age", input: "25", want: 25},
        {name: "zero", input: "0", want: 0},
        {name: "max age", input: "150", want: 150},
        {name: "negative", input: "-1", wantErr: true},
        {name: "too old", input: "151", wantErr: true},
        {name: "not a number", input: "abc", wantErr: true},
        {name: "empty", input: "", wantErr: true},
        {name: "float", input: "25.5", wantErr: true},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got, err := ParseAge(tt.input)
            if tt.wantErr {
                if err == nil {
                    t.Errorf("ParseAge(%q) = %d, want error", tt.input, got)
                }
                return
            }
            if err != nil {
                t.Errorf("ParseAge(%q) error = %v", tt.input, err)
                return
            }
            if got != tt.want {
                t.Errorf("ParseAge(%q) = %d, want %d", tt.input, got, tt.want)
            }
        })
    }
}

// ///////////////////////////////////////////////
// Table-Driven Test with testify
// ///////////////////////////////////////////////

func TestUserService_Create(t *testing.T) {
    tests := []struct {
        name      string
        req       CreateRequest
        setupRepo func(*MockUserRepository)
        wantErr   error
    }{
        {
            name: "success",
            req:  CreateRequest{Email: "alice@example.com", Name: "Alice"},
            setupRepo: func(m *MockUserRepository) {
                m.On("FindByEmail", mock.Anything, "alice@example.com").
                    Return(User{}, ErrNotFound)
                m.On("Create", mock.Anything, mock.AnythingOfType("User")).
                    Return(User{ID: "1", Email: "alice@example.com", Name: "Alice"}, nil)
            },
        },
        {
            name: "duplicate email",
            req:  CreateRequest{Email: "alice@example.com", Name: "Alice"},
            setupRepo: func(m *MockUserRepository) {
                m.On("FindByEmail", mock.Anything, "alice@example.com").
                    Return(User{ID: "1"}, nil)
            },
            wantErr: ErrConflict,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            repo := new(MockUserRepository)
            tt.setupRepo(repo)

            svc := NewUserService(repo, slog.Default())
            _, err := svc.Create(context.Background(), tt.req)

            if tt.wantErr != nil {
                assert.ErrorIs(t, err, tt.wantErr)
            } else {
                assert.NoError(t, err)
            }
            repo.AssertExpectations(t)
        })
    }
}

// ///////////////////////////////////////////////
// Benchmark with testing.B.Loop (Go 1.24+)
// ///////////////////////////////////////////////

func BenchmarkProcess(b *testing.B) {
    input := prepareTestInput()
    b.ResetTimer()
    for b.Loop() {
        process(input)
    }
}

func BenchmarkConcurrentMap(b *testing.B) {
    m := &sync.Map{}
    for i := range 1000 {
        m.Store(i, i)
    }
    b.ResetTimer()
    for b.Loop() {
        m.Load(rand.IntN(1000))
    }
}
```

## Context-Based Cancellation and Timeout

```go
// ///////////////////////////////////////////////
// Context Patterns
// ///////////////////////////////////////////////

// Timeout for external calls
func (c *Client) FetchUser(ctx context.Context, id string) (User, error) {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    req, err := http.NewRequestWithContext(ctx, "GET",
        fmt.Sprintf("%s/users/%s", c.baseURL, id), nil)
    if err != nil {
        return User{}, fmt.Errorf("creating request: %w", err)
    }

    resp, err := c.http.Do(req)
    if err != nil {
        return User{}, fmt.Errorf("fetching user %s: %w", id, err)
    }
    defer resp.Body.Close()

    var user User
    if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
        return User{}, fmt.Errorf("decoding user %s: %w", id, err)
    }
    return user, nil
}

// Cancellation propagation in long-running work
func ProcessBatch(ctx context.Context, items []Item) error {
    for i, item := range items {
        select {
        case <-ctx.Done():
            return fmt.Errorf("cancelled after processing %d/%d items: %w",
                i, len(items), ctx.Err())
        default:
        }

        if err := processItem(ctx, item); err != nil {
            return fmt.Errorf("processing item %d: %w", i, err)
        }
    }
    return nil
}

// Graceful shutdown with context
func RunServer(ctx context.Context, addr string, handler http.Handler) error {
    srv := &http.Server{
        Addr:    addr,
        Handler: handler,
    }

    errCh := make(chan error, 1)
    go func() {
        if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            errCh <- err
        }
        close(errCh)
    }()

    select {
    case err := <-errCh:
        return fmt.Errorf("server error: %w", err)
    case <-ctx.Done():
    }

    shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    if err := srv.Shutdown(shutdownCtx); err != nil {
        return fmt.Errorf("shutdown: %w", err)
    }
    return nil
}
```
