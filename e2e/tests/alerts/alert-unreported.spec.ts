import { test, expect } from '../../fixtures/auth.fixture';

/**
 * An alert Alertmanager has stopped reporting is neither firing nor resolved,
 * and the view has to say so twice: in the card, which is answered without the
 * alerts themselves and counts by what the API says, and inside the group,
 * where the alert has its own badge and its own border.
 *
 * Both are driven from mocked answers, because what is under test is the
 * drawing, in the browser that does it - and the card is reached the way a
 * person reaches it, by clicking it, which is the path that renders the
 * summary before the detail arrives.
 */
const MOCK_ALERT_GROUP_ID = 'test-unreported';

const SILENT_SINCE = '2026-09-18T09:00:00Z';

const MOCK_SUMMARY = {
  id: MOCK_ALERT_GROUP_ID,
  title: 'TestUnreported',
  status: 'triggered',
  severity: 'critical',
  dedup_key: 'test-dedup-unreported',
  team_id: 'test-team',
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  alerts_count: 4,
  firing_count: 1,
  unreported_count: 1,
  oncall_snapshot: null,
};

const MOCK_ALERT_GROUP = {
  ...MOCK_SUMMARY,
  last_notified_at: new Date().toISOString(),
  alerts: [
    {
      status: 'firing',
      fingerprint: 'fp-firing',
      labels: { alertname: 'StillFiring', instance: 'host-1' },
      annotations: { description: 'Alertmanager still reports this one' },
    },
    {
      status: 'firing',
      fingerprint: 'fp-unreported',
      unreportedSince: SILENT_SINCE,
      labels: { alertname: 'WentQuiet', instance: 'host-2' },
      annotations: { description: 'Silenced in Alertmanager' },
    },
    {
      status: 'resolved',
      fingerprint: 'fp-resolved',
      labels: { alertname: 'Cleared', instance: 'host-3' },
      annotations: { description: 'This one cleared' },
    },
    {
      // A status this build does not know, which the API and the list count as
      // resolved. The view has to agree with them rather than call it firing.
      fingerprint: 'fp-unknown',
      labels: { alertname: 'Unknown', instance: 'host-4' },
      annotations: { description: 'A status from a later build' },
    },
  ],
};

test.describe('Alerts Alertmanager stopped reporting', () => {
  test('are counted apart in the card and shown apart in the group', async ({ page, dashboardPage }) => {
    // The list, by path: the detail below is another path under the same
    // prefix, and a pattern would swallow it.
    await page.route(
      (url) => url.pathname === '/api/v1/alert-groups',
      async (route) => {
        if (route.request().method() !== 'GET') {
          await route.continue();
          return;
        }
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            alert_groups: [MOCK_SUMMARY],
            total: 1, page: 1, total_pages: 1, has_next: false, has_prev: false,
          }),
        });
      },
    );
    await page.route('**/api/v1/alert-groups/' + MOCK_ALERT_GROUP_ID, async (route) => {
      if (route.request().method() === 'GET') {
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(MOCK_ALERT_GROUP),
        });
      } else {
        await route.continue();
      }
    });
    await page.route('**/api/v1/alert-groups/' + MOCK_ALERT_GROUP_ID + '/timeline', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ events: [] }),
      });
    });

    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();
    await dashboardPage.expectLoadingComplete();

    // The card counts by what the list answered: one of each state, and the
    // one that resolved is what is left over.
    const card = page.locator('.alert-group-card', { hasText: 'TestUnreported' });
    await expect(card).toBeVisible();
    const counts = card.locator('.alerts-count-main');
    await expect(counts).toContainText('1 firing');
    await expect(counts).toContainText('1 not reported');
    await expect(counts).toContainText('2 resolved');

    await card.click();
    await dashboardPage.expectAlertModalVisible();

    const summary = page.locator('.detail-section-title', { hasText: 'Alerts:' }).first();
    await expect(summary).toContainText('1 firing');
    await expect(summary).toContainText('1 not reported');
    await expect(summary).toContainText('2 resolved');

    // The alert that went quiet says since when, and is not called Firing.
    const quiet = page.locator('.alert-item', { hasText: 'host-2' });
    await expect(quiet).toHaveClass(/status-unreported/);
    const badge = quiet.locator('.alert-status-tag');
    await expect(badge).toHaveClass(/status-unreported/);
    await expect(badge).toContainText('Not reported since');

    // The other two keep the two states they had.
    await expect(page.locator('.alert-item', { hasText: 'host-1' }).locator('.alert-status-tag'))
      .toContainText('Firing');
    await expect(page.locator('.alert-item', { hasText: 'host-3' }).locator('.alert-status-tag'))
      .toContainText('Resolved');
    await expect(page.locator('.alert-item', { hasText: 'host-4' }).locator('.alert-status-tag'))
      .toContainText('Resolved');
  });
});
