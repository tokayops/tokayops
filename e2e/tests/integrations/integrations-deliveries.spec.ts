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

/**
 * Helper: the first delivery row that has finished as failed - the one a replay
 * button is shown for, whatever a replay made earlier sits above it.
 */
function failedRow(page: import('@playwright/test').Page) {
  return page.locator('.delivery-table tbody tr')
    .filter({ has: page.locator('.delivery-status-failed') })
    .first();
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

    // A finished delivery, not the newest row: a replay made by another test
    // in this worker may sit on top, still pending and with no attempt to show.
    const firstRow = failedRow(integrationsPage.page);
    await expect(firstRow).toBeVisible({ timeout: 15000 });

    // Click the row to open detail
    await firstRow.click();

    // Verify detail panel renders (delivery-detail-meta is the detail container)
    const detailMeta = integrationsPage.page.locator('.delivery-detail-meta');
    await expect(detailMeta).toBeVisible({ timeout: 10000 });

    // Verify attempt history table appears
    const attemptTable = integrationsPage.page.locator('.delivery-table').nth(0);
    await expect(attemptTable).toBeVisible();
  });

  test('a replay keeps its key while the answer is in doubt and opens the new delivery', async ({ integrationsPage, page }) => {
    const deliveriesBtn = page.locator(
      `.integration-card[data-integration-id="${webhookIntegrationId}"] .deliveries-btn`,
    );
    await deliveriesBtn.click();
    await expect(integrationsPage.integrationModal).toHaveClass(/active/, { timeout: 10000 });

    const row = failedRow(page);
    await expect(row).toBeVisible({ timeout: 15000 });
    const originalId = await row.getAttribute('data-delivery-id');
    expect(originalId).toBeTruthy();
    await row.click();
    await expect(page.locator('.delivery-detail-meta')).toBeVisible({ timeout: 10000 });
    const replayBtn = page.locator('#delivery-detail-replay-btn');
    await expect(replayBtn).toBeVisible({ timeout: 5000 });

    // The server's answers are staged at the network boundary, so each rule of
    // the key is exercised against the answer that defines it; the last press
    // reaches the real server. The key is read off the request the browser
    // actually sent.
    const keys: string[] = [];
    let answer: 'doubt' | 'nothing' | 'refusal' | 'real' = 'doubt';
    await page.route('**/deliveries/*/replay', route => {
      keys.push(route.request().headers()['idempotency-key'] ?? '');
      switch (answer) {
        case 'doubt':
          return route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"the database is unwell"}' });
        case 'nothing':
          return route.abort('connectionfailed');
        case 'refusal':
          return route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"delivery not found"}' });
        case 'real':
          return route.continue();
      }
    });
    const press = async (expectedRequests: number) => {
      await expect(replayBtn).toBeEnabled();
      await replayBtn.click();
      await expect.poll(() => keys.length).toBe(expectedRequests);
    };

    // A 500 does not prove nothing happened: the key is kept for the next press.
    answer = 'doubt';
    await press(1);
    await expect(page.locator('#toast-container .toast.error').last()).toContainText('Replay failed');
    // No answer at all: kept as well.
    answer = 'nothing';
    await press(2);
    // A 4xx is the server refusing the request itself. It went out with the
    // kept key, and the refusal ends the key.
    answer = 'refusal';
    await press(3);
    expect(keys[0]).toMatch(/\S/);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).toBe(keys[0]);

    // The next press is a new decision: a new key, the real server, and the
    // NEW delivery on the screen - the original stays exactly as it was.
    answer = 'real';
    // The real answer and not a staged one: the staged responses of the
    // presses above are still being delivered to the page when this is armed.
    const replayed = page.waitForResponse(r => r.request().method() === 'POST'
      && r.url().includes('/replay') && r.status() === 200);
    const opened = page.waitForRequest(r => r.method() === 'GET'
      && /\/deliveries\/[^/?]+$/.test(r.url()) && !r.url().endsWith('/' + originalId));
    await press(4);
    expect(keys[3]).not.toBe(keys[0]);
    const body = await (await replayed).json();
    expect(body.ok).toBe(true);
    expect(body.delivery_id).toBeTruthy();
    expect(body.delivery_id).not.toBe(originalId);
    expect((await opened).url()).toContain('/deliveries/' + body.delivery_id);
    await expect(page.locator('#toast-container .toast.success').last()).toContainText('Delivery queued for replay');
  });
});
