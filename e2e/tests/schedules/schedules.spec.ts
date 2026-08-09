import { test, expect } from '../../fixtures/auth.fixture';
import { Page } from '@playwright/test';
import { deleteTeam } from '../../fixtures/team.fixture';

/**
 * Get an on-call row that has a configured schedule.
 * Returns null if no rows with schedules exist.
 */
/**
 * A team with a schedule that can actually be edited.
 *
 * Selected by the state the row declares, not by whether it carries a schedule
 * id: a deleted schedule keeps its id so it can be recreated, and a row in
 * that state offers Recreate rather than Delete - so a test that picked it
 * would fail looking for controls that are correctly absent.
 */
async function getOnCallRowWithSchedule(page: Page) {
  // The standing fixture first: it is the one schedule no other test mutates.
  // Picking "any active row" would mean picking another worker's team, which
  // that worker is free to delete halfway through this test.
  const standing = page.locator('.oncall-row[data-team-id="e2e-standing"][data-schedule-state="active"]');
  if (await standing.count() > 0) {
    return standing.first();
  }
  const rowWithSchedule = page.locator('.oncall-row[data-schedule-state="active"]').first();
  if (await rowWithSchedule.count() > 0) {
    return rowWithSchedule;
  }
  return null;
}

test.describe('Schedule Configuration', () => {
  test.beforeEach(async ({ schedulesPage }) => {
    await schedulesPage.gotoOnCall();
  });

  test('should show on-call rows for teams', async ({ page }) => {
    const oncallRows = page.locator('.oncall-row');
    const count = await oncallRows.count();

    if (count > 0) {
      // Verify on-call rows are displayed
      await expect(oncallRows.first()).toBeVisible();
      expect(count).toBeGreaterThan(0);
    } else {
      // Empty state should be shown
      const emptyState = page.locator('.empty-state');
      await expect(emptyState).toBeVisible();
    }
  });

  test('should show edit schedule button for teams with schedule', async ({ page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    // Look for edit schedule button in the row
    const editBtn = oncallRow.locator('.edit-schedule-btn');
    await expect(editBtn).toBeVisible();
  });

  test('should show create override button for teams with schedule', async ({ page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    // Look for override button in the row
    const overrideBtn = oncallRow.locator('.create-override-btn');
    await expect(overrideBtn).toBeVisible();
  });

  test('should open schedule configuration modal', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const editBtn = oncallRow.locator('.edit-schedule-btn');
    await editBtn.click();
    await page.waitForTimeout(500);

    // Check if schedule modal opened
    await schedulesPage.expectScheduleModalVisible();
    await schedulesPage.closeScheduleModal();
  });

  test('should show schedule configuration fields', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const editBtn = oncallRow.locator('.edit-schedule-btn');
    await editBtn.click();
    await schedulesPage.expectScheduleModalVisible();

    // Check for timezone selector
    const timezoneSelect = page.locator('#schedule-timezone');
    await expect(timezoneSelect).toBeVisible();

    // Check for rotation type selector
    const rotationType = page.locator('#l1-rotation-type');
    await expect(rotationType).toBeVisible();

    // Check for handoff time
    const handoffTime = page.locator('#l1-handoff-time');
    await expect(handoffTime).toBeVisible();

    await schedulesPage.closeScheduleModal();
  });

  test('should show L1 groups editor and L2 controls', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const editBtn = oncallRow.locator('.edit-schedule-btn');
    await editBtn.click();
    await schedulesPage.expectScheduleModalVisible();

    // Check for L1 groups editor section
    const l1Section = page.locator('#l1-groups-editor');
    await expect(l1Section).toBeVisible();

    // Check for "Add Group" button
    const addGroupBtn = page.locator('#l1-add-group');
    await expect(addGroupBtn).toBeVisible();

    // Check for L2 section (might be hidden if L2 not enabled)
    const l2Checkbox = page.locator('#l2-enabled');
    await expect(l2Checkbox).toBeVisible();

    await schedulesPage.closeScheduleModal();
  });

  test('should toggle L2 escalation', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const editBtn = oncallRow.locator('.edit-schedule-btn');
    await editBtn.click();
    await schedulesPage.expectScheduleModalVisible();

    // Check for L2 enabled checkbox
    const l2Checkbox = page.locator('#l2-enabled');
    await expect(l2Checkbox).toBeVisible();

    const initialState = await l2Checkbox.isChecked();

    // Toggle L2
    await l2Checkbox.click();
    const newState = await l2Checkbox.isChecked();

    expect(newState).not.toBe(initialState);

    // Toggle back
    await l2Checkbox.click();

    await schedulesPage.closeScheduleModal();
  });

  test('should save schedule configuration', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const editBtn = oncallRow.locator('.edit-schedule-btn');
    await editBtn.click();
    await schedulesPage.expectScheduleModalVisible();

    // Saving is two steps: the first asks what the save would do, the second
    // makes it. Nothing is written until the second.
    const reviewBtn = page.locator('#schedule-form-submit');
    await expect(reviewBtn).toBeVisible();
    await reviewBtn.click();

    await expect(page.locator('.schedule-preview')).toBeVisible();

    const responsePromise = page.waitForResponse(
      (response) => response.url().includes('/schedule/config') &&
                    response.request().method() === 'PUT'
    );
    await page.locator('#preview-confirm').click();

    const response = await responsePromise;
    expect([200, 201, 204]).toContain(response.status());

    // A no-op save says so rather than claiming to have saved, so the toast
    // is matched on either outcome.
    await expect(page.locator('#toast-container')).toContainText(/saved|No changes to save/i);
  });
});

