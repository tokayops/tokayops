import { test, expect } from '../../fixtures/auth.fixture';

// The policy step editor builds its (provider:kind) dropdown from GET /api/v1/providers.
// The telegram capability is registered at app startup, so it must appear with zero
// frontend changes - this asserts that contract end-to-end against the running app.
test.describe('Telegram in policy editor', () => {
  test('step type dropdown offers Telegram', async ({ policiesPage, page }) => {
    await policiesPage.goto();
    if (!page.url().includes('/cfg/policies')) {
      test.skip();
      return;
    }
    await policiesPage.waitForPoliciesLoad();

    await policiesPage.openCreatePolicyModal();
    await policiesPage.addStep();

    const stepTypeSelect = page.locator('.policy-step-row .step-type-select').first();
    await expect(stepTypeSelect).toBeVisible({ timeout: 10000 });

    const options = (await stepTypeSelect.locator('option').allInnerTexts()).join(' | ').toLowerCase();
    expect(options).toContain('telegram');

    await policiesPage.closePolicyModal();
  });
});
