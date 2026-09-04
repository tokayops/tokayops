import { test, expect } from '../../fixtures/auth.fixture';
import {
  BASE_URL, PagedTeam, alertGroupByKey, currentUser, fireAlert, groupKey, pagedTeam, untilPagingFailed,
} from './deliveries.utils';

/**
 * The operator's decision, in the browser: a page that failed for good
 * without a network call - Slack is not configured here - opens a dialog
 * with the decisions its status allows; a refusal from the store is shown in
 * the store's own words; a withdrawal with a reason ends the delivery, and
 * the journal names the person who decided.
 */
test.describe('Operator decision', () => {
  let alertGroupId = '';
  let deliveryId = '';
  let team: PagedTeam;

  test.beforeAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: '.auth/user.json' });
    const page = await context.newPage();
    team = await pagedTeam(page, `dec${Date.now().toString(36)}`);
    const key = groupKey('decision');
    await fireAlert(page, key, team.teamId, 'E2E Decision');
    const group = await alertGroupByKey(page, key);
    alertGroupId = group.id;
    deliveryId = (await untilPagingFailed(page, alertGroupId)).id;
    await context.close();
  });

  test.afterAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: '.auth/user.json' });
    const page = await context.newPage();
    await team.cleanup(page);
    await context.close();
  });

  test('offers the decisions of the status, shows a refusal in the store\'s words, and records a withdrawal', async ({ page, dashboardPage }) => {
    const me = await currentUser(page);

    await page.goto(`/#/ops/alert-groups/${alertGroupId}`);
    await dashboardPage.waitForDashboardLoad();
    await dashboardPage.expectAlertModalVisible();
    await page.locator('#alert-group-deliveries .deliveries-paging .journal-link').first().click();

    const modal = page.locator('#delivery-modal-overlay');
    await expect(modal).toBeVisible();
    await expect(modal.locator('.journal-status .delivery-status-permanent_failed')).toBeVisible();
    await modal.locator('#delivery-decide-btn').click();

    // The three decisions a failed delivery allows, and no other.
    const options = modal.locator('.decision-option input[name="decision"]');
    await expect(options).toHaveCount(3);
    expect(await options.evaluateAll(els => els.map(el => (el as HTMLInputElement).value)))
      .toEqual(['cancel', 'retry_current_generation', 'retry_new_generation']);
    // The duplicate-risk box follows the decision.
    await expect(modal.locator('.decision-risk')).toBeHidden();
    await modal.locator('input[value="retry_new_generation"]').check();
    await expect(modal.locator('.decision-risk')).toBeVisible();

    // A reason is required before anything is sent.
    await modal.locator('#decision-submit-btn').click();
    await expect(modal.locator('#decision-refusal')).toBeVisible();
    await expect(modal.locator('#decision-refusal')).toContainText('reason is required');

    // The alert is over: the store refuses a retry, and the dialog shows the
    // store's words - the same words the API answers with.
    const resolved = await page.request.patch(`${BASE_URL}/api/v1/alert-groups/${alertGroupId}/resolve`);
    expect(resolved.ok()).toBeTruthy();
    const refusal = await page.request.post(`${BASE_URL}/api/v1/deliveries/${deliveryId}/decisions`, {
      data: { decision: 'retry_current_generation', reason: 'once more' },
    });
    expect(refusal.status()).toBe(409);
    const expected = await refusal.json();
    expect(expected.outcome).toBe('business_closed');
    expect(expected.detail).not.toBe('');

    await modal.locator('input[value="retry_current_generation"]').check();
    await modal.locator('#decision-reason').fill('once more');
    await expect(modal.locator('#decision-reason-count')).toHaveText('9');
    await modal.locator('#decision-submit-btn').click();
    await expect(modal.locator('#decision-refusal')).toBeVisible();
    await expect(modal.locator('.decision-refusal-outcome')).toHaveText('The alert is over');
    await expect(modal.locator('.decision-refusal-detail')).toHaveText(expected.detail);

    // A withdrawal with a reason applies: the delivery ends as canceled, and
    // the journal says who decided, by name.
    await modal.locator('input[value="cancel"]').check();
    await modal.locator('#decision-reason').fill('nobody is listening');
    await modal.locator('#decision-submit-btn').click();
    await expect(modal.locator('.journal-status .delivery-status-canceled')).toBeVisible({ timeout: 15000 });
    await expect(modal.locator('.journal-events [data-kind="canceled"]')).toBeVisible();
    const decision = modal.locator('.journal-events [data-kind="operator_decision"]');
    await expect(decision).toBeVisible();
    await expect(decision).toContainText('nobody is listening');
    await expect(decision.locator('.journal-actor-user .delivery-target-name')).toHaveText(me.name);
    await expect(modal.locator('#delivery-decide-btn')).toHaveCount(0);

    // The group's block reflects the decision.
    await modal.locator('#delivery-modal-close').click();
    await expect(page.locator('#alert-group-deliveries .deliveries-paging .delivery-status-canceled')).toBeVisible({ timeout: 15000 });
  });
});
