# TOML Configuration Patterns Reference

Common configuration patterns and templates for various use cases.

## Web Application Configuration

```toml
##################################################
### Web Application Configuration
##################################################

[app]
name = "web-app"
version = "1.0.0"
environment = "development"

##################################################
### HTTP Server
##################################################

[server]

##### Binding #####

host = "0.0.0.0"
port = 3000

##### Performance #####

# Worker threads (0 = auto-detect CPU count)
workers = 0

# Maximum concurrent connections
max_connections = 10000

# Keep-alive timeout in seconds
keep_alive_seconds = 75

##### Security #####

# Trusted proxy headers (for reverse proxy setups)
trust_proxy = true

# Allowed origins for CORS (empty = same-origin only)
cors_origins = ["https://app.example.com"]

##################################################
### Session Management
##################################################

[session]

# Session storage: memory, redis, database
storage = "redis"

# Session cookie name
cookie_name = "session_id"

# Session TTL in seconds (default: 24 hours)
ttl_seconds = 86400

# Secure cookie flag (set true in production)
secure = false

# SameSite cookie attribute: strict, lax, none
same_site = "lax"

##################################################
### Authentication
##################################################

[auth]

##### JWT #####

[auth.jwt]
# Secret key for signing (load from environment in production)
secret = "${JWT_SECRET}"

# Token expiration in seconds
access_token_ttl = 900
refresh_token_ttl = 604800

# Issuer claim
issuer = "web-app"

##### OAuth Providers #####

[auth.oauth.google]
enabled = false
client_id = "${GOOGLE_CLIENT_ID}"
client_secret = "${GOOGLE_CLIENT_SECRET}"

[auth.oauth.github]
enabled = false
client_id = "${GITHUB_CLIENT_ID}"
client_secret = "${GITHUB_CLIENT_SECRET}"
```

## API Service Configuration

```toml
##################################################
### API Service Configuration
##################################################

[api]
name = "api-service"
version = "2.0.0"
base_path = "/api/v2"

##################################################
### Rate Limiting
##################################################

[rate_limit]

# Enable rate limiting
enabled = true

# Default requests per window
requests_per_window = 100

# Window duration in seconds
window_seconds = 60

# Rate limit by: ip, user, api_key
limit_by = "ip"

##### Tier Overrides #####

[rate_limit.tiers]
free = 100
basic = 500
premium = 2000
enterprise = 10000

##################################################
### Request Validation
##################################################

[validation]

# Maximum request body size
max_body_size = "10MB"

# Maximum URL length
max_url_length = 2048

# Maximum header size
max_header_size = "8KB"

# Allowed content types
allowed_content_types = [
    "application/json",
    "application/xml",
    "multipart/form-data",
]

##################################################
### Response Configuration
##################################################

[response]

# Enable response compression
compression = true

# Compression level (1-9, higher = smaller but slower)
compression_level = 6

# Default pagination limit
default_page_size = 20

# Maximum pagination limit
max_page_size = 100

##################################################
### API Documentation
##################################################

[docs]

# Enable Swagger/OpenAPI docs
enabled = true

# Docs endpoint path
path = "/docs"

# API title in docs
title = "My API"

# Contact email
contact_email = "api-support@example.com"
```

## Microservice Configuration

```toml
##################################################
### Microservice Configuration
##################################################

[service]
name = "user-service"
version = "1.5.2"
instance_id = "${HOSTNAME}"

##################################################
### Service Discovery
##################################################

[discovery]

# Discovery backend: consul, etcd, kubernetes
backend = "kubernetes"

##### Consul #####

[discovery.consul]
host = "consul.service.local"
port = 8500
datacenter = "dc1"

##### Health Check #####

[discovery.health]
# Health check endpoint
path = "/health"

# Check interval in seconds
interval_seconds = 10

# Timeout for health check
timeout_seconds = 5

# Deregister after consecutive failures
deregister_after_failures = 3

##################################################
### gRPC Configuration
##################################################

[grpc]

# gRPC server port
port = 50051

# Enable reflection (for grpcurl/grpcui)
reflection = true

# Maximum message size in bytes
max_message_size = 4194304

# Keep-alive interval in seconds
keepalive_interval = 60

##################################################
### Tracing
##################################################

[tracing]

# Tracing backend: jaeger, zipkin, otlp
backend = "otlp"

# Sampling rate (0.0 - 1.0)
sample_rate = 0.1

##### OTLP Exporter #####

[tracing.otlp]
endpoint = "http://otel-collector:4317"
insecure = true

##################################################
### Metrics
##################################################

[metrics]

# Enable Prometheus metrics
enabled = true

# Metrics endpoint path
path = "/metrics"

# Include runtime metrics
runtime_metrics = true

# Custom metric labels
[metrics.labels]
service = "user-service"
environment = "${APP_ENV}"
```

## Database Configuration Patterns

