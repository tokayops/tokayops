# TokayOps

TokayOps is a lightweight, source-available incident management layer between
Alertmanager and your on-call engineer, designed for speed and simplicity. It
groups noisy alerts into a single incident, routes them to the right team by
severity and on-call schedule, escalates until someone answers, and lets them
Acknowledge / Resolve straight from Slack or Telegram. It ships a clean,
glassmorphism-inspired web UI. One container, your Postgres.

## Architecture

TokayOps is an asynchronous pipeline: ingest (Alertmanager webhook) -> store
(PostgreSQL) -> policy engine -> dispatcher (pluggable providers) -> Slack /
Telegram. The primary runtime entity is the **AlertGroup**: grouped,
deduplicated alerts with a status lifecycle, timeline and escalation state.

Delivery is built to be reliable and horizontally safe: a transactional outbox
commits events in the same transaction as the status change, and job steps run
under DB leases with compare-and-swap ownership, so a crashed worker's work is
re-claimed instead of lost.

## Features

- Automatic alert grouping and deduplication by `groupKey`.
- Team-based routing with severity-selected escalation policies.
- On-call schedules with multi-user groups (1+ users per rotation slot), L2
  backup, overrides and calendar.
- Parallel notification fan-out: every member of the on-call group is DM'd in
  parallel within one escalation step; failures are isolated per member.
- Stage-based job execution with per-step leases and stage-aware claim; failed
  workers are reclaimed automatically.
- Group handoff notifications: every incoming on-call member with a linked
  account gets a shift-start DM.
- Dual-send: team channel + firehose logging.
- Slack interactivity: Ack/Resolve buttons in Block Kit messages via a
  signature-verified endpoint.
- Telegram channel: outbound delivery, policy routing and interactive
  Ack/Resolve.
- Generic outgoing webhooks through a transactional outbox: HMAC-signed,
  SSRF-guarded, retried with backoff, with a delivery log and replay.
- Observability: Prometheus metrics endpoint including MTTA/MTTR histograms and
  on-call health gauges.
- OIDC/SSO: optional single sign-on with auto-registration.
- Enterprise-grade controls in a light package: RBAC (global + team roles),
  AES-256-GCM encryption of integration secrets, CSRF + secure cookies in
  production, and API tokens for automation.

## Getting Started

### Prerequisites
- **Go**: 1.22+
- **PostgreSQL**: 13+

### Configuration
TokayOps uses a YAML configuration file (`tokay.yaml`) and environment variables.

#### Environment Variables

**Database Configuration**:
| Variable | Description | Default |
|----------|-------------|---------|
| `DB_HOST` | PostgreSQL host | `localhost` |
| `DB_PORT` | PostgreSQL port | `5432` |
| `DB_USER` | Database user | `postgres` |
| `DB_PASSWORD` | Database password (special chars OK) | `postgres` |
| `DB_NAME` | Database name | `tokay` |
| `DB_SSLMODE` | SSL mode | `disable` |

**Application Configuration**:
| Variable | Description | Default |
|----------|-------------|---------|
| `JWT_SECRET` | Secret key for signing tokens | `dev-secret-key` (Required in PROD) |
| `APP_ENV` | Environment mode | `development` (Set to `production` for secure cookies + CSRF) |
| `CSRF_ENABLED` | Explicitly enable CSRF protection | `false` (auto-enabled in production) |
| `TOKAY_SELF_URL` | TokayOps base URL for clickable links in Slack (e.g. `https://tokayops.example.com`) | - |
| `ENCRYPTION_KEY` | 32-byte hex key for integrations config encryption | **Required** |

**OIDC Authentication** (optional, requires `TOKAY_SELF_URL`):
| Variable | Description |
|----------|-------------|
| `OIDC_ISSUER_URL` | OIDC Provider URL (e.g. `https://accounts.google.com`) |
| `OIDC_CLIENT_ID` | OIDC Client ID |
| `OIDC_CLIENT_SECRET` | OIDC Client Secret |
| `OIDC_ALLOWED_DOMAINS` | Comma-separated allowed email domains (e.g. `acme.com,example.org`) |

> ⚠️ **Important**: `TOKAY_SELF_URL` must be set when using OIDC. The redirect URL is automatically derived as `{TOKAY_SELF_URL}/api/auth/oidc/callback`.

> **Tip**: Generate a secure `JWT_SECRET` with: `openssl rand -base64 32`

### Firehose Configuration

Enable dual-send to L2 Support channels in `tokay.yaml`:
```yaml
global:
  firehose_critical_channel: "C_L2_CRITICAL_CHANNEL_ID"
  firehose_warning_channel: "C_L2_WARNING_CHANNEL_ID"
  dm_fallback_to_firehose: true # Default true. If no primary delivery, use firehose permalink in DMs
```

Firehose sends full messages with timeline, updates and resolve notifications.

