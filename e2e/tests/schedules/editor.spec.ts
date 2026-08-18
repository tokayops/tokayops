import { test, expect } from '../../fixtures/auth.fixture';
import { Page } from '@playwright/test';
import { TeamFixtures } from '../../fixtures/team.fixture';

/**
 * The editor, end to end.
 *
 * These are the scenarios the schedule work exists to make true: an edit that
 * adds someone to the group on duty must not restart the rotation, a preview
 * must not write, a stale form must be refused rather than silently win, and a
 * rejected save must leave nothing behind.
 */

interface Env {
  teamId: string;
  members: string[];
}

const MEMBERS = ['e2e-ann', 'e2e-ben', 'e2e-cal', 'e2e-dee'];

let fixtures: TeamFixtures;

test.beforeEach(async ({ page }) => {
  fixtures = new TeamFixtures(page);
});

test.afterEach(async () => {
  await fixtures.cleanup();
});

async function seedTeam(page: Page, prefix: string): Promise<Env> {
  return { teamId: await fixtures.team(prefix, MEMBERS), members: MEMBERS };
}

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

async function save(page: Page, teamId: string, body: any) {
  const res = await page.request.put(`/api/v1/teams/${teamId}/schedule/config`, { data: body });
  expect(res.status(), await res.text()).toBe(200);
  return res.json();
}

async function readConfig(page: Page, teamId: string) {
  return (await page.request.get(`/api/v1/teams/${teamId}/schedule/config`)).json();
}

async function onCall(page: Page, teamId: string) {
  return (await page.request.get(`/api/v1/teams/${teamId}/schedule/on-call`)).json();
}

/** Open the team's editor from the on-call overview. */
async function openEditor(page: Page, teamId: string) {
  await page.goto('/#/ops/oncall');
  const button = page.locator(`.oncall-row[data-team-id="${teamId}"] .edit-schedule-btn`);
  await button.waitFor({ state: 'visible' });
  await button.click();
  await page.locator('#schedule-form').waitFor({ state: 'visible' });
}

