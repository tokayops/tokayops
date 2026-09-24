import { test, expect } from '../../fixtures/auth.fixture';

/**
 * Declaring how long an Alertmanager may say nothing about an alert group.
 *
 * The number is the operator's and it is optional: the form takes it in
 * minutes, stores seconds, and an empty field means nothing is claimed and no
 * alert group is marked. What matters here is the round trip - saved, shown
 * again when the integration is edited, and cleared when the field is emptied -
 * because a field that cannot be cleared is a badge that cannot be turned off.
 */
test.describe('Integrations: how long silence is normal', () => {
  test('is saved in minutes, shown again, and cleared when emptied', async ({ integrationsPage, page }) => {
    const name = `E2E Stale ${Date.now()}`;
    const secret = `stale-secret-${Date.now()}`;

    await integrationsPage.goto();
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectInboundDirection();
    await page.waitForTimeout(100);
    await integrationsPage.integrationNameInput.fill(name);
    await integrationsPage.configSecret.fill(secret);
    await page.locator('#config-stale-after').fill('245');
    await integrationsPage.integrationFormSubmit.click();

    const card = page.locator('.integration-card', { hasText: name });
    await expect(card).toBeVisible({ timeout: 10000 });

    // Edited again, the form shows what was saved - in the minutes it was
    // typed in, not the seconds it is stored as.
    await card.locator('.edit-integration-btn').click();
    await expect(integrationsPage.integrationModal).toHaveClass(/active/, { timeout: 10000 });
    await expect(page.locator('#config-stale-after')).toHaveValue('245');

    // Emptied, it goes: nothing is claimed about silence any more.
    await page.locator('#config-stale-after').fill('');
    await integrationsPage.integrationFormSubmit.click();
    await expect(integrationsPage.integrationModal).not.toHaveClass(/active/, { timeout: 10000 });

    await card.locator('.edit-integration-btn').click();
    await expect(integrationsPage.integrationModal).toHaveClass(/active/, { timeout: 10000 });
    await expect(page.locator('#config-stale-after')).toHaveValue('');
  });
});
