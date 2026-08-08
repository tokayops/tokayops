import { test, expect } from '../../fixtures/auth.fixture';
import { Page } from '@playwright/test';

// ========================================
// Helper: seed test data via API
// ========================================

interface TestEnv {
  teamId: string;
  scheduleId: string;
  memberIds: string[];
}

/**
 * Create a team with a schedule and two members via API.
 * Returns IDs for cleanup.
 */
async function seedTestEnv(page: Page, suffix: string): Promise<TestEnv> {
  const nonce = Math.random().toString(36).slice(2, 8);
  const teamId = `e2e-ov-${suffix}-${Date.now()}-${nonce}`;

  // Create team
  const teamRes = await page.request.post('/api/v1/teams', {
    data: { id: teamId, name: `Override Test ${suffix}` },
  });
  expect([200, 201, 409]).toContain(teamRes.status());

  // Create two users (may already exist — ignore errors)
  for (const user of [
    { id: 'e2e-alice', email: 'e2e-alice@test.com', name: 'E2E Alice' },
    { id: 'e2e-bob', email: 'e2e-bob@test.com', name: 'E2E Bob' },
  ]) {
    await page.request.post('/api/v1/users', { data: user });
  }

  // Add members (role must be 'team_member' or 'team_admin')
  const addAlice = await page.request.post(`/api/v1/teams/${teamId}/members`, {
    data: { user_id: 'e2e-alice', role: 'team_member' },
  });
  expect([200, 201]).toContain(addAlice.status());
  const addBob = await page.request.post(`/api/v1/teams/${teamId}/members`, {
    data: { user_id: 'e2e-bob', role: 'team_member' },
  });
  expect([200, 201]).toContain(addBob.status());

  // One save, carrying the whole configuration. Each member is their own
  // group, which is what the flat rotation of the old model meant.
  const schedRes = await page.request.put(`/api/v1/teams/${teamId}/schedule/config`, {
    data: scheduleConfig([['e2e-alice'], ['e2e-bob']], 0),
  });
  expect(schedRes.status(), await schedRes.text()).toBe(200);

  const config = await (await page.request.get(`/api/v1/teams/${teamId}/schedule/config`)).json();

  return {
    teamId,
    scheduleId: config.schedule_id,
    memberIds: ['e2e-alice', 'e2e-bob'],
  };
}

/**
 * A complete configuration payload.
 *
 * The L2 policy is present even though the layer is off: the server validates
 * both layers regardless, so a disabled layer still needs a cadence and a
 * handoff time that parse.
 */
export function scheduleConfig(groups: string[][], expectedVersion: number, timezone = 'UTC') {
  return {
    expected_version: expectedVersion,
    timezone,
    l1: {
      enabled: true,
      rotation_type: 'daily',
      handoff_time: '09:00',
      handoff_day: null,
      groups: groups.map(userIds => ({ id: crypto.randomUUID(), user_ids: userIds })),
    },
    l2: {
      enabled: false,
      escalation_timeout_minutes: 5,
      rotation_type: 'daily',
      handoff_time: '09:00',
      handoff_day: null,
      user_ids: [],
    },
  };
}

/**
 * Create an override via API. Returns the override object.
 */
async function createOverrideViaAPI(
  page: Page,
  teamId: string,
  userId: string,
  startTime: string,
  endTime: string,
  reason: string,
) {
  const res = await page.request.post(`/api/v1/teams/${teamId}/schedule/overrides`, {
    data: { user_id: userId, valid_from: startTime, valid_to: endTime, reason },
  });
  expect(res.status(), await res.text()).toBe(201);
  return await res.json();
}

/** Cleanup: delete team (cascades to schedule + overrides) */
async function cleanup(page: Page, teamId: string) {
  await page.request.delete(`/api/v1/teams/${teamId}`);
}

/**
 * Compute a future override window that is guaranteed to be visible
 * in the current month's calendar view.
 */
function futureOverrideWindow(): { start: string; end: string } {
  const tomorrow = new Date();
  tomorrow.setDate(tomorrow.getDate() + 1);
  tomorrow.setHours(10, 0, 0, 0);
  const dayAfter = new Date(tomorrow);
  dayAfter.setDate(dayAfter.getDate() + 1);
  return { start: tomorrow.toISOString(), end: dayAfter.toISOString() };
}

// ========================================
// Tests: Calendar Override Context Menu
// ========================================

