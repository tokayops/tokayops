import { test, expect } from '../../fixtures/auth.fixture';

const BASE_URL = process.env.BASE_URL || 'http://localhost:8081';

/**
 * Helper: create a generic_webhook integration via API and return its ID.
 */
async function createWebhookIntegration(
  page: import('@playwright/test').Page,
  name: string,
  url: string,
  scope: string,
  teamId?: string,
): Promise<string> {
  const config: Record<string, unknown> = { url, secret: 'e2e-test-secret', timeout_seconds: 2 };
  const body: Record<string, unknown> = {
    type: 'generic_webhook',
    name,
    enabled: true,
    scope,
    config,
  };
  if (teamId) body.team_id = teamId;

  const resp = await page.request.post(`${BASE_URL}/api/v1/integrations`, {
    data: body,
  });
  if (!resp.ok()) {
    const text = await resp.text();
    console.error(`createWebhookIntegration failed: status=${resp.status()} body=${text}`);
  }
  expect(resp.ok()).toBeTruthy();
  const json = await resp.json();
  return json.id;
}

/**
 * Helper: create an alertmanager_webhook (inbound) integration via API.
 */
async function ensureInboundWebhook(
  page: import('@playwright/test').Page,
  secret: string,
): Promise<string> {
  const resp = await page.request.post(`${BASE_URL}/api/v1/integrations`, {
    data: {
      type: 'alertmanager_webhook',
      name: 'E2E Inbound Webhook',
      enabled: true,
      config: { secret },
    },
  });
  if (!resp.ok()) {
    const text = await resp.text();
    console.error(`ensureInboundWebhook failed: status=${resp.status()} body=${text}`);
  }
  expect(resp.ok()).toBeTruthy();
  const json = await resp.json();
  return json.id;
}

/**
 * Helper: fire an alert via the alertmanager webhook endpoint.
 */
async function fireAlert(
  page: import('@playwright/test').Page,
  token: string,
  teamId: string,
): Promise<void> {
  const payload = {
    status: 'firing',
    groupKey: `e2e-delivery-test-${Date.now()}`,
    commonLabels: {
      team: teamId,
      severity: 'warning',
      alertname: 'E2E Delivery Test',
    },
    alerts: [
      {
        status: 'firing',
        labels: { alertname: 'E2E Delivery Test', severity: 'warning', team: teamId },
        annotations: { summary: 'E2E delivery test alert' },
        fingerprint: `fp-e2e-delivery-${Date.now()}`,
        startsAt: new Date().toISOString(),
        endsAt: '0001-01-01T00:00:00Z',
      },
    ],
  };
  const resp = await page.request.post(`${BASE_URL}/webhook/alertmanager?token=${token}`, {
    data: payload,
  });
  // Must be 200 - 401 means the inbound webhook secret is not configured
  expect(resp.status()).toBe(200);
}

/**
 * Helper: delete an integration via API (cleanup).
 */
async function deleteIntegration(
  page: import('@playwright/test').Page,
  id: string,
): Promise<void> {
  if (id) {
    await page.request.delete(`${BASE_URL}/api/v1/integrations/${id}`);
  }
}

/**
 * Helper: poll the deliveries API until records appear.
 * The outbox worker creates delivery records asynchronously after alert ingestion.
 */
async function pollForDeliveries(
  page: import('@playwright/test').Page,
  integrationId: string,
  opts: { requiredStatus?: string; timeoutMs?: number } = {},
): Promise<void> {
  const { requiredStatus, timeoutMs = 30000 } = opts;
  const deadline = Date.now() + timeoutMs;

  while (Date.now() < deadline) {
    const resp = await page.request.get(
      `${BASE_URL}/api/v1/integrations/${integrationId}/deliveries`,
    );
    if (resp.ok()) {
      const json = await resp.json();
      const deliveries = json.deliveries || [];
      if (requiredStatus) {
        if (deliveries.some((d: { status: string }) => d.status === requiredStatus)) return;
      } else {
        if (deliveries.length > 0) return;
      }
    }
    await page.waitForTimeout(2000);
  }

  throw new Error(
    `pollForDeliveries: no deliveries with status=${requiredStatus || 'any'} ` +
    `for integration ${integrationId} after ${timeoutMs}ms`,
  );
}

