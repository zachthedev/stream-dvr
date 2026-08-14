---
name: toml-programmer
description: Write well-organized TOML configuration files with clear section headers and inline comments.
---

# TOML Programmer Skill

Write clear, well-organized TOML configuration files with consistent section headers and inline documentation.

## Instructions

1. **Understand the configuration needs:**
   Identify what settings are being configured (database, logging, features, etc.). Determine which values are required vs optional, and what validation constraints apply.

2. **Organize with section headers:**
   Use comment headers to group related settings into logical sections. This improves readability and makes large configuration files navigable.

3. **Document inline:**
   Add comments above or beside values to explain purpose, valid options, and defaults. This serves as documentation for anyone editing the file.

4. **Use appropriate TOML features:**
   - Tables for nested configuration (`[section]`)
   - Arrays of tables for repeated structures (`[[items]]`)
   - Inline tables for compact nested data
   - Multiline strings for long values

5. **Follow consistent formatting:**
   - Section headers use `##` or `###` comment markers
   - Subsection dividers use `#####`
   - Inline comments explain non-obvious values
   - Blank lines separate logical groups

## Style Guide

### Section Header Conventions

TOML doesn't have doc comments, so use regular comments for organization and documentation.

```toml
##################################################
### Main Section Title
##################################################

[section]

##### Subsection Title #####

# Description of this setting
# Can span multiple lines for complex explanations
setting = "value"

# Another setting (default: true)
enabled = true
```

### Complete Example

```toml
##################################################
### Application Configuration
##################################################

[app]

# Application name shown in logs and UI
name = "my-service"

# Environment: development, staging, production
environment = "development"

# Debug mode enables verbose logging
debug = false

##################################################
### Server Configuration
##################################################

[server]

##### Binding #####

# Host address to bind to
# Use 0.0.0.0 for all interfaces, 127.0.0.1 for localhost only
host = "0.0.0.0"

# Port number (1-65535)
port = 8080

##### TLS #####

# Enable HTTPS
tls_enabled = false

# Path to TLS certificate (required if tls_enabled = true)
tls_cert = ""

# Path to TLS private key (required if tls_enabled = true)
tls_key = ""

##### Timeouts #####

# Read timeout in seconds
read_timeout_seconds = 30

# Write timeout in seconds
write_timeout_seconds = 30

# Idle connection timeout in seconds
idle_timeout_seconds = 60

##################################################
### Database Configuration
##################################################

[database]

##### Connection #####

# Database connection URL
# Format: postgresql://user:password@host:port/database
url = "postgresql://localhost:5432/myapp"

# Maximum number of connections in the pool
max_connections = 10

# Minimum number of idle connections to maintain
min_connections = 2

##### Timeouts #####

# Connection acquisition timeout in seconds
connect_timeout_seconds = 5

# Query execution timeout in seconds
query_timeout_seconds = 30

##### SSL #####

# SSL mode: disable, allow, prefer, require, verify-ca, verify-full
ssl_mode = "prefer"

##################################################
### Logging Configuration
##################################################

[logging]

##### Output #####

# Log level: trace, debug, info, warn, error
level = "info"

# Log format: json, pretty, compact
format = "json"

# Output destination: stdout, stderr, file
output = "stdout"

##### File Output #####

# Log file path (when output = "file")
file_path = "/var/log/myapp/app.log"

# Maximum log file size in MB before rotation
max_size_mb = 100

# Number of rotated files to keep
max_backups = 5

##### Filtering #####

# Per-module log levels override the global level
[logging.modules]
"myapp::db" = "debug"
"myapp::http" = "info"
"hyper" = "warn"

##################################################
### Cache Configuration
##################################################

[cache]

# Cache backend: memory, redis, none
backend = "memory"

##### Memory Cache #####

# Maximum items in memory cache
max_items = 10000

# Default TTL in seconds (0 = no expiration)
default_ttl_seconds = 300

##### Redis Cache #####

# Redis connection URL (when backend = "redis")
redis_url = "redis://localhost:6379"

# Redis key prefix
redis_prefix = "myapp:"

##################################################
### Feature Flags
##################################################

[features]

# Enable new user registration
registration_enabled = true

# Enable OAuth login providers
oauth_enabled = false

# Enable API rate limiting
rate_limiting = true

# Enable request/response logging
request_logging = true

# Experimental features (may change without notice)
[features.experimental]
new_search = false
ai_suggestions = false

##################################################
### External Services
##################################################

[services]

##### Email #####

[services.email]
# SMTP server host
host = "smtp.example.com"
port = 587
username = ""
password = ""
from_address = "noreply@example.com"

##### Storage #####

[services.storage]
# Storage backend: local, s3, gcs
backend = "local"
# Local storage path
local_path = "/var/data/uploads"
# S3 bucket name (when backend = "s3")
s3_bucket = ""
s3_region = "us-east-1"
```

## Common Configuration Patterns

### Environment-Specific Overrides

```toml
##################################################
### Base Configuration
##################################################

[database]
max_connections = 10

##################################################
### Environment Overrides
##################################################

# Load additional config based on APP_ENV:
# - config.development.toml
# - config.staging.toml
# - config.production.toml

# config.production.toml
[database]
max_connections = 50
ssl_mode = "require"
```

### Secrets Placeholder Pattern

```toml
##################################################
### Secrets (loaded from environment)
##################################################

[secrets]
# These are placeholders - actual values come from:
# - Environment variables: DATABASE_PASSWORD, API_KEY
# - Secret manager: vault, aws-secrets-manager
# - Encrypted .env file

# Database password (env: DATABASE_PASSWORD)
database_password = "${DATABASE_PASSWORD}"

# External API key (env: EXTERNAL_API_KEY)
api_key = "${EXTERNAL_API_KEY}"
```

### Array of Tables Pattern

```toml
##################################################
### Multiple Instances
##################################################

# Define multiple database connections
[[databases]]
name = "primary"
url = "postgresql://primary:5432/app"
read_only = false

[[databases]]
name = "replica"
url = "postgresql://replica:5432/app"
read_only = true

# Define multiple cache tiers
[[cache_tiers]]
name = "l1"
backend = "memory"
max_items = 1000

[[cache_tiers]]
name = "l2"
backend = "redis"
redis_url = "redis://localhost:6379"
```

### Conditional Features

```toml
##################################################
### Feature Configuration
##################################################

[features]

##### Core Features #####

# These are always enabled in production
auth = true
logging = true
metrics = true

##### Optional Features #####

# Enable based on license/plan
[features.premium]
enabled = false
advanced_analytics = false
custom_branding = false

##### Beta Features #####

# Opt-in beta features (may be unstable)
[features.beta]
# New recommendation engine
recommendations_v2 = false
# Experimental search indexer
search_v3 = false
```

## When to Use This Skill

Invoke this skill when:
- Creating new configuration files
- Organizing existing TOML configs
- Adding documentation to config files
- Setting up environment-specific configs

## Best Practices

1. **Group related settings** - Don't scatter related options across the file
2. **Document non-obvious values** - Explain what values mean, not just what they are
3. **Show valid options** - List acceptable values for enum-like settings
4. **Note defaults** - Indicate what happens if a value is omitted
5. **Keep secrets separate** - Use environment variables or secret managers
6. **Version your config schema** - Include a version number if format may change

## Quick Reference

| TOML Feature | Use Case |
|-------------|----------|
| `[section]` | Group related settings |
| `[[array]]` | Multiple instances of same structure |
| `key = { inline = "table" }` | Compact nested data |
| `"""multiline"""` | Long strings, SQL queries |
| `# comment` | Documentation and headers |
