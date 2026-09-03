import { test, expect } from '../../fixtures/auth.fixture';
import {
  PagedTeam, alertGroupByKey, createWebhookIntegration, deleteIntegration, fireAlert, groupKey,
  loginAs, pagedTeam, replayDelivery, untilDeliveries, untilPagingFailed,
} from './deliveries.utils';

/**
 * The deliveries of an alert group, in its details: the paging with its
 * addressee and provider - on the commitment and on the timeline line the
 * delivery wrote - and the webhook half from the claims on the group's
 * events: the fan-out's claim and a replay's, each with its delivery. The
 * journal of one delivery opens from here for an administrator, and is not
 * offered to anybody else.
 */
test.describe('Alert group deliveries', () => {
  let alertGroupId = '';
  let webhookIntegrationId = '';
  let team: PagedTeam;

  test.beforeAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: '.auth/user.json' });
    const page = await context.newPage();

    // A subscriber pointing back at the app: a 404 is a refusal, final.
    webhookIntegrationId = await createWebhookIntegration(
      page, 'E2E Group Deliveries Webhook', 'http://127.0.0.1:8080/e2e-group-deliveries-sink');
    team = await pagedTeam(page, `gd${Date.now().toString(36)}`);

    const key = groupKey('group-deliveries');
    await fireAlert(page, key, team.teamId, 'E2E Group Deliveries');
    const group = await alertGroupByKey(page, key);
    alertGroupId = group.id;

    // The paging fails before any network call - Slack is not configured on
    // this installation - and the webhook delivery fails at the sink.
    await untilPagingFailed(page, alertGroupId);
    const withFanOut = await untilDeliveries(page, alertGroupId, 'the fan-out to deliver and fail',
      g => g.events.some(e => e.batches.some(b => b.kind === 'webhook_event' &&
        b.deliveries.some(d => d.target_ref === webhookIntegrationId && d.status === 'permanent_failed'))));
    const fanned = withFanOut.events.flatMap(e => e.batches).find(b => b.kind === 'webhook_event')!;
    const original = fanned.deliveries.find(d => d.target_ref === webhookIntegrationId)!;

    // A replay is its own claim on the same event.
    await replayDelivery(page, webhookIntegrationId, original.id, `e2e-press-${Date.now()}`);
    await untilDeliveries(page, alertGroupId, 'the replay to deliver and fail',
      g => g.events.some(e => e.batches.some(b => b.kind === 'webhook_replay' &&
        b.deliveries.some(d => d.status === 'permanent_failed'))));

    await context.close();
  });

  test.afterAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: '.auth/user.json' });
    const page = await context.newPage();
    await deleteIntegration(page, webhookIntegrationId);
    await team.cleanup(page);
    await context.close();
  });

  test('the block shows the paging, the webhook claims, and opens the journal for an administrator', async ({ page, dashboardPage }) => {
    await page.goto(`/#/ops/alert-groups/${alertGroupId}`);
    await dashboardPage.waitForDashboardLoad();
    await dashboardPage.expectAlertModalVisible();

    const block = page.locator('#alert-group-deliveries');
    await expect(block.locator('.deliveries-paging')).toBeVisible({ timeout: 15000 });

    // The paging: who was paged, through what, and what became of it.
    const paging = block.locator('.deliveries-paging tbody tr');
    await expect(paging.first()).toBeVisible();
    await expect(paging.first().locator('.delivery-status-permanent_failed')).toBeVisible();
    await expect(paging.first()).toContainText('slack');
    await expect(paging.first().locator('.delivery-target')).toContainText(team.userName);

    // The webhook half, from the claims: the fan-out's and the replay's, each
    // with its delivery to the subscriber.
    const event = block.locator('.delivery-event').first();
    await expect(event).toBeVisible();
    await expect(event.locator('.delivery-event-type')).toContainText('alert_group.firing');
    const fanOut = event.locator('.delivery-batch[data-batch-kind="webhook_event"]');
    const replay = event.locator('.delivery-batch[data-batch-kind="webhook_replay"]');
    await expect(fanOut).toHaveCount(1);
    await expect(replay).toHaveCount(1);
    await expect(fanOut.locator('.delivery-batch-kind')).toHaveText('Fan-out');
    await expect(replay.locator('.delivery-batch-kind')).toHaveText('Replay');
    await expect(fanOut.locator('.delivery-row .delivery-target', { hasText: webhookIntegrationId })).toHaveCount(1);
    await expect(replay.locator('.delivery-row .delivery-status-permanent_failed')).toBeVisible();

    // The timeline line the delivery wrote names the same addressee and
    // provider, from the row rather than from the prose.
    const timelineLine = page.locator('#alert-group-timeline .timeline-delivery').first();
    await expect(timelineLine).toBeVisible({ timeout: 15000 });
    await expect(timelineLine.locator('.delivery-target')).toContainText(team.userName);
    await expect(timelineLine.locator('.timeline-delivery-provider')).toHaveText('via slack');
    await expect(timelineLine.locator('.journal-link')).toBeVisible();

    // A click opens the journal of that delivery.
    await paging.first().locator('.journal-link').click();
    const journal = page.locator('#delivery-modal-overlay');
    await expect(journal).toBeVisible();
    await expect(journal.locator('.journal-attempts tbody tr')).toHaveCount(1);
    await expect(journal.locator('.journal-attempts')).toContainText('preparation');
    await expect(journal.locator('.journal-events [data-kind="created"]')).toBeVisible();
    await expect(journal.locator('.journal-events [data-kind="created"] .journal-actor-system')).toHaveText('Escalation engine');
    await journal.locator('#delivery-modal-close').click();
    await expect(journal).toBeHidden();

    // A webhook delivery that failed for good has one door to a new effect,
    // the replay: the dialog offers a withdrawal and nothing the server
    // would refuse.
    await fanOut.locator('.delivery-row', { hasText: webhookIntegrationId }).locator('.journal-link').click();
    await expect(journal).toBeVisible();
    await expect(journal.locator('.journal-status .delivery-status-permanent_failed')).toBeVisible();
    await journal.locator('#delivery-decide-btn').click();
    await expect(journal.locator('.decision-option input[name="decision"]')).toHaveCount(1);
    await expect(journal.locator('.decision-option input[name="decision"]')).toHaveValue('cancel');
    await expect(journal.locator('.decision-replay-hint')).toContainText('replay');
    await journal.locator('#delivery-modal-close').click();
    await expect(journal).toBeHidden();
  });

  test('a user who is not an administrator sees the deliveries and is not offered the journal', async ({ browser }) => {
    const page = await loginAs(browser, 'alice@example.com');
    try {
      await page.goto(`/#/ops/alert-groups/${alertGroupId}`);
      const block = page.locator('#alert-group-deliveries');
      await expect(block.locator('.deliveries-paging')).toBeVisible({ timeout: 20000 });
      await expect(block.locator('.deliveries-paging tbody tr').first().locator('.delivery-target')).toContainText(team.userName);
      await expect(block.locator('.delivery-event')).toHaveCount(1);
      await expect(block.locator('.journal-link')).toHaveCount(0);
      await expect(page.locator('#alert-group-timeline .timeline-delivery').first()).toBeVisible({ timeout: 15000 });
      await expect(page.locator('#alert-group-timeline .journal-link')).toHaveCount(0);
      await expect(page.locator('#sidebar-nav [data-route="activity"]')).toHaveClass(/disabled/);
    } finally {
      await page.context().close();
    }
  });
});
