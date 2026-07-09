import { test, expect } from '../../fixtures/auth.fixture';

// The profile modal exposes a Telegram section (Epic 8 Sprint 3). Linking itself is
// a deep-link flow that calls the Bot API (unreachable in the e2e stack), so this
// only asserts the disconnected section + Connect button render — the link issuance
// and webhook flow are covered by the Go integration test.
test.describe('Telegram profile section', () => {
  test('shows a Connect Telegram action', async ({ dashboardPage, page }) => {
    await dashboardPage.goto();
    await dashboardPage.openUserMenu(); // #open-profile-btn lives in the user dropdown
    await dashboardPage.profileButton.click();

    // Profile modal open + Telegram section present.
    await expect(page.locator('.telegram-integration')).toBeVisible({ timeout: 10000 });
    // Disconnected state offers Connect (a linked account would show Disconnect instead).
    const connectBtn = page.locator('#telegram-connect-btn');
    const unbindBtn = page.locator('#telegram-unbind-btn');
    await expect(connectBtn.or(unbindBtn)).toBeVisible({ timeout: 10000 });
  });
});
