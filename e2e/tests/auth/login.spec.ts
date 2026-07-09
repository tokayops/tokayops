import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Authentication', () => {
  // These tests don't use stored auth state
  test.use({ storageState: { cookies: [], origins: [] } });

  test('should login with valid credentials', async ({ page, loginPage }) => {
    await loginPage.goto();

    const email = process.env.TEST_USER_EMAIL || 'admin@example.com';
    const password = process.env.TEST_USER_PASSWORD || 'Admin123!';

    await loginPage.login(email, password);

    // Should redirect to dashboard (app uses hash routing)
    await expect(page).toHaveURL(/\/#\//, { timeout: 10000 });
    await expect(page.locator('#main-app')).toBeVisible({ timeout: 10000 });
  });

  test('should show error with invalid credentials', async ({ loginPage }) => {
    await loginPage.goto();

    await loginPage.login('invalid@example.com', 'wrongpassword');

    // Should show error message (case-insensitive check)
    await expect(loginPage.errorMessage).toBeVisible();
    await expect(loginPage.errorMessage).toContainText(/invalid/i);

    // Should stay on login page
    await loginPage.expectOnLoginPage();
  });

  test('should show error with empty credentials', async ({ loginPage }) => {
    await loginPage.goto();

    await loginPage.submitButton.click();

    // Browser validation should prevent submission or show error
    await loginPage.expectOnLoginPage();
  });

  test('should redirect to login when not authenticated', async ({ page }) => {
    // Try to access dashboard without authentication
    await page.goto('/');

    // Should redirect to login page
    await expect(page).toHaveURL(/\/login\.html/, { timeout: 10000 });
  });

  test('should have email field focused by default', async ({ loginPage }) => {
    await loginPage.goto();

    await expect(loginPage.emailInput).toBeFocused();
  });

  test('login form should have proper structure', async ({ loginPage }) => {
    await loginPage.goto();

    await expect(loginPage.loginForm).toBeVisible();
    await expect(loginPage.emailInput).toBeVisible();
    await expect(loginPage.passwordInput).toBeVisible();
    await expect(loginPage.submitButton).toBeVisible();
    await expect(loginPage.submitButton).toHaveText('Sign In');
  });
});
