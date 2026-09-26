import { test, expect } from '../../fixtures/auth.fixture';

// The phone section, with GET /me/phone stubbed: calling a real phone is not
// something this stack can do, and what is asserted here is only what the
// section offers for a number in a given state.
//
// An unverified number shows the code field and the provider choice straight
// away. The code field must not depend on this page having asked for the call
// itself: a call whose outcome the provider did not confirm may still ring,
// and a call asked for before the profile was reopened still does. The choice
// must come before verification, because a code call that does not get through
// one provider is what makes a person want another.
const unverifiedWithTwoProviders = {
  contact: { value: '+14155550101', updated_at: '2026-09-26T10:00:00Z' },
  covered: true,
  pin_active: false,
  providers: [
    { number: '+15005550006', integration_id: 'tw-us', integration: 'US' },
    { number: '+15005550007', integration_id: 'tw-world', integration: 'World' },
  ],
  senders: [
    { number: '+15005550006', integration_id: 'tw-us', integration: 'US' },
    { number: '+15005550007', integration_id: 'tw-world', integration: 'World' },
  ],
};

test.describe('Phone profile section', () => {
  test('an unverified number offers the code field and the provider choice', async ({ dashboardPage, page }) => {
    await page.route('**/api/auth/me/phone', route => {
      if (route.request().method() !== 'GET') {
        return route.fallback();
      }
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(unverifiedWithTwoProviders),
      });
    });

    await dashboardPage.goto();
    await dashboardPage.openUserMenu();
    await dashboardPage.profileButton.click();

    await expect(page.locator('.phone-integration')).toBeVisible({ timeout: 10000 });
    await expect(page.locator('#phone-call-code-btn')).toBeVisible();
    await expect(page.locator('#phone-code')).toBeVisible();
    await expect(page.locator('#phone-confirm-code-btn')).toBeVisible();

    const choice = page.locator('#phone-pin-select');
    await expect(choice).toBeVisible();
    await expect(choice.locator('option')).toHaveCount(3);
  });
});