test.describe('Calendar Override Context Menu', () => {
  let env: TestEnv;

  test.beforeEach(async ({ page, schedulesPage }) => {
    env = await seedTestEnv(page, 'ctx');

    const { start, end } = futureOverrideWindow();
    await createOverrideViaAPI(page, env.teamId, 'e2e-bob', start, end, 'E2E test override');

    await schedulesPage.gotoOnCall();
  });

  test.afterEach(async ({ page }) => {
    if (env?.teamId) {
      await cleanup(page, env.teamId);
    }
  });

  test('should show context menu when clicking calendar override badge', async ({
    schedulesPage,
  }) => {
    await schedulesPage.openCalendarView(env.teamId);

    // Override we created must be visible in the calendar
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    // Click the override badge
    await schedulesPage.clickCalendarOverride(0);

    // Context menu should appear with Edit and Delete
    await schedulesPage.expectContextMenuVisible();
    await expect(schedulesPage.contextMenuEdit).toBeVisible();
    await expect(schedulesPage.contextMenuDelete).toBeVisible();

    await schedulesPage.closeCalendarView();
  });

  test('should close context menu on outside click', async ({ schedulesPage }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    await schedulesPage.clickCalendarOverride(0);
    await schedulesPage.expectContextMenuVisible();

    // Click outside the menu (on the calendar list area)
    await schedulesPage.calendarList.click({ position: { x: 5, y: 5 } });

    await schedulesPage.expectContextMenuHidden();

    await schedulesPage.closeCalendarView();
  });

  test('should close context menu on Escape key', async ({ page, schedulesPage }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    await schedulesPage.clickCalendarOverride(0);
    await schedulesPage.expectContextMenuVisible();

    await page.keyboard.press('Escape');

    await schedulesPage.expectContextMenuHidden();

    // Calendar modal should still be open (Escape only closes the context menu)
    await expect(schedulesPage.calendarList).toBeVisible();

    await schedulesPage.closeCalendarView();
  });

  test('should open edit form when clicking Edit in context menu', async ({ schedulesPage }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    await schedulesPage.clickCalendarOverride(0);
    await schedulesPage.expectContextMenuVisible();

    // Click Edit
    await schedulesPage.contextMenuClickEdit();

    // Should open override modal in edit mode
    await schedulesPage.expectOverrideModalVisible();

    // User select should be pre-populated with e2e-bob
    await expect(schedulesPage.overrideUserSelect).toHaveValue('e2e-bob');

    // Reason field should be populated
    await expect(schedulesPage.overrideReason).toHaveValue('E2E test override');

    // Submit button text changes to "Save Changes" in edit mode
    await expect(schedulesPage.overrideSubmitBtn).toContainText(/save changes/i);

    // Cancel returns to calendar (not closes modal)
    await schedulesPage.cancelOverrideAndReturnToCalendar();
    await schedulesPage.closeCalendarView();
  });

  test('should delete override from calendar context menu', async ({ page, schedulesPage }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    // Accept the confirm dialog
    page.on('dialog', (dialog) => dialog.accept());

    await schedulesPage.clickCalendarOverride(0);
    await schedulesPage.expectContextMenuVisible();

    // Listen for DELETE API call
    const deletePromise = page.waitForResponse(
      (r) => r.url().includes('/overrides/') && r.request().method() === 'DELETE',
    );

    await schedulesPage.contextMenuClickDelete();

    const deleteRes = await deletePromise;
    expect(deleteRes.status()).toBe(204);

    // Toast should confirm deletion
    await schedulesPage.expectToastVisible('removed');

    // Override should disappear from the calendar
    await expect(schedulesPage.calendarOverrideEntries).toHaveCount(0, { timeout: 5000 });

    await schedulesPage.closeCalendarView();
  });
});

// ========================================
// Tests: Override Edit from Modal List
// ========================================

