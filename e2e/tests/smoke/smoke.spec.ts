import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Smoke Tests', () => {
  test('should load dashboard successfully', async ({ dashboardPage }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();
    await dashboardPage.expectDashboardVisible();
  });

  test('should have all main UI elements', async ({ dashboardPage }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();

    // Verify main layout
    await expect(dashboardPage.sidebar).toBeVisible();
    await expect(dashboardPage.globalHeader).toBeVisible();
    await expect(dashboardPage.stateTabs).toBeVisible();
    await expect(dashboardPage.alertGroupsGrid).toBeVisible();
  });

  test('should have user menu', async ({ dashboardPage }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();

    await expect(dashboardPage.userMenu).toBeVisible();
  });

  test('should be able to open user menu', async ({ dashboardPage }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();

    await dashboardPage.openUserMenu();
    await expect(dashboardPage.userDropdown).toBeVisible();
  });

  test('should have logout option in user menu', async ({ dashboardPage }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();

    await dashboardPage.openUserMenu();
    await expect(dashboardPage.logoutButton).toBeVisible();
  });

  test('API health check - auth endpoint', async ({ page }) => {
    // Check that the auth endpoint responds
    const response = await page.request.get('/api/auth/me');
    expect(response.status()).toBe(200);

    const data = await response.json();
    expect(data).toHaveProperty('id');
    expect(data).toHaveProperty('email');
  });

  test('API health check - alert groups', async ({ page }) => {
    const response = await page.request.get('/api/v1/alert-groups');
    expect(response.status()).toBe(200);

    const data = await response.json();
    // API returns { alert_groups: [], total: N }
    expect(data).toHaveProperty('alert_groups');
    expect(Array.isArray(data.alert_groups)).toBe(true);
  });

  test('API health check - teams', async ({ page }) => {
    const response = await page.request.get('/api/v1/teams');
    expect(response.status()).toBe(200);

    const data = await response.json();
    // API returns { teams: [] }
    expect(data).toHaveProperty('teams');
    expect(Array.isArray(data.teams)).toBe(true);
  });

  test('API health check - users', async ({ page }) => {
    const response = await page.request.get('/api/v1/users');
    expect(response.status()).toBe(200);

    const data = await response.json();
    // API returns { users: [] }
    expect(data).toHaveProperty('users');
    expect(Array.isArray(data.users)).toBe(true);
  });
});
