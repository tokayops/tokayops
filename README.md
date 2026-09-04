# TokayOps

TokayOps is a lightweight, source-available incident management layer between
Alertmanager and your on-call engineer, designed for speed and simplicity.
Alertmanager decides what belongs together; TokayOps keeps that group as one
live escalation, routes it to the right team by severity and on-call schedule,
escalates until someone answers, and lets them Acknowledge / Resolve straight
from Slack or Telegram. It ships a clean, glassmorphism-inspired web UI. One
container, your Postgres.

## Architecture

TokayOps is an asynchronous pipeline: ingest (Alertmanager webhook) -> store
(PostgreSQL) -> policy engine -> outbound delivery (pluggable channels) ->
Slack / Telegram. The primary runtime entity is the **AlertGroup**: the alerts
Alertmanager grouped, deduplicated by `groupKey`, with a status lifecycle,
timeline and escalation state.

Delivery is built to be reliable and horizontally safe: a transactional outbox
commits events in the same transaction as the status change, and everything the
system owes somebody is a durable commitment written before the network is
touched. Workers claim commitments under DB leases with compare-and-swap
ownership and journal every attempt before making the call, so a crashed
worker's work is re-claimed rather than lost, and a call whose answer never
arrived is recorded as exactly that rather than guessed at.

## Features

- Deduplication by Alertmanager's `groupKey` (alert fingerprint as fallback):
  repeat deliveries merge into the live Alert Group and update its message in
  place instead of opening a new one, and the group resolves itself once every
  alert has cleared.
- Team-based routing with severity-selected escalation policies.
- On-call schedules with multi-user groups (1+ users per rotation slot), L2
  backup, overrides and calendar.
- Parallel notification fan-out: every member of the on-call group is DM'd in
  parallel within one escalation step; failures are isolated per member, and a
  delivery that keeps failing keeps being retried until it succeeds, is
  withdrawn, or reaches its deadline.
- Two delivery queues that cannot delay each other: paging and shift-change
  announcements are claimed separately, by separate workers with pools of their
  own, so a hundred schedules turning over at once never stands between an
  alert and the person on call.
- Group handoff notifications: every incoming on-call member with a linked
  account gets a shift-start DM.
- Dual-send: team channel + firehose logging.
- Slack interactivity: Ack/Resolve buttons in Block Kit messages via a
  signature-verified endpoint. Toggled per integration.
- Telegram channel: outbound delivery, policy routing and Ack/Resolve buttons
  via a secret-verified webhook. Toggled per integration.
- Generic outgoing webhooks as durable deliveries: HMAC-signed, SSRF-guarded,
  retried with backoff for up to a day, with a per-subscriber delivery log and
  an idempotent replay. The contract is under "Outgoing Webhooks" below.
- Observability: Prometheus metrics endpoint including MTTA/MTTR histograms and
  on-call health gauges.
- OIDC/SSO: optional single sign-on with auto-registration.
- Enterprise-grade controls in a light package: RBAC (global + team roles),
  AES-256-GCM encryption of integration secrets, CSRF + secure cookies in
  production, and API tokens for automation.

## Getting Started

