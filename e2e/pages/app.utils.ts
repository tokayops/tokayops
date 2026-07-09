import { Page, expect } from '@playwright/test';

/**
 * Wait for the SPA to finish initialization.
 * Sequence: auth-loading hidden -> main-app visible -> sidebar-nav visible.
 */
export async function waitForAppReady(page: Page): Promise<void> {
  await expect(page.locator('#auth-loading')).toBeHidden({ timeout: 15000 });
  await expect(page.locator('#main-app')).toBeVisible({ timeout: 15000 });
  await expect(page.locator('#sidebar-nav')).toBeVisible({ timeout: 10000 });
}
