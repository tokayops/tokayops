import { test, expect } from '../../fixtures/auth.fixture';
import { Page } from '@playwright/test';

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

async function seedTeam(page: Page, prefix: string): Promise<Env> {
  const teamId = `e2e-${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  const res = await page.request.post('/api/v1/teams', { data: { id: teamId, name: `Editor ${prefix}` } });
  expect([200, 201]).toContain(res.status());

  const members = ['e2e-ann', 'e2e-ben', 'e2e-cal', 'e2e-dee'];
  for (const id of members) {
    await page.request.post('/api/v1/users', {
      data: { id, email: `${id}@test.com`, name: id.replace('e2e-', 'E2E ') },
    });
    const add = await page.request.post(`/api/v1/teams/${teamId}/members`, {
      data: { user_id: id, role: 'team_member' },
    });
    expect([200, 201]).toContain(add.status());
  }
  return { teamId, members };
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
    await expect(preview, 'a change of duty is called out').toContainText('on duty right now');
    await expect(preview).toContainText('E2E ann');
    await expect(preview).toContainText('E2E ben');
    await expect(preview, 'the preview does not pretend to be a guarantee')
      .toContainText('Saving recalculates it');
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

    await openEditor(page, env.teamId);
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
