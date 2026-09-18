import { test, expect } from '../../fixtures/auth.fixture';

/**
 * An alert group Alertmanager has gone quiet about.
 *
 * Nothing is claimed unless the integration that sent the group declared how
 * long silence is normal for it, and nothing is claimed about a group that has
 * ended. What the view compares is answered by the list itself - the card is
 * drawn without the alerts - so these are mocked list answers, and what is
 * under test is the comparison and the drawing, in the browser that does both.
 */
const HOUR = 60 * 60 * 1000;

const base = {
  status: 'triggered',
  severity: 'critical',
  team_id: 'test-team',
  created_at: new Date(Date.now() - 6 * HOUR).toISOString(),
  updated_at: new Date(Date.now() - 6 * HOUR).toISOString(),
  alerts_count: 1,
  firing_count: 1,
  unreported_count: 0,
  oncall_snapshot: null,
};

const GROUPS = [
  {
    ...base,
    id: 'quiet-overdue',
    title: 'QuietOverdue',
    dedup_key: 'quiet-overdue',
    // Silent for five hours where four is what the integration allows.
    last_notified_at: new Date(Date.now() - 5 * HOUR).toISOString(),
    quiet_after_seconds: 4 * 3600,
  },
  {
    ...base,
    id: 'quiet-within',
    title: 'QuietWithin',
    dedup_key: 'quiet-within',
    last_notified_at: new Date(Date.now() - 1 * HOUR).toISOString(),
    quiet_after_seconds: 4 * 3600,
  },
  {
    ...base,
    id: 'quiet-undeclared',
    title: 'QuietUndeclared',
    dedup_key: 'quiet-undeclared',
    // Silent for a day, but nobody said what is normal.
    last_notified_at: new Date(Date.now() - 24 * HOUR).toISOString(),
  },
  {
    ...base,
    id: 'quiet-resolved',
    title: 'QuietResolved',
    dedup_key: 'quiet-resolved',
    status: 'resolved',
    resolved_at: new Date(Date.now() - 3 * HOUR).toISOString(),
    last_notified_at: new Date(Date.now() - 24 * HOUR).toISOString(),
    quiet_after_seconds: 4 * 3600,
  },
];

test.describe('Alert groups Alertmanager has gone quiet about', () => {
  test('are badged only when a silence was declared and the group is open', async ({ page, dashboardPage }) => {
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
            alert_groups: GROUPS,
            total: GROUPS.length, page: 1, total_pages: 1, has_next: false, has_prev: false,
          }),
        });
      },
    );

    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();
    await dashboardPage.expectLoadingComplete();
    // The state filter hides resolved groups by default; this test is about
    // all four at once.
    await page.locator('#state-tabs button[data-state="all"]').click();

    const card = (title: string) => page.locator('.alert-group-card', { hasText: title });

    const overdue = card('QuietOverdue').locator('.badge-quiet');
    await expect(overdue).toBeVisible();
    await expect(overdue).toContainText('No notification for 5h');
    await expect(overdue).toHaveAttribute('title', /silence over 4h is unusual/);

    await expect(card('QuietWithin').locator('.badge-quiet')).toHaveCount(0);
    await expect(card('QuietUndeclared').locator('.badge-quiet')).toHaveCount(0);
    await expect(card('QuietResolved').locator('.badge-quiet')).toHaveCount(0);

    // And inside the group, reached the way a person reaches it. The detail is
    // drawn twice - from the summary already loaded, then from the answer for
    // the group - so the answer for the group says a different silence: a
    // badge reading 9h can only have come from the second drawing, and a badge
    // left over from the first would read 5h and fail.
    await page.route('**/api/v1/alert-groups/quiet-overdue', async (route) => {
      if (route.request().method() !== 'GET') {
        await route.continue();
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          ...GROUPS[0],
          last_notified_at: new Date(Date.now() - 9 * HOUR).toISOString(),
          alerts: [],
        }),
      });
    });
    await page.route('**/api/v1/alert-groups/quiet-overdue/timeline', async (route) => {
      await route.fulfill({
        status: 200, contentType: 'application/json', body: JSON.stringify({ events: [] }),
      });
    });

    await card('QuietOverdue').click();
    await dashboardPage.expectAlertModalVisible();
    const inDetail = page.locator('#modal-body .badge-quiet');
    await expect(inDetail).toBeVisible();
    await expect(inDetail).toContainText('No notification for 9h');
  });
});