test.describe('Schedule editor', () => {
  // Each of these seeds a team, saves through the API and then drives the UI
  // over several screens. The default per-test budget is sized for a single
  // interaction, and running out of it here says nothing about the product.
  test.describe.configure({ timeout: 60_000 });

  /**
   * The scenario the epic was opened for.
   *
   * Groups [A], [B], [C] with B on duty. Adding D to B must leave B and D on
   * duty and C next - not restart the cycle at A, and not move the handoff.
   * The bug it replaces did exactly that, because the rotation's phase was
   * derived from data that any edit rewrote.
   */
  test('adding a person to the group on duty does not restart the rotation', async ({ page }) => {
    const env = await seedTeam(page, 'bug');

    // Start with one group so its member is unambiguously the one on duty,
    // then add the others around them. This pins who is on call without
    // waiting for a handoff to come round.
    await save(page, env.teamId, config([['e2e-ben']], 0));
    const first = await readConfig(page, env.teamId);
    const groupB = first.config.l1.groups[0].id;

    // [A], [B], [C] - keeping B's identity, so the rotation knows it is the
    // same group and stays on it.
    const three = config([['e2e-ann'], ['e2e-ben'], ['e2e-cal']], first.version);
    three.l1.groups[1].id = groupB;
    await save(page, env.teamId, three);

    const before = await onCall(page, env.teamId);
    expect(before.on_call.l1.user_ids).toEqual(['e2e-ben']);

    // Now the edit under test, made through the UI.
    await openEditor(page, env.teamId);

    const rowB = page.locator(`.group-row[data-group-id="${groupB}"]`);
    await expect(rowB, 'the group kept its identity across saves').toHaveCount(1);
    await rowB.locator('.group-add-user').selectOption('e2e-dee');

    await page.locator('#schedule-form-submit').click();
    const preview = page.locator('.schedule-preview');
    await preview.waitFor({ state: 'visible' });
    await expect(preview).toContainText('E2E dee');

    await page.locator('#preview-confirm').click();
    await expect(page.locator('#modal-overlay')).not.toHaveClass(/active/);

    const after = await onCall(page, env.teamId);
    expect(after.on_call.l1.user_ids.sort(), 'B and D are on duty, and the group did not change')
      .toEqual(['e2e-ben', 'e2e-dee']);
    expect(after.on_call.l1.group_id).toBe(groupB);

    // The handoff itself did not move: the shift still began where the grid
    // put it, while the new composition took effect at the edit.
    expect(new Date(after.on_call.l1.grid_slot_start).getTime())
      .toBeLessThan(new Date(after.on_call.l1.assignment_start).getTime());

    // And the next group up is still C.
    const until = new Date();
    until.setDate(until.getDate() + 3);
    const render = await (await page.request.get(
      `/api/v1/teams/${env.teamId}/schedule/render?from=${new Date().toISOString()}&until=${until.toISOString()}`)).json();
    const upcoming = render.entries.filter((e: any) => e.layer === 'l1');
    const nextDifferent = upcoming.find((e: any) => !e.user_ids.includes('e2e-ben'));
    expect(nextDifferent?.user_ids, 'C is next, the cycle did not restart at A').toEqual(['e2e-cal']);
  });

  test('the preview writes nothing', async ({ page }) => {
    const env = await seedTeam(page, 'preview');
    await save(page, env.teamId, config([['e2e-ann']], 0));
    const before = await readConfig(page, env.teamId);

    await openEditor(page, env.teamId);
    await page.locator('#l1-add-group').click();
    await page.locator('.group-row').last().locator('.group-add-user').selectOption('e2e-ben');
    await page.locator('#schedule-form-submit').click();
    await page.locator('.schedule-preview').waitFor({ state: 'visible' });

    const during = await readConfig(page, env.teamId);
    expect(during.version, 'a preview must not create a revision').toBe(before.version);
    expect(during.revision_id).toBe(before.revision_id);

    // Backing out leaves it untouched too.
    await page.locator('#preview-back').click();
    await page.locator('#schedule-form').waitFor({ state: 'visible' });
    const after = await readConfig(page, env.teamId);
    expect(after.version).toBe(before.version);
  });

  test('going back to the form keeps what was typed', async ({ page }) => {
    const env = await seedTeam(page, 'back');
    await save(page, env.teamId, config([['e2e-ann']], 0));

    await openEditor(page, env.teamId);

    // Values a person changes live in the elements, not in their attributes -
    // so a preview that rebuilt the form from its own markup would hand all of
    // this back as it was when the modal opened.
    await page.locator('#l1-handoff-time').fill('17:30');
    await page.locator('#slack-usergroup-id').fill('S99999');
    await page.locator('#schedule-reason').fill('cover for the release');
    await page.locator('#l2-enabled').check();
    await page.locator('#l1-add-group').click();
    await page.locator('.group-row').last().locator('.group-add-user').selectOption('e2e-ben');

    await page.locator('#schedule-form-submit').click();
    await page.locator('.schedule-preview').waitFor({ state: 'visible' });
    await page.locator('#preview-back').click();
    await page.locator('#schedule-form').waitFor({ state: 'visible' });

    await expect(page.locator('#l1-handoff-time')).toHaveValue('17:30');
    await expect(page.locator('#slack-usergroup-id')).toHaveValue('S99999');
    await expect(page.locator('#schedule-reason')).toHaveValue('cover for the release');
    await expect(page.locator('#l2-enabled')).toBeChecked();
    await expect(page.locator('.group-row')).toHaveCount(2);
  });

  test('cancel still works after coming back from the preview', async ({ page }) => {
    const env = await seedTeam(page, 'cancel');
    await save(page, env.teamId, config([['e2e-ann']], 0));

    await openEditor(page, env.teamId);
    await page.locator('#l1-add-group').click();
    await page.locator('.group-row').last().locator('.group-add-user').selectOption('e2e-ben');
    await page.locator('#schedule-form-submit').click();
    await page.locator('.schedule-preview').waitFor({ state: 'visible' });
    await page.locator('#preview-back').click();
    await page.locator('#schedule-form').waitFor({ state: 'visible' });

    // The footer buttons are the originals, hidden and shown again. Rebuilt
    // from markup they would look right and do nothing.
    await page.locator('#schedule-cancel').click();
    await expect(page.locator('#modal-overlay')).not.toHaveClass(/active/);
  });

  test('the reason typed before the preview is the reason recorded', async ({ page }) => {
    const env = await seedTeam(page, 'reason');
    await save(page, env.teamId, config([['e2e-ann']], 0));
    const before = await readConfig(page, env.teamId);

    await openEditor(page, env.teamId);
    await page.locator('#schedule-reason').fill('adding the new joiner');
    await page.locator('#l1-add-group').click();
    await page.locator('.group-row').last().locator('.group-add-user').selectOption('e2e-ben');

    await page.locator('#schedule-form-submit').click();
    await page.locator('.schedule-preview').waitFor({ state: 'visible' });
    await page.locator('#preview-confirm').click();
    await expect(page.locator('#modal-overlay')).not.toHaveClass(/active/);

    // The field is off screen by the time Confirm is pressed, which is how it
    // used to arrive empty.
    const revisions = await (await page.request.get(
      `/api/v1/teams/${env.teamId}/schedule/revisions`)).json();
    const latest = revisions.revisions.find((r: any) => r.version > before.version);
    expect(latest?.change_reason).toBe('adding the new joiner');
  });

  test('the preview says who is on duty before and after', async ({ page }) => {
    const env = await seedTeam(page, 'shows');
    await save(page, env.teamId, config([['e2e-ann']], 0));

    await openEditor(page, env.teamId);
    // Replace the single group's member, which changes who is on duty now.
    await page.locator('.group-row .user-chip .chip-remove').first().click();
    await page.locator('.group-row .group-add-user').first().selectOption('e2e-ben');
    await page.locator('#schedule-form-submit').click();

    const preview = page.locator('.schedule-preview');
    await preview.waitFor({ state: 'visible' });
    // Matched without regard to case: the banner opens with this sentence, so
    // its first letter is a matter of sentence casing, not of meaning.
    await expect(preview, 'a change of duty is called out').toContainText(/on duty right now changes/i);
    await expect(preview).toContainText('E2E ann');
    await expect(preview).toContainText('E2E ben');
    await expect(preview, 'the shift list holds handoffs, and one group never hands off')
      .toContainText('No handoffs in the previewed window');
    await expect(preview, 'the preview does not pretend to be a guarantee')
      .toContainText('Saving recalculates it');
  });

  test('the preview lists handoffs in the schedule zone', async ({ page }) => {
    const env = await seedTeam(page, 'zones');
    await save(page, env.teamId, config([['e2e-ann'], ['e2e-ben']], 0));

    await openEditor(page, env.teamId);
    await page.locator('#schedule-form-submit').click();
    const preview = page.locator('.schedule-preview');
    await preview.waitFor({ state: 'visible' });

    // The times are the schedule's (UTC here), not the reader's, and the zone
    // is named next to them: an unlabelled timestamp gets read in whatever
    // zone the reader is in.
    await expect(preview.locator('.preview-shifts-zone')).toHaveText('UTC');
    await expect(preview.locator('.preview-caveat')).toContainText('(UTC)');

    // Every row is a genuine handoff, so every row shows the configured hour.
    // The render window opens at "now", and the shift already in progress
    // used to come back clipped to that instant - a first row whose time was
    // the preview's own evaluated_at, under a heading promising what comes
    // next.
    const times = await preview.locator('.preview-shift-when').allTextContents();
    expect(times.length).toBeGreaterThan(0);
    for (const time of times) {
      expect(time, 'a handoff happens at the handoff hour').toContain('09:00');
    }
  });

  test('a form opened before someone else saved is refused', async ({ page }) => {
    const env = await seedTeam(page, 'stale');
    await save(page, env.teamId, config([['e2e-ann']], 0));

    await openEditor(page, env.teamId);

    // Someone else saves while this form is open.
    const current = await readConfig(page, env.teamId);
    await save(page, env.teamId, config([['e2e-cal']], current.version));

    await page.locator('#l1-add-group').click();
    await page.locator('.group-row').last().locator('.group-add-user').selectOption('e2e-ben');
    await page.locator('#schedule-form-submit').click();

    // The refusal comes at the preview: it evaluates against the version the
    // form was loaded at, and reports that it no longer matches.
    await expect(page.locator('#toast-container')).toContainText(/saved this schedule while you were editing/i);

    const after = await readConfig(page, env.teamId);
    expect(after.config.l1.groups[0].user_ids, 'the stale form did not overwrite the other save')
      .toEqual(['e2e-cal']);
  });

  test('a rejected save leaves nothing behind', async ({ page }) => {
    const env = await seedTeam(page, 'rollback');
    await save(page, env.teamId, config([['e2e-ann']], 0));
    const before = await readConfig(page, env.teamId);

    // Someone who is not in this team. The API is used directly because the
    // editor deliberately does not offer them - which is the point of the
    // separation, but leaves this rule untested from the UI alone.
    await page.request.post('/api/v1/users',
      { data: { id: 'e2e-outsider', email: 'e2e-outsider@test.com', name: 'E2E Outsider' } });

    const rejected = await page.request.put(`/api/v1/teams/${env.teamId}/schedule/config`, {
      data: config([['e2e-ann'], ['e2e-outsider']], before.version),
    });
    expect(rejected.status()).toBe(422);
    const body = await rejected.json();
    expect(body.code).toBe('user_not_team_member');
    expect(body.user_ids).toContain('e2e-outsider');

    const after = await readConfig(page, env.teamId);
    expect(after.version, 'a refused save must not consume a version').toBe(before.version);
    expect(after.config.l1.groups).toHaveLength(1);
  });

  test('an empty group is refused before anything is sent', async ({ page }) => {
    const env = await seedTeam(page, 'empty');
    await save(page, env.teamId, config([['e2e-ann']], 0));
    const before = await readConfig(page, env.teamId);

    await openEditor(page, env.teamId);
    await page.locator('#l1-add-group').click();
    await page.locator('#schedule-form-submit').click();

    await expect(page.locator('#toast-container')).toContainText(/nobody in it/i);
    await expect(page.locator('.schedule-preview'), 'it never got as far as a preview').toHaveCount(0);
    await expect(page.locator('.group-row.has-error')).toHaveCount(1);

    const after = await readConfig(page, env.teamId);
    expect(after.version).toBe(before.version);
  });

  test('a deleted schedule reopens as a recreate, prefilled', async ({ page }) => {
    const env = await seedTeam(page, 'recreate');
    await save(page, env.teamId, config([['e2e-ann'], ['e2e-ben']], 0));
    const created = await readConfig(page, env.teamId);

    const deleted = await page.request.delete(
      `/api/v1/teams/${env.teamId}/schedule?expected_version=${created.version}`);
    expect(deleted.status()).toBe(204);

    // Deleted is not "not configured": the calendar is still worth opening,
    // and the button says what saving would do. Asserted on the same page load
    // the editor is opened from - loading the on-call overview costs a request
    // per team, and doing it twice for one test is most of its runtime.
    await page.goto('/#/ops/oncall');
    const row = page.locator(`.oncall-row[data-team-id="${env.teamId}"]`);
    await row.waitFor({ state: 'visible' });
    await expect(row).toContainText('Deleted');
    await expect(row.locator('.view-schedule-btn')).toHaveCount(1);
    await expect(row.locator('.create-override-btn')).toHaveCount(0);

    await row.locator('.edit-schedule-btn').click();
    await page.locator('#schedule-form').waitFor({ state: 'visible' });
    await expect(page.locator('#recreate-banner')).toBeVisible();
    await expect(page.locator('.group-row'), 'the last configuration is offered back').toHaveCount(2);

    await page.locator('#schedule-form-submit').click();
    await page.locator('.schedule-preview').waitFor({ state: 'visible' });
    await page.locator('#preview-confirm').click();

    await expect(page.locator('#toast-container')).toContainText(/recreated/i);
    const after = await readConfig(page, env.teamId);
    expect(after.deleted_at).toBeFalsy();
    expect(after.schedule_id, 'a recreate reuses the same schedule record')
      .toBe(created.schedule_id);
  });

  test('history before the schedule existed is reported, not invented', async ({ page }) => {
    const env = await seedTeam(page, 'history');
    await save(page, env.teamId, config([['e2e-ann']], 0));

    // A range that starts well before this schedule was ever created.
    const from = new Date();
    from.setDate(from.getDate() - 30);
    const until = new Date();
    until.setDate(until.getDate() + 1);
    const render = await (await page.request.get(
      `/api/v1/teams/${env.teamId}/schedule/render?from=${from.toISOString()}&until=${until.toISOString()}`)).json();

    expect(render.history_complete).toBe(false);
    expect(render.warnings.map((w: any) => w.code)).toContain('history_incomplete');

    // And the calendar says so rather than drawing an empty month.
    await page.goto('/#/ops/oncall');
    const view = page.locator(`.oncall-row[data-team-id="${env.teamId}"] .view-schedule-btn`);
    await view.waitFor({ state: 'visible' });
    await view.click();
    await page.locator('.monthly-calendar, .schedule-empty').first().waitFor({ state: 'visible' });
  });

  test('the widget separates the handoff from the assignment', async ({ page }) => {
    const env = await seedTeam(page, 'bounds');
    await save(page, env.teamId, config([['e2e-ann']], 0));
    const created = await readConfig(page, env.teamId);

    // An edit mid-shift: the composition changes now, the shift does not.
    const edited = config([['e2e-ann', 'e2e-ben']], created.version);
    edited.l1.groups[0].id = created.config.l1.groups[0].id;
    await save(page, env.teamId, edited);

    const current = await onCall(page, env.teamId);
    expect(new Date(current.on_call.l1.grid_slot_start).getTime())
      .toBeLessThan(new Date(current.on_call.l1.assignment_start).getTime());

    await page.goto('/#/ops/oncall');
    const row = page.locator(`.oncall-row[data-team-id="${env.teamId}"]`);
    await row.waitFor({ state: 'visible' });
    await expect(row).toContainText('E2E ann');
    await expect(row).toContainText('E2E ben');
  });
});