### Prerequisites
- **Go**: 1.25+ (see `go.mod`; only needed to build from source)
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
| `TOKAY_DELIVERY_RETENTION_DAYS` | How many days the history of finished deliveries is kept; `0` keeps it for good | `30` |

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
```

Firehose sends full messages with timeline, updates and resolve notifications.

A direct message about an alert links to the alert in TokayOps rather than to
the firehose message, so the link works whether or not a firehose card exists.

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

### Outgoing Webhooks
A `generic_webhook` integration subscribes to alert group events
(`alert_group.firing`, `alert_group.acknowledged`, `alert_group.resolved`),
globally or for one team. Each event is committed together with the alert
group's own transition and delivered to every subscriber as its own delivery.

What a subscriber receives is one `POST` per event with a JSON body and these
headers:

| Header | Value |
|--------|-------|
| `Content-Type` | `application/json` |
| `X-Tokay-Event` | Event type, e.g. `alert_group.firing` |
| `X-Tokay-Event-ID` | The event's id; every retry and replay of the same event carries the same id, so receivers can deduplicate on it |
| `X-Tokay-Timestamp` | Unix timestamp of the request |
| `X-Tokay-Signature` | `sha256=<hex>` of `HMAC-SHA256(timestamp + "." + body, secret)`, present when the integration has a secret |

The subscriber's `custom_headers` are added to every request; the names above
and `Content-Type` are reserved and cannot be overridden.

How a delivery is treated depends on the answer: a `2xx` finishes it; `429`
and `408` are retried; any other `4xx`, and any `3xx` (redirects are not
followed), ends it as failed; a `5xx`, a timeout or a connection error is retried
with exponential backoff (2s doubling, capped at 30 minutes, with jitter) for up
to 24 hours from the event, after which the delivery expires. A subscriber's
`timeout_seconds` is honoured up to 30 seconds. Only a public address may be
posted to: every address the subscriber's name resolves to is checked before
any request is made and again when the connection is opened. For IPv6 the rule
is fail-closed - public means inside a block the IANA Global Unicast
Assignments registry has allocated to a regional registry, or inside an
assignment the IANA Special-Purpose registry marks globally reachable, so
reserved and unassigned space is refused without being named - and for IPv4 the whole space minus the IANA special-purpose
registry; private, loopback, link-local, unique-local, site-local,
shared-address-space, multicast, unspecified, SRv6, local-NAT64, reserved and
unallocated addresses are all refused unless the operator allows the range with
`TOKAY_WEBHOOK_ALLOW_PRIVATE_CIDRS`. Disabling or deleting the
integration withdraws its undelivered deliveries; the delivery history of a
deleted integration stays readable.

The history of finished deliveries - of every kind, not only webhooks - is kept
for `TOKAY_DELIVERY_RETENTION_DAYS` days (30 by default; `0` keeps it for good)
and then removed, an hour at a time, together with the alert events nothing
refers to any more. A delivery still in progress, or waiting for a person, is
kept whatever its age. Repeating a replay's `Idempotency-Key` after its result
has been removed answers `410 Gone`: the decision it named is over, and a new
replay needs a new key.

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/integrations/:id/deliveries` | Delivery log of a subscriber (paginated) |
| GET | `/api/v1/integrations/:id/deliveries/:deliveryId` | One delivery with its attempts |
| POST | `/api/v1/integrations/:id/deliveries/:deliveryId/replay` | Deliver the event again, as a new delivery |

A replay requires an `Idempotency-Key` header (1 to 128 bytes): repeating the
request with the same key returns the same new delivery, so a retried request
never delivers twice. Only a finished delivery (`sent` or `failed`) of an
enabled integration can be replayed (a delivery in progress, or a disabled
integration, answers `409`); the new delivery uses the integration's current
URL, secret and headers, and the response names it in `delivery_id`.

### Legacy Endpoints
`/api/v1/incidents/*` endpoints are aliased to `/api/v1/alert-groups/*` for backward compatibility.

## Deployment

Prebuilt images are published to GHCR for `linux/amd64` and `linux/arm64`:

| Tag | Points at |
| --- | --- |
| `latest` | the newest stable release |
| `0.1.0` | that release, and nothing else afterwards |
| `0.1` | the newest patch of the `0.1` series |
| `develop` | the current build of the develop branch, not a release |
| `sha-<commit>` | a CI build of that commit, for trying an unreleased fix |

