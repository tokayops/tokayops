import { test, expect } from '../../fixtures/auth.fixture';
import type { Page } from '@playwright/test';

// Telegram integration UI. Drives the real form/API. A telegram bot is
// single-outbound, so each test starts from a clean slate (any pre-existing
// telegram integration is removed in beforeEach) - the create flow is always
// exercised rather than skipped on 409.
const createdIntegrationIds: string[] = [];

async function deleteExistingTelegram(page: Page) {
  const resp = await page.request.get('/api/v1/integrations');
  if (!resp.ok()) return;
  const data = await resp.json();
  const list = Array.isArray(data) ? data : data.integrations || [];
  for (const i of list) {
    if (i.type === 'telegram') {
      await page.request.delete(`/api/v1/integrations/${i.id}`);
    }
  }
}

test.describe('Telegram integration', () => {
  // A telegram bot is single-outbound, so these tests share one global backend
  // resource - run them serially so they don't race under fullyParallel.
  test.describe.configure({ mode: 'serial' });

  test.beforeEach(async ({ integrationsPage, page }) => {
    await integrationsPage.goto();
    // Non-admins are redirected away from the integrations config.
    if (!page.url().includes('/cfg/integrations')) {
      test.skip();
      return;
    }
    await deleteExistingTelegram(page); // start from a clean single-bot slate
    await integrationsPage.goto();
    await integrationsPage.waitForIntegrationsLoad();
  });

  test('creates a Telegram integration', async ({ integrationsPage, page }) => {
    const suffix = Math.random().toString(36).substring(2, 8);
    const name = `Telegram E2E ${Date.now()}-${suffix}`;

    const responsePromise = page.waitForResponse(
      (r) => r.url().includes('/api/v1/integrations') && r.request().method() === 'POST',
    );

    await integrationsPage.createTelegramIntegration(name, `123456:bot-${suffix}`, `secret-${suffix}`, '-1001234567890');

    const response = await responsePromise;
    expect([200, 201]).toContain(response.status());

    const data = await response.json();
    expect(data.type).toBe('telegram');
    if (data.id) createdIntegrationIds.push(data.id);

    await integrationsPage.expectModalHidden();
    await integrationsPage.expectToastVisible('created');
  });

  test('requires a secret token on create', async ({ integrationsPage }) => {
    const suffix = Math.random().toString(36).substring(2, 8);

    // Fill bot token only (no secret) and submit - the form must reject it.
    await integrationsPage.openTelegramForm(`Telegram NoSecret ${Date.now()}-${suffix}`, `123456:bot-${suffix}`);
    await integrationsPage.integrationFormSubmit.click();

    // No integration is created; a validation toast is shown and the modal stays open.
    await integrationsPage.expectToastVisible('Secret token is required');
    await integrationsPage.expectModalVisible();
    await integrationsPage.closeIntegrationModal();
  });

  test('rejects a second Telegram integration (single-bot)', async ({ integrationsPage, page }) => {
    const suffix = Math.random().toString(36).substring(2, 8);

    // Seed the first telegram integration via API.
    const first = await page.request.post('/api/v1/integrations', {
      data: {
        type: 'telegram',
        name: `TG First ${Date.now()}-${suffix}`,
        enabled: true,
        config: { bot_token: `123456:first-${suffix}`, secret_token: `sek-first-${suffix}` },
      },
    });
    expect(first.ok()).toBeTruthy();
    createdIntegrationIds.push((await first.json()).id);

    // A second telegram integration via the UI is rejected with 409.
    const responsePromise = page.waitForResponse(
      (r) => r.url().includes('/api/v1/integrations') && r.request().method() === 'POST',
    );
    await integrationsPage.createTelegramIntegration(`TG Second ${Date.now()}-${suffix}`, `123456:second-${suffix}`, `sek-second-${suffix}`);
    const response = await responsePromise;
    expect(response.status()).toBe(409);
    await integrationsPage.closeIntegrationModal();
  });

  // The checkbox has to answer three different questions: a brand new integration
  // starts off, one stored before the field existed had buttons on, and an explicit
  // false stays off. Only the middle case is a regression risk, and no Go test
  // covers the form.
  test('interactive toggle starts unchecked on create and posts interactive: false', async ({ integrationsPage, page }) => {
    const suffix = Math.random().toString(36).substring(2, 8);

    await integrationsPage.openTelegramForm(`TG Interactive ${Date.now()}-${suffix}`, `123456:bot-${suffix}`);
    await expect(integrationsPage.configInteractiveToggle).toBeVisible();
    await expect(integrationsPage.configInteractive).not.toBeChecked();

    await integrationsPage.configSecretToken.fill(`sek-${suffix}`);

    const responsePromise = page.waitForResponse(
      (r) => r.url().includes('/api/v1/integrations') && r.request().method() === 'POST',
    );
    await integrationsPage.integrationFormSubmit.click();

    const response = await responsePromise;
    expect([200, 201]).toContain(response.status());
    const requestBody = JSON.parse(response.request().postData() || '{}');
    expect(requestBody.config?.interactive).toBe(false);

    const data = await response.json();
    if (data.id) createdIntegrationIds.push(data.id);
  });

  test('edit form shows the toggle checked for a config stored without the field', async ({ integrationsPage, page }) => {
    const suffix = Math.random().toString(36).substring(2, 8);

    // No interactive key at all - the shape of every record written before the
    // switch existed. Create does not normalise the config, so this is exact.
    const created = await page.request.post('/api/v1/integrations', {
      data: {
        type: 'telegram',
        name: `TG Legacy ${Date.now()}-${suffix}`,
        enabled: true,
        config: { bot_token: `123456:legacy-${suffix}`, secret_token: `sek-legacy-${suffix}` },
      },
    });
    expect(created.ok()).toBeTruthy();
    const id = (await created.json()).id;
    createdIntegrationIds.push(id);

    // Reload rather than goto: the app is hash-routed, so navigating to the same
    // fragment does not refetch and the list would still show the previous state.
    await page.reload();
    await integrationsPage.waitForIntegrationsLoad();
    await integrationsPage.openIntegrationModal(id);

    await expect(integrationsPage.configInteractive).toBeChecked();
    await integrationsPage.closeIntegrationModal();
  });

  test('edit form shows the toggle unchecked for interactive: false', async ({ integrationsPage, page }) => {
    const suffix = Math.random().toString(36).substring(2, 8);

    const created = await page.request.post('/api/v1/integrations', {
      data: {
        type: 'telegram',
        name: `TG Off ${Date.now()}-${suffix}`,
        enabled: true,
        config: { bot_token: `123456:off-${suffix}`, secret_token: `sek-off-${suffix}`, interactive: false },
      },
    });
    expect(created.ok()).toBeTruthy();
    const id = (await created.json()).id;
    createdIntegrationIds.push(id);

    // Reload rather than goto: the app is hash-routed, so navigating to the same
    // fragment does not refetch and the list would still show the previous state.
    await page.reload();
    await integrationsPage.waitForIntegrationsLoad();
    await integrationsPage.openIntegrationModal(id);

    await expect(integrationsPage.configInteractive).not.toBeChecked();
    await integrationsPage.closeIntegrationModal();
  });

  test.afterAll(async ({ request }) => {
    for (const id of createdIntegrationIds) {
      try {
        await request.delete(`/api/v1/integrations/${id}`);
      } catch {
        // ignore cleanup errors
      }
    }
    createdIntegrationIds.length = 0;
  });
});