test.describe('Override Edit from Modal List', () => {
  let env: TestEnv;

  test.beforeEach(async ({ page, schedulesPage }) => {
    env = await seedTestEnv(page, 'lst');

    const { start, end } = futureOverrideWindow();
    await createOverrideViaAPI(page, env.teamId, 'e2e-alice', start, end, 'Modal list test');

    await schedulesPage.gotoOnCall();
  });

  test.afterEach(async ({ page }) => {
    if (env?.teamId) {
      await cleanup(page, env.teamId);
    }
  });

  test('should show edit and delete buttons in override modal list', async ({ schedulesPage }) => {
    await schedulesPage.openCreateOverrideModal(env.teamId);

    // The override list should contain our override
    await expect(schedulesPage.overrideItems.first()).toBeVisible({ timeout: 5000 });

    // Should have edit and delete buttons
    const firstItem = schedulesPage.overrideItems.first();
    await expect(firstItem.locator('.edit-override-btn')).toBeVisible();
    await expect(firstItem.locator('.delete-override-btn')).toBeVisible();

    await schedulesPage.closeOverrideModal();
  });

  test('should populate edit form when clicking edit button in list', async ({ schedulesPage }) => {
    await schedulesPage.openCreateOverrideModal(env.teamId);
    await expect(schedulesPage.overrideItems.first()).toBeVisible({ timeout: 5000 });

    // Click the edit button on the first override
    await schedulesPage.editOverrideFromList(0);

    // Form should be populated
    await expect(schedulesPage.overrideUserSelect).toHaveValue('e2e-alice');
    await expect(schedulesPage.overrideReason).toHaveValue('Modal list test');

    // Submit button should say "Save Changes"
    await expect(schedulesPage.overrideSubmitBtn).toContainText(/save changes/i);

    await schedulesPage.closeOverrideModal();
  });

  test('should update override via edit form', async ({ page, schedulesPage }) => {
    await schedulesPage.openCreateOverrideModal(env.teamId);
    await expect(schedulesPage.overrideItems.first()).toBeVisible({ timeout: 5000 });

    // Click edit
    await schedulesPage.editOverrideFromList(0);

    // Change the user to bob
    await schedulesPage.overrideUserSelect.selectOption('e2e-bob');

    // Change the reason
    await schedulesPage.overrideReason.fill('Updated via e2e test');

    // Listen for PUT (update)
    const updatePromise = page.waitForResponse(
      (r) => r.url().includes('/overrides/') && r.request().method() === 'PUT',
    );

    // Submit the form
    await schedulesPage.overrideSubmitBtn.click();

    const updateRes = await updatePromise;
    expect(updateRes.status()).toBe(200);

    // Should show success toast
    await schedulesPage.expectToastVisible('updated');
  });

  test('should delete override from modal list', async ({ page, schedulesPage }) => {
    await schedulesPage.openCreateOverrideModal(env.teamId);
    await expect(schedulesPage.overrideItems.first()).toBeVisible({ timeout: 5000 });

    const countBefore = await schedulesPage.overrideItems.count();

    // Accept the confirm dialog
    page.on('dialog', (dialog) => dialog.accept());

    // Listen for DELETE
    const deletePromise = page.waitForResponse(
      (r) => r.url().includes('/overrides/') && r.request().method() === 'DELETE',
    );

    await schedulesPage.deleteOverride(0);

    const deleteRes = await deletePromise;
    expect(deleteRes.status()).toBe(204);

    await schedulesPage.expectToastVisible('removed');

    // Override list should have one fewer item
    await expect(schedulesPage.overrideItems).toHaveCount(countBefore - 1, { timeout: 5000 });
  });
});

// ========================================
// Tests: Calendar Edit → Return to Calendar
// ========================================