Start from `:latest`, or set `TOKAY_TAG` in `.env` to pin a version. See
[Releases](#releases) for what the numbers promise, and
[CHANGELOG.md](CHANGELOG.md) for what each one changed.

Release policy is that a version tag is never re-pointed at a different build,
but registry tags are mutable by nature, and `sha-<commit>` is not covered by
that promise at all: re-running CI on a commit rebuilds and replaces it. For a
reference that provably cannot move, use the digest. Every GitHub Release lists
one per image, ready to paste into a compose file as
`image: ghcr.io/tokayops/tokayops@sha256:...`.

Deploy with the bundled compose file, which brings up Postgres alongside the app:

```bash
curl -fLO https://raw.githubusercontent.com/tokayops/tokayops/main/docker-compose.prod.yml
curl -fL -o .env https://raw.githubusercontent.com/tokayops/tokayops/main/.env.example
docker compose -f docker-compose.prod.yml up -d
```

Before that `up`, `.env` needs five values. The template ships with the first
three empty or set for development:

| Variable | Set it to |
| --- | --- |
| `ENCRYPTION_KEY` | `openssl rand -hex 32` (exactly 64 hex chars) |
| `JWT_SECRET` | `openssl rand -base64 32` |
| `DB_PASSWORD` | a strong password |
| `APP_ENV` | `production` (the template says `development`) |
| `TOKAY_SELF_URL` | your public HTTPS URL (commented out in the template) |

Create the first user - it becomes admin automatically:

```bash
docker compose -f docker-compose.prod.yml exec tokay \
  /app/tokayops user create admin@example.com '<password>' 'Admin User'
```

Two things that are easy to miss:

- `APP_ENV=production` enables HTTPOnly Secure cookies and CSRF, so the app must
  be served over HTTPS. The compose file binds both ports to loopback; put a
  reverse proxy in front.
- `TOKAY_SELF_URL` must be your public HTTPS URL. Without it the app still runs,
  but Slack/Telegram Ack/Resolve buttons are hidden and Telegram linking cannot
  complete.

**Full installation guide, including Slack, Telegram and Alertmanager wiring:
https://tokayops.com/install**

## Releases

TokayOps follows [Semantic Versioning](https://semver.org/). Releases are cut
from `main`, tagged `vX.Y.Z`, and written up in [CHANGELOG.md](CHANGELOG.md).
The running build reports itself at `/api/version` and in the UI footer.

While the version is `0.x`:

- A **minor** bump (`0.1.0` -> `0.2.0`) may change the public contract, and may
  need a manual step. The changelog says which.
- A **patch** bump (`0.1.0` -> `0.1.1`) is fixes only.

The public contract is the REST API under `/api`, the Alertmanager webhook it
accepts, the environment variables and `tokay.yaml`, the CLI commands, and the
image tags above. Not part of it: the Go packages under `internal/` (this is an
application, not a library), the frontend assets, and `make seed`.

### Support and upgrades

- Only the newest release is supported. Fixes, security ones included, ship as a
  patch on top of it.
- Upgrade forward one minor version at a time, reading the upgrade notes of each
  release you pass through.
- **Downgrades are not supported.** The schema is created and extended in place
  on startup, so going back means restoring a database backup taken before the
  upgrade. Take one.
- Anything on its way out is announced in the changelog at least one minor
  version before it is removed.

1.0 waits on a versioned migration system and on holding the API still; until
then, read the release notes before every minor upgrade.

## Project Structure
- `cmd/tokayops`: Main application entry point.
- `internal/api`: REST API implementation (Echo).
- `internal/auth`: Authentication logic (JWT, BCrypt).
- `internal/config`: Configuration loading (YAML).
- `internal/engine`: Policy assignment engine.
- `internal/handoff`: Detects shift changes and admits the announcement.
- `internal/ingester`: Alertmanager webhook ingestion.
- `internal/model`: Data models (AlertGroup, User, etc.).
- `internal/outbound`: Outbound delivery: durable commitments, the delivery workers, the fan-out of alert events to webhook subscribers, and the Slack, Telegram and outgoing webhook channels under `providers/`.
- `internal/store`: Database access layer (PostgreSQL).
- `web`: Frontend assets (Vanilla JS + CSS).

## License

TokayOps is **source-available** under the [Functional Source License 1.1 (Apache 2.0 Future License)](LICENSE.md) - `FSL-1.1-Apache-2.0`.

You are free to self-host, modify, and redistribute TokayOps for any purpose **except** offering it to third parties as a commercial product or service that competes with TokayOps. Each released version automatically converts to the **Apache License 2.0** two years after its release.

This is not an OSI "open source" license; it is *fair source*. For commercial/competing-use licensing, contact the maintainer.