test.describe('Integrations - Deliveries', () => {
  let webhookIntegrationId = '';
  let inboundIntegrationId = '';
  const inboundSecret = 'e2e-delivery-secret';

  test.beforeAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: '.auth/user.json' });
    const page = await context.newPage();

    // Ensure an inbound webhook exists so alerts are accepted
    inboundIntegrationId = await ensureInboundWebhook(page, inboundSecret);

    // Create a generic_webhook integration pointing at the app itself.
    // Echo returns 404 for unknown paths - the worker treats 4xx as immediate "failed" (no retries).
    webhookIntegrationId = await createWebhookIntegration(
      page,
      'E2E Delivery Webhook',
      'http://127.0.0.1:8080/e2e-webhook-sink',
      'global',
    );

    // Fire an alert to trigger the outbox pipeline
    await fireAlert(page, inboundSecret, 'triage');

    // Poll until the outbox worker creates a delivery with terminal status
    await pollForDeliveries(page, webhookIntegrationId, {
      requiredStatus: 'failed',
      timeoutMs: 30000,
    });

    await context.close();
  });

  test.afterAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: '.auth/user.json' });
    const page = await context.newPage();
    await deleteIntegration(page, webhookIntegrationId);
    await deleteIntegration(page, inboundIntegrationId);
    await context.close();
  });

  test.beforeEach(async ({ integrationsPage, page }) => {
    await integrationsPage.goto();
    await page.waitForTimeout(500);
    // Close any leftover modal from a previous test (hash navigation may not reload the page)
    await page.evaluate(() => {
      const overlay = document.getElementById('integration-modal-overlay');
      if (overlay && overlay.classList.contains('active')) {
        overlay.classList.remove('active');
        document.body.style.overflow = '';
      }
    });
    const currentUrl = page.url();
    if (!currentUrl.includes('/cfg/integrations')) {
      test.skip();
      return;
    }
    await integrationsPage.waitForIntegrationsLoad();
  });

  test('should show Deliveries button on webhook card', async ({ integrationsPage }) => {
    // The deliveries button should appear on generic_webhook cards
    const deliveriesBtn = integrationsPage.page.locator(
      `.integration-card[data-integration-id="${webhookIntegrationId}"] .deliveries-btn`,
    );
    await expect(deliveriesBtn).toBeVisible({ timeout: 10000 });
  });

  test('should open deliveries panel', async ({ integrationsPage }) => {
    const deliveriesBtn = integrationsPage.page.locator(
      `.integration-card[data-integration-id="${webhookIntegrationId}"] .deliveries-btn`,
    );
    await deliveriesBtn.click();

    // Modal should open with "Deliveries" in the title
    await expect(integrationsPage.integrationModal).toHaveClass(/active/, { timeout: 10000 });
    await expect(integrationsPage.integrationModalTitle).toContainText('Deliveries');
  });

  test('should show delivery list after alert fires', async ({ integrationsPage }) => {
    const deliveriesBtn = integrationsPage.page.locator(
      `.integration-card[data-integration-id="${webhookIntegrationId}"] .deliveries-btn`,
    );
    await deliveriesBtn.click();
    await expect(integrationsPage.integrationModal).toHaveClass(/active/, { timeout: 10000 });

    // Delivery rows must exist - beforeAll polls until they appear
    const deliveryTable = integrationsPage.page.locator('.delivery-table tbody tr');
    await expect(deliveryTable.first()).toBeVisible({ timeout: 15000 });

    // Verify at least one row exists
    const rowCount = await deliveryTable.count();
    expect(rowCount).toBeGreaterThanOrEqual(1);

    // Verify status badge is present
    const statusBadge = integrationsPage.page.locator('.delivery-table tbody tr .delivery-status').first();
    await expect(statusBadge).toBeVisible();
  });

  test('should show delivery detail on row click', async ({ integrationsPage }) => {
    const deliveriesBtn = integrationsPage.page.locator(
      `.integration-card[data-integration-id="${webhookIntegrationId}"] .deliveries-btn`,
    );
    await deliveriesBtn.click();
    await expect(integrationsPage.integrationModal).toHaveClass(/active/, { timeout: 10000 });

    // Wait for delivery rows
    const firstRow = integrationsPage.page.locator('.delivery-table tbody tr').first();
    await expect(firstRow).toBeVisible({ timeout: 15000 });

    // Click the first row to open detail
    await firstRow.click();

    // Verify detail panel renders (delivery-detail-meta is the detail container)
    const detailMeta = integrationsPage.page.locator('.delivery-detail-meta');
    await expect(detailMeta).toBeVisible({ timeout: 10000 });

    // Verify attempt history table appears
    const attemptTable = integrationsPage.page.locator('.delivery-table').nth(0);
    await expect(attemptTable).toBeVisible();
  });

  test('should replay a delivery', async ({ integrationsPage }) => {
    const deliveriesBtn = integrationsPage.page.locator(
      `.integration-card[data-integration-id="${webhookIntegrationId}"] .deliveries-btn`,
    );
    await deliveriesBtn.click();
    await expect(integrationsPage.integrationModal).toHaveClass(/active/, { timeout: 10000 });

    // Wait for delivery rows
    const firstRow = integrationsPage.page.locator('.delivery-table tbody tr').first();
    await expect(firstRow).toBeVisible({ timeout: 15000 });

    // Click the first row to open detail
    await firstRow.click();

    // Wait for detail to load
    const detailMeta = integrationsPage.page.locator('.delivery-detail-meta');
    await expect(detailMeta).toBeVisible({ timeout: 10000 });

    // beforeAll polls until status=failed, so replay button should be visible
    const replayBtn = integrationsPage.page.locator('#delivery-detail-replay-btn');
    await expect(replayBtn).toBeVisible({ timeout: 5000 });

    await replayBtn.click();

    // Verify success toast appears
    await integrationsPage.expectToastVisible();
  });
});
