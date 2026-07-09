import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Alert Filters', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();
  });

  test('should display state filter tabs', async ({ dashboardPage }) => {
    await dashboardPage.expectStateTabsVisible();
  });

  test('should have Active tab selected by default', async ({ dashboardPage }) => {
    await dashboardPage.expectStateTabActive('active');
  });

  test('should filter by triggered state', async ({ dashboardPage, page }) => {
    await dashboardPage.filterByState('triggered');
    await page.waitForTimeout(500);
    await dashboardPage.expectStateTabActive('triggered');
  });

  test('should filter by acknowledged state', async ({ dashboardPage, page }) => {
    await dashboardPage.filterByState('acknowledged');
    await page.waitForTimeout(500);
    await dashboardPage.expectStateTabActive('acknowledged');
  });

  test('should filter by resolved state', async ({ dashboardPage, page }) => {
    await dashboardPage.filterByState('resolved');
    await page.waitForTimeout(500);
    await dashboardPage.expectStateTabActive('resolved');
  });

  test('should show all alerts when clicking all tab', async ({ dashboardPage, page }) => {
    await dashboardPage.filterByState('all');
    await page.waitForTimeout(500);
    await dashboardPage.expectStateTabActive('all');
  });

  test('should display alert groups grid', async ({ dashboardPage }) => {
    await expect(dashboardPage.alertGroupsGrid).toBeVisible();
  });

  test('should have sidebar navigation', async ({ dashboardPage }) => {
    await expect(dashboardPage.sidebar).toBeVisible();
  });

  test('should have global header', async ({ dashboardPage }) => {
    await expect(dashboardPage.globalHeader).toBeVisible();
  });

  test('should display severity filter chips', async ({ dashboardPage }) => {
    await expect(dashboardPage.severityChips).toBeVisible();
  });
});
