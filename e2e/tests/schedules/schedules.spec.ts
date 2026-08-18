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

  /**
   * The Until column is in the reader's timezone, not each schedule's, and the
   * header is the only thing that says so. Without it the column reads as if
   * it were in the zone the schedule was configured with.
   */
  test('should say whose timezone the Until column is in', async ({ page }) => {
    const until = page.locator('.oncall-list-header .oncall-cell').filter({ hasText: 'Until' });
    // The short zone itself is not asserted: it is the runner's own, and it
    // changes with the locale and with daylight saving. That it is there, and
    // that the title says whose it is, are the parts that carry the meaning.
    await expect(until.locator('.oncall-cell-note')).not.toBeEmpty();
    await expect(until).toHaveAttribute('title', /Shown in your timezone/);

    // The label sits beside the column name rather than under it. On its own
    // line it made the header taller than the rows and pulled the columns out
    // of line, so the alignment is what gets pinned.
    const columns = await page.evaluate(() => {
      const left = (el: Element | null) => el
        ? [...el.children].map(child => Math.round(child.getBoundingClientRect().left))
        : null;
      return {
        header: left(document.querySelector('.oncall-list-header')),
        row: left(document.querySelector('.oncall-row')),
      };
    });
    expect(columns.header).not.toBeNull();
    if (columns.row) {
      // A row carries a border and padding the header does not, so its
      // columns start a pixel over rather than exactly on.
      for (const [i, headerLeft] of columns.header!.entries()) {
        expect(Math.abs(headerLeft - columns.row[i])).toBeLessThanOrEqual(2);
      }
    }
  });

  /**
   * The cadence reads as a sentence, and the modal is exactly as wide as those
   * sentences need at their longest. A weekly rotation is the case that sets
   * the width, and L2's copy of it is the tighter of the two - it sits a
   * section's padding deeper. One more word in a strip, or one more pixel on a
   * control, and they wrap again.
   */
  test('should keep both cadence sentences on one line', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    await oncallRow.locator('.edit-schedule-btn').click();
    await schedulesPage.expectScheduleModalVisible();

    // Weekly adds the handoff day, the clause that makes a sentence longest.
    const l2Checkbox = page.locator('#l2-enabled');
    if (!await l2Checkbox.isChecked()) {
      await l2Checkbox.check();
    }
    await page.locator('#l1-rotation-type').selectOption('weekly');
    await page.locator('#l2-rotation-type').selectOption('weekly');
    await expect(page.locator('.l1-weekly-only')).toBeVisible();
    await expect(page.locator('.l2-weekly-only')).toBeVisible();

    // A strip centres its controls, so items on one line share a vertical
    // centre even where their heights differ. Their top edges do not.
    const lines = await page.locator('.cadence-strip').evaluateAll(strips =>
      strips.map(strip => new Set([...strip.children].map(child => {
        const box = child.getBoundingClientRect();
        return Math.round(box.top + box.height / 2);
      })).size));
    expect(lines).toEqual([1, 1]);

    await schedulesPage.closeScheduleModal();
  });

  /**
   * A layer that is off has nothing to set, so the escalation timeout is a
   * number in a sentence rather than a control. Nothing here saves.
   */
  test('should show the L2 timeout only while the layer is on', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    await oncallRow.locator('.edit-schedule-btn').click();
    await schedulesPage.expectScheduleModalVisible();

    const l2Checkbox = page.locator('#l2-enabled');
    const timeout = page.locator('#l2-escalation-timeout');
    const asText = page.locator('.l2-timeout-static');

    if (!await l2Checkbox.isChecked()) {
      await l2Checkbox.check();
    }
    await expect(timeout).toBeVisible();
    await expect(asText).toBeHidden();

    await timeout.fill('12');
    await l2Checkbox.uncheck();
    await expect(timeout).toBeHidden();
    // What was typed, not what the modal opened with: otherwise switching the
    // layer off and on again would look like the edit had been discarded.
    await expect(asText).toHaveText('12');

    await l2Checkbox.check();
    await expect(timeout).toBeVisible();
    await expect(timeout).toHaveValue('12');

    await schedulesPage.closeScheduleModal();
  });

  /**
   * The L2 order is edited the way L1 is - pick to add, trash to remove - and
   * nothing here saves: the standing fixture belongs to every other test in
   * this file, and the behaviour under test is the editor's, not the API's.
   */
  test('should edit the L2 backup order', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    await oncallRow.locator('.edit-schedule-btn').click();
    await schedulesPage.expectScheduleModalVisible();

    const l2Checkbox = page.locator('#l2-enabled');
    if (!await l2Checkbox.isChecked()) {
      await l2Checkbox.check();
    }

    const picker = page.locator('#l2-add-user');
    await expect(picker).toBeVisible();

    // Two people the picker offers who are not already in the order. Two,
    // because removing one of two is what shows the rest renumbering.
    const before = await schedulesPage.getL2UserIds();
    const inOrder = new Set(before);
    const candidates = (await picker.locator('option').evaluateAll(
      opts => opts.map(o => (o as HTMLOptionElement).value)))
      .filter(value => value && !inOrder.has(value))
      .slice(0, 2);

    if (candidates.length < 2) {
      test.skip();
      return;
    }

    const [first, second] = candidates;
    await schedulesPage.addL2User(first);
    await schedulesPage.addL2User(second);
    await expect.poll(() => schedulesPage.getL2UserIds())
      .toEqual([...before, first, second]);

    // The picker keeps offering everyone; selecting a duplicate is refused
    // rather than appended, which is the L1 bargain.
    await schedulesPage.addL2User(first);
    await expect.poll(() => schedulesPage.getL2UserIds())
      .toEqual([...before, first, second]);

    // Positions are the order, so removing one renumbers what follows.
    await schedulesPage.removeL2User(before.length);
    await expect.poll(() => schedulesPage.getL2UserIds())
      .toEqual([...before, second]);
    expect(await schedulesPage.getL2Positions())
      .toEqual(Array.from({ length: before.length + 1 }, (_, i) => String(i + 1)));

    await schedulesPage.closeScheduleModal();
  });

  /**
   * The editor is bounded by the modal on a phone too, and the name of whoever
   * is in a group survives being there.
   *
   * Both were lost the same way: the flexible tracks are the ones carrying
   * names, so they are the ones that reach zero, and a tooltip 200px wide
   * anchored inside a 238px column pushed the rest sideways while invisible.
   */
  test('should stay inside the modal at 320px', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    await page.setViewportSize({ width: 320, height: 900 });
    await oncallRow.locator('.edit-schedule-btn').click();
    await schedulesPage.expectScheduleModalVisible();

    // By id: every modal in the app carries a .modal-body, and only this one
    // is the shell the schedule editor was rendered into.
    const overflow = await page.locator('#modal-body')
      .evaluate(body => body.scrollWidth - body.clientWidth);
    expect(overflow, 'the editor does not scroll sideways').toBeLessThanOrEqual(0);

    const chip = page.locator('#l1-groups-editor .user-chip').first();
    if (await chip.count() > 0) {
      const width = await chip.evaluate(el => Math.round(el.getBoundingClientRect().width));
      expect(width, 'the name in a group is still on screen').toBeGreaterThan(40);
    }

    // The track count, and not just the look of the row, because this is the
    // rule the cascade can undo without any visible sign. A media query has no
    // specificity of its own, so the same selector later in the file wins: the
    // chips still move to a line of their own - they are matched by a selector
    // nothing else claims - inside a grid that is quietly still five columns
    // wide, with the picker back in a track that can be squeezed to nothing.
    const tracks = await page.locator('#l1-groups-editor .group-row').first()
      .evaluate(row => getComputedStyle(row).gridTemplateColumns.split(/\s+/).length);
    expect(tracks, 'the row restacks to four tracks at this width').toBe(4);

    await schedulesPage.closeScheduleModal();
  });

  /**
   * What the rows say and what the save would send are two different claims,
   * and the one that matters is the second: the order on screen is only worth
   * anything if it is the order that leaves the browser. Reading the DOM back
   * cannot tell a broken serializer from a working one - collectConfig could
   * lose the selector, sort the ids or ignore a reorder, and every assertion
   * about the rows would still pass.
   *
   * Preview carries the exact payload the save would and writes nothing, so
   * the claim can be checked without touching the standing fixture.
   */
  test('should send the L2 order that is on screen', async ({ schedulesPage, page }) => {
    const oncallRow = await getOnCallRowWithSchedule(page);

    if (!oncallRow) {
      test.skip();
      return;
    }

    await oncallRow.locator('.edit-schedule-btn').click();
    await schedulesPage.expectScheduleModalVisible();

    const l2Checkbox = page.locator('#l2-enabled');
    if (!await l2Checkbox.isChecked()) {
      await l2Checkbox.check();
    }

    const picker = page.locator('#l2-add-user');
    const inOrder = new Set(await schedulesPage.getL2UserIds());
    const candidates = (await picker.locator('option').evaluateAll(
      opts => opts.map(o => (o as HTMLOptionElement).value)))
      .filter(value => value && !inOrder.has(value))
      .slice(0, 2);

    if (candidates.length < 2) {
      test.skip();
      return;
    }

    for (const id of candidates) {
      await schedulesPage.addL2User(id);
    }

    // Dragged rather than only added, because reordering is the part a DOM
    // assertion cannot vouch for: the rows move, and whether the serializer
    // reads them in their new order is exactly the open question.
    const rows = page.locator('#l2-users-list .group-row');
    const count = await rows.count();
    await page.dragAndDrop(
      `#l2-users-list .group-row:nth-child(${count}) .group-drag-handle`,
      '#l2-users-list .group-row:nth-child(1)');

    const onScreen = await schedulesPage.getL2UserIds();
    expect(onScreen[0], 'the dragged row moved to the top').toBe(candidates[1]);

    const previewRequest = page.waitForRequest(request =>
      request.url().includes('/schedule/preview') && request.method() === 'POST');
    await page.locator('#schedule-form-submit').click();
    const sent = (await previewRequest).postDataJSON();

    expect(sent.l2.user_ids).toEqual(onScreen);
    await expect(page.locator('.schedule-preview')).toBeVisible();

    // Left at the preview: confirming is what would write, and this test has
    // no business changing the schedule every other test reads.
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

    // Danger Zone title should be visible. Its own section class, not the
    // generic team one: the block is styled here rather than by inline rules
    // borrowed from another modal.
    const dangerZone = page.locator('.schedule-danger');
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