test.describe('Overrides', () => {
  /**
   * The plain path: create one from the on-call page and see the modal close.
   *
   * The assertions after the toast are the point. A stray reference in the
   * save handler once threw after the override had been written and the
   * success toast shown - so the override existed, the API said so, and the
   * only visible symptom was a second toast and a modal that would not close.
   * A test that stopped at "created" and checked the API would have called
   * that a pass.
   */
  test('creating an override closes the modal and reports nothing else', async ({ page }) => {
    const env = await seedTeam(page, 'ovr');
    await save(page, env.teamId, config([['e2e-ann']], 0));

    await page.goto('/#/ops/oncall');
    const overrideBtn = page.locator(`.oncall-row[data-team-id="${env.teamId}"] .create-override-btn`);
    await overrideBtn.waitFor({ state: 'visible' });
    await overrideBtn.click();
    await page.locator('#override-form').waitFor({ state: 'visible' });

    await page.locator('#override-user').selectOption('e2e-ben');
    await page.locator('#override-reason').fill('swap');
    await page.locator('#modal-footer button[type="submit"]').click();

    await expect(page.locator('#toast-container')).toContainText(/created/i);
    await expect(page.locator('#modal-overlay'), 'the modal closes when the save succeeds')
      .not.toHaveClass(/active/, { timeout: 10_000 });
    await expect(page.locator('#toast-container'), 'and nothing reports a failure')
      .not.toContainText(/failed|error/i);

    const overrides = await (await page.request.get(
      `/api/v1/teams/${env.teamId}/schedule/overrides`)).json();
    expect(overrides.overrides).toHaveLength(1);
    expect(overrides.overrides[0].user_id).toBe('e2e-ben');
  });


  /**
   * The timezone control chooses how times are shown and entered. It is not
   * part of an override, which is stored as two absolute instants - so
   * changing it must re-render those instants, never move them.
   *
   * Before this was fixed, opening an override saved as 09:00Z, switching the
   * display from Moscow to UTC and saving without touching the times wrote
   * 12:00Z: the override silently rescheduled by three hours by a control that
   * claims to change nothing.
   */
  test('switching the display timezone does not move the override', async ({ page }) => {
    const env = await seedTeam(page, 'tz');
    await save(page, env.teamId, config([['e2e-ann']], 0));

    const validFrom = new Date(Date.now() + 24 * 3600 * 1000);
    validFrom.setUTCMinutes(0, 0, 0);
    const validTo = new Date(validFrom.getTime() + 4 * 3600 * 1000);

    const created = await page.request.post(`/api/v1/teams/${env.teamId}/schedule/overrides`, {
      data: {
        user_id: 'e2e-ben',
        valid_from: validFrom.toISOString(),
        valid_to: validTo.toISOString(),
        reason: 'cover',
      },
    });
    expect(created.status(), await created.text()).toBe(201);

    await page.goto('/#/ops/oncall');
    const overrideBtn = page.locator(`.oncall-row[data-team-id="${env.teamId}"] .create-override-btn`);
    await overrideBtn.waitFor({ state: 'visible' });
    await overrideBtn.click();
    await page.locator('#override-form').waitFor({ state: 'visible' });

    // Load it into the form, then change only the display zone.
    await page.locator('.edit-override-btn').first().click();
    const startBefore = await page.locator('#override-start').inputValue();

    // The dropdown is appended to the body, to escape the modal's clipping,
    // so it is not found under the picker's own container.
    await page.locator('#override-timezone .tz-picker-display').click();
    const dropdown = page.locator('.tz-picker-dropdown:visible');
    // Chatham, because the offset is unusual enough that a wrong conversion
    // would be obvious, and the engine lists it under exactly this name -
    // Chromium still calls Kathmandu "Asia/Katmandu", for instance.
    await dropdown.locator('.tz-picker-search').fill('Chatham');
    await dropdown.locator('.tz-picker-item').first().click();

    // The wall-clock digits move, because the same moment reads differently
    // in another zone. That is the point.
    const startAfter = await page.locator('#override-start').inputValue();
    expect(startAfter, 'the field re-renders in the new zone').not.toBe(startBefore);

    await page.locator('#modal-footer button[type="submit"]').click();
    await expect(page.locator('#toast-container')).toContainText(/updated/i);

    const overrides = await (await page.request.get(
      `/api/v1/teams/${env.teamId}/schedule/overrides`)).json();
    const head = overrides.overrides.find((o: any) => o.user_id === 'e2e-ben');
    expect(new Date(head.valid_from).toISOString(),
      'the stored instant is untouched').toBe(validFrom.toISOString());
    expect(new Date(head.valid_to).toISOString()).toBe(validTo.toISOString());
  });
});

