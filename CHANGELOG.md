# Changelog

Everything worth knowing before you upgrade. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow the
[release policy](README.md#releases).

Each release converts to the Apache License 2.0 two years after it ships, per
[FSL-1.1-Apache-2.0](LICENSE.md). The conversion date is stated with each entry.

## [Unreleased]

### Upgrade notes

- **Stop every running instance before starting this version.** An older
  instance left running against the upgraded database fails on any read of an
  alert group - the column carrying an alert's own key is renamed at startup,
  see below - so it stops escalating anything for as long as it is left up.
  Take a database backup first, as always, and do not start an older image
  against the upgraded database afterwards: downgrading is not supported.
- **This version cannot upgrade a database that never went through the schedule
  cutover.** The one-shot `tokayops migrate reset-schedules` command, and the
  startup check that refused to serve until it had run, are both gone: every
  supported installation completed that step releases ago. An older database
  must be brought up on a release that still carries the command before it is
  upgraded to this one.
- The column that carries an alert's own key is renamed at startup
  (`alert_groups.dedup_key` becomes `alert_key`); the rename is instant, touches
  no data, and the name every API response, webhook and page uses is unchanged.
  It is also what an older instance fails on, immediately and on every alert,
  which is a louder way to find out that instances were left running.
- While a mixture of versions is running, the "you are now on-call" message is
  the part that suffers first: the two versions recognise a handover
  differently, so one shift change can be announced twice and another not at
  all. This is the same instruction as above - stop every instance - said for
  the case where escalations are not what you notice.
- **Escalations in flight do not survive this upgrade.** How an alert is
  escalated changed completely: it is no longer a background job with steps, and
  there is no engine left to execute the old kind at all. An escalation that
  is mid-flight when instances are stopped ends there - its remaining steps are
  never sent - and alerts that arrive afterwards escalate normally. This is the
  same instruction as the first note, said for what it costs: pick a quiet
  moment.

  It does NOT block the upgrade, and nothing reports it either. The rows stay in
  the database exactly as they were; nobody reads them, so nothing fails and
  nothing says anything. Count them before you stop, with `SELECT count(*) FROM
  jobs WHERE status IN ('pending', 'running')` - afterwards that number is the
  only record that the work was owed.
- **Update and resolve jobs in flight do not survive this upgrade either, and
  the cards they were going to edit stay as they are.** Keeping an alert group's
  messages current is no longer a background job: what a message has to show is
  a revision of the alert group, applied by the delivery worker that made the
  message. The old jobs are left where they are, unread, like the escalations
  above.

  What that costs is one edit, not an alert: the card keeps whatever it last
  showed. Cards posted by the version before the escalation cutover are not
  brought up to date by this version at all - it addresses a message by the
  receipt the commitment recorded, and those cards have none. Alerts that arrive
  afterwards get new cards, which are kept current.
- **A shift change that happens while TokayOps is stopped is not announced.**
  Which shift is current is held in memory, and a starting instance takes
  whatever it finds as the baseline. That is what keeps one shift change from
  being announced twice by two instances, and it is also why a change that
  happened during the stop is taken for the state that was always there. Nobody
  is left uninformed about being on call - the schedule says who is on duty, and
  the announcement is a convenience beside it - but for that reason **do not
  pick an hour boundary for the stop window**, which is when shifts change.
- **Delivery history is kept for 30 days by default, and the first sweep runs
  a minute after this version starts.** `TOKAY_DELIVERY_RETENTION_DAYS` sets
  the window; `0` keeps everything for good. The sweep removes deliveries that
  ended more than that long ago, with their attempts and journals, and webhook
  events nothing is owed for any more - including the events the previous
  version's webhook worker had finished, measured from when it sent them. What
  a sweep removes cannot be brought back, so an operator who wants more than a
  month of history sets the variable **before the first start** of this
  version, not after. The record that an alert was admitted for delivery is
  never removed.
- **Outgoing webhooks are delivered by the same machinery as pages now, and the
  change is a cutover, not a migration.** The delivery history from before
  this version is not carried over: every subscriber's delivery list starts
  empty and fills from the first delivery this version makes. The old worker's
  tables (`event_outbox_deliveries`, `event_outbox_delivery_attempts`) are no
  longer created; an upgraded database keeps them and their rows, unread, and
  the start removes their foreign keys to events and integrations so that
  neither retention nor deleting an integration is blocked by rows nobody
  reads. An event the old worker had not finished is picked up by the new
  fan-out and sent to **every** current subscriber, including ones the old
  worker had already reached; receivers deduplicate by `X-Tokay-Event-ID`.
  `migrations/drop-webhook-outbox.sql` removes the two tables, and the events
  the old worker had finished, once rolling back is no longer an option - the
  same rules as `drop-job-engine.sql` below.
- **Review your webhook subscribers before upgrading.** Delivery is stricter
  in four ways, and nothing in the upgrade itself reports a subscriber that
  the new rules refuse - the first delivery to it does, in its delivery list.
  A URL that answers with a redirect is refused for good (redirects are no
  longer followed); a URL that resolves to anything but a public address is
  refused before any call is made, under a policy that now follows the IANA
  registries rather than a list of private ranges; a `timeout_seconds` above
  30 is clamped to 30; and a custom header named `Content-Type` or `X-Tokay-*`
  is ignored.
- **Prometheus needs three things checked.** Load
  `deploy/prometheus/tokayops.rules.yml` (its unit tests run in CI) and drop
  any alert you wrote on the `outbox_*` metrics or on
  `notification_errors_total`: those series are gone, and a rule on a series
  nobody exports never fires and never says so. Set `scrape_timeout` for the
  TokayOps job **strictly greater than 5 seconds** - `/metrics` gives the
  database five seconds for its snapshot and answers without those series
  after that, and a scrape timeout of five or less races it. Keep **at least
  30 days** of samples (`--storage.tsdb.retention.time`; the default is 15):
  the SLO recording rules read a 30-day window, and a shorter store makes them
  report on less history without saying so. If you followed `:develop` builds
  that already exported `outbound_admission_latency_seconds`, its buckets
  changed, and the 30-day quantiles are approximate until the window has
  passed the upgrade.
- The schema changes on this start: three columns are added and filled in
  from existing rows (`outbound_batches.event_id`,
  `event_outbox.fanned_out_at`, `outbound_intent_events.actor_kind`), six
  indexes are created, and the old webhook worker's foreign keys are removed
  as described above. The start refuses a database in which a webhook
  delivery names an event that no longer exists - it says which row - and
  applies nothing of the delivery domain's block until the row is repaired or
  removed.

### Changed

- **The message about an alert is now kept up to date by the part of TokayOps
  that sent it.** Before, a separate background job edited it, and the two could
  disagree about what it should say. Every change to an alert - an alert
  arriving, somebody acknowledging it, the alert clearing - now records what its
  messages have to show, and each message is brought to that on its own. In
  practice: fewer edits, and the ones that happen say the right thing.
- **The Ack and Resolve buttons in Slack answer with a short confirmation, and
  the card is brought up to date separately** rather than being replaced on the
  spot. Replacing it immediately meant two things writing the same message with
  nothing deciding the order, and an alert that arrived at that moment could be
  rubbed out of the card until the next one came. The confirmation is still
  instant; the card follows once the delivery worker picks the change up, which
  is usually the next second and can be longer when there is a backlog.
- **A resolved alert group stays "resolved".** The extra "closed" state it moved
  to afterwards no longer happens. It rendered identically, every filter that
  excluded one excluded the other, and what it really recorded - that the
  resolution reached the messages - is now kept per message, where it can be
  read precisely.
- **The alerts of a resolved incident are what they were when it resolved.** A
  payload that arrives afterwards is not merged into it: that alert firing again
  is the next incident, and it starts one. Previously a late payload could still
  change an incident that was over.
- **There is no thread under a card any more.** The alert's history is not
  posted as replies, and a resolution is not announced as a second message. Both
  were extra messages nobody could retry or point at, and the history is on the
  alert's own page. Cards themselves say more instead: each alert now carries
  its description and when it started on a second line, the same in Slack and
  Telegram.
- **An escalation step goes out when the policy said it would.** A step's delay
  is now counted from the moment the alert was picked up, not from the moment
  the previous step finished. A policy that says "the channel now, the on-call
  engineer in five minutes" pages the engineer five minutes after the alert
  arrives, even if the first message is still being retried. Before, a step that
  was slow to send pushed everything behind it back by however long it took.
- **Coming on call is announced through the same delivery machinery as an
  alert, in a queue of its own.** A hundred schedules turning over on the same
  hour boundary can no longer stand between an alert and the person on call:
  the two kinds of work are claimed separately, by separate workers with pools
  of their own. An announcement is also owed until it is delivered rather than
  attempted a fixed number of times, the same as a page.
- **An announcement about a shift that has already changed is not sent.** Every
  announcement carries a deadline - the earlier of the end of the shift it is
  about and an hour after it was accepted - and one that cannot be delivered by
  then ends as expired, with the reason in its history, rather than arriving to
  tell somebody to start a duty they are already on.
- **The message about coming on call is written by the channel that sends it.**
  One line of text used to be composed once and handed to every channel. The
  facts travel now - the team, the schedule, when the shift starts and ends -
  and each channel writes them its own way.
- **`outbound_admissions_total` carries a `family` label** (`notification` or
  `handoff`). Its `no_targets` outcome means different things for a page and for
  a shift-change announcement, and a rule cannot be written on the two mixed
  together. `outbound_admission_latency_seconds` gains buckets up to 900
  seconds, because a queue of announcements lives in the hundreds and with the
  old last bucket at 60 every healthy observation fell into `+Inf` and read as
  a minute. `outbound_queue_lateness_seconds` reports both families from the
  first scrape, so a rule about handover work can be written before the first
  shift change rather than after it.
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
- **A webhook that keeps failing keeps being retried, for a day.** The eight
  attempts and the `failed` that followed them are gone: a delivery is retried
  with a growing wait - two seconds, doubling, capped at thirty minutes, with
  some jitter - until the receiver accepts it, until the subscriber is disabled
  or deleted, or until a day has passed since the event was admitted, when it
  ends as `expired` and says so in the API.
- **What a receiver's answer means changed.** 429 and 408 are retried. Every
  other 4xx, and every 3xx, ends the delivery for good: redirects are no longer
  followed, and a subscriber whose URL answers 301 has to be corrected. A 5xx,
  or anything the client cannot classify, is retried with the same
  `X-Tokay-Event-ID`, which is what a receiver deduplicates by.
- **A URL that does not resolve to a public address is refused before any
  call is made**, as a permanent failure named `ip_policy`, where it used to be
  retried to the attempt limit. What counts as public follows the IANA
  registries for IPv4 and IPv6, with a dated snapshot in the code; an IPv4
  address embedded in IPv6 is judged as the IPv4 address. The allowlist works
  as before.
- `timeout_seconds` of a webhook subscriber is at most 30 (was 60); a saved
  value between 31 and 60 is clamped at 30 on every delivery. Custom headers
  can no longer replace the headers TokayOps sets: a reserved name is refused
  when a subscriber is saved, and ignored in a subscriber saved before the
  check existed.
- **Replaying a webhook delivery creates a new delivery** rather than resetting
  the old one to pending. The request needs an `Idempotency-Key` (400 without
  one), and a repeat with the same key answers with the same `delivery_id`. A
  delivery still in progress is not replayed (409, as before), and neither is
  one of a disabled subscriber (409).
- **Disabling a webhook subscriber withdraws what has not been delivered to it
  yet**, the same as deleting it does. A subscriber created between an event
  and its fan-out receives the event.
- **The webhook event about an alert goes out regardless of what happens to
  the alert.** An acknowledgement or a resolution no longer withdraws the
  event about the state before it.
- `response_body_trunc` in the webhook delivery list holds up to 500
  characters of the receiver's answer (was 1024 bytes). `last_error` of a
  delivery that ended without an attempt names how it ended (`expired`,
  `canceled`) rather than staying empty.
- **Testing a webhook subscriber (`POST /integrations/{id}/test`) goes through
  the same channel as a delivery**: the same address policy, headers and
  signature, the same 30-second ceiling, and no redirects. It used to have a
  sender of its own, with a 60-second timeout and redirects followed.
- `PUT` and `DELETE` on an integration wait up to three seconds for a busy row
  and answer 409 instead of waiting indefinitely; repeat the request.

### Fixed

- An Alertmanager payload and somebody pressing Acknowledge can no longer
  produce two different answers about the same incident. Whether a payload adds
  alerts to the open incident or ends it is now decided while holding that
  incident, so two payloads arriving together cannot both win, and a firing
  alert that lost the race starts the next incident instead of disappearing.
- A payload that repeats what TokayOps already knows no longer redraws anything.
  Alertmanager resends the same alerts every few minutes; each resend used to be
  a fresh edit of every message about that alert. Nothing is sent now unless
  what a message would say has actually changed.
- Two unrelated pieces of background work no longer cancel each other out by
  accident. Deduplication used to compare one opaque key across every kind of
  job, so a collision between, say, an escalation and a message update silently
  dropped one of them. Each kind of work now declares what identifies it, and
  identical keys in different kinds are simply different work.
- The escalation of a repeat incident is no longer at risk of being taken for
  the previous one. An escalation is now identified by its alert group rather
  than by the alert fingerprint, which several groups share over time as the
  same alert fires, resolves and fires again.
- The "you are now on-call" direct message no longer arrives in Telegram with
  Slack's formatting in it. The line was composed for Slack and sent verbatim to
  every channel, so a Telegram user read `:mega:` and `*Backend*` as written.
- Coming on call is announced once, however many instances of TokayOps are
  running. Whether the second instance was told depended on how quickly the
  first one's notification finished: a shift change noticed a moment later was a
  second direct message to the same people. A handover notification is now
  identified by the shift change it announces, and that identity is kept for
  good rather than for as long as the notification takes to send.
- An alert that arrives while the group's message is being updated is no longer
  left out of it. The mark that said "this message is out of date" was written
  separately from the alert itself and could be cleared by an update prepared
  before the alert arrived, so an alert could land in no message at all. There
  is no mark and no update job any more: applying an alert raises, in the same
  commit, the revision its messages have to show, and each message is brought to
  that revision by the worker that made it.
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
- Deleting a webhook subscriber that has delivery history no longer answers
  500. The subscriber is deleted, what was still owed to it is withdrawn, and
  its history stays readable through the delivery list and detail routes.
- Replaying a delivery of a disabled subscriber no longer creates a delivery
  that goes out anyway; it answers 409 until the subscriber is enabled again.
- Two instances editing or deleting the same Telegram integration at once no
  longer leave an intermediate bot token registered with Telegram: the webhook
  is reconciled against the current row under a lock rather than set from the
  arguments of whichever request finished last.
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
- Metrics for messages that are behind the alert they are about.
  `outbound_cards_behind` counts them by whether anything is going to fix that:
  `queued` will be caught up on its own, `stuck` needs somebody, and `abandoned`
  is somebody having decided not to - so an alert written on the total would
  fire on a decision that was made on purpose. `outbound_card_staleness_seconds`
  is how long the oldest one still owed has been behind, and leaves `abandoned`
  out for the same reason. `outbound_desired_revisions_total` counts what came
  of each change to an alert, by what caused it: only `applied` can create
  work - an alert group with no editable message applies a change that reaches
  nobody, which is normal and still counted.
- `handoff_recipients_skipped_total` counts the people a shift-change
  announcement could not be addressed to, by reason: no linked account, an
  account on a channel that carries no direct messages, and so on. A shift
  change nobody can be told about is still recorded as having happened, so this
  counter is where the people who were missed are visible at all.
- `storage_contract_failures_total` counts durable rows that no longer parse.
  Any increase is a data problem to look at, not a transient failure.
- **Delivery history, readable.** `GET /alert-groups/{id}/deliveries` lists
  what was owed for an alert group - pages, shift-change announcements and
  webhook events with the deliveries made for them - and the same block is on
  the group's page in the UI, with the group's timeline naming the provider
  and target of each delivery and linking to its journal.
  `GET /deliveries` is the journal across everything, filtered by family,
  provider, status, target, alert group or event, over a period that defaults
  to the last day, and is the Activity page in the UI. `GET /deliveries/{id}`
  is one delivery's journal: its attempts, what was observed about each, and
  every event in its life with who or what caused it. All three, like the
  webhook delivery routes, are in the Swagger document.
- **A decision on a delivery that has stopped.** `POST /deliveries/{id}/decisions`
  takes one of `assume_accepted`, `cancel`, `retry_current_generation` and
  `retry_new_generation` with a reason, for a delivery in `manual_review`,
  `permanent_failed` or `expired`. A decision the delivery's state does not
  allow is refused with the words of the rule that refuses it; a new deadline
  is judged by the database's clock after the row is locked, so two operators
  cannot both win; and the decision is recorded in the delivery's journal
  under the operator's name. A webhook delivery takes only `cancel` here: its
  door to a new send is the replay.
- The RBAC actions `delivery.view` and `delivery.resolve`, both held by the
  global administrator role.
- Every event in a delivery's journal says what kind of actor caused it:
  `user`, `system`, or `legacy` for a row written by a previous version, which
  is shown as the text it recorded rather than resolved to a person or a
  component. Existing rows are classified at start by the path that wrote
  them.
- `TOKAY_DELIVERY_RETENTION_DAYS` (see the upgrade notes). A replay whose
  result has since been removed by retention answers 410 to a repeat of its
  `Idempotency-Key`: the answer cannot be reproduced, and a new key starts a
  new replay.
- Metrics: `outbound_worker_ticks_total` and `outbound_fanout_ticks_total` for
  the liveness of the workers and of the webhook fan-out;
  `outbound_leases_expired_total` for attempts abandoned by a worker that
  died; `outbound_retention_window_days`,
  `outbound_retention_last_success_timestamp_seconds` (absent until the first
  successful pass) and `outbound_retention_deleted_total{table}` for the
  sweep; and `outbound_no_targets_admissions_total`, read from the database,
  for alerts admitted with nobody to page. `/metrics` gives the database
  snapshot five seconds and answers without those series after that instead
  of holding the scrape.
- `deploy/prometheus/tokayops.rules.yml`: the alerting and recording rules
  for a TokayOps installation, with the unit tests beside them
  (`tokayops.rules.test.yml`), checked in CI against the metrics the build
  exports.

### Removed

- **The background job engine.** Escalations, keeping a message current and
  shift-change announcements are all delivery commitments now, and nothing
  creates a job any more. A fresh installation has no `jobs`, `job_stages`,
  `job_steps`, `job_dedup_policies` or `notification_deliveries` tables at all.
  An upgraded one keeps them exactly as they are, with nothing reading or
  writing them: rows nobody looks at, which cost nothing but disk.

  When you are sure you will not roll back, `migrations/drop-job-engine.sql`
  removes them, along with two `alert_groups` columns that belonged to the same
  loop. It is run by hand, never at startup, and there is no way back from it -
  an older image started afterwards comes up perfectly well on an empty job
  engine and says nothing about the work and history that are missing. Only a
  backup taken beforehand restores them. Until then, note that the addresses of
  erased users survive in `notification_deliveries`: erasure never covered that
  table, and dropping it is what finally removes them.
- `job_steps_processed_total`, `notification_sent_total` and
  `notification_errors_total`. Nothing writes them any more; what a delivery
  attempted and what came of it is counted by `outbound_attempts_total`.
- `outbox_events_claimed_total`, `outbox_events_completed_total`,
  `outbox_delivery_attempts_total`, `outbox_delivery_duration_seconds`,
  `outbox_delivery_blocked_total` and `outbox_deliveries_by_status`. What a
  webhook delivery attempted and what came of it is counted by the
  `outbound_*` metrics under `family="webhook"`. `outbox_events_by_status`
  stays, and gains the `fanned_out` status.
- **The old webhook worker and its tables.** A fresh installation has no
  `event_outbox_deliveries` or `event_outbox_delivery_attempts`; an upgraded
  one keeps them unread until `migrations/drop-webhook-outbox.sql` removes
  them by hand, together with the events the old worker had finished. The
  same warning as for the job engine applies: an older image started
  afterwards comes up on an empty delivery history and says nothing about it.

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
