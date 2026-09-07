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
test.describe('Alert group deliveries in the timeline', () => {
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

  test('the timeline names the page and opens its journal for an administrator', async ({ page, dashboardPage }) => {
    await page.goto(`/#/ops/alert-groups/${alertGroupId}`);
    await dashboardPage.waitForDashboardLoad();
    await dashboardPage.expectAlertModalVisible();

    // The timeline line the delivery wrote names the addressee and the
    // provider, from the row rather than from the prose. The alert's page
    // lists no deliveries of its own: the timeline is where they are read.
    const timelineLine = page.locator('#alert-group-timeline .timeline-delivery').first();
    await expect(timelineLine).toBeVisible({ timeout: 15000 });
    await expect(timelineLine.locator('.delivery-target')).toContainText(team.userName);
    await expect(timelineLine.locator('.timeline-delivery-provider')).toHaveText('via slack');
    await expect(timelineLine.locator('.journal-link')).toBeVisible();

    // A click opens the journal of that delivery.
    await timelineLine.locator('.journal-link').click();
    const journal = page.locator('#delivery-modal-overlay');
    await expect(journal).toBeVisible();
    await expect(journal.locator('.journal-history .journal-attempt')).toHaveCount(1);
    await expect(journal.locator('.journal-history .journal-attempt')).toContainText('Not sent');
    await expect(journal.locator('.journal-history [data-kind="created"]')).toBeVisible();
    await expect(journal.locator('.journal-history [data-kind="created"] .journal-actor-system')).toHaveText('Escalation engine');
    await journal.locator('#delivery-modal-close').click();
    await expect(journal).toBeHidden();

    // A webhook delivery that failed for good has one door to a new effect,
    // the replay: the dialog offers a withdrawal and nothing the server
    // would refuse. Webhook deliveries belong to the event, not to the
    // alert's page; the Activity list is where they are opened.
    await page.goto('/#/ops/activity');
    const failedWebhook = page.locator('.activity-row[data-family="webhook"][data-status="permanent_failed"]',
      { hasText: webhookIntegrationId }).first();
    await expect(failedWebhook).toBeVisible({ timeout: 15000 });
    await failedWebhook.locator('.journal-link').click();
    await expect(journal).toBeVisible();
    await expect(journal.locator('.journal-status .delivery-status-permanent_failed')).toBeVisible();
    await journal.locator('#delivery-decide-btn').click();
    await expect(journal.locator('.decision-option input[name="decision"]')).toHaveCount(1);
    await expect(journal.locator('.decision-option input[name="decision"]')).toHaveValue('cancel');
    await expect(journal.locator('.decision-replay-hint')).toContainText('replay');
    await journal.locator('#delivery-modal-close').click();
    await expect(journal).toBeHidden();
  });

  test('a user who is not an administrator sees the timeline and is not offered the journal', async ({ browser }) => {
    const page = await loginAs(browser, 'alice@example.com');
    try {
      await page.goto(`/#/ops/alert-groups/${alertGroupId}`);
      await expect(page.locator('#alert-group-timeline .timeline-delivery').first()).toBeVisible({ timeout: 20000 });
      await expect(page.locator('#alert-group-timeline .timeline-delivery').first().locator('.delivery-target')).toContainText(team.userName);
      await expect(page.locator('#alert-group-timeline .journal-link')).toHaveCount(0);
      await expect(page.locator('#sidebar-nav [data-route="activity"]')).toHaveClass(/disabled/);
    } finally {
      await page.context().close();
    }
  });
});