test.describe('On-call overview', () => {
  /**
   * The page draws a row per team. Each row names two or three people, and
   * looking them up per row meant a request per team on a page that is opened
   * constantly. They are resolved once for the page now.
   *
   * The second assertion matters more than the first: batching must not make
   * the page fail as a whole. A team whose state cannot be read gets a row
   * saying so, and the rest of the page is unaffected - which is what the
   * per-row version already did.
   */
  test('resolves names once for the page, and survives a failing row', async ({ page }) => {
    const a = await seedTeam(page, 'ovw-a');
    const b = await seedTeam(page, 'ovw-b');
    const broken = await seedTeam(page, 'ovw-x');

    // a and b share a person on purpose: two rows that both load must name
    // them once between them, which is what a single pass buys. The earlier
    // version of this test shared the person with the row it then forced to
    // fail, so the id never reached the pass and nothing was deduplicated.
    await save(page, a.teamId, config([['e2e-ann']], 0));
    await save(page, b.teamId, config([['e2e-ann']], 0));
    await save(page, broken.teamId, config([['e2e-cal']], 0));

    const resolveCalls: string[][] = [];
    await page.route('**/api/v1/users/resolve', async route => {
      const body = route.request().postDataJSON() as { user_ids: string[] };
      resolveCalls.push(body.user_ids);
      await route.continue();
    });

    // A third team cannot be read. Its row has to say so without taking the
    // page with it.
    await page.route(`**/api/v1/teams/${broken.teamId}/schedule/on-call`, route =>
      route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"boom"}' }));

    await page.goto('/#/ops/oncall');
    for (const id of [a.teamId, b.teamId, broken.teamId]) {
      await page.locator(`.oncall-row[data-team-id="${id}"]`).waitFor({ state: 'visible' });
    }

    await expect(page.locator(`.oncall-row[data-team-id="${a.teamId}"]`)).toContainText('E2E ann');
    await expect(page.locator(`.oncall-row[data-team-id="${b.teamId}"]`)).toContainText('E2E ann');
    await expect(page.locator(`.oncall-row[data-team-id="${broken.teamId}"]`))
      .toContainText(/unavailable/i);

    // Exactly one pass. There are people on this page, so a pass must happen;
    // the directory splits at 500 ids and this page is nowhere near that.
    // Per-row resolution scored one request per team with a distinct set of
    // people - the standing fixture alone guarantees a second.
    expect(resolveCalls.length,
      `expected a single resolve pass, saw ${resolveCalls.length}`).toBe(1);

    // The person shared by two loaded rows is asked about once, not twice.
    //
    // Only ids this test controls are asserted on: the page also lists teams
    // left over from earlier tests, which cannot be deleted while they have
    // schedule history (TD10), and their people are in the pass too.
    const asked = resolveCalls[0];
    expect(asked.filter(id => id === 'e2e-ann'),
      'a person on two rows is asked about once').toHaveLength(1);
  });
});

