import { test as setup, expect, Page } from '@playwright/test';
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

  // On the same authenticated page: a separate setup test would get a fresh
  // context, and this project is the one that creates the session rather than
  // consuming it.
  await createStandingSchedule(page);
});

/**
 * One team with a schedule that no test changes.
 *
 * The upgrade reset leaves the environment with no schedules at all, so any
 * test that wants "a team that has one" would otherwise latch onto whatever
 * fixture another worker happened to create - and fail when that worker
 * deleted it. This is the stable thing to point at.
 *
 * Nothing here is torn down: it is part of the environment, like the seeded
 * teams, and tests that need to mutate a schedule create their own.
 */
async function createStandingSchedule(page: Page) {
  const teamId = 'e2e-standing';

  await page.request.post('/api/v1/teams', {
    data: { id: teamId, name: 'E2E Standing' },
  });

  for (const user of [
    { id: 'e2e-standing-a', email: 'e2e-standing-a@test.com', name: 'E2E Standing A' },
    { id: 'e2e-standing-b', email: 'e2e-standing-b@test.com', name: 'E2E Standing B' },
  ]) {
    await page.request.post('/api/v1/users', { data: user });
    await page.request.post(`/api/v1/teams/${teamId}/members`, {
      data: { user_id: user.id, role: 'team_member' },
    });
  }

  // Idempotent: a re-run against a surviving database finds it already there,
  // and a save with the wrong expected_version is refused rather than
  // duplicating anything.
  const existing = await page.request.get(`/api/v1/teams/${teamId}/schedule/config`);
  if (existing.status() === 200) return;

  const created = await page.request.put(`/api/v1/teams/${teamId}/schedule/config`, {
    data: {
      expected_version: 0,
      timezone: 'UTC',
      l1: {
        enabled: true,
        rotation_type: 'daily',
        handoff_time: '09:00',
        handoff_day: null,
        groups: [
          { id: crypto.randomUUID(), user_ids: ['e2e-standing-a'] },
          { id: crypto.randomUUID(), user_ids: ['e2e-standing-b'] },
        ],
      },
      l2: {
        enabled: false,
        escalation_timeout_minutes: 5,
        rotation_type: 'daily',
        handoff_time: '09:00',
        handoff_day: null,
        user_ids: [],
      },
    },
  });
  expect(created.status(), await created.text()).toBe(200);
}