test.describe('Calendar Edit Return Flow', () => {
  let env: TestEnv;

  test.beforeEach(async ({ page, schedulesPage }) => {
    env = await seedTestEnv(page, 'ret');

    const { start, end } = futureOverrideWindow();
    await createOverrideViaAPI(page, env.teamId, 'e2e-bob', start, end, 'Return flow test');

    await schedulesPage.gotoOnCall();
  });

  test.afterEach(async ({ page }) => {
    if (env?.teamId) {
      await cleanup(page, env.teamId);
    }
  });

  test('should return to calendar after editing override from calendar', async ({
    page,
    schedulesPage,
  }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    // Click override → Edit
    await schedulesPage.clickCalendarOverride(0);
    await schedulesPage.expectContextMenuVisible();
    await schedulesPage.contextMenuClickEdit();

    // Override modal should be open in edit mode
    await schedulesPage.expectOverrideModalVisible();
    await expect(schedulesPage.overrideSubmitBtn).toContainText(/save changes/i);

    // Change reason
    await schedulesPage.overrideReason.fill('Edited from calendar');

    // Listen for PUT
    const updatePromise = page.waitForResponse(
      (r) => r.url().includes('/overrides/') && r.request().method() === 'PUT',
    );

    // Submit
    await schedulesPage.overrideSubmitBtn.click();

    const updateRes = await updatePromise;
    expect(updateRes.status()).toBe(200);

    // After save, should return to calendar view (modal stays active, calendar re-renders)
    await expect(schedulesPage.calendarList).toBeVisible({ timeout: 5000 });

    await schedulesPage.closeCalendarView();
  });

  test('should return to calendar when cancelling edit from calendar', async ({ schedulesPage }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    // Click override → Edit
    await schedulesPage.clickCalendarOverride(0);
    await schedulesPage.expectContextMenuVisible();
    await schedulesPage.contextMenuClickEdit();

    // Override modal should be open
    await schedulesPage.expectOverrideModalVisible();

    // Cancel — should return to calendar (modal stays active)
    await schedulesPage.cancelOverrideAndReturnToCalendar();

    // Calendar list should be visible again
    await expect(schedulesPage.calendarList).toBeVisible({ timeout: 5000 });

    await schedulesPage.closeCalendarView();
  });

  test('should return to calendar when pressing Escape during edit from calendar', async ({
    page,
    schedulesPage,
  }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    // Click override → Edit
    await schedulesPage.clickCalendarOverride(0);
    await schedulesPage.expectContextMenuVisible();
    await schedulesPage.contextMenuClickEdit();

    // Override modal should be open in edit mode
    await schedulesPage.expectOverrideModalVisible();

    // Press Escape — should close override edit and return to parent view
    await page.keyboard.press('Escape');
    await page.waitForTimeout(500);

    // Modal should still be active (override edit closed, parent view shown)
    await schedulesPage.expectScheduleModalVisible();

    // Close whatever view is open
    const calendarClose = page.locator('#calendar-close');
    const modalClose = page.locator('#schedule-modal-close, .modal-close').first();
    if (await calendarClose.isVisible({ timeout: 2000 }).catch(() => false)) {
      await schedulesPage.closeCalendarView();
    } else if (await modalClose.isVisible({ timeout: 1000 }).catch(() => false)) {
      await modalClose.click();
    }
  });

  test('should return to calendar when clicking X during edit from calendar', async ({
    schedulesPage,
  }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    // Click override → Edit
    await schedulesPage.clickCalendarOverride(0);
    await schedulesPage.expectContextMenuVisible();
    await schedulesPage.contextMenuClickEdit();

    // Override modal should be open in edit mode
    await schedulesPage.expectOverrideModalVisible();

    // Click X button — should return to calendar, NOT close modal
    await schedulesPage.page.locator('#modal-close').click();

    // Calendar list should be visible (modal stays active)
    await expect(schedulesPage.calendarList).toBeVisible({ timeout: 5000 });
    await schedulesPage.expectScheduleModalVisible();

    await schedulesPage.closeCalendarView();
  });
});

// ========================================
// Tests: Override Data Attributes on Calendar
// ========================================

test.describe('Calendar Override Data Attributes', () => {
  let env: TestEnv;

  test.beforeEach(async ({ page, schedulesPage }) => {
    env = await seedTestEnv(page, 'attr');

    const { start, end } = futureOverrideWindow();
    await createOverrideViaAPI(page, env.teamId, 'e2e-bob', start, end, 'Data attrs test');

    await schedulesPage.gotoOnCall();
  });

  test.afterEach(async ({ page }) => {
    if (env?.teamId) {
      await cleanup(page, env.teamId);
    }
  });

  test('should have correct data attributes on override calendar entries', async ({
    schedulesPage,
  }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    const entry = schedulesPage.calendarOverrideEntries.first();

    // A calendar entry is a shift, not an override record. It carries enough
    // to identify which override to act on; the revision and the reason are
    // read from the override list when one is opened, because a shift does not
    // know them and guessing would be how a stale edit gets through.
    const overrideId = await entry.getAttribute('data-override-id');
    const userId = await entry.getAttribute('data-user-id');
    const validFrom = await entry.getAttribute('data-valid-from');
    const validTo = await entry.getAttribute('data-valid-to');

    expect(overrideId).toBeTruthy();
    expect(userId).toBe('e2e-bob');
    expect(validFrom).toBeTruthy();
    expect(validTo).toBeTruthy();

    // The entry should display "OVERRIDE" label and user name
    await expect(entry.locator('.entry-layer')).toContainText(/override/i);
    await expect(entry.locator('.entry-user')).toContainText('E2E Bob');

    await schedulesPage.closeCalendarView();
  });

  test('override entry should have pointer cursor', async ({ schedulesPage }) => {
    await schedulesPage.openCalendarView(env.teamId);
    await expect(schedulesPage.calendarOverrideEntries.first()).toBeVisible({ timeout: 5000 });

    const entry = schedulesPage.calendarOverrideEntries.first();
    const cursor = await entry.evaluate((el) => getComputedStyle(el).cursor);
    expect(cursor).toBe('pointer');

    await schedulesPage.closeCalendarView();
  });
});
