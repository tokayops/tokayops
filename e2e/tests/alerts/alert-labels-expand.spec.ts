import { test, expect } from '../../fixtures/auth.fixture';

const MOCK_ALERT_GROUP_ID = 'test-labels-expand';

const MOCK_ALERT_GROUP = {
  id: MOCK_ALERT_GROUP_ID,
  title: 'TestLabelsExpand',
  status: 'triggered',
  severity: 'critical',
  dedup_key: 'test-dedup',
  team_id: 'test-team',
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  alerts: [
    {
      status: 'firing',
      labels: {
        alertname: 'TestAlert',
        instance: 'localhost:9090',
        alertgroup: 'TestGroup',
        dc: 'dc1',
        exported_job: 'test-job',
        label4: 'value4',
        label5: 'value5',
        label6: 'value6',
        label7: 'value7',
        label8: 'value8',
        label9: 'value9',
        label10: 'value10',
        label11: 'value11',
        label12: 'value12',
        label13: 'value13',
        label14: 'value14',
      },
      annotations: {
        description: 'Test alert with many labels',
      },
    },
  ],
  oncall_snapshot: null,
};

test.describe('Alert Labels Expand/Collapse', () => {
  test('should expand hidden labels when clicking "+N more" and collapse on "hide"', async ({ page, dashboardPage }) => {
    // Mock the alert group detail API to return alert with many labels
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

    // Mock timeline to avoid errors
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

    // Navigate directly to the mocked alert group to open the modal
    await page.evaluate((id) => {
      window.location.hash = `#/ops/alert-groups/${id}`;
    }, MOCK_ALERT_GROUP_ID);

    // Wait for modal to appear
    await dashboardPage.expectAlertModalVisible();

    // Find the labels section inside the modal
    const alertItem = page.locator('.alert-item').first();
    const labelsInline = alertItem.locator('.alert-labels-inline');
    await expect(labelsInline).toBeVisible();

    // Verify the toggle button exists with "+N more" text
    const toggle = labelsInline.locator('.alert-labels-toggle');
    await expect(toggle).toBeVisible();
    await expect(toggle).toContainText('more');

    // Hidden labels should not be visible initially
    const hiddenLabels = labelsInline.locator('.alert-labels-hidden');
    await expect(hiddenLabels).toBeHidden();

    // Click the toggle to expand
    await toggle.click();

    // Hidden labels should now be visible
    await expect(hiddenLabels).toBeVisible();

    // Toggle text should change to "hide"
    await expect(toggle).toContainText('hide');

    // Verify that expanded labels contain expected content
    const hiddenText = await hiddenLabels.textContent();
    expect(hiddenText).toContain('label4=value4');

    // Click again to collapse
    await toggle.click();

    // Hidden labels should be hidden again
    await expect(hiddenLabels).toBeHidden();

    // Toggle text should restore to "+N more"
    await expect(toggle).toContainText('more');
  });
});