**DM fallback:**
- `dm_fallback_to_firehose: true` (default) - if no primary delivery, DM links to firehose message.
- `dm_fallback_to_firehose: false` - DM omits the Slack link when primary is missing.

### Running Locally
1. Start the database using Docker Compose:
   ```bash
   docker-compose up -d
   ```
2. Run the application:
   ```bash
   export ENCRYPTION_KEY="$(openssl rand -hex 32)"
   # Configured to connect to localhost:5432 by default
   go run cmd/tokayops/main.go
   ```
   **Tip**: On first run, remember to seed the database:
   ```bash
   go run cmd/tokayops/main.go seed
   ```
3. Access UI at `http://localhost:8080`.

## Database Seeding
The application does not seed data automatically on startup. You can seed initial data (Teams and Demo Users) using the CLI:

```bash
go run cmd/tokayops/main.go seed
```

## User Management
TokayOps provides CLI commands to manage users directly, useful for initial admin creation.

**Create a new user:**
```bash
go run cmd/tokayops/main.go user create <email> <password> [Name]
```

Example:
```bash
go run cmd/tokayops/main.go user create admin@example.com superstrongpassword "Admin User"
```

## Team Management
Create teams via CLI for production setup:

**Create a new team:**
```bash
go run cmd/tokayops/main.go team create <id> <name> [slack_channel]
```

Example:
```bash
go run cmd/tokayops/main.go team create devops "DevOps Team" C01234567
```

## API Endpoints

### Alert Groups (Primary)
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/alert-groups` | List alert groups |
| GET | `/api/v1/alert-groups/:id` | Get alert group |
| PATCH | `/api/v1/alert-groups/:id/ack` | Acknowledge |
| PATCH | `/api/v1/alert-groups/:id/resolve` | Resolve |
| GET | `/api/v1/alert-groups/:id/timeline` | Timeline events |
| POST | `/api/v1/alert-groups/:id/notes` | Add note |

### Slack Interactive
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/slack/interactive` | Slack button callbacks (signature-verified, no AuthMiddleware) |

### API Tokens (Automation)
API tokens allow programmatic access without CSRF protection.

```bash
# Create token (requires existing session)
curl -X POST http://localhost:8080/api/v1/tokens \
  -H "Cookie: access_token=..." \
  -d '{"name": "Automation"}'
# Returns: {"token": "tok_abc123..."}

# Use token
curl -H "Authorization: Bearer tok_abc123..." \
  http://localhost:8080/api/v1/teams
```

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/tokens` | Create token |
| GET | `/api/v1/tokens` | List tokens |
| DELETE | `/api/v1/tokens/:id` | Revoke token |

### User Profile
Update your profile via the API:
```bash
curl -X PATCH http://localhost:8080/api/auth/me \
  -H "Cookie: access_token=..." \
  -d '{"slack_user_id": "U12345678"}'
```
Note: SSO users cannot change their name (synced from provider).

### Legacy Endpoints
`/api/v1/incidents/*` endpoints are aliased to `/api/v1/alert-groups/*` for backward compatibility.

## Deployment
TokayOps is packaged as a Docker container.

```bash
docker build -t tokayops .
docker run -d \
  -p 8080:8080 \
  -e DB_HOST="postgres.example.com" \
  -e DB_USER="tokay" \
  -e DB_PASSWORD="my%secret#password" \
  -e DB_NAME="tokay" \
  -e ENCRYPTION_KEY="$(openssl rand -hex 32)" \
  -e JWT_SECRET="$(openssl rand -base64 32)" \
  -e APP_ENV="production" \
  tokayops
```

**Note**: In production, ensure `APP_ENV=production` is set to enable HTTPOnly Secure cookies (requires HTTPS).

## Project Structure
- `cmd/tokayops`: Main application entry point.
- `internal/api`: REST API implementation (Echo).
- `internal/auth`: Authentication logic (JWT, BCrypt).
- `internal/config`: Configuration loading (YAML).
- `internal/dispatcher`: Notification dispatch (Slack, Telegram) behind a provider abstraction.
- `internal/engine`: Policy assignment engine.
- `internal/ingester`: Alertmanager webhook ingestion.
- `internal/model`: Data models (AlertGroup, User, etc.).
- `internal/store`: Database access layer (PostgreSQL).
- `web`: Frontend assets (Vanilla JS + CSS).

## License

TokayOps is **source-available** under the [Functional Source License 1.1 (Apache 2.0 Future License)](LICENSE.md) - `FSL-1.1-Apache-2.0`.

You are free to self-host, modify, and redistribute TokayOps for any purpose **except** offering it to third parties as a commercial product or service that competes with TokayOps. Each released version automatically converts to the **Apache License 2.0** two years after its release.

This is not an OSI "open source" license; it is *fair source*. For commercial/competing-use licensing, contact the maintainer.