```toml
##################################################
### PostgreSQL Configuration
##################################################

[database.postgres]

##### Connection #####

host = "localhost"
port = 5432
database = "myapp"
username = "app_user"
password = "${DB_PASSWORD}"

##### Pool #####

# Minimum idle connections
min_connections = 5

# Maximum connections
max_connections = 20

# Connection lifetime in seconds (0 = forever)
max_lifetime_seconds = 1800

# Idle connection timeout in seconds
idle_timeout_seconds = 600

##### SSL #####

ssl_mode = "prefer"
ssl_root_cert = ""

##### Performance #####

# Statement timeout in milliseconds
statement_timeout_ms = 30000

# Prepared statement cache size
statement_cache_size = 100

##################################################
### Redis Configuration
##################################################

[database.redis]

##### Connection #####

# Single instance
url = "redis://localhost:6379"

# Or cluster mode
# cluster_urls = [
#     "redis://node1:6379",
#     "redis://node2:6379",
#     "redis://node3:6379",
# ]

##### Pool #####

max_connections = 50
min_idle = 10

##### Timeouts #####

connect_timeout_ms = 5000
read_timeout_ms = 3000
write_timeout_ms = 3000

##################################################
### MongoDB Configuration
##################################################

[database.mongodb]

##### Connection #####

uri = "mongodb://localhost:27017"
database = "myapp"

##### Options #####

# Connection pool size
max_pool_size = 100
min_pool_size = 10

# Server selection timeout in milliseconds
server_selection_timeout_ms = 30000

# Connect timeout in milliseconds
connect_timeout_ms = 10000

##### Read/Write Concerns #####

read_preference = "primaryPreferred"
write_concern = "majority"
```

## Logging Configuration Patterns

```toml
##################################################
### Structured Logging
##################################################

[logging]

##### Global Settings #####

# Minimum log level: trace, debug, info, warn, error
level = "info"

# Output format: json, logfmt, pretty
format = "json"

# Include timestamps
timestamps = true

# Timestamp format (RFC3339, Unix, UnixMilli)
timestamp_format = "RFC3339"

##### Output Targets #####

[[logging.outputs]]
type = "stdout"
level = "info"

[[logging.outputs]]
type = "file"
level = "debug"
path = "/var/log/app/debug.log"
max_size_mb = 100
max_backups = 5
max_age_days = 30
compress = true

[[logging.outputs]]
type = "loki"
level = "info"
url = "http://loki:3100/loki/api/v1/push"
labels = { app = "my-service", env = "${APP_ENV}" }

##### Module-Level Overrides #####

[logging.modules]
"myapp" = "debug"
"myapp::db" = "info"
"hyper" = "warn"
"tower" = "warn"
"sqlx" = "warn"

##### Sensitive Data #####

[logging.redact]
# Field names to redact in logs
fields = ["password", "token", "secret", "authorization"]
# Replacement text
replacement = "[REDACTED]"
```

## Queue/Message Configuration

```toml
##################################################
### Message Queue Configuration
##################################################

[queue]

# Queue backend: rabbitmq, kafka, sqs
backend = "rabbitmq"

##################################################
### RabbitMQ
##################################################

[queue.rabbitmq]

##### Connection #####

url = "amqp://guest:guest@localhost:5672"

# Virtual host
vhost = "/"

##### Channels #####

# Channel pool size
channel_pool_size = 10

##### Consumer #####

# Prefetch count
prefetch_count = 10

# Auto-ack messages
auto_ack = false

##################################################
### Kafka
##################################################

[queue.kafka]

##### Brokers #####

brokers = ["localhost:9092"]

##### Producer #####

[queue.kafka.producer]
acks = "all"
retries = 3
batch_size = 16384
linger_ms = 5

##### Consumer #####

[queue.kafka.consumer]
group_id = "my-consumer-group"
auto_offset_reset = "earliest"
enable_auto_commit = false
session_timeout_ms = 30000

##################################################
### AWS SQS
##################################################

[queue.sqs]

##### Connection #####

region = "us-east-1"
endpoint = ""  # Leave empty for AWS, set for LocalStack

##### Queue Settings #####

queue_url = "https://sqs.us-east-1.amazonaws.com/123456789/my-queue"
visibility_timeout_seconds = 30
wait_time_seconds = 20
max_messages = 10
```

## Scheduled Tasks Configuration

```toml
##################################################
### Scheduled Tasks (Cron Jobs)
##################################################

[scheduler]

# Enable scheduler
enabled = true

# Timezone for cron expressions
timezone = "UTC"

##### Tasks #####

[[scheduler.tasks]]
name = "cleanup_expired_sessions"
cron = "0 */15 * * * *"  # Every 15 minutes
timeout_seconds = 300
enabled = true

[[scheduler.tasks]]
name = "send_daily_report"
cron = "0 0 9 * * MON-FRI"  # 9 AM on weekdays
timeout_seconds = 600
enabled = true

[[scheduler.tasks]]
name = "backup_database"
cron = "0 0 2 * * *"  # 2 AM daily
timeout_seconds = 3600
enabled = true
# Only run on leader node in cluster
leader_only = true

[[scheduler.tasks]]
name = "sync_external_data"
cron = "0 0 */6 * * *"  # Every 6 hours
timeout_seconds = 1800
enabled = true
retry_count = 3
retry_delay_seconds = 60
```

## Testing Configuration

```toml
##################################################
### Test Configuration
##################################################

[test]

# Test database (separate from production)
[test.database]
url = "postgresql://test:test@localhost:5432/test_db"
reset_before_each = true

# Test cache
[test.cache]
backend = "memory"

# Test email (capture instead of send)
[test.email]
backend = "memory"
capture = true

# Mock external services
[test.mocks]
enabled = true
responses_path = "./test/fixtures/mocks"

# Coverage settings
[test.coverage]
enabled = true
threshold = 80.0
exclude = [
    "src/main.rs",
    "src/bin/*",
    "tests/*",
]
```