test.describe('Schedule editor permissions', () => {
  /**
   * A viewer can see who is on duty and when. Editing, previewing and deleting
   * are not theirs, and the UI must not offer what the server will refuse -
   * a button that always fails is worse than no button.
   */
  test('a non-member sees the schedule but is offered no edits', async ({ page, browser, baseURL }) => {
    test.setTimeout(90000);
    const env = await seedTeam(page, 'perms');
    await save(page, env.teamId, config([['e2e-ann']], 0));

    const viewer = { id: 'e2e-viewer', email: 'e2e-viewer@test.com', name: 'E2E Viewer' };
    await page.request.post('/api/v1/users', { data: { ...viewer, password: 'Viewer123!' } });

    // A context made here inherits the project's options, including the saved
    // admin session - so the empty storage state is stated explicitly. Without
    // it this test would log in as nobody and quietly assert the admin's view.
    const context = await browser.newContext({
      baseURL,
      storageState: { cookies: [], origins: [] },
    });
    const viewerPage = await context.newPage();
    await viewerPage.goto('/login.html');
    await viewerPage.locator('#email').fill(viewer.email);
    await viewerPage.locator('#password').fill('Viewer123!');
    await viewerPage.locator('button[type="submit"]').click();
    await viewerPage.waitForURL(url => !url.pathname.includes('login.html'), { timeout: 30000 });

    await viewerPage.goto('/#/ops/oncall');
    const row = viewerPage.locator(`.oncall-row[data-team-id="${env.teamId}"]`);
    await row.waitFor({ state: 'visible' });

    await expect(row.locator('.view-schedule-btn'), 'viewing the calendar is allowed').toHaveCount(1);
    await expect(row.locator('.edit-schedule-btn'), 'configuring is not').toHaveCount(0);
    await expect(row.locator('.create-override-btn'), 'nor is creating an override').toHaveCount(0);

    await context.close();
  });
});