test.describe('Schedule Deletion', () => {
  test.beforeEach(async ({ schedulesPage }) => {
    await schedulesPage.gotoOnCall();
  });

  test('should show delete button in schedule config modal', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const editBtn = oncallRow.locator('.edit-schedule-btn');
    await editBtn.click();
    await schedulesPage.expectScheduleModalVisible();

    // Danger Zone delete button should be visible for existing schedules
    await expect(schedulesPage.deleteScheduleBtn).toBeVisible();

    await schedulesPage.closeScheduleModal();
  });

  test('should show danger zone section in schedule config modal', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const editBtn = oncallRow.locator('.edit-schedule-btn');
    await editBtn.click();
    await schedulesPage.expectScheduleModalVisible();

    // Danger Zone title should be visible
    const dangerZone = page.locator('.team-modal-section').filter({ hasText: 'Danger Zone' });
    await expect(dangerZone).toBeVisible();

    // The warning says what is actually true: the rotation stops and overrides
    // are cleared, while past shifts and the record itself survive.
    await expect(dangerZone).toContainText('Past shifts stay in the calendar');

    await schedulesPage.closeScheduleModal();
  });

  test('should delete schedule with confirmation', async ({ schedulesPage, page }) => {
    // First create a schedule to delete via API
    const teamId = `sched-del-${Date.now()}`;

    // Create team via API
    const createTeamResponse = await page.request.post('/api/v1/teams', {
      data: { id: teamId, name: 'Delete Schedule Test' },
    });
    expect([200, 201]).toContain(createTeamResponse.status());

    try {
      // A member, because a rotation group cannot name someone outside the
      // team, and then the schedule in one save.
      await page.request.post('/api/v1/users', {
        data: { id: 'e2e-del-user', email: 'e2e-del-user@test.com', name: 'E2E Del' },
      });
      await page.request.post(`/api/v1/teams/${teamId}/members`, {
        data: { user_id: 'e2e-del-user', role: 'team_member' },
      });

      const createScheduleResponse = await page.request.put(
        `/api/v1/teams/${teamId}/schedule/config`, {
          data: {
            expected_version: 0,
            timezone: 'UTC',
            l1: {
              enabled: true,
              rotation_type: 'daily',
              handoff_time: '11:00',
              handoff_day: null,
              groups: [{ id: crypto.randomUUID(), user_ids: ['e2e-del-user'] }],
            },
            l2: {
              enabled: false,
              escalation_timeout_minutes: 5,
              rotation_type: 'daily',
              handoff_time: '11:00',
              handoff_day: null,
              user_ids: [],
            },
          },
        });
      expect(createScheduleResponse.status(), await createScheduleResponse.text()).toBe(200);

      // Reload on-call page
      await schedulesPage.gotoOnCall();

      // Find the row for our team
      const oncallRow = page.locator(`.oncall-row[data-team-id="${teamId}"]`);
      if (await oncallRow.count() === 0) {
        test.skip();
        return;
      }

      // Open schedule config
      const editBtn = oncallRow.locator('.edit-schedule-btn');
      await editBtn.click();
      await schedulesPage.expectScheduleModalVisible();

      // Set up dialog handler for confirmation
      page.on('dialog', dialog => dialog.accept());

      // Set up API response listener for delete
      const deleteResponsePromise = page.waitForResponse(
        (response) => response.url().includes(`/api/v1/teams/${teamId}/schedule`) &&
                      response.request().method() === 'DELETE'
      );

      // Click delete
      await schedulesPage.deleteSchedule();

      // Wait for delete API response
      const deleteResponse = await deleteResponsePromise;
      expect(deleteResponse.status()).toBe(204);

      // Should show success toast
      await schedulesPage.expectToastVisible('deleted');

      // Modal should close
      await schedulesPage.expectScheduleModalHidden();
    } finally {
      // Through the shared helper: ignoring the response here counted a 500 as
      // a successful cleanup. See deleteTeam for the team it cannot remove.
      const outcome = await deleteTeam(page, teamId);
      expect(outcome.result, `cleanup of ${teamId} failed`).not.toBe('failed');
    }
  });

  test('should not delete schedule when confirmation is cancelled', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const editBtn = oncallRow.locator('.edit-schedule-btn');
    await editBtn.click();
    await schedulesPage.expectScheduleModalVisible();

    // Set up dialog handler to dismiss (cancel)
    page.on('dialog', dialog => dialog.dismiss());

    // Click delete
    await schedulesPage.deleteSchedule();

    // Modal should remain open (schedule not deleted)
    await schedulesPage.expectScheduleModalVisible();

    await schedulesPage.closeScheduleModal();
  });
});

test.describe('Schedule Overrides', () => {
  test.beforeEach(async ({ schedulesPage }) => {
    await schedulesPage.gotoOnCall();
  });

  test('should show create override button', async ({ page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    // Look for override button in the row
    const overrideBtn = oncallRow.locator('.create-override-btn');
    await expect(overrideBtn).toBeVisible();
  });

  test('should open override modal', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const overrideBtn = oncallRow.locator('.create-override-btn');
    await overrideBtn.click();
    await page.waitForTimeout(500);

    // Check if override modal opened
    await schedulesPage.expectOverrideModalVisible();

    // Close override modal
    await schedulesPage.closeOverrideModal();
  });

  test('should have override form fields', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    const overrideBtn = oncallRow.locator('.create-override-btn');
    await overrideBtn.click();
    await schedulesPage.expectOverrideModalVisible();

    // Check for user select
    const userSelect = page.locator('#override-user');
    await expect(userSelect).toBeVisible();

    // Check for datetime-local fields
    const startField = page.locator('#override-start');
    const endField = page.locator('#override-end');

    await expect(startField).toBeVisible();
    await expect(endField).toBeVisible();

    await schedulesPage.closeOverrideModal();
  });
});
