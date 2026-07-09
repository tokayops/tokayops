import { test as setup, expect } from '@playwright/test';
import { LoginPage } from '../pages/login.page';

const AUTH_FILE = '.auth/user.json';

setup('authenticate', async ({ page }) => {
  const loginPage = new LoginPage(page);

  // Navigate to login page
  await loginPage.goto();

  // Login with test credentials
  const email = process.env.TEST_USER_EMAIL || 'admin@example.com';
  const password = process.env.TEST_USER_PASSWORD || 'Admin123!';

  await loginPage.login(email, password);

  // Wait for redirect to dashboard (app uses hash routing)
  await expect(page).toHaveURL(/\/#\//, { timeout: 10000 });

  // Verify we're logged in by checking main app is visible
  await expect(page.locator('#main-app')).toBeVisible({ timeout: 10000 });

  // Save the storage state (cookies + localStorage)
  await page.context().storageState({ path: AUTH_FILE });
});
