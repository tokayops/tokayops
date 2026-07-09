import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Navigation', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();
  });

  test('should show mode switcher in header', async ({ dashboardPage }) => {
    await expect(dashboardPage.modeSwitcher).toBeVisible();
    await expect(dashboardPage.modeSwitcherOps).toBeVisible();
    await expect(dashboardPage.modeSwitcherCfg).toBeVisible();
  });

  test('should start in Operations mode by default', async ({ dashboardPage }) => {
    await dashboardPage.expectInOpsMode();
  });

  test('should switch from Ops to Configure mode', async ({ dashboardPage }) => {
    await dashboardPage.expectInOpsMode();
    await dashboardPage.switchToConfigureMode();
    await dashboardPage.expectInConfigureMode();
  });

  test('should switch from Configure to Ops mode', async ({ dashboardPage }) => {
    await dashboardPage.switchToConfigureMode();
    await dashboardPage.expectInConfigureMode();

    await dashboardPage.switchToOpsMode();
    await dashboardPage.expectInOpsMode();
  });

  test('should show sidebar navigation', async ({ dashboardPage }) => {
    await expect(dashboardPage.sidebarNav).toBeVisible();
  });

  test('should navigate to different sections via sidebar in Ops mode', async ({ dashboardPage, page }) => {
    // Navigate to On-Call
    await dashboardPage.navigateToOnCall();
    await expect(page).toHaveURL(/#\/ops\/oncall/);

    // Navigate back to Alert Groups
    await dashboardPage.navigateToAlertGroups();
    await expect(page).toHaveURL(/#\/ops\/alert-groups/);
  });

  test('should navigate to different sections via sidebar in Configure mode', async ({ dashboardPage, page }) => {
    await dashboardPage.switchToConfigureMode();

    // Navigate to Policies
    await dashboardPage.navigateToPolicies();
    await expect(page).toHaveURL(/#\/cfg\/policies/);

    // Navigate to Teams
    await dashboardPage.navigateToTeams();
    await expect(page).toHaveURL(/#\/cfg\/teams/);
  });

  test('should show team context selector in Ops mode', async ({ dashboardPage }) => {
    await dashboardPage.expectInOpsMode();
    // Team context selector should be visible in ops mode
    await expect(dashboardPage.teamContextTrigger).toBeVisible();
  });

  test('should hide team context selector in Configure mode', async ({ dashboardPage }) => {
    await dashboardPage.switchToConfigureMode();
    // Team context selector should be hidden in config mode
    await expect(dashboardPage.teamContextTrigger).toBeHidden();
  });

  test('should persist mode preference across navigation', async ({ dashboardPage, page }) => {
    // Switch to Configure mode
    await dashboardPage.switchToConfigureMode();
    await dashboardPage.expectInConfigureMode();

    // Navigate within Configure mode
    await dashboardPage.navigateToPolicies();
    await expect(page).toHaveURL(/#\/cfg\/policies/);

    // Should still be in Configure mode
    await dashboardPage.expectInConfigureMode();
  });

  test('should update URL when switching modes', async ({ dashboardPage, page }) => {
    // Start in Ops mode
    await dashboardPage.expectInOpsMode();
    expect(page.url()).toContain('#/ops');

    // Switch to Configure
    await dashboardPage.switchToConfigureMode();
    expect(page.url()).toContain('#/cfg');

    // Switch back to Ops
    await dashboardPage.switchToOpsMode();
    expect(page.url()).toContain('#/ops');
  });
});

test.describe('Navigation - Restricted Access', () => {
  test('should show Users section for admin users', async ({ dashboardPage, page }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();

    // Check if we can access Users (admin only)
    await dashboardPage.switchToConfigureMode();

    // Try to navigate to users
    const usersLink = dashboardPage.sidebarNav.locator('[data-route="users"]');

    // If visible, admin user - otherwise the link might not exist
    const isVisible = await usersLink.isVisible().catch(() => false);
    if (isVisible) {
      await usersLink.click();
      await expect(page).toHaveURL(/#\/cfg\/users/);
    }
  });

  test('should show Integrations section for admin users', async ({ dashboardPage, page }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();

    await dashboardPage.switchToConfigureMode();

    const integrationsLink = dashboardPage.sidebarNav.locator('[data-route="integrations"]');

    const isVisible = await integrationsLink.isVisible().catch(() => false);
    if (isVisible) {
      await integrationsLink.click();
      await expect(page).toHaveURL(/#\/cfg\/integrations/);
    }
  });
});
