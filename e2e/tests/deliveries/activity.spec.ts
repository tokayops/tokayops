import { test, expect } from '../../fixtures/auth.fixture';
import {
  PagedTeam, alertGroupByKey, createWebhookIntegration, deleteIntegration, fireAlert, groupKey,
  loginAs, pagedTeam, untilDeliveries, untilPagingFailed,
} from './deliveries.utils';

/**
 * #/ops/activity is the operational log: every delivery of every family over
 * the last day, newest first, narrowed by family and status, in pages. It is
 * the administrator's, and the navigation says so to everybody else.
 */
test.describe('Activity log', () => {
  let webhookIntegrationId = '';
  let team: PagedTeam;

  test.beforeAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: '.auth/user.json' });
    const page = await context.newPage();
    webhookIntegrationId = await createWebhookIntegration(
      page, 'E2E Activity Webhook', 'http://127.0.0.1:8080/e2e-activity-sink');
    team = await pagedTeam(page, `act${Date.now().toString(36)}`);
    const key = groupKey('activity');
    await fireAlert(page, key, team.teamId, 'E2E Activity');
    const group = await alertGroupByKey(page, key);
    await untilPagingFailed(page, group.id);
    await untilDeliveries(page, group.id, 'the webhook delivery to settle',
      g => g.events.some(e => e.batches.some(b => b.deliveries.some(d => d.status === 'permanent_failed'))));
    await context.close();
  });

  test.afterAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: '.auth/user.json' });
    const page = await context.newPage();
    await deleteIntegration(page, webhookIntegrationId);
    await team.cleanup(page);
    await context.close();
  });

  test('lists the last day, narrows by family and status, and pages', async ({ page, dashboardPage }) => {
    await page.goto('/#/ops/alert-groups');
    await dashboardPage.waitForDashboardLoad();

    // The navigation item is a link now, not a placeholder.
    const navItem = page.locator('#sidebar-nav [data-route="activity"]');
    await expect(navItem).not.toHaveClass(/disabled/);
    await navItem.click();
    await expect(page).toHaveURL(/#\/ops\/activity/);

    const table = page.locator('.activity-table');
    await expect(table).toBeVisible({ timeout: 15000 });
    await expect(page.locator('#activity-period')).toHaveText('Last 24 hours');
    await expect(page.locator('#activity-page')).toContainText('Page 1');
    await expect(page.locator('#activity-total')).toContainText('deliveries');
    await expect(page.locator('#activity-prev')).toBeDisabled();

    const rows = page.locator('.activity-row');
    expect(await rows.count()).toBeGreaterThan(0);

    // Family narrows to the family.
    await page.locator('#activity-family').selectOption('webhook');
    await expect(page.locator('.activity-table')).toBeVisible({ timeout: 15000 });
    await expect.poll(async () => {
      const families = await page.locator('.activity-row').evaluateAll(
        els => els.map(el => (el as HTMLElement).dataset.family));
      return families.length > 0 && families.every(f => f === 'webhook');
    }).toBe(true);

    // Status narrows within it.
    await page.locator('#activity-status').selectOption('permanent_failed');
    await expect.poll(async () => {
      const statuses = await page.locator('.activity-row').evaluateAll(
        els => els.map(el => (el as HTMLElement).dataset.status));
      return statuses.length > 0 && statuses.every(s => s === 'permanent_failed');
    }).toBe(true);

    // A status nothing is in answers with an empty log, not an error.
    await page.locator('#activity-status').selectOption('sending');
    await expect(page.locator('#activity-empty')).toBeVisible({ timeout: 15000 });

    // Every row offers the journal.
    await page.locator('#activity-status').selectOption('');
    await expect(page.locator('.activity-row').first()).toBeVisible({ timeout: 15000 });
    await page.locator('.activity-row').first().locator('.journal-link').click();
    await expect(page.locator('#delivery-modal-overlay')).toBeVisible();
    await expect(page.locator('#delivery-modal-overlay .journal-events')).toBeVisible();
  });

  test('is the administrator\'s', async ({ browser }) => {
    const page = await loginAs(browser, 'alice@example.com');
    try {
      await page.goto('/#/ops/alert-groups');
      const navItem = page.locator('#sidebar-nav [data-route="activity"]');
      await expect(navItem).toHaveClass(/disabled/, { timeout: 15000 });
      await page.goto('/#/ops/activity');
      await expect(page.locator('#activity-forbidden')).toBeVisible({ timeout: 15000 });
      await expect(page.locator('.activity-table')).toHaveCount(0);
    } finally {
      await page.context().close();
    }
  });
});
