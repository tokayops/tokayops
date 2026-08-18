# Changelog

Everything worth knowing before you upgrade. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow the
[release policy](README.md#releases).

Each release converts to the Apache License 2.0 two years after it ships, per
[FSL-1.1-Apache-2.0](LICENSE.md). The conversion date is stated with each entry.

## [Unreleased]

## [0.1.0] - 2026-08-18

First tagged release. Converts to Apache-2.0 on 2028-08-18.

Everything below already shipped in the `:develop` images; this entry records
what the first supported version contains rather than what changed in it.

### Added

- Alertmanager webhook ingestion. Alerts are deduplicated by `groupKey` (alert
  fingerprint as fallback), repeat deliveries update the live Alert Group in
  place, and the group resolves itself once every alert has cleared.
- Team-based routing with severity-selected escalation policies.
- On-call schedules: multi-user rotation groups, L2 backup, overrides, calendar
  view, and shift-start DMs to every incoming member with a linked account.
- Parallel notification fan-out within an escalation step, with per-member
  failure isolation, and stage-based job execution under DB leases so a crashed
  worker's steps are reclaimed instead of lost.
- Slack: Block Kit messages, signature-verified Ack/Resolve buttons that can be
  switched off per integration, and dual-send to a team channel plus firehose.
- Telegram: outbound delivery, policy routing, and secret-verified Ack/Resolve
  buttons that can be switched off per integration.
- Outgoing webhooks through a transactional outbox: HMAC-signed, SSRF-guarded,
  retried with backoff, with a delivery log and replay.
- Web UI for alert groups, teams, schedules, policies and integrations.
- Accounts and access: local accounts, optional OIDC/SSO with auto-registration,
  API tokens for automation, RBAC with global and team roles, AES-256-GCM
  encryption of integration secrets, and CSRF plus secure cookies in production.
- Prometheus metrics including MTTA/MTTR histograms and on-call health gauges,
  and a `/health` endpoint on the internal port.
- Multi-architecture images (`linux/amd64`, `linux/arm64`) on GHCR, and a
  `docker-compose.prod.yml` that brings up Postgres alongside the app.
- CLI commands for creating users and teams, and a `seed` command for local
  demo data.
- `/api/version` and the UI footer now report the release version alongside the
  commit and build date.

### Upgrade notes

- Moving an existing `:develop` deployment to `0.1.0` needs no manual step: the
  schema is created and extended on startup. Take a database backup first.
  Downgrading to an older image is not supported.
- Pin `TOKAY_TAG` to an exact version (`0.1.0`) if you do not want `:latest` to
  move under you on the next release, or pin the image digest listed with this
  release for a reference that cannot move at all.

[Unreleased]: https://github.com/tokayops/tokayops/compare/v0.1.0...develop
[0.1.0]: https://github.com/tokayops/tokayops/releases/tag/v0.1.0
