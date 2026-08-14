import { test, expect } from '../../fixtures/auth.fixture';
import { Page } from '@playwright/test';
import { TeamFixtures } from '../../fixtures/team.fixture';

/**
 * The bugs the old structure produced, kept from coming back.
 *
 * Each of these was a symptom of the same thing: what a modal knew lived
 * beside the module rather than inside the open, so it survived the close.
 * The fix is structural, which is exactly why the tests have to be about
 * behaviour - a structure can be rearranged again.
 *
 * The other two of the four live in `override-editing.spec.ts`, which already
 * checks that every way out of an override opened from the calendar returns to
 * the calendar, and in `editor.spec.ts`, where Cancel is pressed after a round
 * trip through the preview.
 */

const MEMBERS = ['e2e-ann', 'e2e-ben'];

let fixtures: TeamFixtures;

test.beforeEach(async ({ page }) => {
  fixtures = new TeamFixtures(page);
});

test.afterEach(async () => {
  await fixtures.cleanup();
});

function config(groups: string[][], expectedVersion: number) {
  return {
    expected_version: expectedVersion,
    timezone: 'UTC',
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

async function seed(page: Page, prefix: string): Promise<string> {
  const teamId = await fixtures.team(prefix, MEMBERS);
  const saved = await page.request.put(`/api/v1/teams/${teamId}/schedule/config`, {
    data: config([['e2e-ann'], ['e2e-ben']], 0),
  });
  expect(saved.status(), await saved.text()).toBe(200);
  return teamId;
}

async function openOverrides(page: Page, teamId: string) {
  const button = page.locator(`.oncall-row[data-team-id="${teamId}"] .create-override-btn`);
  await button.waitFor({ state: 'visible' });
  await button.click();
  await page.locator('#override-form').waitFor({ state: 'visible' });
}

async function openEditor(page: Page, teamId: string) {
  const button = page.locator(`.oncall-row[data-team-id="${teamId}"] .edit-schedule-btn`);
  await button.waitFor({ state: 'visible' });
  await button.click();
  await page.locator('#schedule-form').waitFor({ state: 'visible' });
}

test.describe('what a closed modal leaves behind', () => {
  test.describe.configure({ timeout: 60_000 });

  // Berlin repeats 02:00-03:00 on 2026-10-25, so a time entered inside that
  // hour has to be resolved with a choice - which is the choice that used to
  // outlive the modal it was made in.
  test.use({ timezoneId: 'Europe/Berlin' });

  test('a fold chosen in one override is not chosen for the next', async ({ page }) => {
    const teamId = await seed(page, 'fold');
    await page.goto('/#/ops/oncall');
    await openOverrides(page, teamId);

    const note = page.locator('#override-time-note');
    await page.locator('#override-start').fill('2026-10-25T02:30');
    await page.locator('#override-end').fill('2026-10-25T05:30');
    await expect(note).toContainText('using the first one');

    // Taking the second pass of the repeated hour, deliberately.
    await note.locator('.override-fold-toggle').click();
    await expect(note).toContainText('using the second one');

    // Closed by the X, which is the way out that never reached the reset the
    // old code had to remember in three places.
    await page.locator('#modal-close').click();
    await expect(page.locator('#modal-overlay')).not.toHaveClass(/active/);

    await openOverrides(page, teamId);
    await page.locator('#override-start').fill('2026-10-25T02:30');
    await page.locator('#override-end').fill('2026-10-25T05:30');

    await expect(note,
      'a choice about one override says nothing about the next').toContainText('using the first one');
  });

  test('the preview does not survive the modal it was shown in', async ({ page }) => {
    const teamId = await seed(page, 'preview');
    await page.goto('/#/ops/oncall');
    await openEditor(page, teamId);

    await page.locator('#l1-handoff-time').fill('17:30');
    await page.locator('#schedule-form-submit').click();
    await page.locator('.schedule-preview').waitFor({ state: 'visible' });
    await expect(page.locator('#schedule-preview-host')).toHaveCount(1);

    // Backing out takes it with it.
    await page.locator('#preview-back').click();
    await page.locator('#schedule-form').waitFor({ state: 'visible' });
    await expect(page.locator('#schedule-preview-host')).toHaveCount(0);

    // So does saving.
    await page.locator('#schedule-form-submit').click();
    await page.locator('.schedule-preview').waitFor({ state: 'visible' });
    await page.locator('#preview-confirm').click();
    await expect(page.locator('#modal-overlay')).not.toHaveClass(/active/);
    await expect(page.locator('#schedule-preview-host')).toHaveCount(0);

    // And the next modal opens on a form, not on the last preview.
    await openEditor(page, teamId);
    await expect(page.locator('.schedule-preview')).toHaveCount(0);
    await expect(page.locator('#schedule-form')).toBeVisible();
  });

  test('the footer survives being hidden and shown twice', async ({ page }) => {
    const teamId = await seed(page, 'footer');
    await page.goto('/#/ops/oncall');
    await openEditor(page, teamId);

    // Twice, because the footer is hidden and restored rather than rewritten:
    // a second round trip through markup would leave two dead Save buttons and
    // a Cancel bound to a node that is no longer in the document.
    for (let round = 0; round < 2; round++) {
      await page.locator('#schedule-form-submit').click();
      await page.locator('.schedule-preview').waitFor({ state: 'visible' });
      await expect(page.locator('#preview-confirm')).toHaveCount(1);
      await page.locator('#preview-back').click();
      await page.locator('#schedule-form').waitFor({ state: 'visible' });
      await expect(page.locator('#preview-confirm')).toHaveCount(0);
    }

    await expect(page.locator('#schedule-form-submit')).toBeVisible();
    await page.locator('#schedule-cancel').click();
    await expect(page.locator('#modal-overlay')).not.toHaveClass(/active/);
  });
});
