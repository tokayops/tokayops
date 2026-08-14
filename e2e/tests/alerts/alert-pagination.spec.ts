import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Alert Pagination', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();
    // Filter to "all" to get maximum alerts for pagination testing
    await dashboardPage.filterByState('all');
    await dashboardPage.page.waitForTimeout(500);
  });

  test('should show pagination when there are many alerts', async ({ dashboardPage }) => {
    await expect(dashboardPage.pagination).toBeVisible();
  });

  test('should have pagination controls', async ({ dashboardPage }) => {
    await expect(dashboardPage.prevPage).toBeVisible();
    await expect(dashboardPage.nextPage).toBeVisible();
    await expect(dashboardPage.pageInfo).toBeVisible();
  });

  test('should have Previous button disabled on first page', async ({ dashboardPage }) => {
    await expect(dashboardPage.prevPage).toBeDisabled();
  });

  test('should have Next button enabled when there are more pages', async ({ dashboardPage }) => {
    await expect(dashboardPage.nextPage).toBeEnabled();
  });

  test('should show page info with correct format', async ({ dashboardPage }) => {
    const pageInfo = await dashboardPage.pageInfo.textContent();
    expect(pageInfo).toMatch(/Page \d+ of \d+/);
  });

  test('should navigate to next page when Next button is clicked', async ({ dashboardPage, page }) => {
    // Get initial page info
    const initialPageInfo = await dashboardPage.pageInfo.textContent();
    expect(initialPageInfo).toContain('Page 1');

    // Click next
    await dashboardPage.goToNextPage();
    await page.waitForTimeout(300);

    // Page info should show page 2
    const newPageInfo = await dashboardPage.pageInfo.textContent();
    expect(newPageInfo).toContain('Page 2');

    // Previous button should now be enabled
    await expect(dashboardPage.prevPage).toBeEnabled();
  });

  test('should navigate back to previous page when Previous button is clicked', async ({ dashboardPage, page }) => {
    // Go to page 2
    await dashboardPage.goToNextPage();
    await page.waitForTimeout(300);

    // Verify we're on page 2
    const pageInfoAfterNext = await dashboardPage.pageInfo.textContent();
    expect(pageInfoAfterNext).toContain('Page 2');

    // Go back to page 1
    await dashboardPage.goToPrevPage();
    await page.waitForTimeout(300);

    // Verify we're back on page 1
    const pageInfoAfterPrev = await dashboardPage.pageInfo.textContent();
    expect(pageInfoAfterPrev).toContain('Page 1');

    // Previous should be disabled again on page 1
    await expect(dashboardPage.prevPage).toBeDisabled();
  });

  test('should reset to page 1 when changing state filter', async ({ dashboardPage, page }) => {
    // Go to page 2
    await dashboardPage.goToNextPage();
    await page.waitForTimeout(300);

    // Verify we're on page 2
    const pageInfoBefore = await dashboardPage.pageInfo.textContent();
    expect(pageInfoBefore).toContain('Page 2');

    // Change filter to "active"
    await dashboardPage.filterByState('active');
    await page.waitForTimeout(500);

    // Should be back on page 1
    const pageInfoAfter = await dashboardPage.pageInfo.textContent();
    expect(pageInfoAfter).toContain('Page 1');
  });

  test('should update alert list when navigating pages', async ({ dashboardPage, page }) => {
    // Identity, not title: titles are not unique, and a database with enough
    // alerts from earlier runs puts a same-named group at the top of both
    // pages. The page would have turned; the test would say it had not.
    const alertCards = dashboardPage.alertGroupsGrid.locator('.alert-group-card');
    const firstAlertPage1 = await alertCards.first().getAttribute('data-alert-group-id');

    await dashboardPage.goToNextPage();
    await page.waitForTimeout(300);

    const firstAlertPage2 = await alertCards.first().getAttribute('data-alert-group-id');

    expect(firstAlertPage2).not.toBe(firstAlertPage1);
  });
});
