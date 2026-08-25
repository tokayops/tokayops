# Changelog

Everything worth knowing before you upgrade. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow the
[release policy](README.md#releases).

Each release converts to the Apache License 2.0 two years after it ships, per
[FSL-1.1-Apache-2.0](LICENSE.md). The conversion date is stated with each entry.

## [Unreleased]

### Upgrade notes

- **Stop every running instance before starting this version.** The schema
  change it applies at startup is not one an older instance can write against:
  the previous version keeps starting, but every background job it tries to
  create is rejected, which means alerts stop being escalated for as long as it
  is left running. Take a database backup first, as always, and do not start an
  older image against the upgraded database afterwards - downgrading is not
  supported.
- **This version cannot upgrade a database that never went through the schedule
  cutover.** The one-shot `tokayops migrate reset-schedules` command, and the
  startup check that refused to serve until it had run, are both gone: every
  supported installation completed that step releases ago. An older database
  must be brought up on a release that still carries the command before it is
  upgraded to this one.
- The column that carries an alert's own key is renamed at startup
  (`alert_groups.dedup_key` becomes `alert_key`); the rename is instant, touches
  no data, and the name every API response, webhook and page uses is unchanged.
  An older instance started against the upgraded database now fails on any read
  of an alert group rather than quietly failing to create jobs - louder, and
  still a reason to stop every instance first.
- While a mixture of versions is running, the "you are now on-call" message is
  the part that suffers first: the two versions recognise a handover
  differently, so one shift change can be announced twice and another not at
  all. This is the same instruction as above - stop every instance - said for
  the case where escalations are not what you notice.
- **Escalations in flight do not survive this upgrade.** How an alert is
  escalated changed completely: it is no longer a background job with steps, and
  the steps of the old kind are not executed by this version. An escalation that
  is mid-flight when instances are stopped ends there - its remaining steps fail
  rather than being sent - and alerts that arrive afterwards escalate normally.
  This is the same instruction as the first note, said for what it costs: pick a
  quiet moment.

  It does NOT block the upgrade, and the note below is about something else. An
  escalation still running is recognised, so the startup check lets it through;
  it is its first remaining step that fails, once the new version is up.
- The upgrade refuses to run while a job it cannot classify is still executing,
  and names the job in the message. That job either finishes or is cancelled,
  and the upgrade is started again. The alternative would be to let it run on
  without its claim on the work, which for an escalation means a second round of
  pages for one incident.

### Changed

- **An escalation step goes out when the policy said it would.** A step's delay
  is now counted from the moment the alert was picked up, not from the moment
  the previous step finished. A policy that says "the channel now, the on-call
  engineer in five minutes" pages the engineer five minutes after the alert
  arrives, even if the first message is still being retried. Before, a step that
  was slow to send pushed everything behind it back by however long it took.
- **A notification that keeps failing keeps being retried.** There is no attempt
  limit any more: a delivery that fails in a way worth retrying is tried again,
  with a growing wait between tries, until it succeeds, until the alert is
  acknowledged or resolved, or until it reaches a deadline. Previously the third
  failure ended it silently - the page simply never arrived, and nothing said
  so. `max_attempts` on an escalation step no longer ends a page; a provider
  refusing for good still does, immediately.
- **A direct message about an alert links to the alert in TokayOps.** It used to
  link to the message in the channel, which meant the link was missing whenever
  there was no channel message to point at. The `dm_fallback_to_firehose`
  setting is gone with the path it configured; remove it from `tokay.yaml` if it
  is there, where it is now ignored.
- **The alerts inside a message are listed by when they started**, rather than
  in whatever order they arrived from Alertmanager. Two instances rendering the
  same alert now produce the same message.
- **Erasing a user also removes the addresses their notifications were sent to**,
  and withdraws anything still owed to them. What was already delivered is kept
  as a record that it happened, without the coordinates of the message; nothing
  written afterwards can put an address back.

### Fixed

- Two unrelated pieces of background work no longer cancel each other out by
  accident. Deduplication used to compare one opaque key across every kind of
  job, so a collision between, say, an escalation and a message update silently
  dropped one of them. Each kind of work now declares what identifies it, and
  identical keys in different kinds are simply different work.
- The escalation of a repeat incident is no longer at risk of being taken for
  the previous one. An escalation is now identified by its alert group rather
  than by the alert fingerprint, which several groups share over time as the
  same alert fires, resolves and fires again.
- Coming on call is announced once, however many instances of TokayOps are
  running. Whether the second instance was told depended on how quickly the
  first one's notification finished: a shift change noticed a moment later was a
  second direct message to the same people. A handover notification is now
  identified by the shift change it announces, and that identity is kept for
  good rather than for as long as the notification takes to send.
- An alert that arrives while the group's message is being updated is no longer
  left out of it. Three things could lose one: the mark that said "this message
  is out of date" was cleared by an update that had been built before the alert
  arrived; it was written separately from the alert itself, so an interruption
  in between kept the alert and dropped the mark; and a notification that
  appeared while the update was being prepared was taken as proof that no update
  was needed. An alert and the mark are now recorded together, and the mark is
  cleared only once an update job has accepted that change.
- An alert no longer loses its page when the on-call state cannot be read. A
  policy step aimed at a schedule used to escalate to nobody if the read failed
  at that moment, and nothing retried it, so the person on duty was never told.
  The escalation is now held back and rebuilt on the next tick instead. A
  schedule whose stored data cannot be read at all still escalates without that
  recipient, since retrying cannot repair it, and the rest of the policy runs as
  before.
- A Slack call that never answers no longer hangs forever. Requests now give up
  after 30 seconds, so a stalled connection stops holding the notification
  worker that made it, and on-call usergroup syncing keeps running instead of
  stopping until the next restart.
- A mistyped or unrecognised command no longer starts the server. `tokayops
  migrat`, or a command line that repeats the binary's own path because the
  image already runs it, used to fall through and bring up a second full
  instance - notifier, syncer and workers - beside the running one. The binary
  now refuses, lists the commands it knows, and does so before it opens the
  database.

### Added

- `engine_escalation_build_deferrals_total` counts escalations held back because
  the on-call recipients could not be resolved. Its increase over a window
  should normally be zero; alert on a positive increase rather than on the
  value, which never returns to zero once it has moved.
- Metrics for outbound delivery. `outbound_queue_lateness_seconds` is the one to
  watch: it is how far behind the oldest notification that should ALREADY have
  gone out is, and work scheduled for later - a delayed policy step, a retry
  waiting out its backoff - is deliberately not counted as late. Beside it,
  `outbound_intents_terminal_total` counts notifications that ended, by how:
  anything other than `succeeded` ended without anybody being able to say it
  worked. That is not the same as never sent - a call whose answer never arrived
  can be withdrawn or time out, and the message may well have gone; the alert's
  own history is where that is settled.
  `outbound_attempts_total`, `outbound_admissions_total` and
  `outbound_admission_latency_seconds` cover the rest, and
  `outbound_contract_violations_total` should stay at zero - it counts things
  that indicate a bug rather than a failed delivery.
- `storage_contract_failures_total` counts durable rows that no longer parse.
  Any increase is a data problem to look at, not a transient failure.

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
